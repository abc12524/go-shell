// Package interject：运行中轮次的中途插话（移植 xiaofy `core/inject.py` 的进程内版本）
//
// 一句话：用户在一轮还在干活时追说的话，先入队，等这一轮走到工具边界再递进上下文。
//
//	POST /interject {"text":"..."}（访问校验见 config.API_ACCESS_KEY 全局验证码）
//	→ injected（已进上下文并落库）/ timeout（这一轮没走到工具边界）/ expired（轮次已结束）/ idle（没在跑）
//
// 与 Python 版的差别：go-shell 的引擎就在同一个进程里，不需要「父进程写子进程 stdin」
// 那条管道，于是只剩「入队 → 工具边界取用 → 落库后回执」三件事，一把锁即可。
//
// 三条硬约束（改之前先读，改错会 400 或丢消息）：
//  1. 落点只能在**整批**工具结果之后。夹在两批并行 function_call_output 中间会留下
//     没有 output 的孤立 function_call，Responses 与 Chat Completions 都 400，更糟的是
//     它已写进历史，之后每轮重放都会再错一次。
//  2. 插话按「新任务」处理：落地后工具预算重置，否则剩两轮时插进来的要求根本来不及做。
//  3. 回执必须发生在落库之后：HTTP 只有拿到 Ack 才敢回 injected，客户端才敢不再重发。
package interject

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 插话消息的包装标签（模型据此知道"这是中途追的话"，不必塞进 system prompt）
const (
	OpenTag  = "<user_interjection>"
	CloseTag = "</user_interjection>"
)

// 交付结果
const (
	StatusInjected = "injected" // 已写进上下文并落库
	StatusTimeout  = "timeout"  // 等超时（这一轮还没走到工具边界）
	StatusExpired  = "expired"  // 轮次结束，没赶上
)

// 提示写在这条消息自己身上：只有真的发生插话才花这几十个 token，也不影响前缀缓存。
const note = "（上面是你在处理上一件事期间，用户又发来的一句。先回应这句；" +
	"如果它和手头正在做的事冲突，就停下当前任务，说清做到哪一步了，再按新要求来。）"

// Wrap 把插话包成一条能自解释的 user 消息。
func Wrap(text string) string {
	return OpenTag + "\n" + strings.TrimSpace(text) + "\n" + CloseTag + "\n" + note
}

// Item 一条待落地的插话。
type Item struct {
	ID   string
	Text string

	once sync.Once
	ack  chan string
}

func (it *Item) finish(status string) {
	it.once.Do(func() { it.ack <- status })
}

// Wait 等交付结果。超时返回 StatusTimeout——这条仍留在队列里，之后仍可能被取用。
func (it *Item) Wait(ctx context.Context, d time.Duration) string {
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case status := <-it.ack:
		return status
	case <-timer.C:
		return StatusTimeout
	case <-ctx.Done():
		return StatusTimeout
	}
}

// Turn 一次正在跑的轮次，即插话目标。
type Turn struct {
	ID        string
	SessionID string

	mu    sync.Mutex
	queue []*Item
	ended bool
}

var (
	regMu   sync.Mutex
	turns   = map[string]*Turn{}
	order   []string // 注册顺序，最后一个即当前插话目标
	seq     uint64
	itemSeq uint64
)

// Begin 注册一个正在跑的轮次，成为新的插话目标。
func Begin(sessionID string) *Turn {
	t := &Turn{ID: fmt.Sprintf("t%d", atomic.AddUint64(&seq, 1)), SessionID: sessionID}
	regMu.Lock()
	turns[t.ID] = t
	order = append(order, t.ID)
	regMu.Unlock()
	return t
}

// SetSession 会话解析完成后回填 session id（RunTurn 里 ResolveSession 在 Begin 之后）。
func (t *Turn) SetSession(sessionID string) {
	t.mu.Lock()
	t.SessionID = sessionID
	t.mu.Unlock()
}

// End 注销轮次；没赶上落地的插话以 expired 收尾，返回条数（调用方据此提示）。
func (t *Turn) End() int {
	regMu.Lock()
	delete(turns, t.ID)
	for i, id := range order {
		if id == t.ID {
			order = append(order[:i], order[i+1:]...)
			break
		}
	}
	regMu.Unlock()

	t.mu.Lock()
	pending := t.queue
	t.queue = nil
	t.ended = true
	t.mu.Unlock()

	for _, it := range pending {
		it.finish(StatusExpired)
	}
	return len(pending)
}

// Push 把一条插话投给当前的插话目标，返回 (item, turnID)。
// 没有活跃轮次（或目标已结束）时返回 (nil, "")，由调用方回 idle。
func Push(id, text string) (*Item, string) {
	return PushTo("", id, text)
}

// PushTo 投给指定轮次；turnID 为空时用当前插话目标（最近开始的那一轮）。
func PushTo(turnID, id, text string) (*Item, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ""
	}

	regMu.Lock()
	target := turnID
	if _, ok := turns[target]; !ok {
		target = ""
		if n := len(order); n > 0 {
			target = order[n-1]
		}
	}
	t := turns[target]
	regMu.Unlock()
	if t == nil {
		return nil, ""
	}

	if strings.TrimSpace(id) == "" {
		id = fmt.Sprintf("inj%d", atomic.AddUint64(&itemSeq, 1))
	}
	it := &Item{ID: id, Text: text, ack: make(chan string, 1)}

	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return nil, ""
	}
	t.queue = append(t.queue, it)
	t.mu.Unlock()
	return it, t.ID
}

// Drain 取走当前所有待落地插话（只允许在工具边界调用）。
func (t *Turn) Drain() []*Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) == 0 {
		return nil
	}
	out := t.queue
	t.queue = nil
	return out
}

// Ack 落库之后回执，等待方此时才会收到 StatusInjected。
func (t *Turn) Ack(it *Item) { it.finish(StatusInjected) }

// Pending 队列里还剩几条。
func (t *Turn) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.queue)
}

// Active 当前插话目标的 turn id（没有活跃轮次时为空）。
func Active() string {
	regMu.Lock()
	defer regMu.Unlock()
	if n := len(order); n > 0 {
		return order[n-1]
	}
	return ""
}

// ---------- context 携带 turn：引擎在工具边界就地取用，不必改一长串函数签名 ----------

type turnKey struct{}

// WithTurn 把轮次挂进 ctx。
func WithTurn(ctx context.Context, t *Turn) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, turnKey{}, t)
}

// FromContext 取 ctx 里的轮次（CLI 直跑或测试里可能为空）。
func FromContext(ctx context.Context) *Turn {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(turnKey{}).(*Turn)
	return t
}
