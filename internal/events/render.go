// 终端渲染工具：复刻 Python 版 rich 输出风格（对应 core/events.py + client/dp_client.py 的渲染层）。
//
// 视觉令牌与原版 THEME 对齐：
//   - 工具调用/结果共用一个圆角框（灰描边，出错时红描边 + ✗ 失败徽标，完成时 ✓ 完成）
//   - 结果剥 {status,result} 信封：JSON 高亮 / Markdown 渲染 / 纯文本，按行与字符折叠
//   - 用量三列表格：表头灰、数字列右对齐加粗、合计行加粗
//   - 灰色只用于两处：思考过程（245 灰阶斜体）与未安装的 skill 名；
//     其余文字一律终端默认前景色（面板描边 246 灰属装饰边框）
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
	stMuted     = lipgloss.NewStyle()                                              // 次要文字：正常色（灰只留给思考过程/未装 skill）
	stHint      = lipgloss.NewStyle()                                              // 表头/提示：正常色
	stOk        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))              // green
	stWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))              // yellow
	stErr       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))   // bold red
	stKey       = lipgloss.NewStyle()                                              // kv 键名：正常色
	stPanelTtl  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))   // panel.title
	stSection   = lipgloss.NewStyle()                                              // 框内分区标题：正常色
	stThink     = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true) // 思考过程：唯一保留灰色的正文（灰阶斜体）
	stNum       = lipgloss.NewStyle().Bold(true)                                   // 数字列
	stBorderC   = lipgloss.Color("246")                                            // panel.border grey54（装饰边框）
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

// GreyMark 终端下返回灰阶文本（ANSI256 245），否则原样。
// 灰色仅用于思考过程与未安装的 skill 名，不要挪作他用。
func GreyMark(s string) string { return AnsiMark("245", s) }

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

// clipChars 按字节预算截断，但只切在 rune 边界上：原来的 text[:limit] 会把
// 多字节字符切成两半，产出非法 UTF-8，终端显示乱码或直接吞字。提示里的
// 字符数也改按 rune 计（此前报的是字节数，中文会虚高 3 倍）。
func clipChars(text string, limit int, what string) (string, string) {
	if len(text) <= limit {
		return text, ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], fmt.Sprintf("… %s已截断（共 %d 字符）", what, utf8.RuneCountInString(text))
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

// tildeMask 是替换单个 ~ 的占位符（Unicode 私有区）。用占位符而不是 \~ 转义：
// goldmark 不消费 ~ 的反斜杠转义，\~ 会连着反斜杠一起印出来。占位符显示宽度 1，
// 与 ~ 相同，不影响 glamour 折行；渲染完再换回 ~。
const tildeMask = "\uE000"

// maskTildesLine 把「单个 ~」换成占位符，连续两个及以上的 ~ 原样保留
// （~~删除线~~ 要照常生效）。行内代码里的 ~ 不动。
func maskTildesLine(s string) string {
	var b strings.Builder
	inCode := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '`' {
			inCode = !inCode
			b.WriteByte(c)
			continue
		}
		if c != '~' || inCode {
			b.WriteByte(c)
			continue
		}
		j := i
		for j < len(s) && s[j] == '~' {
			j++
		}
		if j-i == 1 {
			b.WriteString(tildeMask)
		} else {
			b.WriteString(s[i:j])
		}
		i = j - 1
	}
	return b.String()
}

// maskTildes 按行处理，围栏代码块（```）内部的 ~ 不替换
func maskTildes(text string) string {
	if !strings.Contains(text, "~") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 8)
	inFence := false
	for _, seg := range strings.SplitAfter(text, "\n") {
		line := strings.TrimRight(seg, "\n")
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			b.WriteString(seg)
			continue
		}
		if inFence {
			b.WriteString(seg)
			continue
		}
		b.WriteString(maskTildesLine(line))
		b.WriteString(seg[len(line):])
	}
	return b.String()
}

