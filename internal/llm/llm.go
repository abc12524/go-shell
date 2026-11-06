// Package llm：API 调用层：流式请求 + 工具调用循环（对应 core/llm.py）
//
// - responses：DeepSeek 官方 Responses API（默认；支持服务端内置 web_search）
// - chat：OpenAI 兼容 Chat Completions API（OpenCode Zen / OpenCode Go 等网关）
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go-shell/internal/config"
	"go-shell/internal/db"
	"go-shell/internal/events"
	"go-shell/internal/interject"
	"go-shell/internal/recall"
	"go-shell/internal/tools"
)

// Usage 归一化后的 token 用量
type Usage struct {
	PromptTokens         int64
	CompletionTokens     int64
	PromptCacheHitTokens int64
	PromptCacheMissTokens int64
	TotalTokens          int64
	ReasoningTokens      int64
}

// StreamResult 单次流式请求结果（两协议签名一致）
type StreamResult struct {
	Content     string
	Reasoning   string
	ToolCalls   []map[string]any
	OutputItems []map[string]any
	Usage       *Usage
}

var transientStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// ---------- 消息格式转换 ----------

// toResponsesInput 聊天格式 → Responses API input items
func toResponsesInput(messages []map[string]any) []map[string]any {
	var items []map[string]any
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == db.InsertUserRole {
			role = "user"
		}
		switch role {
		case "system", "user":
			items = append(items, map[string]any{"role": role, "content": stringOf(m["content"])})
		case "assistant":
			if outputItems, ok := m["output_items"].([]map[string]any); ok && len(outputItems) > 0 {
				for _, oi := range outputItems {
					items = append(items, oi)
				}
			} else {
				if content := stringOf(m["content"]); content != "" {
					items = append(items, map[string]any{"role": "assistant", "content": content})
				}
				if reasoning := stringOf(m["reasoning_content"]); reasoning != "" {
					items = append(items, map[string]any{
						"type": "reasoning",
						"content": []map[string]any{{"type": "reasoning_text", "text": reasoning}},
					})
				}
				for _, tc := range toolCallList(m["tool_calls"]) {
					fn, _ := tc["function"].(map[string]any)
					items = append(items, map[string]any{
						"type": "function_call", "call_id": tc["id"],
						"name": fn["name"], "arguments": stringOf(fn["arguments"]),
					})
				}
			}
		case "tool":
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": m["tool_call_id"],
				"output": stringOf(m["content"]),
			})
		}
	}
	return items
}

// toResponsesTools 聊天格式 tool schema → Responses API 格式
func toResponsesTools(tools []map[string]any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		ttype, _ := t["type"].(string)
		if ttype == "function" {
			fn, _ := t["function"].(map[string]any)
			item := map[string]any{"type": "function", "name": fn["name"]}
			if d, ok := fn["description"]; ok && d != nil {
				item["description"] = d
			}
			if p, ok := fn["parameters"]; ok && p != nil {
				item["parameters"] = p
			}
			out = append(out, item)
		} else if ttype == "web_search" {
			out = append(out, map[string]any{"type": "web_search"})
		}
	}
	return out
}

// toChatMessages 聊天格式 → Chat Completions 请求消息
func toChatMessages(messages []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == db.InsertUserRole {
			role = "user"
		}
		switch role {
		case "system", "user":
			out = append(out, map[string]any{"role": role, "content": stringOf(m["content"])})
		case "assistant":
			msg := map[string]any{"role": "assistant", "content": stringOf(m["content"])}
			if reasoning := stringOf(m["reasoning_content"]); reasoning != "" {
				msg["reasoning_content"] = reasoning
			}
			if tcs := toolCallList(m["tool_calls"]); len(tcs) > 0 {
				var serialized []map[string]any
				for _, tc := range tcs {
					fn, _ := tc["function"].(map[string]any)
					serialized = append(serialized, map[string]any{
						"id": tc["id"], "type": "function",
						"function": map[string]any{"name": fn["name"], "arguments": stringOf(fn["arguments"])},
					})
				}
				msg["tool_calls"] = serialized
			}
			out = append(out, msg)
		case "tool":
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": m["tool_call_id"], "content": stringOf(m["content"]),
			})
		}
	}
	return out
}

