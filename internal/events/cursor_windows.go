//go:build !linux

package events

// cursorRow 非 Linux 平台不做 DSR 查询（Windows conpty 的应答消费不可靠），
// 返回 -1 让 Live 区退回按整屏高度裁剪的保守策略。
func cursorRow() int { return -1 }
