// Package events：语义事件层（对应 core/events.py + console.emit）
//
// 服务端只输出"变化的数据"，展示格式由 Sink 自行构建：
//   - Terminal：CLI 终端模式，正文/思考流式打印，工具调用/结果打印明细
//   - Channel：API/服务端模式，事件写入 channel（SSE 转发或同步聚合）
//
// 事件字段与 Python 版协议一一对应（t=kind + 语义字段）。
package events

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Sink 事件出口。evt["t"] 为事件类型，其余为语义字段。
type Sink interface {
	Emit(evt map[string]any)
}

// ---------- 工具函数：构造各类事件 ----------

func emit(s Sink, kind string, fields map[string]any) {
	if s == nil {
		return
	}
	evt := map[string]any{"t": kind}
	for k, v := range fields {
		evt[k] = v
	}
	s.Emit(evt)
}

// Emit 直发自定义事件（含 t 键）
func Emit(s Sink, evt map[string]any) {
	if s == nil {
		return
	}
	s.Emit(evt)
}

func Config(s Sink, keys []string)  { emit(s, "config", map[string]any{"keys": keys}) }
func DB(s Sink, online bool)        { emit(s, "db", map[string]any{"online": online}) }
func Question(s Sink, text string)  { emit(s, "question", map[string]any{"text": text}) }
func ReasoningHeader(s Sink)        { emit(s, "reasoning_header", map[string]any{}) }
func Continuing(s Sink, index int)  { emit(s, "continuing", map[string]any{"index": index}) }

func Session(s Sink, action, sessionID string) {
	emit(s, "session", map[string]any{"action": action, "id": sessionID})
}

func Memory(s Sink, phase string, found bool) {
	emit(s, "memory", map[string]any{"phase": phase, "found": found})
}

func ToolRound(s Sink, index, local, search int) {
	emit(s, "tool_round", map[string]any{"index": index, "local": local, "search": search})
}

func ToolCall(s Sink, name string, arguments any, raw string, callID string, parseError string) {
	payload := map[string]any{"name": name, "id": callID}
	if arguments != nil {
		payload["arguments"] = arguments
	}
	if raw != "" {
		payload["raw"] = raw
	}
	if parseError != "" {
		payload["parse_error"] = parseError
	}
	emit(s, "tool_call", payload)
}

func ToolResult(s Sink, name, callID, output string, truncated, isErr bool) {
	maxLen := 2000
	full := output
	shown := full
	if len(shown) > maxLen {
		shown = shown[:maxLen]
	}
	payload := map[string]any{
		"name": name, "id": callID, "output": shown,
		"truncated": truncated || len(full) > maxLen, "error": isErr,
	}
	// 终端模式：面板按自身阈值（4000 字符/30 行）折叠，拿到完整输出
	if _, ok := s.(*Terminal); ok {
		payload["output"] = full
	}
	emit(s, "tool_result", payload)
}

func Search(s Sink, state, callID string, first bool) {
	emit(s, "search", map[string]any{"state": state, "id": callID, "first": first})
}

func Warning(s Sink, message, code string) {
	payload := map[string]any{"message": message}
	if code != "" {
		payload["code"] = code
	}
	emit(s, "warning", payload)
}

// Usage 用量统计事件
func Usage(s Sink, model string, peak bool, hit, miss, out int64, costHit, costMiss, costOut, total float64, balance, currency, symbol, period string) {
	emit(s, "usage", map[string]any{
		"model": model, "peak": peak,
		"hit": float64(hit), "miss": float64(miss), "out": float64(out),
		"cost_hit": costHit, "cost_miss": costMiss, "cost_out": costOut,
		"total": total, "balance": balance, "currency": currency,
		"symbol": symbol, "period": period,
	})
}

// ---------- Channel Sink：服务端模式 ----------

// Channel 把事件推入 channel；closed 后 Emit 变为 no-op（防 panic）。
type Channel struct {
	C      chan map[string]any
	mu     sync.Mutex
	closed bool
}