// toChatTools 仅保留 function 工具
func toChatTools(tools []map[string]any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		if t["type"] == "function" {
			out = append(out, t)
		}
	}
	return out
}

func toolCallList(v any) []map[string]any {
	if list, ok := v.([]map[string]any); ok {
		return list
	}
	return nil
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---------- HTTP ----------

func apiURL(path string) string {
	base := strings.TrimRight(config.BaseURL, "/")
	if strings.HasSuffix(base, "/"+path) {
		return base
	}
	return base + "/" + path
}

func headers(sessionID string) (map[string]string, error) {
	if config.APIKey == "" {
		return nil, fmt.Errorf("DEEPSEEK_API_KEY 未设置")
	}
	h := map[string]string{
		"Authorization": "Bearer " + config.APIKey,
		"Content-Type":  "application/json",
	}
	if config.UserAgent != "" {
		h["User-Agent"] = config.UserAgent
	}
	if config.SessionHeader != "" && sessionID != "" {
		h[config.SessionHeader] = sessionID
	}
	return h, nil
}

var errMessageRe = regexp.MustCompile(`"message"\s*:\s*"([^"]+)"`)

// APIError 服务端返回非 200 的错误（带状态码）。
// 上层据此区分「服务繁忙，稍后重试」与「配置 / 网络问题」，不必再猜错误文案。
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("API %d: %s", e.Status, e.Msg) }

// dialHint 针对常见的连接层失败补一句可操作的提示；认不出来就返回空串，不瞎猜。
func dialHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "server misbehaving"),
		strings.Contains(msg, "lookup ") && strings.Contains(msg, ":53"):
		return "\n疑似 DNS 解析失败：检查本机 DNS 设置；Android/Termux 上纯 Go 二进制还需可读的 /etc/resolv.conf" +
			"（可 proot -b $PREFIX/etc/resolv.conf:/etc/resolv.conf 运行）"
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return "\n疑似根证书缺失：Android/Termux 上设置 SSL_CERT_FILE=$PREFIX/etc/tls/cert.pem"
	case strings.Contains(msg, "network is unreachable"), strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "handshake timeout"),
		strings.Contains(msg, "connection refused"):
		return "\n检查网络连通性与代理设置（HTTP_PROXY / HTTPS_PROXY）"
	}
	return ""
}

