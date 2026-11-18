// Package trim：把命令输出交给输出修剪服务（后端 rtk-web）压缩。
//
// execute_system_command 在返回模型前调用 Apply。修剪只是省 token，不影响命令本身的
// 结果：服务不可达、超时、非 2xx、返回无法解码或压缩后不更短，一律静默回退原文。
package trim

import (
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go-shell/internal/config"
)

// maxBodyBytes 单次请求体上限：rtk-web 的 RTK_WEB_MAX_BODY 默认 10 MiB，超了就跳过修剪。
const maxBodyBytes = 9 << 20

// lossyRe 压缩结果里代表"有条目没进输出"的标记：
//
//	ls 过滤器        -> "... (12 more)" / "... (3 filtered)"
//	cap 省略         -> "[… 33 lines elided …]"
//	budget 裁剪      -> "[… 33 lines elided …]" / "[+120 chars]"
//
// 这只是标注，不是回退理由：宿主带了 max_chars，服务端就是按预算裁的，
// 报错行、首行、尾段都已保住。回退原文等于把"按预算选过的 6000 字符"
// 换成"从头砍的 6000 字符"，覆盖范围更差。
var lossyRe = regexp.MustCompile(`(?m)lines elided|\d+ chars truncated|^\s*\.\.\. \(\d+ (?:more|filtered)`)

var (
	mu        sync.Mutex    // 保护 downUntil
	downUntil time.Time     // 熔断截止时间：服务挂了不必每条命令都等满超时
	once      sync.Once     // 客户端只建一次，复用连接
	client    *http.Client
)

// BaseURL 按 OUTPUT_TRIM_HOST / OUTPUT_TRIM_PORT 拼服务地址，兼容 host 自带 scheme/port。
// 未配置 host、host 非法或拿不到端口时返回空串，调用方跳过修剪。
func BaseURL() string {
	host := strings.TrimSpace(config.TrimHost)
	if host == "" {
		return ""
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	// 内网服务默认明文 http；https 也允许（反向代理终结 TLS 时）
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if config.TrimPort <= 0 {
			return ""
		}
		port = strconv.Itoa(config.TrimPort)
	}
	// 保留 path 前缀（反代挂在子路径时有用），去掉尾斜杠
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port) + strings.TrimRight(u.Path, "/")
}

// Apply 把 text 交给修剪服务压缩。
// 成功返回 (压缩后文本, 统计, true)；不值得/不能修剪时返回零值 + false，调用方用原文。
//
// 统计字段与 shell-tool 的 trim 字段同构：saved_percent / bytes_before / bytes_after。
func Apply(text string) (string, map[string]any, bool) {
	if !config.TrimEnabled || text == "" {
		return "", nil, false
	}
	if inBackoff() {
		return "", nil, false
	}
	base := BaseURL()
	if base == "" || len(text) > maxBodyBytes {
		return "", nil, false
	}

	q := url.Values{}
	q.Set("format", config.TrimFormat)
	q.Set("mode", config.TrimMode)
	q.Set("lossless", "false")
	q.Set("charset", "utf-8") // 子进程输出已统一转成 UTF-8 再进来
	// 让服务端知道宿主的截断口径：它在预算内按重要性裁（保报错行 / 首尾 /
	// 采样中间），比我们拿到结果后砍尾巴更省也更准。
	q.Set("max_chars", strconv.Itoa(config.TrimMaxChars))

	req, err := http.NewRequest(http.MethodPost, base+"/api/compress?"+q.Encode(), strings.NewReader(text))
	if err != nil {
		return "", nil, false
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := httpClient().Do(req)
	if err != nil {
		trip()
		return "", nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		trip()
		return "", nil, false
	}
	reset()

	trimmed := string(body)
	if !utf8.ValidString(trimmed) {
		return "", nil, false
	}
	before, after := utf8.RuneCountInString(text), utf8.RuneCountInString(trimmed)
	// 没变短就没必要换——修剪的意义只有省 token
	if trimmed == "" || after >= before {
		return "", nil, false
	}
	// 压缩结果带省略标记 = 有条目没进输出，但这是服务端按预算做的取舍，
	// 只在统计里标出来（调用方/模型可据此决定要不要精确化命令），不回退原文。
	lossy := lossyRe.MatchString(trimmed)

	stats := map[string]any{
		"saved_percent": headerFloat(resp.Header, "X-Rtk-Saved-Percent",
			math.Round(float64(before-after)/float64(before)*1000)/10),
		"bytes_before": headerInt(resp.Header, "X-Rtk-Bytes-Before", int64(len(text))),
		"bytes_after":  headerInt(resp.Header, "X-Rtk-Bytes-After", int64(len(body))),
		"lossy":        lossy,
	}
	return trimmed, stats, true
}

// httpClient 独立 Transport：Proxy 置 nil，避免本机常驻的 HTTP_PROXY 把内网请求
// 送进代理（与 internal/client 同样的处理）。
func httpClient() *http.Client {
	once.Do(func() {
		client = &http.Client{
			Timeout: time.Duration(config.TrimTimeout * float64(time.Second)),
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: time.Duration(config.TrimTimeout * float64(time.Second)),
				MaxIdleConns:          4,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	})
	return client
}

func inBackoff() bool {
	mu.Lock()
	defer mu.Unlock()
	return time.Now().Before(downUntil)
}

// trip 记下熔断截止时间；OUTPUT_TRIM_RETRY_SECONDS=0 表示不熔断（每条都试）。
func trip() {
	retry := time.Duration(config.TrimRetrySeconds * float64(time.Second))
	if retry <= 0 {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	downUntil = time.Now().Add(retry)
}

// reset 服务恢复正常，清掉熔断状态。
func reset() {
	mu.Lock()
	defer mu.Unlock()
	downUntil = time.Time{}
}

// headerFloat 读 X-Rtk-* 数字头，缺失或非法时用兜底值。
func headerFloat(h http.Header, name string, def float64) float64 {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// headerInt 读 X-Rtk-* 整数头，缺失或非法时用兜底值。
func headerInt(h http.Header, name string, def int64) int64 {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
