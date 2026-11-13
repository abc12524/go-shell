// skill 注册表：扫描 skill/ 目录自动注册 + 执行（对应 core/tools/skill_tool.py）
//
// Go 版差异：skill 脚本仍是 Python（保持与 shell-skills 仓库兼容），执行时通过
// runner.py 子进程导入 skill.<名称>.skill 并调用 run(arguments)，协议不变。
package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go-shell/internal/config"
	"go-shell/internal/envelope"
)

// SkillMeta 解析出的 skill 元数据
type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Usage       string `json:"usage"`
	Path        string `json:"-"`
}

var (
	skillDir string

	skillNameRe    = regexp.MustCompile(`(?i)^#\s*skill\s*:\s*(\S+)\s*$`)
	descRe         = regexp.MustCompile(`(?i)^#\s*description\s*:\s*(.*)$`)
	statusRe       = regexp.MustCompile(`(?i)^#\s*status\s*:\s*(\S+)\s*$`)
	usageRe        = regexp.MustCompile(`(?i)^#\s*usage\s*:\s*(.*)$`)
	hashPrefixRe   = regexp.MustCompile(`^#\s?`)
	skillLineRe    = regexp.MustCompile("^- `([^`]+)`: ")
	disabledValues = map[string]bool{"disable": true, "disabled": true, "off": true, "false": true, "0": true}
)

func SkillDir() string {
	if skillDir == "" {
		skillDir = config.SkillDirCfg // Load 已按 ProjectRoot 解析为绝对路径
		if skillDir == "" {
			skillDir = filepath.Join(config.ProjectRoot, "skill")
		}
	}
	return skillDir
}

func isSkillEnabled(meta *SkillMeta) bool {
	v := strings.ToLower(strings.TrimSpace(meta.Status))
	if v == "" {
		v = "enable"
	}
	return !disabledValues[v]
}

// parseSkillHeader 静态解析脚本头部
func parseSkillHeader(path string) *SkillMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	name := ""
	description := ""
	status := "enable"
	var usageLines []string
	inUsage := false
	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		if i >= 300 {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			if name != "" && inUsage {
				usageLines = append(usageLines, "")
			}
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		if m := skillNameRe.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name == "" {
			continue
		}
		if m := descRe.FindStringSubmatch(line); m != nil && !inUsage {
			description = strings.TrimSpace(m[1])
			continue
		}
		if m := statusRe.FindStringSubmatch(line); m != nil && !inUsage {
			status = strings.ToLower(strings.TrimSpace(m[1]))
			continue
		}
		if m := usageRe.FindStringSubmatch(line); m != nil {
			inUsage = true
			if rest := strings.TrimSpace(m[1]); rest != "" {
				usageLines = append(usageLines, rest)
			}
			continue
		}
		if inUsage {
			usageLines = append(usageLines, hashPrefixRe.ReplaceAllString(line, ""))
		}
	}
	if name == "" {
		return nil
	}
	return &SkillMeta{
		Name: name, Description: description, Status: status,
		Usage: strings.TrimSpace(strings.Join(usageLines, "\n")), Path: path,
	}
}

// ScanRegistry 扫描 skill/ 目录（每次调用重扫；目录规模小，无性能问题）
func ScanRegistry() map[string]*SkillMeta {
	registry := map[string]*SkillMeta{}
	entries, err := os.ReadDir(SkillDir())
	if err != nil {
		return registry
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || !e.IsDir() {
			continue
		}
		entry := filepath.Join(SkillDir(), name, "skill.py")
		if meta := parseSkillHeader(entry); meta != nil {
			registry[meta.Name] = meta
		}
	}
	return registry
}

// DisclosedSkills 对 LLM 披露的 skill：name -> 说明
func DisclosedSkills() map[string]string {
	out := map[string]string{}
	for name, meta := range ScanRegistry() {
		if isSkillEnabled(meta) {
			out[name] = meta.Description
		}
	}
	return out
}

// SkillsOverview 全部 skill：name -> (status, description)
func SkillsOverview() map[string][2]string {
	out := map[string][2]string{}
	for name, meta := range ScanRegistry() {
		out[name] = [2]string{meta.Status, meta.Description}
	}
	return out
}

// SetStatus 改写 skill 脚本头部的 # status:
func SetStatus(name, status string) (bool, string) {
	value := strings.ToLower(strings.TrimSpace(status))
	if value != "enable" && value != "disable" {
		return false, fmt.Sprintf("status 只能是 enable 或 disable（收到 '%s'）", status)
	}
	registry := ScanRegistry()
	meta, ok := registry[name]
	if !ok {
		names := registryNames(registry)
		return false, fmt.Sprintf("未知 skill '%s'，可用: %s", name, names)
	}
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return false, "读取失败 - " + err.Error()
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	lines := strings.Split(text, "\n")
	end := 0
	for end < len(lines) {
		trimmed := strings.TrimSpace(lines[end])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		end++
	}
	insertAt := -1
	statusLineRe := regexp.MustCompile(`(?i)^#\s*status\s*:`)
	for i := 0; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if statusLineRe.MatchString(trimmed) {
			lines[i] = "# status: " + value
			insertAt = -2
			break
		}
		if insertAt == -1 && descRe.MatchString(trimmed) {
			insertAt = i + 1
		}
	}
	if insertAt == -1 {
		for i := 0; i < end; i++ {
			if skillNameRe.MatchString(strings.TrimSpace(lines[i])) {
				insertAt = i + 1
				break
			}
		}
	}
	if insertAt == -1 {
		insertAt = end
	}
	if insertAt >= 0 {
		lines = append(lines[:insertAt], append([]string{"# status: " + value}, lines[insertAt:]...)...)
	}
	if err := os.WriteFile(meta.Path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return false, "写入失败 - " + err.Error()
	}
	return true, fmt.Sprintf("skill '%s' 的 status 已设为 %s（新会话生效）", name, value)
}