// postStream 发起流式 POST（带瞬时错误重试与 stream_options 容错）
func postStream(ctx context.Context, path string, hdrs map[string]string, payload map[string]any, sink events.Sink) (io.ReadCloser, error) {
	url := apiURL(path)
	attempts := config.MaxAPIAttempts
	for attempt := 1; attempt <= attempts; attempt++ {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
		if err != nil {
			return nil, fmt.Errorf("API 请求异常: %v", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		client := &http.Client{Timeout: 0} // 流式：不设总超时，依赖 ctx
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("API 连接失败：%v%s", err, dialHint(err))
		}
		if resp.StatusCode == 200 {
			return resp.Body, nil
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyText := string(raw)
		status := resp.StatusCode
		if strings.Contains(strings.ToLower(bodyText), "stream_options") && payload["stream_options"] != nil {
			delete(payload, "stream_options")
			continue
		}
		if transientStatus[status] && attempt < attempts {
			delay := time.Duration(1<<uint(attempt)) * time.Second
			events.Warning(sink, fmt.Sprintf("API %d（服务繁忙/限流），%ds 后自动重试（第 %d/%d 次）",
				status, int(delay.Seconds()), attempt, attempts-1), "api_retry")
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		msg := strings.TrimSpace(bodyText)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if m := errMessageRe.FindStringSubmatch(bodyText); m != nil {
			msg = m[1]
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return nil, &APIError{Status: status, Msg: msg}
	}
	return nil, fmt.Errorf("API 请求失败")
}

// iterSSE 逐条解析 SSE 的 data: 行
func iterSSE(reader io.ReadCloser) chan map[string]any {
	ch := make(chan map[string]any, 32)
	go func() {
		defer close(ch)
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			dataStr := strings.TrimSpace(line[5:])
			if dataStr == "[DONE]" {
				return
			}
			var evt map[string]any
			if json.Unmarshal([]byte(dataStr), &evt) == nil {
				ch <- evt
			}
		}
	}()
	return ch
}

// ---------- usage 归一化 ----------

func normalizeUsage(usage map[string]any) *Usage {
	if usage == nil {
		return nil
	}
	in := intOf(usage["input_tokens"])
	out := intOf(usage["output_tokens"])
	var cached int64
	if d, ok := usage["input_tokens_details"].(map[string]any); ok {
		cached = intOf(d["cached_tokens"])
	}
	var reasoning int64
	if d, ok := usage["output_tokens_details"].(map[string]any); ok {
		reasoning = intOf(d["reasoning_tokens"])
	}
	return &Usage{
		PromptTokens: in, CompletionTokens: out,
		PromptCacheHitTokens: cached, PromptCacheMissTokens: in - cached,
		TotalTokens: in + out, ReasoningTokens: reasoning,
	}
}

func normalizeChatUsage(usage map[string]any) *Usage {
	if usage == nil {
		return nil
	}
	in := intOf(usage["prompt_tokens"])
	out := intOf(usage["completion_tokens"])
	var hit, miss int64
	hasHit := false
	hasMiss := false
	if v, ok := usage["prompt_cache_hit_tokens"]; ok {
		hit = intOf(v)
		hasHit = true
	}
	if v, ok := usage["prompt_cache_miss_tokens"]; ok {
		miss = intOf(v)
		hasMiss = true
	}
	if !hasHit {
		var cached int64
		if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			cached = intOf(d["cached_tokens"])
		}
		hit = cached
		miss = in - cached
	} else if !hasMiss {
		miss = in - hit
	}
	var reasoning int64
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoning = intOf(d["reasoning_tokens"])
	}
	total := intOf(usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return &Usage{
		PromptTokens: in, CompletionTokens: out,
		PromptCacheHitTokens: hit, PromptCacheMissTokens: miss,
		TotalTokens: total, ReasoningTokens: reasoning,
	}
}

func intOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// AddUsage 累加 usage
func AddUsage(total, u *Usage) *Usage {
	if u == nil {
		return total
	}
	if total == nil {
		total = &Usage{}
	}
	total.PromptTokens += u.PromptTokens
	total.CompletionTokens += u.CompletionTokens
	total.PromptCacheHitTokens += u.PromptCacheHitTokens
	total.PromptCacheMissTokens += u.PromptCacheMissTokens
	total.TotalTokens += u.TotalTokens
	total.ReasoningTokens += u.ReasoningTokens
	return total
}

// ---------- 流式实现 ----------

// StreamResponsesAPI 调用 Responses API 流式接口
func StreamResponsesAPI(ctx context.Context, messages []map[string]any, sink events.Sink, sessionID string) (*StreamResult, error) {
	instructions, inputItems := splitInstructions(messages)

	if config.DebugSendSeq {
		debugSeq(inputItems, "role", "type")
	}

	payload := map[string]any{
		"model":       config.Model,
		"input":       inputItems,
		"tools":       toResponsesTools(tools.ToolsSchema()),
		"tool_choice": "auto",
		"stream":      true,
	}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	hdrs, err := headers(sessionID)
	if err != nil {
		return nil, err
	}
	body, err := postStream(ctx, "responses", hdrs, payload, sink)
	if err != nil {
		return nil, err
	}

	result := &StreamResult{}
	fcByIdx := map[string]*map[string]any{}
	searchShown := false

	for ev := range iterSSE(body) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ctype, _ := ev["type"].(string)
		switch {
		case ctype == "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			itype, _ := item["type"].(string)
			if itype == "function_call" {
				idx := fmt.Sprint(ev["output_index"])
				fcByIdx[idx] = &map[string]any{
					"id": item["call_id"], "name": item["name"], "arguments": "",
				}
			} else if itype == "web_search_call" {
				id, _ := item["id"].(string)
				events.Search(sink, "call", id, !searchShown)
				searchShown = true
			}
		case strings.HasPrefix(ctype, "response.web_search_call."):
			events.Search(sink, ctype[len("response.web_search_call."):], "", false)
		case ctype == "response.function_call_arguments.delta":
			idx := fmt.Sprint(ev["output_index"])
			if fc, ok := fcByIdx[idx]; ok {
				m := *fc
				m["arguments"] = stringOf(m["arguments"]) + stringOf(ev["delta"])
			}
		case ctype == "response.reasoning_text.delta":
			if delta := stringOf(ev["delta"]); delta != "" {
				result.Reasoning += delta
				events.Emit(sink, map[string]any{"t": "reasoning", "content": delta})
			}
		case ctype == "response.output_text.delta":
			if delta := stringOf(ev["delta"]); delta != "" {
				result.Content += delta
				events.Emit(sink, map[string]any{"t": "content", "content": delta})
			}
		case ctype == "response.completed":
			final, _ := ev["response"].(map[string]any)
			if u, ok := final["usage"].(map[string]any); ok {
				result.Usage = normalizeUsage(u)
			}
			result.OutputItems = extractOutputItems(final["output"])
		case ctype == "response.failed":
			resp, _ := ev["response"].(map[string]any)
			errObj, _ := resp["error"].(map[string]any)
			errMsg := ""
			if errObj != nil {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
			}
			if errMsg == "" {
				errMsg = fmt.Sprint(errObj)
			}
			return nil, fmt.Errorf("Responses API 请求失败: %s", errMsg)
		}
	}

	// 按 output_index 排序收集 function_call
	result.ToolCalls = collectCalls(fcByIdx)
	return result, nil
}

// StreamChatCompletions 调用 OpenAI 兼容 Chat Completions 流式接口
func StreamChatCompletions(ctx context.Context, messages []map[string]any, sink events.Sink, sessionID string) (*StreamResult, error) {
	chatMessages := toChatMessages(messages)
	payload := map[string]any{
		"model":          config.Model,
		"messages":       chatMessages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	chatTools := toChatTools(tools.ToolsSchema())
	if len(chatTools) > 0 {
		payload["tools"] = chatTools
		payload["tool_choice"] = "auto"
	}
	if config.DebugSendSeq {
		debugSeq(chatMessages, "role", "")
	}
	hdrs, err := headers(sessionID)
	if err != nil {
		return nil, err
	}
	body, err := postStream(ctx, "chat/completions", hdrs, payload, sink)
	if err != nil {
		return nil, err
	}

	result := &StreamResult{}
	toolAcc := map[int]*map[string]any{}

	for chunk := range iterSSE(body) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			result.Usage = normalizeChatUsage(u)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if r := stringOf(delta["reasoning_content"]); r != "" {
			result.Reasoning += r
			events.Emit(sink, map[string]any{"t": "reasoning", "content": r})
		}
		if c := stringOf(delta["content"]); c != "" {
			result.Content += c
			events.Emit(sink, map[string]any{"t": "content", "content": c})
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcv := range tcs {
				tc, _ := tcv.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if v, ok := tc["index"].(float64); ok {
					idx = int(v)
				}
				slot := toolAcc[idx]
				if slot == nil {
					m := map[string]any{"id": "", "name": "", "arguments": ""}
					slot = &m
					toolAcc[idx] = slot
				}
				m := *slot
				if id := stringOf(tc["id"]); id != "" {
					m["id"] = id
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if n := stringOf(fn["name"]); n != "" {
						m["name"] = n
					}
					if a := stringOf(fn["arguments"]); a != "" {
						m["arguments"] = stringOf(m["arguments"]) + a
					}
				}
			}
		}
	}

	// 按 index 排序收集
	idxs := make([]int, 0, len(toolAcc))
	for i := range toolAcc {
		idxs = append(idxs, i)
	}
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	for _, i := range idxs {
		result.ToolCalls = append(result.ToolCalls, *toolAcc[i])
	}
	return result, nil
}

// StreamLLM 按 config.APIType 分发
func StreamLLM(ctx context.Context, messages []map[string]any, sink events.Sink, sessionID string) (*StreamResult, error) {
	if config.APIType == "chat" {
		return StreamChatCompletions(ctx, messages, sink, sessionID)
	}
	return StreamResponsesAPI(ctx, messages, sink, sessionID)
}

func splitInstructions(messages []map[string]any) (string, []map[string]any) {
	items := toResponsesInput(messages)
	instructions := ""
	if len(items) > 0 {
		if role, _ := items[0]["role"].(string); role == "system" {
			instructions, _ = items[0]["content"].(string)
			items = items[1:]
		}
	}
	return instructions, items
}

func extractOutputItems(output any) []map[string]any {
	list, ok := output.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		if t != "message" && t != "reasoning" && t != "web_search_call" && t != "function_call" {
			continue
		}
		cleaned := map[string]any{}
		for k, v := range m {
			if v != nil {
				cleaned[k] = v
			}
		}
		out = append(out, cleaned)
	}
	return out
}

