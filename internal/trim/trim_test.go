package trim

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-shell/internal/config"
)

// 客户端只建一次，超时在这里定死；各用例只改地址/开关。
func TestMain(m *testing.M) {
	config.TrimTimeout = 5
	config.TrimRetrySeconds = 0
	config.TrimMode = "auto"
	config.TrimMaxChars = 6000
	config.TrimFormat = "term"
	m.Run()
}

func resetState() {
	config.TrimEnabled = true
	config.TrimHost = ""
	config.TrimPort = 0
	config.TrimRetrySeconds = 0
	reset() // 清熔断
}

// pointTo 把配置指向 httptest 服务
func pointTo(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析测试服务地址失败：%v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	config.TrimHost = u.Hostname()
	config.TrimPort = port
}

func TestBaseURL(t *testing.T) {
	defer resetState()
	cases := []struct {
		name string
		host string
		port int
		want string
	}{
		{"未配置 host", "", 8787, ""},
		{"只有 host 没端口", "192.168.30.181", 0, ""},
		{"host + PORT", "192.168.30.181", 8787, "http://192.168.30.181:8787"},
		{"host 自带端口优先", "10.0.0.2:9999", 8787, "http://10.0.0.2:9999"},
		{"host 带 scheme", "http://example.com", 80, "http://example.com:80"},
		{"反代子路径保留", "http://example.com/rtk", 8080, "http://example.com:8080/rtk"},
		{"尾斜杠去掉", "http://example.com/rtk/", 8080, "http://example.com:8080/rtk"},
		{"非法 scheme 跳过", "ftp://example.com", 21, ""},
		{"空白 host 跳过", "   ", 8787, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config.TrimHost = c.host
			config.TrimPort = c.port
			if got := BaseURL(); got != c.want {
				t.Fatalf("BaseURL()=%q，期望 %q", got, c.want)
			}
		})
	}
}

func TestApplySuccess(t *testing.T) {
	defer resetState()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/compress" {
			t.Errorf("意外的请求 %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("Content-Type=%q", ct)
		}
		gotQuery = r.URL.Query()
		w.Header().Set("X-Rtk-Saved-Percent", "62.5")
		w.Header().Set("X-Rtk-Bytes-Before", "160")
		w.Header().Set("X-Rtk-Bytes-After", "60")
		_, _ = w.Write([]byte("压缩后的输出"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)

	out, stats, ok := Apply(strings.Repeat("a\n", 80))
	if !ok {
		t.Fatal("期望修剪成功")
	}
	if out != "压缩后的输出" {
		t.Fatalf("输出=%q", out)
	}
	if stats["saved_percent"] != 62.5 {
		t.Fatalf("saved_percent=%v", stats["saved_percent"])
	}
	if stats["bytes_before"] != int64(160) || stats["bytes_after"] != int64(60) {
		t.Fatalf("bytes 统计不对：%v", stats)
	}
	for k, want := range map[string]string{"format": "term", "mode": "auto", "lossless": "false", "charset": "utf-8"} {
		if got := gotQuery.Get(k); got != want {
			t.Fatalf("%s=%q，期望 %q", k, got, want)
		}
	}
}

func TestApplyStatsFallback(t *testing.T) {
	defer resetState()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rtk-Saved-Percent", "不是数字")
		_, _ = w.Write([]byte("短"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)

	text := strings.Repeat("原始输出行\n", 10) // 60 字符
	_, stats, ok := Apply(text)
	if !ok {
		t.Fatal("期望修剪成功")
	}
	// 头非法：saved_percent 按字符数本地算；bytes_* 按字节数兜底
	if got := stats["saved_percent"].(float64); got <= 0 || got >= 100 {
		t.Fatalf("saved_percent=%v", got)
	}
	if stats["bytes_before"] != int64(len(text)) || stats["bytes_after"] != int64(len("短")) {
		t.Fatalf("bytes 兜底不对：%v", stats)
	}
}

func TestApplyKeepsOriginalWhenNotShorter(t *testing.T) {
	defer resetState()
	cases := []struct{ name, body string }{
		{"空响应", ""},
		{"一样长", "abcdef"},
		{"更长", strings.Repeat("x", 100)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			pointTo(t, srv.URL)
			if _, _, ok := Apply("abcdef"); ok {
				t.Fatalf("返回 %q 不该被采用", c.body)
			}
		})
	}
}

func TestApplySkipsWhenDisabledOrUnconfigured(t *testing.T) {
	defer resetState()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("短"))
	}))
	defer srv.Close()

	config.TrimEnabled = false
	pointTo(t, srv.URL)
	if _, _, ok := Apply("一段足够长的原始输出内容"); ok {
		t.Fatal("关闭后不该修剪")
	}

	config.TrimEnabled = true
	config.TrimHost = "" // 未配置地址
	if _, _, ok := Apply("一段足够长的原始输出内容"); ok {
		t.Fatal("没配地址不该修剪")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("不该发出请求，实际 %d 次", got)
	}
}

