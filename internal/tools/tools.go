// Package tools：原生 function 工具 + 工具调用分发器（对应 core/tools/__init__.py + system_tools.py）
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"go-shell/internal/config"
	"go-shell/internal/envelope"
	"go-shell/internal/events"
)

var skillRe = regexp.MustCompile(`(?i)^#\s*skill\s*:\s*(\S+)\s*$`)

// ToolsSchema 返回工具定义列表（与 Python 版完全一致；skill_hub 描述含当前仓库）
func ToolsSchema() []map[string]any {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name":        "get_system_info",
			"description": "获取当前操作系统的详细信息，包括系统类型、版本、架构等。适用于了解运行环境。",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
		}},
		{"type": "function", "function": map[string]any{
			"name":        "execute_system_command",
			"description": "执行系统命令（支持 Windows PowerShell/CMD 和 Linux/macOS bash）。注意：命令需要是当前操作系统支持的格式。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "要执行的系统命令，例如：'ls -la' (Linux/macOS) 或 'dir' (Windows)",
					},
				},
				"required": []any{"command"},
			},
		}},
		{"type": "function", "function": map[string]any{
			"name":        "skill",
			"description": "skill 合集（由 skill/ 目录脚本自动注册）。all=true 列出所有 skill 及说明；skill='名称' 查看用法；skill='名称' 并传 arguments={参数} 执行。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"all":       map[string]any{"type": "boolean", "description": "设为 true 时列出所有已注册 skill 及说明"},
					"skill":     map[string]any{"type": "string", "description": "要查询或执行的 skill 名称；完整列表见会话中的 skill 上下文或用 all=true 获取"},
					"arguments": map[string]any{"type": "object", "description": `执行参数对象（如 {"action": "search", "query": "..."}）；不传则返回该 skill 用法`},
				},
				"required": []any{},
			},
		}},
		{"type": "function", "function": map[string]any{
			"name":        "skill_hub",
			"description": "从远程技能仓库（GitHub / Gitea）查看 / 下载 / 卸载 skill，默认仓库 " + HubSource() + "。action='list' 列出远程可装技能（含平台/依赖）；action='install' 并传 name 下载安装到本地 skill/；action='remove' 并传 name 卸载。下载即信任其代码（会在进程内执行）。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "description": "操作：list（默认，列出远程技能）/ install（安装）/ remove（卸载）"},
					"name":   map[string]any{"type": "string", "description": "技能名，action=install/remove 时必填"},
				},
				"required": []any{},
			},
		}},
	}
	if config.WebSearch {
		tools = append(tools, map[string]any{"type": "web_search"})
	}
	return tools
}

// ---------- 原生工具实现 ----------

// GetSystemInfo 获取系统信息
func GetSystemInfo() map[string]any {
	info := map[string]any{
		"os":            runtime.GOOS,
		"os_release":    "",
		"machine":       runtime.GOARCH,
		"processor":     "",
		"go_version":    runtime.Version(),
		"python_version": "",
	}
	if runtime.GOOS == "windows" {
		info["os"] = "Windows"
	} else {
		info["os"] = strings.Title(runtime.GOOS) //nolint:staticcheck
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					info["os_release"] = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
					break
				}
			}
		}
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "model name") {
					if i := strings.Index(line, ":"); i >= 0 {
						info["processor"] = strings.TrimSpace(line[i+1:])
					}
					break
				}
			}
		}
	}
	return info
}

// ExecuteSystemCommand 执行系统命令并返回结果（信封 JSON 字符串）
func ExecuteSystemCommand(command string) string {
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("powershell.exe"); err == nil {
			cmdArgs = []string{"powershell.exe", "-Command", command}
		} else {
			cmdArgs = []string{"cmd.exe", "/c", command}
		}
	} else {
		if _, err := exec.LookPath("bash"); err == nil {
			cmdArgs = []string{"/bin/bash", "-c", command}
		} else {
			cmdArgs = []string{"/bin/sh", "-c", command}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3600*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdout = &syncBuffer{}
	cmd.Stderr = &syncBuffer{}
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return envelope.Error("执行失败 - "+err.Error(), "internal")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		// 超时：杀整棵进程树
		killTree(cmd.Process)
		<-done
		return envelope.Error("命令执行超时（1小时）", "timeout")
	}

	// toUTF8：中文 Windows 上 cmd/PowerShell 输出的是 GBK 字节，直接当 UTF-8
	// 用会变成乱码（终端显示与模型上下文同时被毁）
	stdout := normalizeCRLF(toUTF8(cmd.Stdout.(*syncBuffer).String()))
	stderr := normalizeCRLF(toUTF8(cmd.Stderr.(*syncBuffer).String()))
	returnCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			returnCode = ee.ExitCode()
			if returnCode < 0 {
				returnCode = 1
			}
		} else {
			returnCode = 1
		}
	}
	output := strings.TrimSpace(stdout)
	if output == "" {
		output = strings.TrimSpace(stderr)
	}
	if output == "" {
		output = "命令执行成功（无输出）"
	}
	truncated := false
	if len(output) > 6000 {
		output = output[:6000] + fmt.Sprintf("\n... (输出被截断，原长度 %d 字符)", len(output))
		truncated = true
	}
	return envelope.Ok(map[string]any{
		"output": output, "exit_code": returnCode, "truncated": truncated,
	})
}