func collectCalls(fcByIdx map[string]*map[string]any) []map[string]any {
	idxs := make([]int, 0, len(fcByIdx))
	for k := range fcByIdx {
		if i, err := strconv.Atoi(k); err == nil {
			idxs = append(idxs, i)
		}
	}
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	var out []map[string]any
	for _, i := range idxs {
		fc := *fcByIdx[strconv.Itoa(i)]
		out = append(out, map[string]any{
			"id": fc["id"], "type": "function",
			"function": map[string]any{"name": fc["name"], "arguments": fc["arguments"]},
		})
	}
	return out
}

func debugSeq(items []map[string]any, roleKey, typeKey string) {
	var parts []string
	for _, m := range items {
		role := "?"
		if roleKey != "" {
			if r, ok := m[roleKey].(string); ok && r != "" {
				role = r
			}
		}
		if typeKey != "" {
			if t, ok := m[typeKey].(string); ok && t != "" && role == "?" {
				role = t
			}
		}
		blob, _ := json.Marshal(m)
		parts = append(parts, fmt.Sprintf("%s:%s", role, fmt.Sprintf("%x", md5Short(blob))))
	}
	fmt.Println("DBGSEQ> " + strings.Join(parts, " | "))
}

func md5Short(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h & 0xffffff
}

