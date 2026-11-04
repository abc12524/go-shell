// 终端渲染工具：复刻 Python 版 rich 输出风格（对应 core/events.py + client/dp_client.py 的渲染层）。
//
// 视觉令牌与原版 THEME 对齐：
//   - 工具调用/结果共用一个圆角框（灰描边，出错时红描边 + ✗ 失败徽标，完成时 ✓ 完成）
//   - 结果剥 {status,result} 信封：JSON 高亮 / Markdown 渲染 / 纯文本，按行与字符折叠
//   - 用量三列表格：表头灰、数字列右对齐加粗、合计行加粗
//   - 颜色用固定灰阶（ANSI256 244~249）而非 dim，浅色/深色终端都可读
package events

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
	glamourAnsi "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// ---------- 样式令牌（对齐原版 THEME） ----------

var (
	stAccent    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))   // bold cyan
	stBold      = lipgloss.NewStyle().Bold(true)                                   // title
	stMuted     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))            // grey58 次要文字
	stHint      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))            // 表头/提示
	stOk        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))              // green
	stWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))              // yellow
	stErr       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))   // bold red
	stKey       = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))            // kv.key 灰阶键名
	stPanelTtl  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))   // panel.title
	stSection   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))            // 框内分区标题
	stThink     = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true) // 思考过程
	stNum       = lipgloss.NewStyle().Bold(true)                                   // 数字列
	stBorderC   = lipgloss.Color("246")                                            // panel.border grey54
	stBorderErr = lipgloss.Color("1")                                              // 出错描边

	// JSON 高亮：键 bold cyan / 字符串 green / 数字 yellow / 字面量 magenta
	stJSONKey   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	stJSONStr   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	stJSONNum   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	stJSONConst = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

// colorEnabled：stdout 是否为终端。非终端（管道/日志）时全部输出纯文本，避免 ANSI 乱码。
var colorEnabled bool

// go-runewidth 在东亚 locale（zh_CN 等）下会把 East Asian Ambiguous 字符
// （制表符 ─│╭、°、~ 等）按宽度 2 计数；glamour/uniseg 与现代终端
// （Windows Terminal/VTE/kitty）都按 1。两边不一致会让 hardWrap 把
// glamour 表格分隔线拦腰截断（表格显示破碎）。未显式设置 RUNEWIDTH_EASTASIAN
// 时强制按窄歧义计数对齐三方；终端确实按宽歧义渲染的用户可设
// RUNEWIDTH_EASTASIAN=1 切回。CJK 全角字符（W/F）不受影响，仍按 2 计数。
// DefaultCondition 需同步改：包 init 时 handleEnv 已按 locale 把它置 true。
func init() {
	if os.Getenv("RUNEWIDTH_EASTASIAN") == "" {
		runewidth.EastAsianWidth = false
		runewidth.DefaultCondition.EastAsianWidth = false
	}
}

var colorOnce sync.Once

// ColorEnabled stdout 是否为终端（按需检测，供未创建 Terminal 的路径如 -I 列表使用）
func ColorEnabled() bool {
	colorOnce.Do(func() {
		colorEnabled = term.IsTerminal(int(os.Stdout.Fd()))
	})
	return colorEnabled
}

