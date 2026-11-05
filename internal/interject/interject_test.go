package interject

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPushWithoutActiveTurn(t *testing.T) {
	if Active() != "" {
		t.Fatalf("期望没有活跃轮次，实际 %q", Active())
	}
	if it, turnID := Push("", "在吗"); it != nil || turnID != "" {
		t.Fatalf("没有活跃轮次时不该接受插话，得到 %v / %q", it, turnID)
	}
}

func TestDrainThenAck(t *testing.T) {
	turn := Begin("s1")
	defer turn.End()

	item, turnID := Push("", "顺便看下磁盘")
	if item == nil || turnID != turn.ID {
		t.Fatalf("Push 应投给当前轮次 %s，得到 %v / %q", turn.ID, item, turnID)
	}
	if turn.Pending() != 1 {
		t.Fatalf("队列应为 1，实际 %d", turn.Pending())
	}

	drained := turn.Drain()
	if len(drained) != 1 || drained[0].Text != "顺便看下磁盘" {
		t.Fatalf("Drain 结果不对：%+v", drained)
	}
	if turn.Pending() != 0 {
		t.Fatalf("取走后队列应清空，实际 %d", turn.Pending())
	}

	turn.Ack(item)
	if got := item.Wait(context.Background(), time.Second); got != StatusInjected {
		t.Fatalf("回执后应是 injected，实际 %s", got)
	}
}

func TestEndExpiresPending(t *testing.T) {
	turn := Begin("s1")
	item, _ := Push("", "改个方向")
	if item == nil {
		t.Fatal("Push 失败")
	}

	if dropped := turn.End(); dropped != 1 {
		t.Fatalf("End 应报告 1 条未交付，实际 %d", dropped)
	}
	if got := item.Wait(context.Background(), time.Second); got != StatusExpired {
		t.Fatalf("轮次结束应回 expired，实际 %s", got)
	}
	if Active() != "" {
		t.Fatalf("End 后不该还有活跃轮次，实际 %q", Active())
	}
}

func TestWaitTimeoutKeepsItem(t *testing.T) {
	turn := Begin("s1")
	defer turn.End()

	item, _ := Push("", "改个方向")
	if item == nil {
		t.Fatal("Push 失败")
	}
	if got := item.Wait(context.Background(), 20*time.Millisecond); got != StatusTimeout {
		t.Fatalf("无人回执时应超时，实际 %s", got)
	}
	// 超时不等于作废：这条还在队列里，之后走到工具边界仍会被取用
	if len(turn.Drain()) != 1 {
		t.Fatal("超时后插话应仍在队列中")
	}
}

func TestPushToSpecificTurn(t *testing.T) {
	first := Begin("s1")
	second := Begin("s2")
	defer first.End()
	defer second.End()

	if Active() != second.ID {
		t.Fatalf("当前目标应是最新轮次 %s，实际 %q", second.ID, Active())
	}
	item, turnID := PushTo(first.ID, "inj1", "给第一轮的话")
	if item == nil || turnID != first.ID {
		t.Fatalf("PushTo 应投给指定轮次，得到 %v / %q", item, turnID)
	}
	if len(first.Drain()) != 1 || len(second.Drain()) != 0 {
		t.Fatal("插话投错了轮次")
	}
	// 指定一个不存在的轮次 → 退回当前目标
	if _, turnID := PushTo("t404", "inj2", "兜底"); turnID != second.ID {
		t.Fatalf("未知轮次应退回当前目标，实际 %q", turnID)
	}
}

func TestPushRejectsEmptyText(t *testing.T) {
	turn := Begin("s1")
	defer turn.End()
	if it, _ := Push("", "   "); it != nil {
		t.Fatal("空文本不该入队")
	}
}

func TestWrapCarriesTags(t *testing.T) {
	out := Wrap("  先别删库  ")
	if !strings.Contains(out, OpenTag) || !strings.Contains(out, CloseTag) {
		t.Fatalf("包装缺标签：%s", out)
	}
	if !strings.Contains(out, "先别删库") || strings.Contains(out, "  先别删库") {
		t.Fatalf("正文未 trim：%s", out)
	}
}

func TestContextRoundTrip(t *testing.T) {
	turn := Begin("s1")
	defer turn.End()

	if FromContext(context.Background()) != nil {
		t.Fatal("空 ctx 不该取到轮次")
	}
	ctx := WithTurn(context.Background(), turn)
	if got := FromContext(ctx); got != turn {
		t.Fatalf("ctx 取回的轮次不对：%+v", got)
	}
	if WithTurn(ctx, nil) != ctx {
		t.Fatal("nil 轮次应原样返回 ctx")
	}
}
