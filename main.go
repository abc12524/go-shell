// go-shell — shell-tool 的 Go 实现（单二进制）
//
// 用法：
//   go-shell [选项] [问题]        本机对话（同 Python dp.py）
//   go-shell server              远程服务端模式：启动 HTTP API（SERVER_HOST/SERVER_PORT）
//   go-shell -h <别名> [问题]     远程客户端模式：连远端 server，内容在本终端输出
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go-shell/internal/chat"
	"go-shell/internal/client"
	"go-shell/internal/config"
	"go-shell/internal/console"
	"go-shell/internal/events"
	"go-shell/internal/llm"
	"go-shell/internal/server"
)

// exampleConfig 配置模板随二进制走：全新机器上只有 go-shell 一个文件，
// 首次启动也能在 ~/.config/go-shell/go.config 生成一份完整可用的配置。
//
//go:embed .env.example
var exampleConfig string

// pullConfigFlag 预扫描 -c/--config 指定配置文件（支持 `-c 路径` 与 `-c=路径`），
// 返回剔除该选项后的参数。须在 config.Load 与 server 子命令判定之前执行。
func pullConfigFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintln(os.Stderr, "-c/--config 需要 <配置文件路径>，例如：go-shell -c /etc/go-shell.env server")
				os.Exit(1)
			}
			config.ConfigOverride = args[i+1]
			i++
		case strings.HasPrefix(a, "-c="), strings.HasPrefix(a, "--config="):
			config.ConfigOverride = strings.TrimPrefix(strings.TrimPrefix(a, "--config="), "-c=")
		default:
			out = append(out, a)
		}
	}
	return out
}

func main() {
	// Windows 控制台默认按 GBK(936) 解释字节流，而本程序全程输出 UTF-8：
	// 先把代码页切到 65001 并打开 ANSI 转义，否则中文与彩色全是乱码
	restoreConsole := console.Init()
	defer restoreConsole()

	args := pullConfigFlag(os.Args[1:])
	config.SetExampleConfig(exampleConfig)
	config.Load()
	if len(args) > 0 && (args[0] == "server" || args[0] == "serve") {
		if err := server.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "服务退出：%v\n", err)
			os.Exit(1)
		}
		return
	}

	opts, errMsg := chat.ParseArgs(args)
	if errMsg != "" {
		t := events.NewTerminal()
		events.Warning(t, errMsg, "")
		os.Exit(1)
	}

	// -h/--host：远程客户端模式（引擎在远端，本终端只做渲染）
	if opts.Remote != "" {
		runRemote(opts)
		return
	}

	// -S/--skill 无参数：列出全部 skill
	if opts.ShowSkills {
		fmt.Print(chat.SkillsText())
		return
	}
	// -S/--skill：开关某 skill 的 status
	if opts.SkillStatus[0] != "" {
		ok, message := chat.SetStatus(opts.SkillStatus[0], opts.SkillStatus[1])
		fmt.Println(message)
		if !ok {
			os.Exit(1)
		}
		return
	}
	// -I/-D：远程 skill 管理
	if opts.HubAction != "" {
		text, code := chat.HubText(opts.HubAction, opts.HubNames)
		fmt.Print(text)
		if code != 0 {
			os.Exit(1)
		}
		return
	}

	// -s 无参数：只列出最近会话
	if opts.ListSessions {
		sink := events.NewTerminal()
		if err := chat.EnsureDB(sink); err != nil {
			fmt.Fprintf(os.Stderr, "数据库初始化失败：%v\n", err)
			os.Exit(1)
		}
		fmt.Print(chat.ListSessionsText())
		return
	}

	// -n 不带 question：只开新会话
	if opts.Question == "" {
		if opts.New {
			sink := events.NewTerminal()
			if err := chat.EnsureDB(sink); err != nil {
				fmt.Fprintf(os.Stderr, "数据库初始化失败：%v\n", err)
				os.Exit(1)
			}
			sessionID, isNew, errMsg := chat.ResolveSession(opts)
			if errMsg != "" {
				events.Warning(sink, errMsg, "")
				os.Exit(1)
			}
			events.Session(sink, map[bool]string{true: "new", false: "resume"}[isNew], sessionID)
			fmt.Printf("已开启新会话：%s\n", sessionID)
			return
		}
		fmt.Print(chat.UsageText)
		return
	}

	// 对话
	sink := events.NewTerminal()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reply, err := chat.RunTurn(ctx, opts, sink)
	sink.Flush()
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\n已中断")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "调用失败：%v\n", err)
		// 只有状态码类错误才值得「稍等重试」；DNS / 证书 / 网络不通再这么说会误导成密钥问题。
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 429 || apiErr.Status >= 500) {
			fmt.Fprintln(os.Stderr, "若为服务繁忙（503/429），稍等片刻重试即可；也可在 .env 中切换 DEEPSEEK_BASE_URL / DEEPSEEK_MODEL 使用备用服务商。")
		}
		os.Exit(1)
	}
	_ = reply
}

// runRemote 远程客户端模式：连远端 /chat/stream，事件在本终端渲染。
// 本进程不碰本地数据库、不需要 DEEPSEEK_API_KEY（一切都在远端执行）。
func runRemote(opts *chat.Options) {
	hasAction := opts.ShowSkills || opts.HubAction != "" || opts.ListSessions || opts.SkillStatus[0] != ""
	if opts.Question == "" && !opts.New && !hasAction {
		fmt.Print(chat.UsageText)
		return
	}
	tgt, err := config.ResolveRemote(opts.Remote)
	if err != nil {
		events.Warning(events.NewTerminal(), err.Error(), "remote_config")
		os.Exit(1)
	}
	if opts.APIKey != "" || opts.Model != "" {
		fmt.Fprintln(os.Stderr, "提示：远程模式不改远端配置，-k/-m 已忽略（到远端配置文件里改）")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sink := events.NewTerminal()
	fmt.Printf("→ 远程 %s\n", tgt.Label())
	if err := client.Ping(ctx, tgt); err != nil {
		sink.Flush()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	req := client.Request{Question: opts.Question, New: opts.New, Args: chat.RemoteArgs(opts)}
	err = client.Run(ctx, tgt, req, sink, os.Stdout)
	sink.Flush()
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\n已中断")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "远程调用失败：%v\n", err)
		os.Exit(1)
	}
}