// AnsiMark 终端下用指定 ANSI 颜色码包裹文本，否则原样（"32"=绿，"31"=红…）
func AnsiMark(code, s string) string {
	if !ColorEnabled() {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// GreenMark 终端下返回绿色文本，否则原样（用于「已装」等状态标记）。
// 用原始 ANSI 而非 lipgloss：不受 lipgloss 自身 TERM 档位检测影响，isatty 即生效。
func GreenMark(s string) string { return AnsiMark("32", s) }

func sty(st lipgloss.Style, s string) string {
	if !colorEnabled {
		return s
	}
	return st.Render(s)
}

// ---------- 基础工具 ----------

// dw 显示宽度（ANSI 感知）
func dw(s string) int { return lipgloss.Width(s) }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func dumpish(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// ---------- 折叠（对应 _clip_lines / _clip_chars） ----------

const (
	resultMaxLines = 20
	resultDiffMax  = 30
	resultMaxChars = 4000
	callMaxLines   = 15
	callMaxChars   = 4000
	callTextLimit  = 300
	callJSONLimit  = 1200
)

func clipLines(text string, limit int, what string) (string, string) {
	lines := strings.Split(text, "\n")
	if len(lines) <= limit {
		return text, ""
	}
	keep := limit - 1
	if keep < 0 {
		keep = 0
	}
	return strings.Join(lines[:keep], "\n"),
		fmt.Sprintf("… %s还有 %d 行已折叠（共 %d 行）", what, len(lines)-keep, len(lines))
}

func clipChars(text string, limit int, what string) (string, string) {
	if len(text) <= limit {
		return text, ""
	}
	return text[:limit], fmt.Sprintf("… %s已截断（共 %d 字符）", what, len(text))
}

func clipped(text string, charLimit, maxLines int, what string) (string, string) {
	body, note := "", ""
	if maxLines > 0 {
		body, note = clipLines(text, maxLines, what)
	} else {
		body = text
	}
	body, note2 := clipChars(body, charLimit, what)
	if note == "" {
		note = note2
	}
	return body, note
}

// ---------- Markdown（对应 flatten_headings + rich Markdown） ----------

var headingRe = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+?)\s*#*\s*$`)

// flattenHeadings 把 Markdown 标题降级成「加粗独立行」（避免 glamour 居中大字横幅）
func flattenHeadings(text string) string {
	if !strings.Contains(text, "#") {
		return text
	}
	lines := strings.Split(text, "\n")
	inCode := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			lines[i] = "**" + m[2] + "**"
		}
	}
	return strings.Join(lines, "\n")
}

var (
	glMu    sync.Mutex
	glCache = map[int]*glamour.TermRenderer{}
)

// mdStyleConfig 基于 glamour 标准 dark/light 风格，着重文字统一改绿色：
// 加粗（strong）、斜体（em）、行内代码（code）默认渲染偏红/不着色，按需求改为绿色。
func mdStyleConfig() glamourAnsi.StyleConfig {
	base := styles.LightStyleConfig
	if preferDarkBackground() {
		base = styles.DarkStyleConfig
	}
	green := "2" // ansi256 绿色（StylePrimitive.Color 为 *string）
	base.Strong.Color = &green
	base.Emph.Color = &green
	base.Code.Color = &green
	return base
}

// preferDarkBackground 只凭环境变量判断深色终端，绝不向终端发查询：
// lipgloss.HasDarkBackground() 会发 OSC 11（查背景色）+ DSR 6n（查光标位置），
// 部分终端/ssh 链路下应答不被消费会残留到输入队列，引发提示符异常。
func preferDarkBackground() bool {
	if fg := os.Getenv("COLORFGBG"); fg != "" {
		parts := strings.Split(fg, ";")
		if v, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			// 背景色 0/8 为黑，40~47 为标准深色系
			return v == 0 || v == 8 || (v >= 40 && v <= 47)
		}
	}
	return true // 缺省按深色（Windows Terminal / 主流终端默认）
}

// renderMarkdown 一次性渲染 Markdown（glamour，按宽度换行）
func renderMarkdown(text string, width int) string {
	text = flattenHeadings(text)
	if !colorEnabled {
		return text
	}
	if width < 20 {
		width = 20
	}
	glMu.Lock()
	defer glMu.Unlock()
	r, ok := glCache[width]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(mdStyleConfig()),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return text
		}
		glCache[width] = r
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

// ---------- ANSI 感知硬换行 / 填充 ----------