// normalizeCRLF 规范化子进程输出的换行符：\r\n → \n，孤立 \r → \n。
// Windows 下 powershell/where.exe 等输出 CRLF；若 \r 残留进信封，终端渲染时
// 每行末尾的 \r 会把光标拉回列 0，随后的填充空格从行首覆盖，把内容左半截
// 盖没、边框字符顶到行中间（表现为路径缺左边、散落竖线）。孤立 \r（进度条
// 刷新）按换行处理，避免更新内容挤成一行。
func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// syncBuffer 并发安全字节缓冲
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------- 工具分发 ----------

// ParseToolArguments 解析工具调用参数 JSON，对 LLM 常见输出瑕疵容错
func ParseToolArguments(raw any) (map[string]any, string) {
	switch v := raw.(type) {
	case map[string]any:
		return v, ""
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			text = "{}"
		}
		var out map[string]any
		if json.Unmarshal([]byte(text), &out) == nil {
			return out, ""
		}
		// 去除尾随逗号
		trailing := regexp.MustCompile(`,(\s*[}\]])`)
		cleaned := trailing.ReplaceAllString(text, "$1")
		if json.Unmarshal([]byte(cleaned), &out) == nil {
			return out, ""
		}
		// 补全未闭合括号
		opens := strings.Count(cleaned, "{") + strings.Count(cleaned, "[")
		closes := strings.Count(cleaned, "}") + strings.Count(cleaned, "]")
		if opens > closes {
			if json.Unmarshal([]byte(cleaned+strings.Repeat("}", opens-closes)), &out) == nil {
				return out, ""
			}
		}
		return nil, fmt.Sprintf("参数不是合法 JSON（已尝试容错修复仍失败）: %.200s", v)
	}
	return map[string]any{}, ""
}

// ProcessToolCalls 并发执行工具调用，返回 tool 结果消息列表
func ProcessToolCalls(ctx context.Context, sink events.Sink, toolCalls []map[string]any) []map[string]any {
	if len(toolCalls) == 0 {
		return nil
	}
	results := make([]map[string]any, len(toolCalls))
	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		wg.Add(1)
		go func(i int, tc map[string]any) {
			defer wg.Done()
			results[i] = executeSingleTool(ctx, sink, tc)
		}(i, tc)
	}
	wg.Wait()
	return results
}

func executeSingleTool(ctx context.Context, sink events.Sink, tc map[string]any) map[string]any {
	fn, _ := tc["function"].(map[string]any)
	functionName, _ := fn["name"].(string)
	rawArgs := stringOf(fn["arguments"])
	callID := stringOf(tc["id"])

	arguments, errStr := ParseToolArguments(fn["arguments"])
	if errStr != "" {
		events.ToolCall(sink, functionName, nil, rawArgs, callID, errStr)
		return map[string]any{
			"role": "tool", "tool_call_id": callID,
			"content": fmt.Sprintf("Error: 工具 %s 参数不是合法 JSON，无法执行 - %s", functionName, errStr),
		}
	}
	events.ToolCall(sink, functionName, arguments, "", callID, "")

	resultStr := runTool(ctx, sink, functionName, arguments)
	events.ToolResult(sink, functionName, callID, resultStr, false, strings.HasPrefix(resultStr, "{\"status\": \"error") || strings.Contains(resultStr[:min(len(resultStr), 20)], "error"))
	return map[string]any{"role": "tool", "tool_call_id": callID, "content": resultStr}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// orDefault 空串兜底
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func runTool(ctx context.Context, sink events.Sink, name string, args map[string]any) string {
	switch name {
	case "get_system_info":
		return envelope.Ok(GetSystemInfo())
	case "execute_system_command":
		return ExecuteSystemCommand(stringOf(args["command"]))
	case "skill":
		all := false
		if b, ok := args["all"].(bool); ok {
			all = b
		}
		skillName := stringOf(args["skill"])
		arguments, hasArgs := args["arguments"]
		return SkillTool(all, skillName, arguments, hasArgs)
	case "skill_hub":
		return SkillHub(stringOf(args["action"]), stringOf(args["name"]))
	default:
		return fmt.Sprintf("Error: 未知工具 %s", name)
	}
}