func NewChannel(buf int) *Channel {
	return &Channel{C: make(chan map[string]any, buf)}
}

func (c *Channel) Emit(evt map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.C <- evt:
	default:
		// 队列满（客户端消费慢）时丢弃最旧事件，保住新事件
		select {
		case <-c.C:
		default:
		}
		select {
		case c.C <- evt:
		default:
		}
	}
}

func (c *Channel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.C)
	}
}

// ---------- Terminal Sink：CLI 模式（rich 风格渲染） ----------

// Terminal 渲染到 stdout，复刻 Python 版 dp.py 的 rich 视觉：
//   - 正文/思考：终端下按 ~6fps 节流 Live 重绘（正文经 glamour 渲染 Markdown，
//     思考过程灰阶斜体）；非终端（管道/日志）下增量原样透传
//   - 工具调用/结果：圆角面板（调用先缓存，结果到达后合并输出）
//   - 内容超过屏幕高度后自动「冻结」Live 区，退化为原样追加，避免重绘串屏
type Terminal struct {
	mu      sync.Mutex
	isTTY   bool
	width   int
	live    *liveRegion
	pending map[string]*pendingCall
	order   []string
}

type pendingCall struct {
	label string
	parts []string
}

// liveRegion 流式渲染区（正文/思考各一个，切换时先定格旧区）
type liveRegion struct {
	kind   string
	buf    strings.Builder
	lines  int // 上一次渲染占用的行数
	last   time.Time
	frozen bool // 超过屏幕高度后冻结：不再重绘，增量原样追加
	t      *Terminal
}

func (l *liveRegion) render() string {
	text := l.buf.String()
	if l.kind == "content" {
		return renderMarkdown(text, l.t.streamWidth())
	}
	lines := hardWrap(text, l.t.streamWidth()-2)
	for i := range lines {
		lines[i] = sty(stThink, lines[i])
	}
	return strings.Join(lines, "\n")
}

func (l *liveRegion) feed(delta string) {
	l.buf.WriteString(delta)
	if l.frozen {
		os.Stdout.WriteString(delta)
		return
	}
	// 节流：~6fps，与原版 _LIVE_FPS 一致，避免逐 token 重绘闪烁
	if time.Since(l.last) < time.Second/6 {
		return
	}
	l.redraw()
}

func (l *liveRegion) redraw() {
	out := l.render()
	n := strings.Count(out, "\n") + 1
	h := l.t.termHeight() - 1
	if h > 0 && n > h {
		// 冻结：把当前累积内容渲染一次后自然滚屏，后续增量原样追加
		if l.lines > 0 {
			fmt.Fprintf(os.Stdout, "\x1b[%dA\r\x1b[J", l.lines-1)
		}
		fmt.Println(out)
		l.frozen = true
		l.lines = 0
		l.last = time.Now()
		return
	}
	if l.lines > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA\r\x1b[J", l.lines-1)
	}
	fmt.Print(out)
	l.lines = n
	l.last = time.Now()
}

func NewTerminal() *Terminal {
	t := &Terminal{pending: map[string]*pendingCall{}}
	t.isTTY = ColorEnabled() // 单一来源：惰性检测并置位 colorEnabled
	t.width = 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		t.width = w
	}
	return t
}

func (t *Terminal) termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return t.width
}

func (t *Terminal) streamWidth() int {
	w := t.termWidth()
	if w > 10 {
		return w - 1
	}
	return w
}

func (t *Terminal) termHeight() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 {
		return h
	}
	return 24
}

func (t *Terminal) panelWidth() int {
	w := t.termWidth()
	if w > 88 {
		w = 88
	}
	if w < 24 {
		w = 24
	}
	return w
}

func (t *Terminal) callKey(id, name string) string {
	if id != "" {
		return id
	}
	return "::" + name + "::"
}