// hardWrap 按显示宽度硬换行，ANSI 转义序列不占宽度
func hardWrap(s string, limit int) []string {
	if limit <= 0 {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	w := 0
	i := 0
	flush := func() { out = append(out, cur.String()); cur.Reset(); w = 0 }
	for i < len(s) {
		if s[i] == 0x1b { // ANSI 序列原样透传
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			} else if j < len(s) {
				j++
			}
			cur.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		if w+rw > limit {
			flush()
		}
		cur.WriteRune(r)
		w += rw
		i += size
	}
	out = append(out, cur.String())
	return out
}

// padTo 按显示宽度补空格（ANSI 感知），right=true 右对齐
func padTo(s string, width int, right bool) string {
	d := dw(s)
	if d >= width {
		return s
	}
	pad := strings.Repeat(" ", width-d)
	if right {
		return pad + s
	}
	return s + pad
}

// ---------- 工具面板（对应 _tool_panel） ----------

// drawPanel 圆角框：标题左侧「▸ name」，副标右侧「✓ 完成 / ✗ 失败」，出错换红描边。
func drawPanel(lines []string, title, subtitle string, isErr bool, width int) string {
	bc := stBorderC
	if isErr {
		bc = stBorderErr
	}
	b := lipgloss.NewStyle().Foreground(bc)
	inner := width - 5 // 行宽=1+1+inner+1+1=width-1，刻意不满宽（规避 conpty 满行换行 bug）
	if inner < 10 {
		inner = 10
	}
	// 内容硬换行到框内宽
	var body []string
	for _, ln := range lines {
		body = append(body, hardWrap(ln, inner)...)
	}
	fullTitle := "▸ " + title

	// 顶边：╭─ title ────╮（总宽 width-1）
	tw := runewidth.StringWidth(fullTitle)
	dash := width - tw - 6
	if dash < 1 {
		dash = 1
	}
	top := b.Render("╭") + b.Render("─ ") + sty(stPanelTtl, fullTitle) + " " +
		b.Render(strings.Repeat("─", dash)) + b.Render("╮")

	// 底边：╰──── ✓ 完成 ──╯（副标右对齐，总宽 width-1：1+left+1+sw+1+2+1）
	var bottom string
	if subtitle != "" {
		sw := runewidth.StringWidth(subtitle)
		left := width - sw - 7
		if left < 1 {
			left = 1
		}
		subStyle := stOk
		if isErr {
			subStyle = stErr
		}
		bottom = b.Render("╰") + b.Render(strings.Repeat("─", left)) + " " +
			sty(subStyle, subtitle) + " " + b.Render(strings.Repeat("─", 2)) + b.Render("╯")
	} else {
		bottom = b.Render("╰") + b.Render(strings.Repeat("─", width-3)) + b.Render("╯")
	}

	var sb strings.Builder
	sb.WriteString(top)
	sb.WriteString("\n")
	for _, ln := range body {
		sb.WriteString(b.Render("│") + " " + padTo(ln, inner, false) + " " + b.Render("│") + "\n")
	}
	sb.WriteString(bottom)
	return sb.String()
}

// ---------- 参数与结果视图（对应 _json_view / _compact_view / _auto_parts） ----------

var plainScalarRe = regexp.MustCompile(`^[\w\x{4e00}-\x{9fff}][\w\x{4e00}-\x{9fff}./:@~^+-]*$`)

const compactValueMax = 56
const compactInlineMax = 84

// scalarView 紧凑行里的标量写法：裸值够清楚就不加引号
func scalarView(v any) string {
	var text string
	switch x := v.(type) {
	case nil:
		text = "null"
	case bool:
		if x {
			text = "true"
		} else {
			text = "false"
		}
	case string:
		if plainScalarRe.MatchString(x) {
			text = x
		} else {
			b, _ := json.Marshal(x)
			text = string(b)
		}
	case float64:
		text = strconv.FormatFloat(x, 'g', -1, 64)
	default:
		b, _ := json.Marshal(x)
		text = string(b)
	}
	if dw(text) > compactValueMax {
		runes := []rune(text)
		if len(runes) > compactValueMax-1 {
			text = string(runes[:compactValueMax-1]) + "…"
		}
	}
	return text
}

// compactView 扁平 dict → 一行 k=v · k=v；放不下就逐行 key = value
func compactView(value map[string]any) []string {
	keys := sortedKeys(value)
	pairs := make([][2]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, [2]string{k, scalarView(value[k])})
	}
	one := ""
	for i, p := range pairs {
		if i > 0 {
			one += "  ·  "
		}
		one += p[0] + "=" + p[1]
	}
	if dw(one) <= compactInlineMax {
		line := ""
		for i, p := range pairs {
			if i > 0 {
				line += sty(stMuted, "  ·  ")
			}
			line += sty(stKey, p[0]) + sty(stMuted, "=") + p[1]
		}
		return []string{line}
	}
	w := 0
	for _, p := range pairs {
		if len(p[0]) > w {
			w = len(p[0])
		}
	}
	var lines []string
	for _, p := range pairs {
		lines = append(lines, sty(stKey, padTo(p[0], w, false))+sty(stMuted, " = ")+p[1])
	}
	return lines
}

var jsonTokenRe = regexp.MustCompile(`("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)`)

