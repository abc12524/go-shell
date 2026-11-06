// Package recall：OpenViking 记忆召回 / 抓取（对应 core/recall.py）
package recall

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go-shell/internal/config"
)

// RecallMarker / ProfileMarker 注入块标记
const (
	RecallMarker  = "## 相关记忆"
	ProfileMarker = `<openviking-context source="profile">`
	ReminderOpen  = "<system-reminder>"
	ReminderClose = "</system-reminder>"
	OVBlockOpen   = "<available_openviking>"
	OVBlockClose  = "</available_openviking>"
)

var recallSeen = map[string]map[string]bool{} // sessionID -> set(uri)

// PeerID 解析当前 actor peer
func PeerID() string {
	if config.OVPeerID != "" {
		return config.OVPeerID
	}
	if config.OVWorkspacePeer {
		sum := md5.Sum([]byte(config.ProjectRoot))
		return "ws-" + hex.EncodeToString(sum[:])[:12]
	}
	if config.OpenVikingAgent != "" {
		return config.OpenVikingAgent
	}
	return "default"
}

func headerValue(v string) string {
	// HTTP 头值必须是 latin-1 可编码；非 ASCII 做 RFC2047 编码
	if isASCII(v) {
		return v
	}
	return mime.QEncoding.Encode("utf-8", v)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func ovHeaders() (map[string]string, error) {
	if config.OpenVikingKey == "" {
		return nil, fmt.Errorf("OPENVIKING_KEY 未设置")
	}
	return map[string]string{
		"Authorization":        "Bearer " + config.OpenVikingKey,
		"Content-Type":         "application/json",
		"X-OpenViking-Account": "default",
		"X-OpenViking-User":    headerValue(config.OpenVikingUser),
		"X-OpenViking-Peer":    headerValue(PeerID()),
	}, nil
}

var urlRe = regexp.MustCompile(`(?i)https?://[^\s"')]+`)
var ipRe = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)

func sanitize(text string) string {
	text = urlRe.ReplaceAllString(text, "<ov>")
	return ipRe.ReplaceAllString(text, "<ov>")
}

func errPayload(message, code string) map[string]any {
	payload := map[string]any{"status": "error", "error": message}
	if code != "" {
		payload["code"] = code
	}
	return payload
}

func isError(v map[string]any) bool {
	s, _ := v["status"].(string)
	return s == "error"
}

func ovRequest(method, path string, payload any, timeout time.Duration) map[string]any {
	if config.OpenVikingURL == "" {
		return errPayload("OPENVIKING_URL 未配置", "transport")
	}
	headers, err := ovHeaders()
	if err != nil {
		return errPayload(err.Error(), "transport")
	}
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(config.OpenVikingURL, "/")+path, body)
	if err != nil {
		return errPayload("OpenViking 请求失败 - "+sanitize(err.Error()), "transport")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return errPayload("OpenViking 请求超时", "timeout")
		}
		return errPayload("OpenViking 请求失败 - "+sanitize(err.Error()), "transport")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		detail := ""
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if e, ok := parsed["error"].(map[string]any); ok {
				if m, ok := e["message"].(string); ok {
					detail = m
				}
			} else if e, ok := parsed["error"].(string); ok {
				detail = e
			} else if m, ok := parsed["message"].(string); ok {
				detail = m
			} else if m, ok := parsed["detail"].(string); ok {
				detail = m
			}
		}
		msg := fmt.Sprintf("OpenViking 请求失败 (HTTP %d)", resp.StatusCode)
		if detail != "" {
			msg += " - " + sanitize(detail)
		}
		return errPayload(msg, "http")
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return errPayload("OpenViking 返回了非预期的数据格式", "bad_response")
	}
	return out
}

func ovPost(path string, payload any, timeout time.Duration) map[string]any {
	return ovRequest("POST", path, payload, timeout)
}

func ovGet(path string, query string, timeout time.Duration) map[string]any {
	if query != "" {
		path += "?" + query
	}
	return ovRequest("GET", path, nil, timeout)
}

// ---------- 召回 ----------

func msgQueryText(m map[string]any) string {
	role, _ := m["role"].(string)
	content, _ := m["content"].(string)
	switch role {
	case dbInsertUserRole:
		return ""
	case "user":
		if strings.Contains(content, RecallMarker) || strings.Contains(content, ProfileMarker) {
			return ""
		}
		return content
	case "tool":
		return content
	case "assistant":
		parts := []string{content}
		if tcs, ok := m["tool_calls"].([]map[string]any); ok {
			for _, tc := range tcs {
				if fn, ok := tc["function"].(map[string]any); ok {
					if name, _ := fn["name"].(string); name != "" {
						parts = append(parts, name)
					}
				}
			}
		}
		return strings.Join(nonEmpty(parts), "\n")
	}
	return ""
}