func unmaskTildes(s string) string { return strings.ReplaceAll(s, tildeMask, "~") }

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
	// 正文（document 级）不带固定前景色：dark 样式默认把整篇染成 ANSI256 "252" 浅灰，
	// 在浅色背景终端（Debian 下常见）整段正文都发灰难读。清掉后正文走终端默认
	// 前景色，深浅背景都可读；标题/代码块等元素级颜色保留。
	base.Document.StylePrimitive.Color = nil
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
	text = maskTildes(text)
	if !colorEnabled {
		return unmaskTildes(text)
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
			return unmaskTildes(text)
		}
		glCache[width] = r
	}
	out, err := r.Render(text)
	if err != nil {
		return unmaskTildes(text)
	}
	return strings.Trim(unmaskTildes(out), "\n")
}

// ---------- 自绘表格 ----------
//
// glamour 的表格按「可用宽度比例」分配列宽：宽度够时把表铺满，不够时直接把
// 单元格硬折（pausedU/P、100./0%），表格塌成一条条竖线。所以面板内的表格改成
// 自绘：按内容算列宽，放不下就压最宽的列并用 … 截断，每行恒定一行、列与列
// 永远对齐。面板宽度不随内容变化（见 panelWidth），始终是终端宽。

// mdTableRowRe 粗判表格行（以 | 开头并以 | 结尾）
var mdTableRowRe = regexp.MustCompile(`(?m)^\s{0,3}\|.*\|\s*$`)

var mdTableSepCellRe = regexp.MustCompile(`^:?-{1,}:?$`)

var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]*)\)`)

// mdTable 面板内自绘表格的数据
type mdTable struct {
	hdr  []string
	dir  []int // 0 左 1 右 2 居中
	rows [][]string
}

// mdSeg 文本片段：table 非空表示这一段是表格，否则走 glamour
type mdSeg struct {
	text  string
	table *mdTable
	raw   string // 表格段的原始 Markdown 行，放不下时交回 glamour 折行渲染
}

// renderPanelMarkdown 面板内 Markdown：表格自绘，其余交给 glamour。
// 宽度由面板给定（不随内容浮动），表格放不下就地截断而不是把框撑宽。
func renderPanelMarkdown(text string, width int) string {
	return renderWithTables(text, width, false)
}

// renderBodyMarkdown 正文（流式）Markdown：表格同样自绘，列宽按内容自适应。
// glamour 的表格按容器宽度分列，两列的窄表也会被撑到满宽；正文改成自绘后，
// 表格宽度只由内容决定，和面板内保持一致的观感。
// 表格本身比可用宽度还宽时退回 glamour 折行渲染——自绘在这种情况只能靠 …
// 截断单元格，信息会丢。
// 非 TTY（管道 / 重定向）下不介入，原样返回，保持 `go-shell "…" | cat` 的输出不变。
func renderBodyMarkdown(text string, width int) string {
	if !colorEnabled {
		return text
	}
	return renderWithTables(text, width, true)
}

// renderWithTables 表格自绘 + 其余段落走 glamour。
// fallbackWide=true（正文）时，放不下的表格交回 glamour，保住单元格的完整内容。
func renderWithTables(text string, width int, fallbackWide bool) string {
	segs := splitMarkdownTables(text)
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.table != nil {
			if fallbackWide && tableLineWidth(tableWidths(s.table)) > width {
				out = append(out, renderMarkdown(s.raw, width))
				continue
			}
			out = append(out, strings.Join(renderTableBlock(s.table, width), "\n"))
			continue
		}
		if strings.TrimSpace(s.text) == "" {
			continue
		}
		out = append(out, renderMarkdown(s.text, width))
	}
	return strings.Join(out, "\n\n")
}

// splitMarkdownTables 把文本切成「普通 Markdown」与「表格」片段
func splitMarkdownTables(text string) []mdSeg {
	lines := strings.Split(text, "\n")
	var segs []mdSeg
	var buf []string
	inFence := false
	flush := func() {
		if len(buf) > 0 {
			segs = append(segs, mdSeg{text: strings.Join(buf, "\n")})
			buf = nil
		}
	}
	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			buf = append(buf, lines[i])
			i++
			continue
		}
		// 围栏代码块里的「| … |」是代码，不能当表格吃掉
		if inFence || !mdTableRowRe.MatchString(lines[i]) {
			buf = append(buf, lines[i])
			i++
			continue
		}
		j := i
		for j < len(lines) && mdTableRowRe.MatchString(lines[j]) {
			j++
		}
		t, ok := parseMarkdownTable(lines[i:j])
		if !ok {
			buf = append(buf, lines[i:j]...)
			i = j
			continue
		}
		flush()
		segs = append(segs, mdSeg{table: t, raw: strings.Join(lines[i:j], "\n")})
		i = j
	}
	flush()
	return segs
}

// parseMarkdownTable 解析一段连续表格行；第二行必须是分隔行（|---|---|）
func parseMarkdownTable(lines []string) (*mdTable, bool) {
	if len(lines) < 2 {
		return nil, false
	}
	hdr, ok := splitTableRow(lines[0])
	if !ok {
		return nil, false
	}
	sep, ok := splitTableRow(lines[1])
	if !ok || !isTableSep(sep) {
		return nil, false
	}
	t := &mdTable{hdr: hdr, dir: make([]int, len(hdr))}
	for i, c := range sep {
		if i >= len(t.dir) {
			break
		}
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
			t.dir[i] = 2
		case strings.HasSuffix(c, ":"):
			t.dir[i] = 1
		}
	}
	for _, ln := range lines[2:] {
		if cells, ok := splitTableRow(ln); ok {
			t.rows = append(t.rows, cells)
		}
	}
	return t, true
}

func splitTableRow(line string) ([]string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "|") || !strings.HasSuffix(s, "|") {
		return nil, false
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "|"), "|")
	parts := strings.Split(s, "|")
	cells := make([]string, len(parts))
	for i, c := range parts {
		cells[i] = plainCell(c)
	}
	return cells, true
}

func isTableSep(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !mdTableSepCellRe.MatchString(c) {
			return false
		}
	}
	return true
}

// plainCell 去掉单元格行内标记：自绘表格不解析 Markdown，留着会露出 ** 与 `
func plainCell(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "~~", "")
	return strings.TrimSpace(s)
}