// colorJSON 逐行 JSON 着色（先折叠后着色，ANSI 不影响宽度计算）
func colorJSON(text string) string {
	if !colorEnabled {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		lines[i] = jsonTokenRe.ReplaceAllStringFunc(ln, func(m string) string {
			if strings.HasSuffix(m, ":") || strings.HasSuffix(m, " :") {
				// 键（带冒号）：只给键名部分上色
				key := m
				colon := ""
				if j := strings.LastIndex(m, ":"); j >= 0 {
					key = m[:j]
					colon = m[j:]
				}
				return stJSONKey.Render(key) + colon
			}
			switch m[0] {
			case '"':
				return stJSONStr.Render(m)
			case 't', 'f', 'n':
				return stJSONConst.Render(m)
			default:
				return stJSONNum.Render(m)
			}
		})
	}
	return strings.Join(lines, "\n")
}

var jsonEscapeRe = regexp.MustCompile(`\\(u[0-9a-fA-F]{4}|.)`)

// expandNewlines 把 JSON 字面里的 \n/\r/\t 还原成真实字符（命令输出才能按行折叠）
func expandNewlines(text string) string {
	if !strings.Contains(text, "\\") {
		return text
	}
	return jsonEscapeRe.ReplaceAllStringFunc(text, func(m string) string {
		switch m[1:] {
		case "n":
			return "\n"
		case "r":
			return ""
		case "t":
			return "\t"
		default:
			return m
		}
	})
}

// jsonView dict/list → 缩进 2 的彩色 JSON（inline>0 且扁平短参数时压成 k=v 行）
func jsonView(value any, charLimit, maxLines, inline, inlineChars int, what string) []string {
	if d, ok := value.(map[string]any); ok && inline > 0 && len(d) > 0 {
		flat := true
		for _, v := range d {
			if isContainer(v) {
				flat = false
				break
			}
		}
		if flat {
			return compactView(d)
		}
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		pretty = []byte(dumpish(value))
	}
	text := expandNewlines(string(pretty))
	var body, note string
	if inline > 0 {
		if strings.Count(text, "\n")+1 <= inline {
			compact, _ := json.Marshal(value)
			body, note = clipChars(string(compact), inlineChars, what)
			return []string{colorJSON(body)}
		}
		body, note = clipLines(text, maxLines, what)
		var note2 string
		body, note2 = clipChars(body, charLimit, what)
		if note == "" {
			note = note2
		}
	} else {
		body, note = clipped(text, charLimit, maxLines, what)
	}
	parts := strings.Split(colorJSON(body), "\n")
	if note != "" {
		parts = append(parts, sty(stMuted, note))
	}
	return parts
}

func textView(text string, charLimit, maxLines int, what string) []string {
	body, note := clipped(text, charLimit, maxLines, what)
	parts := strings.Split(body, "\n")
	if note != "" {
		parts = append(parts, sty(stMuted, note))
	}
	return parts
}

// ---------- Markdown 识别（对应 _is_markdown） ----------

var mdPatterns = []struct {
	re *regexp.Regexp
	w  int
}{
	{regexp.MustCompile("(?m)^\\s*```"), 3},
	{regexp.MustCompile(`(?m)^\s*\|\s*:?-{3,}`), 3},
	{regexp.MustCompile(`(?m)^\s*#{2,6}\s`), 2},
	{regexp.MustCompile(`(?m)^\s*#\s`), 1},
	{regexp.MustCompile(`(?m)\[[^\]\n]+\]\([^)\n]+\)`), 2},
	{regexp.MustCompile(`(?m)\*\*[^*\n]+\*\*`), 1},
	{regexp.MustCompile(`(?m)^\s*[-*+]\s+\S`), 1},
	{regexp.MustCompile(`(?m)^\s*\d+\.\s+\S`), 1},
	{regexp.MustCompile(`(?m)^\s*>\s+\S`), 1},
}

func isMarkdown(text string) bool {
	score := 0
	for _, p := range mdPatterns {
		if p.re.MatchString(text) {
			score += p.w
		}
	}
	return score >= 3
}

// autoParts 按内容自动选渲染：JSON → 高亮；Markdown → 渲染；其他 → 纯文本
func autoParts(text string, maxLines, charLimit, width int, what string) []string {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return []string{sty(stMuted, "（空）")}
	}
	if raw[0] == '{' || raw[0] == '[' {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			return jsonView(v, charLimit, maxLines, 0, 0, what)
		}
	}
	if isMarkdown(raw) {
		body, note := clipped(raw, charLimit, maxLines, what)
		md := renderMarkdown(body, width)
		parts := strings.Split(md, "\n")
		if note != "" {
			parts = append(parts, sty(stMuted, note))
		}
		return parts
	}
	return textView(raw, charLimit, maxLines, what)
}