func TestApplySkipsOversizeBody(t *testing.T) {
	defer resetState()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("短"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)

	big := strings.Repeat("a", maxBodyBytes+1)
	if _, _, ok := Apply(big); ok {
		t.Fatal("超过 9 MiB 应跳过")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("超限不该发请求，实际 %d 次", got)
	}
}

func TestApplyFlagsLossyResults(t *testing.T) {
	defer resetState()
	// 带省略标记的结果照样采用（服务端按 max_chars 预算裁的，报错行已保住），
	// 但统计里要标出来，调用方/模型才知道结果不完整。
	lossy := []string{
		"drwxr-xr-x 2 root root 4096 a\n... (1 filtered)",
		"文件列表\n  ... (12 more)",
		"文件列表\n... (2 more, 1 filtered)",
		"[… 33 lines elided …]\n总计 1.2M",
		"很长的一行 [+120 chars]",
	}
	for _, body := range lossy {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		pointTo(t, srv.URL)
		out, stats, ok := Apply(strings.Repeat("原始输出行\n", 30))
		srv.Close()
		if !ok {
			t.Fatalf("带省略标记的结果 %q 应被采用", body)
		}
		if stats["lossy"] != true {
			t.Fatalf("结果 %q 应标记 lossy，实际 %v", body, stats["lossy"])
		}
		if out == "" {
			t.Fatal("采用了结果却返回空文本")
		}
	}

	// 正文里恰好出现的括号不是独立提示行，不该标 lossy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("共 3 项，省略 ... (3 more) 行\n附加说明行"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)
	_, stats, ok := Apply(strings.Repeat("原始输出行\n", 30))
	if !ok {
		t.Fatal("正文内联括号不该影响采用")
	}
	if stats["lossy"] == true {
		t.Fatal("正文内联括号不该被标成 lossy")
	}
}

func TestApplySendsMaxCharsBudget(t *testing.T) {
	defer resetState()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("max_chars")
		_, _ = w.Write([]byte("短"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)

	if _, _, ok := Apply(strings.Repeat("原始输出行\n", 30)); !ok {
		t.Fatal("应返回压缩结果")
	}
	if got != strconv.Itoa(config.TrimMaxChars) {
		t.Fatalf("max_chars 应传 %d，实际 %q", config.TrimMaxChars, got)
	}
}

func TestApplyBackoffAfterFailure(t *testing.T) {
	defer resetState()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	pointTo(t, srv.URL)
	config.TrimRetrySeconds = 60

	text := "一段足够长的原始输出内容，用来触发修剪请求"
	if _, _, ok := Apply(text); ok {
		t.Fatal("500 不该返回成功")
	}
	if !inBackoff() {
		t.Fatal("失败后应进入熔断")
	}
	// 熔断期内不再打服务
	if _, _, ok := Apply(text); ok {
		t.Fatal("熔断期内不该修剪")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("熔断期内又发了请求，总请求数 %d", got)
	}

	// 熔断到期（把截止时间拨到过去）后恢复尝试
	mu.Lock()
	downUntil = time.Now().Add(-time.Second)
	mu.Unlock()
	Apply(text)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("熔断到期应重试，总请求数 %d", got)
	}
}

func TestApplyRecoversFromBackoff(t *testing.T) {
	defer resetState()
	var fail int32 = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("短"))
	}))
	defer srv.Close()
	pointTo(t, srv.URL)
	config.TrimRetrySeconds = 0 // 不熔断：下一条立刻重试

	text := "一段足够长的原始输出内容"
	Apply(text)
	atomic.StoreInt32(&fail, 0)
	if _, _, ok := Apply(text); !ok {
		t.Fatal("服务恢复后应修剪成功")
	}
}