func registryNames(registry map[string]*SkillMeta) string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// executeSkill 子进程执行 skill（python3 <内嵌runner.py> <root> <module> <json>）
func executeSkill(name string, arguments map[string]any) string {
	registry := ScanRegistry()
	if _, ok := registry[name]; !ok {
		return envelope.Error(fmt.Sprintf("未知 skill '%s'，可用: %s", name, registryNames(registry)), "unknown_skill")
	}
	runnerPath, cleanup, err := writeRunner()
	if err != nil {
		return envelope.Error("skill runner 准备失败 - "+err.Error(), "internal")
	}
	defer cleanup()
	argsJSON, _ := json.Marshal(arguments)
	// runner 把 argv[1] 加进 sys.path 后按包路径导入模块。默认 skill 目录就叫
	// skill/，模块路径 skill.<名>.skill；配置了自定义 SKILL_DIR 时以目录名作
	// 包前缀（如 my_skills.<名>.skill），并把该目录的父目录加入 sys.path。
	// 目录名会当 Python 包名用，建议字母/数字/下划线。
	skillPath := SkillDir()
	sysPathDir := filepath.Dir(skillPath)
	module := filepath.Base(skillPath) + "." + name + ".skill"
	cmdArgs := []string{
		runnerPath,
		sysPathDir, module, string(argsJSON),
	}
	cmd := exec.Command(config.PythonBin, cmdArgs...)
	// cwd 跟着 skill 根走：新布局下配置根（含 go.config 与 data/）就是 sysPathDir，
	// skill 里用相对路径找 data 时能命中；老部署 sysPathDir 仍是二进制目录，行为不变
	cmd.Dir = sysPathDir
	env := os.Environ()
	// skill 子进程的 stdout/stderr 是管道，中文 Windows 上 Python 会退回 cp936(GBK)：
	// 输出 emoji 直接 UnicodeEncodeError，subprocess(text=True) 解非 ASCII 也会崩。
	// 强制 UTF-8 模式——PYTHONUTF8 还让 getpreferredencoding() 返回 utf-8，
	// 连 skill 自己再起子进程的解码一并修正，不必每个 skill 各自兜编码。
	env = setEnv(env, "PYTHONUTF8", "1")
	env = setEnv(env, "PYTHONIOENCODING", "utf-8")
	// PYTHON_BIN 带目录（如 /root/.venv/bin/python3）时，把解释器所在目录前置到
	// 子进程 PATH：skill 内再调 python / pip 装的 CLI 工具时可命中同一环境
	if dir := filepath.Dir(config.PythonBin); dir != "." && dir != "" {
		env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	// skill 脚本若是 Windows 上的 python（或调了 cmd 工具），输出同样可能是 GBK
	outText := toUTF8(string(out))
	if err != nil {
		msg := strings.TrimSpace(outText)
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return envelope.Error(fmt.Sprintf("skill %s 执行失败 - %s", name, msg), "internal")
	}
	result := strings.TrimSpace(normalizeCRLF(outText))
	if result == "" {
		return envelope.Error(fmt.Sprintf("skill %s 无输出", name), "internal")
	}
	return result
}

// getUsage 返回某个 skill 的用法
func getUsage(name string) string {
	registry := ScanRegistry()
	meta, ok := registry[name]
	if !ok {
		return envelope.Error(fmt.Sprintf("未知 skill '%s'，可用: %s", name, registryNames(registry)), "unknown_skill")
	}
	return envelope.Ok(map[string]any{
		"skill": name, "description": meta.Description, "usage": meta.Usage,
	})
}

// SkillTool skill 合集入口：查询或执行
func SkillTool(all bool, skillName string, arguments any, hasArgs bool) string {
	if all {
		skills := DisclosedSkills()
		return envelope.Ok(map[string]any{"count": len(skills), "skills": skills})
	}
	if skillName != "" {
		if hasArgs && arguments != nil {
			args, ok := arguments.(map[string]any)
			if !ok {
				args = map[string]any{}
			}
			return executeSkill(skillName, args)
		}
		return getUsage(skillName)
	}
	disclosed := make([]string, 0, 0)
	for n := range DisclosedSkills() {
		disclosed = append(disclosed, n)
	}
	sort.Strings(disclosed)
	return "skill 合集（由 skill/ 目录脚本自动注册）：\n" +
		"  - 列出全部 skill：传 all=true\n" +
		"  - 查看某个 skill 用法：传 skill='名称'（不传 arguments）\n" +
		"  - 执行某个 skill：传 skill='名称' 并传 arguments={参数}\n" +
		"已注册 skill：" + orDefault(strings.Join(disclosed, ", "), "(空)")
}

// setEnv 覆盖式设置环境变量：同名只保留一份，避免重复键在子进程里行为不一致
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}