// ---------- 信封结果（对应 _envelope_parts / _payload_parts） ----------

var textKeys = map[string]bool{
	"output": true, "stdout": true, "stderr": true, "content": true, "text": true,
	"message": true, "body": true, "detail": true, "result": true,
}

func metaLine(items [][2]string) string {
	line := ""
	for i, kv := range items {
		if i > 0 {
			line += sty(stMuted, " · ")
		}
		st := stMuted
		if (kv[0] == "truncated" || kv[0] == "diff_truncated") && kv[1] != "" && kv[1] != "false" {
			st = stWarn
		}
		line += sty(st, kv[0]+"="+kv[1])
	}
	return line
}

func payloadParts(text string, maxLines, charLimit, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{sty(stMuted, "（空）")}
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err == nil {
		if _, ok := env["status"]; ok {
			return envelopeParts(env, maxLines, charLimit, width)
		}
	}
	return autoParts(text, maxLines, charLimit, width, "结果 ")
}

func envelopeParts(env map[string]any, maxLines, charLimit, width int) []string {
	status, _ := env["status"].(string)
	result := env["result"]

	var extras [][2]string
	for _, k := range sortedKeys(env) {
		if k == "status" || k == "result" || k == "error" {
			continue
		}
		v := env[k]
		if isContainer(v) {
			continue
		}
		if b, ok := v.(bool); ok && !b {
			continue
		}
		extras = append(extras, [2]string{k, dumpish(v)})
	}

	var parts []string
	if status == "error" {
		if e, ok := env["error"].(string); ok && e != "" {
			parts = append(parts, sty(stErr, e))
		}
		if len(extras) > 0 {
			parts = append(parts, metaLine(extras))
		}
		message := ""
		if d, ok := result.(map[string]any); ok {
			for _, k := range []string{"message", "error", "detail"} {
				if s, ok := d[k].(string); ok && s != "" {
					message = s
					break
				}
			}
		}
		body := message
		if body == "" {
			if s, ok := result.(string); ok {
				body = s
			} else {
				body = dumpish(result)
			}
		}
		if body != "" && body != "null" {
			budget := maxLines - len(parts)
			if budget < 2 {
				budget = 2
			}
			parts = append(parts, autoParts(body, budget, charLimit, width, "结果 ")...)
		}
		if len(parts) == 0 {
			parts = append(parts, jsonView(env, charLimit, maxLines, 0, 0, "结果 ")...)
		}
		return parts
	}

	// 成功的 result：字符串 / 字典 / 列表分别处理
	meta := append([][2]string{}, extras...)
	var bodies [][2]string // (label, text)，label 为空表示无标签
	empty := false
	switch r := result.(type) {
	case nil:
		empty = true
	case string:
		if r == "" {
			empty = true
		} else {
			bodies = append(bodies, [2]string{"", r})
		}
	case map[string]any:
		if len(r) == 0 {
			empty = true
			break
		}
		var texts, scalars [][2]string
		for _, k := range sortedKeys(r) {
			v := r[k]
			if s, ok := v.(string); ok && s != "" && textKeys[k] {
				texts = append(texts, [2]string{k, s})
			} else if isContainer(v) {
				continue
			} else if b, ok := v.(bool); ok && !b {
				continue
			} else {
				scalars = append(scalars, [2]string{k, dumpish(v)})
			}
		}
		if len(texts) > 0 {
			bodies = texts
			meta = append(meta, scalars...)
		} else {
			var parts []string
			if len(meta) > 0 {
				parts = append(parts, metaLine(meta))
			}
			budget := maxLines - len(parts)
			if budget < 2 {
				budget = 2
			}
			parts = append(parts, jsonView(r, charLimit, budget, 0, 0, "结果 ")...)
			return parts
		}
	case []any:
		if len(r) == 0 {
			empty = true
		} else {
			bodies = append(bodies, [2]string{"", dumpish(r)})
		}
	default:
		bodies = append(bodies, [2]string{"", dumpish(result)})
	}

	if empty {
		parts := []string{sty(stMuted, "（空）")}
		if len(meta) > 0 {
			parts = append([]string{metaLine(meta)}, parts...)
		}
		return parts
	}

	if len(meta) > 0 {
		parts = append(parts, metaLine(meta))
	}
	remaining := maxLines - len(parts)
	if remaining < 2 {
		remaining = 2
	}
	for i, b := range bodies {
		left := len(bodies) - i
		budget := remaining / left
		if budget < 2 {
			budget = 2
		}
		bp := autoParts(b[1], budget, charLimit, width, "结果 ")
		if b[0] != "" && len(bodies) > 1 {
			parts = append(parts, append([]string{sty(stMuted, b[0])}, bp...)...)
		} else {
			parts = append(parts, bp...)
		}
		remaining -= budget
		if remaining < 2 {
			remaining = 2
		}
	}
	return parts
}