// tableWidths 列宽：取表头与各行的最大显示宽度
func tableWidths(t *mdTable) []int {
	w := make([]int, len(t.hdr))
	for i, h := range t.hdr {
		w[i] = dw(h)
	}
	for _, r := range t.rows {
		for i := 0; i < len(w) && i < len(r); i++ {
			if d := dw(r[i]); d > w[i] {
				w[i] = d
			}
		}
	}
	return w
}

// tableLineWidth 整行宽度：每格两侧各 1 空格，格间 1 个 │
func tableLineWidth(widths []int) int {
	if len(widths) == 0 {
		return 0
	}
	s := 0
	for _, w := range widths {
		s += w + 2
	}
	return s + len(widths) - 1
}

// renderTableBlock 自绘定宽表格：一行一条记录、列线永远对齐。
// 放不下时先只压长文本列（名称这类），短列（大小/进度/时间）保持完整；
// 连这一步都不够，才整体压缩，并在末尾提示终端太窄。
func renderTableBlock(t *mdTable, budget int) []string {
	widths := tableWidths(t)
	n := len(widths)
	if n == 0 {
		return nil
	}
	// 第一阶段的下限：短列（≤12）不压，长列至少留 12，长文本列吸收截断
	mins := make([]int, n)
	for i := range mins {
		if widths[i] <= 12 {
			mins[i] = widths[i]
		} else {
			mins[i] = 12
		}
		if h := dw(t.hdr[i]); h > mins[i] {
			mins[i] = h
		}
		if mins[i] > widths[i] {
			mins[i] = widths[i]
		}
	}
	shrink := func(floor func(i int) int) bool {
		for tableLineWidth(widths) > budget {
			idx := -1
			for i, w := range widths {
				if w > floor(i) && (idx < 0 || w > widths[idx]) {
					idx = i
				}
			}
			if idx < 0 {
				return false
			}
			widths[idx]--
		}
		return true
	}
	forced := false
	if !shrink(func(i int) int { return mins[i] }) {
		forced = true
		// 第二阶段：连下限都放不下（列太多），退到表头宽度，再不行退到 4
		shrink(func(i int) int {
			if h := dw(t.hdr[i]); h > 4 {
				return h
			}
			return 4
		})
	}

	renderRow := func(cells []string) string {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			c = fitWidth(c, widths[i])
			pad := widths[i] - dw(c)
			switch t.dir[i] {
			case 1:
				c = strings.Repeat(" ", pad) + c
			case 2:
				l := pad / 2
				c = strings.Repeat(" ", l) + c + strings.Repeat(" ", pad-l)
			default:
				c = c + strings.Repeat(" ", pad)
			}
			parts[i] = " " + c + " "
		}
		return strings.Join(parts, "│")
	}

	out := make([]string, 0, len(t.rows)+3)
	out = append(out, renderRow(t.hdr))
	sep := make([]string, n)
	for i := range sep {
		sep[i] = strings.Repeat("─", widths[i]+2)
	}
	out = append(out, strings.Join(sep, "┼"))
	for _, r := range t.rows {
		out = append(out, renderRow(r))
	}
	if forced {
		out = append(out, sty(stMuted, "… 终端宽度不足，表格已压缩显示"))
	}
	return out
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
		if r == '\r' {
			// 丢弃 CR：残留的 \r（CRLF 输出等）会把光标拉回行首，
			// 让后续字符从行首覆盖，画面出现左半截被吃、边框错位
			i += size
			continue
		}
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

