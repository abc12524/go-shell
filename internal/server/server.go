// Package server：HTTP API（对应 server/api.py，单进程直跑引擎，不再 fork 子进程）
//
//   POST /chat          同步对话
//   POST /chat/stream   SSE 流式对话
//   GET  /health        健康检查
//
// 启动：go-shell server；监听地址取 .env 的 SERVER_HOST（127.0.0.1 / 0.0.0.0）与 SERVER_PORT。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go-shell/internal/chat"
	"go-shell/internal/config"
	"go-shell/internal/events"
)

// sseEvent 构造 SSE 事件块
func sseEvent(eventType string, fields map[string]any) string {
	payload := map[string]any{"type": eventType}
	for k, v := range fields {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b) + "\n\n"
}

// parseRequest 解析请求体为 Options
func parseRequest(r *http.Request) (*chat.Options, error) {
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil || data == nil {
		return nil, fmt.Errorf("请提供 question 参数")
	}
	opts := &chat.Options{}
	if v, ok := data["new"].(bool); ok {
		opts.New = v
	}
	if v, ok := data["key"].(string); ok && v != "" {
		opts.APIKey = v
	}
	if v, ok := data["model"].(string); ok && v != "" {
		opts.Model = v
	}
	if v, ok := data["question"].(string); ok {
		opts.Question = strings.TrimSpace(v)
	}
	// args 列表复用 CLI 解析（-n/-s/-k/-m 等）
	if rawArgs, ok := data["args"].([]any); ok {
		var argv []string
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				argv = append(argv, s)
			}
		}
		if len(argv) > 0 {
			parsed, errMsg := chat.ParseArgs(argv)
			if errMsg != "" {
				return nil, fmt.Errorf("%s", errMsg)
			}
			if !opts.New {
				opts.New = parsed.New
			}
			if opts.APIKey == "" {
				opts.APIKey = parsed.APIKey
			}
			if opts.Model == "" {
				opts.Model = parsed.Model
			}
			if opts.SessionID == "" {
				opts.SessionID = parsed.SessionID
			}
			opts.ListSessions = opts.ListSessions || parsed.ListSessions
			opts.ShowSkills = opts.ShowSkills || parsed.ShowSkills
			if opts.HubAction == "" {
				opts.HubAction = parsed.HubAction
				opts.HubNames = parsed.HubNames
			}
			if opts.Question == "" {
				opts.Question = parsed.Question
			}
		}
	}
	if tips, ok := data["tips"].(string); ok && tips != "" {
		chat.ApplyRequestEnv(tips, "")
	}
	if openid, ok := data["openid"].(string); ok && openid != "" {
		chat.ApplyRequestEnv("", openid)
	}
	return opts, nil
}

func validateQuestion(opts *chat.Options) string {
	if opts.Question == "" && !opts.New && opts.SessionID == "" &&
		!opts.ListSessions && !opts.ShowSkills && opts.HubAction == "" {
		return "question 不能为空"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, 400, map[string]any{"status": "error", "error": msg, "code": "bad_request"})
}

// runAction 处理非对话动作（列会话/skill/hub），返回输出文本
func runAction(opts *chat.Options) (string, bool) {
	switch {
	case opts.ShowSkills:
		return chat.SkillsText(), true
	case opts.HubAction != "":
		text, code := chat.HubText(opts.HubAction, opts.HubNames)
		return text, code == 0
	case opts.ListSessions:
		return chat.ListSessionsText(), true
	}
	return "", false
}

