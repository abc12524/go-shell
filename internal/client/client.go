// Package client：远程客户端模式（go-shell -h 别名）
//
// 本进程不跑引擎，只把问题发给远程 go-shell server 的 /chat/stream，
// 把 SSE 事件还原成终端渲染（与本地模式同一套 events.Terminal）。
//
//	事件流：data: {"type":"content","content":"..."} → evt{"t":"content",...}
//	注释行：: heartbeat（保活，忽略）
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go-shell/internal/config"
	"go-shell/internal/events"
)

// Request 发往远程 /chat/stream 的请求体（服务端 parseRequest 解析）
type Request struct {
	Question string   `json:"question,omitempty"`
	New      bool     `json:"new,omitempty"`
	Args     []string `json:"args,omitempty"`
	Tips     string   `json:"tips,omitempty"`
	OpenID   string   `json:"openid,omitempty"`
}

func newHTTPClient() *http.Client {
	timeout := time.Duration(config.RemoteTimeout) * time.Second
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: timeout,
			// 流式响应：不设总超时，靠 ctx 取消
			ResponseHeaderTimeout: 60 * time.Second,
			Proxy:                 nil, // 内网直连，别让 HTTP_PROXY 把请求送进代理
		},
	}
}

// Ping 连一下 /health：地址写错 / 服务没起 / 网段不通都在这儿立刻暴露，
// 不要等 /chat/stream 卡在 DNS 或 TCP 上。
func Ping(ctx context.Context, tgt config.RemoteTarget) error {
	c := newHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s：%v\n"+
			"  确认对端已启动 `go-shell server`，且 REMOTE_HOSTS 里的地址/端口是本机可达的内网地址", tgt.Label(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("远程 %s 的 /health 返回 %d", tgt.Label(), resp.StatusCode)
	}
	return nil
}

// Run 发起一轮远程对话并渲染到 sink。返回流内的致命错误（HTTP / 连接层）。
func Run(ctx context.Context, tgt config.RemoteTarget, reqBody Request, sink events.Sink, out io.Writer) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tgt.BaseURL()+"/chat/stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if tgt.Key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tgt.Key)
	}

	resp, err := newHTTPClient().Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("连不上 %s：%v\n"+
			"  确认对端已启动 `go-shell server`，且地址/端口可达", tgt.Label(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return httpError(tgt, resp)
	}
	return consumeSSE(resp.Body, sink, out)
}

// httpError 把服务端的错误 JSON 翻成一句能照做的话
func httpError(tgt config.RemoteTarget, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(raw, &payload)
	msg := strings.TrimSpace(payload.Error)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = resp.Status
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && payload.Code == "access_code_required":
		return fmt.Errorf("远程 %s 要求访问验证码：在配置里给该主机配上访问码（REMOTE_HOSTS=别名:端口:访问码），或设 REMOTE_KEY=%s", tgt.Label(), "<对端 API_ACCESS_KEY>")
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("远程 %s 拒绝了访问码（%s）：确认与对端配置文件里的 API_ACCESS_KEY 一致", tgt.Label(), msg)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("远程 %s 没有 /chat/stream（%s）：对端可能是旧版本，升级后再连", tgt.Label(), msg)
	}
	return fmt.Errorf("远程 %s 返回 %d：%s", tgt.Label(), resp.StatusCode, msg)
}

// consumeSSE 逐行读 SSE，事件写进 sink
func consumeSSE(r io.Reader, sink events.Sink, out io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var streamErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行分隔 / ": heartbeat" 保活
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue // 半条/坏帧：跳过，别把整轮打断
		}
		kind, _ := evt["type"].(string)
		delete(evt, "type")
		switch kind {
		case "log":
			// 非对话动作的整段文本（-s / -S / -I / -D）：原样打到终端
			if text, ok := evt["text"].(string); ok {
				fmt.Fprint(out, text)
			}
		case "start", "done":
			// 起止标记：渲染层不需要
		case "error":
			// 流内错误：交给上层统一打印一次（这里不再给 sink，避免重复输出）
			msg, _ := evt["error"].(string)
			if msg == "" {
				msg = "远程返回未知错误"
			}
			streamErr = fmt.Errorf("%s", msg)
		default:
			evt["t"] = kind
			events.Emit(sink, evt)
		}
	}
	if err := scanner.Err(); err != nil {
		// ctx 取消（用户 Ctrl-C）走这里：交给上层判定成"已中断"
		return err
	}
	return streamErr
}