// ---------- 工具调用循环 ----------

var forceFinalWarning = "<system-reminder>\n<user_warning>\n本轮已达到工具调用次数上限，请不要再调用工具，直接基于已有信息给出最终回答。\n</user_warning>\n</system-reminder>"

// BuildAssistantMsg 构建可序列化的 assistant 消息
func BuildAssistantMsg(content, reasoning string, toolCalls []map[string]any, outputItems []map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(outputItems) > 0 {
		msg["output_items"] = outputItems
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// persist 统一持久化出口：写本地库 + 同步 OV 会话
func persist(sessionID, ovSessionID string, msgs []map[string]any) {
	if sessionID != "" {
		db.AppendMessages(sessionID, msgs)
	}
	recall.OpenVikingCapture(ovSessionID, msgs)
}

// ChatCompletionWithTools 工具调用循环（协议无关）
func ChatCompletionWithTools(ctx context.Context, messages []map[string]any, sink events.Sink, sessionID, ovSessionID string) (
	content, reasoning string, total *Usage, assistantMsg map[string]any, allToolResults, newHistoryMsgs []map[string]any, err error,
) {
	res, err := StreamLLM(ctx, messages, sink, sessionID)
	if err != nil {
		return "", "", nil, nil, nil, nil, err
	}
	total = AddUsage(total, res.Usage)
	var webSearchCalls []map[string]any
	for _, oi := range res.OutputItems {
		if t, _ := oi["type"].(string); t == "web_search_call" {
			webSearchCalls = append(webSearchCalls, oi)
		}
	}
	assistantMsg = BuildAssistantMsg(res.Content, res.Reasoning, res.ToolCalls, res.OutputItems)
	content, reasoning = res.Content, res.Reasoning

	// 无工具调用 → 直接返回最终回答
	if len(res.ToolCalls) == 0 && len(webSearchCalls) == 0 {
		persist(sessionID, ovSessionID, []map[string]any{assistantMsg})
		return content, reasoning, total, assistantMsg, nil, []map[string]any{assistantMsg}, nil
	}

	toolRounds := 0
	forcedFinal := false
	for len(res.ToolCalls) > 0 || len(webSearchCalls) > 0 {
		toolRounds++
		if toolRounds > config.MaxToolRounds {
			forcedFinal = true
			break
		}
		events.ToolRound(sink, toolRounds, len(res.ToolCalls), len(webSearchCalls))

		toolResults := tools.ProcessToolCalls(ctx, sink, res.ToolCalls)
		allToolResults = append(allToolResults, toolResults...)
		newHistoryMsgs = append(newHistoryMsgs, assistantMsg)
		newHistoryMsgs = append(newHistoryMsgs, toolResults...)
		messages = append(messages, assistantMsg)
		messages = append(messages, toolResults...)
		persist(sessionID, ovSessionID, append([]map[string]any{assistantMsg}, toolResults...))

		// ---- 插话落地：用户在干活期间追的话，在这里进入上下文 ----
		// 位置只能在**整批**工具结果之后：掺在并行结果中间会留下没有 output 的
		// function_call，两个协议都会 400，写进历史后每轮重放还会再错一次。
		// 插话算「新任务」，工具预算重新计——否则剩两轮时插进来，新要求根本来不及做。
		if turn := interject.FromContext(ctx); turn != nil {
			if pending := turn.Drain(); len(pending) > 0 {
				for _, it := range pending {
					imsg := map[string]any{"role": "user", "content": interject.Wrap(it.Text)}
					messages = append(messages, imsg)
					newHistoryMsgs = append(newHistoryMsgs, imsg)
					persist(sessionID, ovSessionID, []map[string]any{imsg})
					turn.Ack(it) // 落库之后才回执，HTTP 才能回 injected
					events.Interject(sink, it.ID, it.Text, turn.ID)
				}
				toolRounds = 0
			}
		}

		// step 注入：工具结果回来后基于完整批次重新检索
		if config.OVInject == "step" || config.OVInject == "both" {
			if stepRecall := recall.OpenVikingLoadContext(messages, sessionID); stepRecall != "" {
				recallMsg := map[string]any{"role": db.InsertUserRole, "content": recall.WrapRecallBlock(stepRecall)}
				messages = append(messages, recallMsg)
				persist(sessionID, ovSessionID, []map[string]any{recallMsg})
			}
		}

		events.Continuing(sink, toolRounds)
		res, err = StreamLLM(ctx, messages, sink, sessionID)
		if err != nil {
			return "", "", total, nil, allToolResults, newHistoryMsgs, err
		}
		total = AddUsage(total, res.Usage)
		webSearchCalls = nil
		for _, oi := range res.OutputItems {
			if t, _ := oi["type"].(string); t == "web_search_call" {
				webSearchCalls = append(webSearchCalls, oi)
			}
		}
		assistantMsg = BuildAssistantMsg(res.Content, res.Reasoning, res.ToolCalls, res.OutputItems)
		content, reasoning = res.Content, res.Reasoning
	}

	if forcedFinal {
		forceMsg := map[string]any{"role": db.InsertUserRole, "content": forceFinalWarning}
		events.Warning(sink, "工具调用次数已达上限，强制基于已有结果给出最终回答", "max_tool_rounds")
		messages = append(messages, forceMsg)
		persist(sessionID, ovSessionID, []map[string]any{forceMsg})
		newHistoryMsgs = append(newHistoryMsgs, forceMsg)
		if config.OVInject == "step" || config.OVInject == "both" {
			if stepRecall := recall.OpenVikingLoadContext(messages, sessionID); stepRecall != "" {
				recallMsg := map[string]any{"role": db.InsertUserRole, "content": recall.WrapRecallBlock(stepRecall)}
				messages = append(messages, recallMsg)
				persist(sessionID, ovSessionID, []map[string]any{recallMsg})
			}
		}
		res, err = StreamLLM(ctx, messages, sink, sessionID)
		if err != nil {
			return "", "", total, nil, allToolResults, newHistoryMsgs, err
		}
		total = AddUsage(total, res.Usage)
		assistantMsg = BuildAssistantMsg(res.Content, res.Reasoning, res.ToolCalls, res.OutputItems)
		content, reasoning = res.Content, res.Reasoning
	}

	// 落库前剥离孤儿工具调用
	_, hadCalls := assistantMsg["tool_calls"]
	assistantMsg = db.StripToolCalls(assistantMsg)
	if hadCalls {
		events.Warning(sink, "最终轮模型仍请求调用工具，已剥离该调用以保证历史可重放", "orphan_tool_calls")
	}
	newHistoryMsgs = append(newHistoryMsgs, assistantMsg)
	persist(sessionID, ovSessionID, []map[string]any{assistantMsg})
	return content, reasoning, total, assistantMsg, allToolResults, newHistoryMsgs, nil
}
