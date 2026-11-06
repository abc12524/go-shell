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

// Turn 本轮登记完成：id 是中途插话的目标标识（/interject 可用它指定投给哪一轮）
func Turn(s Sink, id string) {
	emit(s, "turn", map[string]any{"id": id})
}

// Interject 一条插话已经写进上下文（客户端据此把这句并入本轮，不再另起一轮）
func Interject(s Sink, id, text, turnID string) {
	emit(s, "interject", map[string]any{"id": id, "text": text, "turn_id": turnID})
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
//   - 流式区全帧重绘、自然滚动：旧行一旦输出就定格在屏幕/回滚缓冲里，
//     不会被后续帧覆盖侵占
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
	kind  string
	buf   strings.Builder
	lines int // 上一次渲染占用的行数（可能超过屏高：区域顶已滚出屏幕时配合钳制语义）
	last  time.Time
	t     *Terminal
}

// termIsTerminal 供 cursor_*.go 共用的 isatty 判断
func termIsTerminal(fd int) bool { return term.IsTerminal(fd) }

func (l *liveRegion) render() string {
	text := l.buf.String()
	if l.kind == "content" {
		out := renderBodyMarkdown(text, l.t.streamWidth())
		// glamour 对超长无空格 token（URL 等）不折行，终端会软换行，
		// 导致逻辑行数 < 显示行数、重绘光标算术漂移啃掉上方内容。
		// 这里按显示宽度再硬折一次，保证逻辑行 == 终端显示行。
		lines := strings.Split(out, "\n")
		for i, ln := range lines {
			wl := hardWrap(ln, l.t.streamWidth())
			lines[i] = strings.Join(wl, "\n")
		}
		return strings.Join(lines, "\n")
	}
	lines := hardWrap(text, l.t.streamWidth()-2)
	for i := range lines {
		lines[i] = sty(stThink, lines[i])
	}
	return strings.Join(lines, "\n")
}

func (l *liveRegion) feed(delta string) {
	l.buf.WriteString(delta)
	// 节流：~6fps，与原版 _LIVE_FPS 一致，避免逐 token 重绘闪烁
	if time.Since(l.last) < time.Second/6 {
		return
	}
	l.redraw()
}

// redraw 全帧重绘。不做尾部裁剪/钉屏：旧行一旦输出就留在屏幕上，帧超出
// 屏幕时自然滚入回滚缓冲——新内容只会把上方内容正常顶走，绝不会覆盖
// 尚未读完的行（此前「尾部窗口滑动重绘」正是旧行被侵占的原因）。
func (l *liveRegion) redraw() {
	full := l.render()
	n := strings.Count(full, "\n") + 1
	h := l.t.termHeight()
	// 帧高超过一屏时区域顶已滚出屏幕，上移量钳制到屏幕内（CUU 会在顶行截停，
	// 此时屏幕上已全是本区域内容，从顶行清起重绘是正确行为）
	up := l.lines - 1
	if up > h-1 {
		up = h - 1
	}
	if up > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA\r\x1b[J", up)
	}
	fmt.Print(full)
	fmt.Fprint(os.Stdout, "\x1b[J") // 清除帧尾以下可能的残留（计数漂移兜底）
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

// pickPanelWidth 只有一个宽度阶梯：基准档 = min(终端宽, 88)，下限 24；
// 内容（表格）在基准档放不下时跳到终端宽这一档。没有中间值。
func pickPanelWidth(termW, contentW int) int {
	base := termW
	if base > 88 {
		base = 88
	}
	if base < 24 {
		base = 24
	}
	if contentW > 0 && contentW+5 > base && termW > base {
		return termW // 第二档：拉满终端宽，表格才不至于被压成竖条
	}
	return base
}

// panelWidth 面板基准宽度 = min(终端宽, 88)，下限 24。
// 行宽 = width-1，留 1 列规避 conpty 的满行换行。宽终端上停在 88，
// 而不是把面板拉满整屏。
func (t *Terminal) panelWidth() int {
	return pickPanelWidth(t.termWidth(), 0)
}

// panelWidthFor contentW 是内容不被压缩所需的渲染宽度（目前只按表格算，
// 正文按宽度折行即可）。放得下基准档就用基准档，放不下才拉满终端宽。
func (t *Terminal) panelWidthFor(contentW int) int {
	return pickPanelWidth(t.termWidth(), contentW)
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
	out := l.render()
	h := t.termHeight()
	up := l.lines - 1
	if up > h-1 {
		up = h - 1
	}
	if up > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA\r\x1b[J", up)
	}
	fmt.Println(out)
	fmt.Fprint(os.Stdout, "\x1b[J") // 定格后清尾，防残留
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

// flushPending 结果一直没到（执行被中断）时兜底输出，避免调用信息丢失。
// 按调用发生的先后输出：此前从队尾逐个弹出，被打断时补打的面板是倒序的。
func (t *Terminal) flushPending() {
	for _, key := range t.order {
		pc := t.pending[key]
		delete(t.pending, key)
		if pc == nil {
			continue
		}
		fmt.Println()
		fmt.Println(drawPanel(pc.parts, pc.label, "", false, t.panelWidth()))
	}
	t.order = nil
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
		panelW := t.panelWidth()
		if !rendered {
			if isErr {
				parts = append(parts, sty(stErr, "错误"))
			} else {
				parts = append(parts, sty(stSection, "结果"))
			}
			// 内容宽（表格自然宽）超过基准档时，面板放宽到终端宽这一档
			panelW = t.panelWidthFor(payloadContentWidth(output))
			// 内宽 = 面板宽 - 5（│ + 空格 + 内容 + 空格 + │）。这里原为 -4，
			// 渲染出的一行比框内宽多 1 列，随后被 hardWrap 再折一次，表格行当场断裂。
			parts = append(parts, payloadParts(output, resultMaxLines, resultMaxChars, panelW-5)...)
		}
		sub := "✓ 完成"
		if isErr {
			sub = "✗ 失败"
		}
		fmt.Println()
		fmt.Println(drawPanel(parts, label, sub, isErr, panelW))
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
				fmt.Println("  " + sty(stAccent, "▸ 服务端搜索"))
			}
			fmt.Println("    " + sty(stMuted, "·") + " " + sty(stMuted, "发起") + " " + sty(stKey, id))
		} else {
			fmt.Println("    " + sty(stMuted, "· 状态 "+state))
		}
	case "warning":
		msg, _ := evt["message"].(string)
		fmt.Println("  " + sty(stWarn, "⚠ "+msg))
	case "interject":
		// 插话落地：先把流式区定格，再单独一行说明"这句话已经进上下文了"
		t.stopLive()
		text, _ := evt["text"].(string)
		fmt.Println("  " + sty(stAccent, "▸ 已插入") + sty(stMuted, "（本轮改道，不另起一轮）"))
		fmt.Println("    " + sty(stMuted, "›") + " " + sty(stBold, text))
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
			fmt.Println("  " + sty(stMuted, "余额 ") + sty(stOk, symbol+balance))
		} else {
			fmt.Println("  " + sty(stMuted, "余额 查询失败"))
		}
	case "error":
		t.stopLive()
		t.flushPending()
		msg, _ := evt["error"].(string)
		fmt.Println("  " + sty(stErr, "✗ "+msg))
	}
}

// Flush 收尾：定格流式区
func (t *Terminal) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLive()
}
