// Package server：HTTP API（对应 server/api.py，单进程直跑引擎，不再 fork 子进程）
//
//   POST /chat          同步对话
//   POST /chat/stream   SSE 流式对话
//   POST /interject     中途插话（需 X-Interject-Key，见 .env 的 INTERJECT_KEY）
//   GET  /interject/status 插话开关与当前轮次
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
	"go-shell/internal/interject"
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

	// start 带上插话能力：客户端据此决定"流式期间想让模型改道时"走 /interject 还是重发
	fmt.Fprint(w, sseEvent("start", map[string]any{
		"interject": interject.Enabled(),
		"grace":     config.InterjectGrace,
	}))
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

// requestKey 取客户端验证串：优先请求头 X-Interject-Key，其次 body 的 key 字段
func requestKey(r *http.Request, bodyKey string) string {
	if v := strings.TrimSpace(r.Header.Get("X-Interject-Key")); v != "" {
		return v
	}
	return strings.TrimSpace(bodyKey)
}

// handlerInterject POST /interject：把一句话递进正在跑的那一轮
//
// 只有带对 8 位验证串（.env 的 INTERJECT_KEY）的客户端才能用；递进去的时机由引擎决定
// （工具边界），本接口最多等 INTERJECT_GRACE 秒拿结果：
//
//	{"status":"injected"} 已写进上下文，客户端不必再发新消息
//	{"status":"timeout"}  这一轮还没走到工具边界（多为卡在一条长命令上），请按新消息重发
//	{"status":"expired"}  这一轮已经结束，同上
//	{"status":"idle"}     当前没有正在跑的轮次，直接发新消息即可
func handlerInterject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"status": "error", "error": "只支持 POST", "code": "method_not_allowed"})
		return
	}
	var body struct {
		ID     string  `json:"id"`
		Text   string  `json:"text"`
		Key    string  `json:"key"`
		TurnID string  `json:"turn_id"`
		Wait   float64 `json:"wait"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, "请求体需为 JSON：{\"text\":\"...\"}")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		badRequest(w, "text 不能为空")
		return
	}

	// 开关与鉴权：只看 INTERJECT_KEY 是否 8 位、客户端是否带对
	if !interject.Enabled() {
		writeJSON(w, 403, map[string]any{
			"status": "error", "code": "interject_disabled",
			"error": fmt.Sprintf("服务端未开启中途插话（.env 的 INTERJECT_KEY 需为 %d 位）", interject.KeyLen),
		})
		return
	}
	if !interject.CheckKey(requestKey(r, body.Key)) {
		writeJSON(w, 403, map[string]any{
			"status": "error", "code": "interject_key_invalid",
			"error": "验证串不正确，客户端无权插话",
		})
		return
	}

	item, turnID := interject.PushTo(body.TurnID, body.ID, body.Text)
	if item == nil {
		writeJSON(w, 409, map[string]any{
			"status": "idle", "code": "no_active_turn",
			"error": "当前没有正在运行的对话轮次，请直接发新消息",
		})
		return
	}

	grace := time.Duration(config.InterjectGrace) * time.Second
	if body.Wait > 0 {
		grace = time.Duration(body.Wait * float64(time.Second))
	}
	status := item.Wait(r.Context(), grace)
	writeJSON(w, 200, map[string]any{
		"status": status, "id": item.ID, "turn_id": turnID,
	})
}

// handlerInterjectStatus GET /interject/status：客户端用它决定要不要走插话这条路
func handlerInterjectStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"enabled": interject.Enabled(),
		"key_len": interject.KeyLen,
		"grace":   config.InterjectGrace,
		"turn_id": interject.Active(),
	})
}

// Run 启动 HTTP 服务
func Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handlerHealth)
	mux.HandleFunc("/chat", handlerChat)
	mux.HandleFunc("/chat/stream", handlerStream)
	mux.HandleFunc("/interject", handlerInterject)
	mux.HandleFunc("/interject/status", handlerInterjectStatus)

	if err := chat.EnsureDB(events.NewTerminal()); err != nil {
		return fmt.Errorf("数据库初始化失败：%v", err)
	}

	addr := fmt.Sprintf("%s:%d", config.ServerHost, config.ServerPort)
	fmt.Printf("shell-tool API (go) 监听 http://%s  （SERVER_HOST=%s SERVER_PORT=%d）\n",
		addr, config.ServerHost, config.ServerPort)
	switch {
	case interject.Enabled():
		fmt.Printf("中途插话：已开启（POST /interject，需 X-Interject-Key；等待上限 %ds）\n", config.InterjectGrace)
	case config.InterjectKey != "":
		fmt.Printf("中途插话：已关闭（INTERJECT_KEY 长度不是 %d 位）\n", interject.KeyLen)
	default:
		fmt.Printf("中途插话：未配置（在 .env 里设 INTERJECT_KEY=%s 开启）\n", strings.Repeat("x", interject.KeyLen))
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return srv.ListenAndServe()
}