// ---------- diff（对应 _diff_text / _diff_parts） ----------

func diffText(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "diff "):
			lines = append(lines, sty(stBold, line))
		case strings.HasPrefix(line, "@@"):
			lines = append(lines, sty(stAccent, line))
		case strings.HasPrefix(line, "+"):
			lines = append(lines, sty(stOk, line))
		case strings.HasPrefix(line, "-"):
			lines = append(lines, sty(stErr, line))
		default:
			lines = append(lines, sty(stMuted, line))
		}
	}
	return lines
}

func diffParts(result map[string]any, maxLines int) []string {
	head := ""
	for _, k := range []string{"action", "file"} {
		if s, ok := result[k].(string); ok && s != "" {
			if head != "" {
				head += " · "
			}
			head += s
		}
	}
	var stats []string
	if lines, ok := result["lines"].([]any); ok && len(lines) == 2 {
		stats = append(stats, fmt.Sprintf("行数 %v→%v", lines[0], lines[1]))
	}
	for _, k := range sortedKeys(result) {
		switch k {
		case "action", "file", "diff", "lines", "diff_truncated":
			continue
		}
		v := result[k]
		if isContainer(v) {
			stats = append(stats, k+"="+dumpish(v))
		} else {
			stats = append(stats, k+"="+dumpish(v))
		}
	}
	if b, ok := result["diff_truncated"].(bool); ok && b {
		stats = append(stats, "diff 已折叠")
	}
	summary := sty(stBold, head)
	if len(stats) > 0 {
		summary += "   " + sty(stMuted, strings.Join(stats, " · "))
	}
	diff, _ := result["diff"].(string)
	body, note := clipped(diff, resultMaxChars, maxLines-1, "diff ")
	parts := []string{summary}
	if body != "" {
		parts = append(parts, diffText(body)...)
	}
	if note != "" {
		parts = append(parts, sty(stMuted, note))
	}
	return parts
}

// ---------- 用量表格（对应 render_grid） ----------

// gridBlock 多列清单：表头灰阶小字，数字列右对齐加粗，最后 foot 行按合计加粗。
// 返回含前导空行与标题的完整文本块。
func gridBlock(title string, header []string, rows [][]string, right []bool, foot int) string {
	ncol := len(header)
	widths := make([]int, ncol)
	measure := func(cells []string) {
		for c, cell := range cells {
			if w := runewidth.StringWidth(cell); w > widths[c] {
				widths[c] = w
			}
		}
	}
	measure(header)
	for _, row := range rows {
		measure(row)
	}
	line := func(cells []string, styCell func(string, int) string) string {
		parts := make([]string, ncol)
		for c := 0; c < ncol; c++ {
			cell := ""
			if c < len(cells) {
				cell = cells[c]
			}
			parts[c] = styCell(padTo(cell, widths[c], right[c]), c)
		}
		return "  " + strings.Join(parts, "  ")
	}

	var out []string
	out = append(out, "")
	out = append(out, "  "+sty(stBold, title))
	out = append(out, line(header, func(s string, _ int) string { return sty(stHint, s) }))
	for i, row := range rows {
		footRow := foot > 0 && i >= len(rows)-foot
		out = append(out, line(row, func(s string, c int) string {
			if footRow {
				return sty(stBold, s)
			}
			if right[c] {
				return sty(stNum, s)
			}
			return s
		}))
	}
	return strings.Join(out, "\n")
}

func commaInt(v float64) string {
	s := strconv.FormatInt(int64(v), 10)
	if len(s) <= 3 {
		return s
	}
	var b []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, c)
	}
	return string(b)
}

func money(symbol string, v float64) string {
	return symbol + fmt.Sprintf("%.4f", v)
}