// handlerChat POST /chat：同步对话（引擎在本进程内执行）
func handlerChat(w http.ResponseWriter, r *http.Request) {
	opts, err := parseRequest(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if msg := validateQuestion(opts); msg != "" {
		badRequest(w, msg)
		return
	}

	// 非对话动作：执行后直接返回文本
	if opts.Question == "" && (opts.ShowSkills || opts.HubAction != "" || opts.ListSessions || opts.New) {
		if opts.New && opts.HubAction == "" && !opts.ShowSkills && !opts.ListSessions {
			// 只开新会话不对话
			if err := chat.EnsureDB(nil); err != nil {
				writeJSON(w, 500, map[string]any{"status": "error", "error": err.Error(), "code": "internal"})
				return
			}
			_, _, errMsg := chat.ResolveSession(opts)
			if errMsg != "" {
				writeJSON(w, 400, map[string]any{"status": "error", "error": errMsg, "code": "bad_request"})
				return
			}
			writeJSON(w, 200, map[string]any{"status": "ok", "result": map[string]any{
				"question": "", "reply": "", "error": nil,
			}})
			return
		}
		text, ok := runAction(opts)
		status := 200
		if !ok {
			status = 500
		}
		writeJSON(w, status, map[string]any{"status": "ok", "result": map[string]any{
			"question": "", "reply": text, "error": nil,
		}})
		return
	}

	sink := events.NewChannel(256)
	done := make(chan error, 1)
	var reply string
	go func() {
		var err error
		reply, err = chat.RunTurn(r.Context(), opts, sink)
		sink.Close()
		done <- err
	}()

	// 聚合事件为纯文本（content/reasoning/log）
	var parts []string
	for evt := range sink.C {
		switch evt["t"] {
		case "content":
			if c, ok := evt["content"].(string); ok {
				parts = append(parts, c)
			}
		case "reasoning":
			if c, ok := evt["content"].(string); ok {
				parts = append(parts, c)
			}
		case "log":
			if c, ok := evt["text"].(string); ok {
				parts = append(parts, c)
			}
		}
	}
	if err := <-done; err != nil {
		writeJSON(w, 500, map[string]any{"status": "error", "error": err.Error(), "code": "internal"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "result": map[string]any{
		"question": opts.Question, "reply": strings.Join(parts, ""), "error": nil,
	}})
	_ = reply
}

// handlerStream POST /chat/stream：SSE 流式对话
func handlerStream(w http.ResponseWriter, r *http.Request) {
	opts, err := parseRequest(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if msg := validateQuestion(opts); msg != "" {
		badRequest(w, msg)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"status": "error", "error": "streaming unsupported", "code": "internal"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	fmt.Fprint(w, sseEvent("start", nil))
	flusher.Flush()

	// 非对话动作：一次性输出文本为 log 事件后 done
	if opts.Question == "" && (opts.ShowSkills || opts.HubAction != "" || opts.ListSessions) {
		text, ok := runAction(opts)
		if !ok {
			fmt.Fprint(w, sseEvent("error", map[string]any{"error": text, "code": "process_error"}))
		} else {
			fmt.Fprint(w, sseEvent("log", map[string]any{"text": text}))
			fmt.Fprint(w, sseEvent("done", nil))
		}
		flusher.Flush()
		return
	}

	sink := events.NewChannel(256)
	done := make(chan error, 1)
	go func() {
		_, err := chat.RunTurn(r.Context(), opts, sink)
		sink.Close()
		done <- err
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	runErr := error(nil)
streamLoop:
	for {
		select {
		case evt, open := <-sink.C:
			if !open {
				break streamLoop
			}
			kind, _ := evt["t"].(string)
			fields := map[string]any{}
			for k, v := range evt {
				if k != "t" {
					fields[k] = v
				}
			}
			fmt.Fprint(w, sseEvent(kind, fields))
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			break streamLoop // 客户端断开：引擎 ctx 同步取消，终止 LLM 请求
		}
	}
	runErr = <-done
	if runErr != nil {
		fmt.Fprint(w, sseEvent("error", map[string]any{"error": runErr.Error(), "code": "process_error"}))
	} else {
		fmt.Fprint(w, sseEvent("done", nil))
	}
	flusher.Flush()
}

func handlerHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "shell-tool-api"})
}

// Run 启动 HTTP 服务
func Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handlerHealth)
	mux.HandleFunc("/chat", handlerChat)
	mux.HandleFunc("/chat/stream", handlerStream)

	if err := chat.EnsureDB(events.NewTerminal()); err != nil {
		return fmt.Errorf("数据库初始化失败：%v", err)
	}

	addr := fmt.Sprintf("%s:%d", config.ServerHost, config.ServerPort)
	fmt.Printf("shell-tool API (go) 监听 http://%s  （SERVER_HOST=%s SERVER_PORT=%d）\n",
		addr, config.ServerHost, config.ServerPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return srv.ListenAndServe()
}