// fitWidth 把 s 收到显示宽度 ≤ max 以内，超出用 … 收尾。
// 面板标题/副标、紧凑值超宽都会顶破边框或撑开列宽，必须按显示宽度收；
// 按 rune 数收会把中文（一个 rune 两列）算少，按字节收会把字符切坏。
func fitWidth(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if dw(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// PadRight 按显示宽度右侧补空格。导出给 chat 的列表用：%-20s 按字节补位，
// 中文名占 3 字节/2 列，列宽会歪。
func PadRight(s string, width int) string { return padTo(s, width, false) }

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
	// 标题超宽会把顶边顶出 width-1：dash 被钳到 1 后仍然溢出，整框右边线错行。
	// 顶边可用宽度 = width-1-5（╭ + "─ " + 空格 + dash + ╮），故标题最多 width-7。
	fullTitle := fitWidth("▸ "+title, width-7)

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
		// 副标同理：底边 ╰ + left + 空格 + 副标 + 空格 + ── + ╯，最多 width-8
		subtitle = fitWidth(subtitle, width-8)
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
	// 收口按显示宽度：原来用 dw 判断、用 rune 数截，中文（1 rune 2 列）会被判超长
	// 却只截掉一半的宽度，行宽照样撑破
	text = fitWidth(text, compactValueMax)
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
		// 键列宽按显示宽度量（len 是字节数，中文键会被算宽 1.5 倍，列距被拉开）
		if dw(p[0]) > w {
			w = dw(p[0])
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
	// 加粗是模型最常用的排版（「**一句话标题**」+ 正文），权重必须够：此前只给 1，
	// 「纯加粗」「加粗+列表」都到不了阈值 3，被当成纯文本原样印出 ** 与 - 标记
	{regexp.MustCompile(`(?m)\*\*[^*\n]+\*\*`), 3},
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
		md := renderPanelMarkdown(body, width)
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

// payloadContentWidth 内容不被压缩所需的渲染宽度，取三者的较大值：
// 表格自然宽、纯文本最长行宽。JSON 不算——它会被 jsonView 重新格式化折行，
// 单行几千字符没有参考意义，否则每块 JSON 面板都会拉满终端。
func payloadContentWidth(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	best := 0
	consider := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || looksLikeJSON(s) {
			return
		}
		if w := maxLineWidth(s); w > best {
			best = w
		}
		for _, seg := range splitMarkdownTables(s) {
			if seg.table == nil {
				continue
			}
			if w := tableLineWidth(tableWidths(seg.table)); w > best {
				best = w
			}
		}
	}
	consider(text)
	// envelope（{status, result}）里的表格在 result 的文本字段中，一并量
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err == nil {
		if _, ok := env["status"]; ok {
			switch r := env["result"].(type) {
			case string:
				consider(r)
			case map[string]any:
				for _, k := range sortedKeys(r) {
					if s, ok := r[k].(string); ok && textKeys[k] {
						consider(s)
					}
				}
			}
		}
	}
	return best
}

// looksLikeJSON 判断一段文本是否是 JSON 对象/数组：这类内容会交给 jsonView
// 重新格式化折行，原始单行长度不代表渲染宽度。
func looksLikeJSON(s string) bool {
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// maxLineWidth 最长一行的显示宽度
func maxLineWidth(s string) int {
	w := 0
	for _, ln := range strings.Split(s, "\n") {
		if d := dw(ln); d > w {
			w = d
		}
	}
	return w
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