const dbInsertUserRole = "insert_user"

func nonEmpty(items []string) []string {
	var out []string
	for _, s := range items {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// BuildRecallQuery 由完整消息批次构造检索 query
func BuildRecallQuery(messages []map[string]any) string {
	var parts []string
	for _, m := range messages {
		if t := msgQueryText(m); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// OpenVikingBlock 只包 <available_openviking>
func OpenVikingBlock(content string) string {
	if content == "" {
		return ""
	}
	return OVBlockOpen + "\n" + content + "\n" + OVBlockClose
}

// WrapRecallBlock 包进 <system-reminder><available_openviking>
func WrapRecallBlock(block string) string {
	if block == "" {
		return ""
	}
	return ReminderOpen + "\n" + OpenVikingBlock(block) + "\n" + ReminderClose
}

func extractMemories(result map[string]any) []map[string]any {
	if result == nil {
		return nil
	}
	var candidates []map[string]any
	merge := func(raw any) {
		switch v := raw.(type) {
		case []any:
			for _, h := range v {
				if m, ok := h.(map[string]any); ok {
					candidates = append(candidates, m)
				}
			}
		case map[string]any:
			for _, key := range []string{"memories", "resources", "skills", "hits", "items", "results"} {
				if seg, ok := v[key].([]any); ok {
					for _, h := range seg {
						if m, ok := h.(map[string]any); ok {
							if _, has := m["context_type"]; !has {
								switch key {
								case "memories":
									m["context_type"] = "memory"
								case "resources":
									m["context_type"] = "resource"
								case "skills":
									m["context_type"] = "skill"
								}
							}
							candidates = append(candidates, m)
						}
					}
				}
				if len(candidates) > 0 {
					break
				}
			}
		}
	}
	raw := result["result"]
	switch v := raw.(type) {
	case []any:
		merge(raw)
	case map[string]any:
		if data, ok := v["data"].(map[string]any); ok {
			merge(data)
		}
		if len(candidates) == 0 {
			merge(raw)
		}
	default:
		merge(result)
	}

	seen := map[string]bool{}
	var out []map[string]any
	for _, h := range candidates {
		uri, _ := h["uri"].(string)
		if uri != "" {
			if seen[uri] {
				continue
			}
			seen[uri] = true
		}
		out = append(out, h)
	}
	// score 降序
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && scoreOf(out[j]) > scoreOf(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func scoreOf(m map[string]any) float64 {
	switch v := m["score"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// SearchMemories 记忆语义搜索（find 接口）
func SearchMemories(query string) []map[string]any {
	if query == "" || len([]rune(query)) < config.OVMinQueryLength {
		return nil
	}
	result := ovPost("/api/v1/search/find", map[string]any{
		"query": query, "score_threshold": config.OVFindThreshold, "limit": config.OVFindLimit,
	}, 15*time.Second)
	if !isError(result) {
		return extractMemories(result)
	}
	return nil
}

// dedupFilter 过滤本 session 已注入的 URI
func dedupFilter(mems []map[string]any, sessionID string) []map[string]any {
	if sessionID == "" || config.OVRecallDedupTurns <= 0 {
		return mems
	}
	seen := recallSeen[sessionID]
	if seen == nil {
		seen = map[string]bool{}
		recallSeen[sessionID] = seen
	}
	var out []map[string]any
	for _, m := range mems {
		uri, _ := m["uri"].(string)
		if uri != "" && seen[uri] {
			continue
		}
		out = append(out, m)
		if uri != "" {
			seen[uri] = true
		}
	}
	return out
}

// OpenVikingLoadContext 基于完整消息批次检索相关记忆，返回可注入上下文的块
func OpenVikingLoadContext(messages []map[string]any, sessionID string) string {
	if config.OVInject == "off" {
		return ""
	}
	query := BuildRecallQuery(messages)
	mems := SearchMemories(query)
	mems = dedupFilter(mems, sessionID)
	if len(mems) > config.OVFindLimit {
		mems = mems[:config.OVFindLimit]
	}
	if len(mems) == 0 {
		return ""
	}
	parts := []string{RecallMarker}
	for _, h := range mems {
		uri, _ := h["uri"].(string)
		abstract, _ := h["abstract"].(string)
		ctype, _ := h["context_type"].(string)
		if abstract != "" {
			if len([]rune(abstract)) > 300 {
				abstract = string([]rune(abstract)[:300])
			}
			parts = append(parts, fmt.Sprintf("- [%s] (score=%.2f, %s)\n  %s", uri, scoreOf(h), ctype, abstract))
		}
	}
	return strings.Join(parts, "\n")
}

// OpenVikingLoadProfile 会话开始时拉取可用记忆索引
func OpenVikingLoadProfile() string {
	if !config.OVProfile || config.OpenVikingURL == "" {
		return ""
	}
	user := config.OpenVikingUser
	agent := config.OpenVikingAgent
	if agent == "" {
		agent = "default"
	}
	root := fmt.Sprintf("viking://user/%s/peers/%s/memories/", user, agent)
	tree := ovGet("/api/v1/fs/tree", "uri="+root, 15*time.Second)
	if isError(tree) {
		return ""
	}
	entries := extractTreeEntries(tree)
	if len(entries) == 0 {
		return ""
	}
	if len(entries) > 40 {
		entries = entries[:40]
	}
	text := strings.Join(prefixAll(entries, "- "), "\n")
	cap := config.OVProfileTokenBudget * 2
	if len([]rune(text)) > cap {
		text = string([]rune(text)[:cap])
	}
	return ProfileMarker + "\n可用记忆索引（主题概览）：\n" + text + "\n</openviking-context>"
}

func prefixAll(items []string, prefix string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + s
	}
	return out
}

func extractTreeEntries(tree map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			if name, ok := t["name"].(string); ok && name != "" {
				add(name)
			}
			if name, ok := t["title"].(string); ok && name != "" {
				add(name)
			}
			if uri, ok := t["uri"].(string); ok && name_or(uri) {
				add(uriOf(uri))
			}
			for _, key := range []string{"entries", "list", "children", "files", "nodes"} {
				if child, ok := t[key]; ok {
					walk(child)
				}
			}
		}
	}
	if raw, ok := tree["result"]; ok {
		walk(raw)
	}
	walk(tree)
	return out
}

func name_or(uri string) bool { return uri != "" }

func uriOf(uri string) string {
	uri = strings.TrimRight(uri, "/")
	parts := strings.Split(uri, "/")
	if len(parts) == 0 {
		return uri
	}
	return parts[len(parts)-1]
}

// ---------- 抓取 ----------

var ackRe = regexp.MustCompile(`(?i)^(?:ok|okay|k|yes|yep|no|nope|thanks|thank you|thx|done|收到|好的|好|嗯|可以|继续|不用|不需要|没了|好了)[.!?。！？\s]*$`)
var slashRe = regexp.MustCompile(`(?i)^/[a-z0-9_-]{1,64}\b`)
var cjkRe = regexp.MustCompile(`[\x{3400}-\x{9fff}]`)
var alnumRe = regexp.MustCompile(`(?i)[a-z0-9]`)
var nonWordRe = regexp.MustCompile(`(?i)[a-z0-9\x{3400}-\x{9fff}]`)

func hasEnoughSignal(text string) bool {
	cjk := len(cjkRe.FindAllString(text, -1))
	alnum := len(alnumRe.FindAllString(text, -1))
	return cjk >= 4 || alnum >= 6 || len([]rune(text)) >= 12
}

func shouldCapture(text, role string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if role == "user" && slashRe.MatchString(text) {
		return false
	}
	if ackRe.MatchString(text) {
		return false
	}
	if !nonWordRe.MatchString(text) {
		return false
	}
	return hasEnoughSignal(text)
}

func toolInputValue(raw any) any {
	if raw == nil || raw == "" {
		return nil
	}
	switch v := raw.(type) {
	case map[string]any:
		return v
	case []any:
		return map[string]any{"value": v}
	case string:
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
			return map[string]any{"value": parsed}
		}
		return map[string]any{"value": v}
	default:
		return map[string]any{"value": raw}
	}
}

func toolCallFields(tc map[string]any) (string, string, any) {
	fn, _ := tc["function"].(map[string]any)
	id, _ := tc["id"].(string)
	name, _ := fn["name"].(string)
	return id, name, fn["arguments"]
}

func toolCallPart(tc map[string]any) map[string]any {
	tid, name, args := toolCallFields(tc)
	part := map[string]any{"type": "tool", "tool_status": "running"}
	if tid != "" {
		part["tool_id"] = tid
	}
	if name != "" {
		part["tool_name"] = name
	}
	if v := toolInputValue(args); v != nil {
		part["tool_input"] = v
	}
	return part
}

func toolResultStatus(content string) string {
	if strings.HasPrefix(strings.TrimLeft(content, " \t\n"), "Error:") {
		return "error"
	}
	return "completed"
}

// toOVMessages 把聊天消息转成 OV session 批次消息
func toOVMessages(messages []map[string]any) []map[string]any {
	toolNames := map[string]string{}
	for _, m := range messages {
		if tcs, ok := m["tool_calls"].([]map[string]any); ok {
			for _, tc := range tcs {
				tid, name, _ := toolCallFields(tc)
				if tid != "" && name != "" {
					toolNames[tid] = name
				}
			}
		}
	}
	index := map[string]map[string]any{}
	var out []map[string]any
	for _, m := range messages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == dbInsertUserRole {
			continue
		}
		if strings.Contains(content, RecallMarker) || strings.Contains(content, ProfileMarker) {
			continue
		}
		if role == "assistant" {
			if !config.OVCaptureAssistantTurns {
				continue
			}
			if len([]rune(content)) > config.OVCaptureMaxLength {
				content = string([]rune(content)[:config.OVCaptureMaxLength])
			}
			keepText := strings.TrimSpace(content) != "" && shouldCapture(content, "assistant")
			var toolParts []any
			if tcs, ok := m["tool_calls"].([]map[string]any); ok {
				for _, tc := range tcs {
					part := toolCallPart(tc)
					toolParts = append(toolParts, part)
					if tid, _ := part["tool_id"].(string); tid != "" {
						index[tid] = part
					}
				}
			}
			if len(toolParts) > 0 {
				var parts []any
				if keepText {
					parts = append(parts, map[string]any{"type": "text", "text": content})
				}
				parts = append(parts, toolParts...)
				out = append(out, map[string]any{"role": "assistant", "parts": parts})
			} else if keepText {
				out = append(out, map[string]any{"role": "assistant", "content": content})
			}
			continue
		}
		if role == "tool" {
			if !config.OVCaptureAssistantTurns {
				continue
			}
			if len(content) > config.OVCaptureToolMaxChars {
				content = content[:config.OVCaptureToolMaxChars]
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			tid, _ := m["tool_call_id"].(string)
			part := index[tid]
			if part != nil {
				part["tool_status"] = toolResultStatus(content)
				part["tool_output"] = content
			} else {
				newPart := map[string]any{
					"type": "tool", "tool_status": toolResultStatus(content), "tool_output": content,
				}
				if tid != "" {
					newPart["tool_id"] = tid
				}
				if name := toolNames[tid]; name != "" {
					newPart["tool_name"] = name
				}
				out = append(out, map[string]any{"role": "assistant", "parts": []any{newPart}})
			}
			continue
		}
		if role == "user" {
			if len([]rune(content)) > config.OVCaptureMaxLength {
				content = string([]rune(content)[:config.OVCaptureMaxLength])
			}
			if !shouldCapture(content, role) {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	return out
}

// OpenVikingEnsureSession 为会话建立 OV session，返回 OV session_id
func OpenVikingEnsureSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	result := ovPost("/api/v1/sessions", map[string]any{}, 15*time.Second)
	if isError(result) {
		return ""
	}
	res, _ := result["result"].(map[string]any)
	if res == nil {
		return ""
	}
	if id, ok := res["session_id"].(string); ok && id != "" {
		return id
	}
	if id, ok := res["id"].(string); ok && id != "" {
		return id
	}
	if sess, ok := res["session"].(map[string]any); ok {
		if id, ok := sess["id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// OpenVikingCapture 向 OV session 批量追加消息
func OpenVikingCapture(sessionID string, messages []map[string]any) {
	if sessionID == "" || !config.OVCapture {
		return
	}
	ovMsgs := toOVMessages(messages)
	if len(ovMsgs) == 0 {
		return
	}
	ovPost("/api/v1/sessions/"+sessionID+"/messages/batch", map[string]any{"messages": ovMsgs}, 15*time.Second)
}

// OpenVikingCommitSession 提交/归档 OV session
func OpenVikingCommitSession(sessionID string) {
	if sessionID == "" {
		return
	}
	ovPost("/api/v1/sessions/"+sessionID+"/commit", map[string]any{"keep_recent_count": 0}, 30*time.Second)
}