// stream 正文/思考增量：切换类型时先定格旧 Live 区
func (t *Terminal) stream(kind, delta string) {
	if delta == "" {
		return
	}
	if !t.isTTY {
		os.Stdout.WriteString(delta)
		return
	}
	if t.live == nil || t.live.kind != kind {
		t.stopLive()
		t.live = &liveRegion{kind: kind, t: t}
	}
	t.live.feed(delta)
}

// stopLive 定格当前 Live 区（最终渲染一次后补换行）。调用方需持有 t.mu。
func (t *Terminal) stopLive() {
	if t.live == nil {
		return
	}
	l := t.live
	t.live = nil
	if !t.isTTY {
		return
	}
	if l.frozen {
		fmt.Println()
		return
	}
	out := l.render()
	if l.lines > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA\r\x1b[J", l.lines-1)
	}
	fmt.Println(out)
}

func (t *Terminal) cacheCall(id, name string, pc *pendingCall) {
	key := t.callKey(id, name)
	t.pending[key] = pc
	t.order = append(t.order, key)
}

func (t *Terminal) popCall(id, name string) *pendingCall {
	key := t.callKey(id, name)
	pc := t.pending[key]
	delete(t.pending, key)
	for i, k := range t.order {
		if k == key {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
	return pc
}

// flushPending 结果一直没到（执行被中断）时兜底输出，避免调用信息丢失
func (t *Terminal) flushPending() {
	for len(t.order) > 0 {
		key := t.order[len(t.order)-1]
		t.order = t.order[:len(t.order)-1]
		pc := t.pending[key]
		delete(t.pending, key)
		if pc == nil {
			continue
		}
		fmt.Println()
		fmt.Println(drawPanel(pc.parts, pc.label, "", false, t.panelWidth()))
	}
}

// unwrapArguments 拆掉 {skill: x, arguments: {...}} 外层信封，直接展示真正参数
func unwrapArguments(arguments any) any {
	d, ok := arguments.(map[string]any)
	if !ok || len(d) > 2 {
		return arguments
	}
	for _, key := range []string{"arguments", "args", "parameters", "params"} {
		inner, ok := d[key].(map[string]any)
		if !ok || len(inner) == 0 {
			continue
		}
		rest := map[string]any{}
		for k, v := range d {
			if k != key && k != "skill" && k != "tool" && k != "name" {
				rest[k] = v
			}
		}
		for k, v := range inner {
			rest[k] = v
		}
		return rest
	}
	return arguments
}

// callLabel 框标题：`工具名` 或 `工具名 · 目标`
func callLabel(name string, arguments any) string {
	if d, ok := arguments.(map[string]any); ok {
		for _, key := range []string{"skill", "tool", "host"} {
			if v, ok := d[key].(string); ok && v != "" && v != name {
				return name + " · " + v
			}
		}
	}
	return name
}

func (t *Terminal) Emit(evt map[string]any) {
	kind, _ := evt["t"].(string)
	t.mu.Lock()
	defer t.mu.Unlock()
	switch kind {
	case "content":
		delta, _ := evt["content"].(string)
		t.stream("content", delta)
	case "reasoning":
		delta, _ := evt["content"].(string)
		t.stream("reasoning", delta)
	case "tool_call":
		t.stopLive()
		name, _ := evt["name"].(string)
		id, _ := evt["id"].(string)
		perr, _ := evt["parse_error"].(string)
		raw, _ := evt["raw"].(string)
		args, hasArgs := evt["arguments"]
		var parts []string
		if perr != "" {
			parts = append(parts, sty(stErr, "参数解析失败："+perr))
		}
		if hasArgs && args != nil {
			parts = append(parts, jsonView(unwrapArguments(args), callMaxChars, callMaxLines, 5, callJSONLimit, "参数 ")...)
		} else if raw != "" {
			parts = append(parts, textView(raw, callTextLimit, callMaxLines, "参数 ")...)
		} else {
			parts = append(parts, sty(stMuted, "（无参数）"))
		}
		t.cacheCall(id, name, &pendingCall{label: callLabel(name, args), parts: parts})
	case "tool_result":
		t.stopLive()
		name, _ := evt["name"].(string)
		id, _ := evt["id"].(string)
		output, _ := evt["output"].(string)
		isErr, _ := evt["error"].(bool)
		pc := t.popCall(id, name)
		label, parts := name, []string{}
		if pc != nil {
			label, parts = pc.label, pc.parts
		}
		var env map[string]any
		_ = json.Unmarshal([]byte(output), &env)
		result := env["result"]
		rendered := false
		if !isErr {
			if d, ok := result.(map[string]any); ok {
				if _, ok := d["diff"].(string); ok {
					// script 类写动作：一行摘要 + 彩色 diff
					parts = append(parts, sty(stSection, "变更"))
					parts = append(parts, diffParts(d, resultDiffMax)...)
					rendered = true
				}
			}
		}
		if !rendered {
			if isErr {
				parts = append(parts, sty(stErr, "错误"))
			} else {
				parts = append(parts, sty(stSection, "结果"))
			}
			parts = append(parts, payloadParts(output, resultMaxLines, resultMaxChars, t.panelWidth()-4)...)
		}
		sub := "✓ 完成"
		if isErr {
			sub = "✗ 失败"
		}
		fmt.Println()
		fmt.Println(drawPanel(parts, label, sub, isErr, t.panelWidth()))
	case "tool_round":
		// 上一轮若有调用没等到结果，先兜底打出来（与原版一致，不打印轮次分隔线）
		t.stopLive()
		t.flushPending()
	case "search":
		t.stopLive()
		state, _ := evt["state"].(string)
		id, _ := evt["id"].(string)
		first, _ := evt["first"].(bool)
		if state == "call" {
			if first {
				fmt.Println()
				fmt.Println("  " + sty(stAccent, "🔎 服务端搜索"))
			}
			fmt.Println("    " + sty(stMuted, "·") + " " + sty(stMuted, "发起") + " " + sty(stKey, id))
		} else {
			fmt.Println("    " + sty(stMuted, "· 状态 "+state))
		}
	case "warning":
		msg, _ := evt["message"].(string)
		fmt.Println("  " + sty(stWarn, "⚠ "+msg))
	case "config":
		keys, _ := evt["keys"].([]any)
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			if s, ok := k.(string); ok {
				names = append(names, s)
			}
		}
		fmt.Println("  " + sty(stOk, "▸ 已更新 .env："+strings.Join(names, "、")))
	case "usage":
		t.stopLive()
		t.flushPending()
		symbol, _ := evt["symbol"].(string)
		model, _ := evt["model"].(string)
		period, _ := evt["period"].(string)
		total, _ := evt["total"].(float64)
		hit, _ := evt["hit"].(float64)
		miss, _ := evt["miss"].(float64)
		out, _ := evt["out"].(float64)
		costHit, _ := evt["cost_hit"].(float64)
		costMiss, _ := evt["cost_miss"].(float64)
		costOut, _ := evt["cost_out"].(float64)
		balance, _ := evt["balance"].(string)
		short := strings.Split(period, "（")[0]
		rows := [][]string{
			{"输入 · 缓存命中", commaInt(hit), money(symbol, costHit)},
			{"输入 · 未命中", commaInt(miss), money(symbol, costMiss)},
			{"输出", commaInt(out), money(symbol, costOut)},
			{"合计", "", money(symbol, total)},
		}
		fmt.Println(gridBlock("用量 · "+model+" · "+short,
			[]string{"项目", "tokens", "费用"}, rows, []bool{false, true, true}, 1))
		if balance != "" {
			fmt.Println("  " + sty(stMuted, "💰 余额 ") + sty(stOk, symbol+balance))
		} else {
			fmt.Println("  " + sty(stMuted, "💰 余额 查询失败"))
		}
	case "error":
		t.stopLive()
		t.flushPending()
		msg, _ := evt["error"].(string)
		fmt.Println("  " + sty(stErr, "❌ "+msg))
	}
}

// Flush 收尾：定格流式区
func (t *Terminal) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLive()
}
