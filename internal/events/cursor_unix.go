//go:build linux

package events

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// cursorRow 查询光标所在行（1-based，DSR 6n 应答）。
// 仅用于 Live 区起点测量：知道起点行才能把流式帧钉在屏幕剩余空间内，
// 保证流式重绘绝不滚屏。termios VMIN=0/VTIME=2 限时读应答，终端不应答
// 时最多阻塞 ~500ms 后放弃（返回 -1，调用方退回保守策略），绝不挂死。
func cursorRow() int {
	fd := int(os.Stdin.Fd())
	if !termIsTerminal(fd) {
		return -1
	}
	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return -1
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO | unix.ICANON
	newState.Cc[unix.VMIN] = 0
	newState.Cc[unix.VTIME] = 2 // 200ms
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &newState); err != nil {
		return -1
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, oldState) }()

	fmt.Fprint(os.Stdout, "\x1b[6n")
	buf := make([]byte, 0, 16)
	one := make([]byte, 1)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, err := unix.Read(fd, one)
		if err != nil || n < 0 {
			return -1
		}
		if n == 0 {
			continue
		}
		if one[0] == 'R' {
			break
		}
		buf = append(buf, one[0])
		if len(buf) > 32 {
			break
		}
	}
	// 应答形如 \x1b[<row>;<col>R（部分终端带 ? 前缀）
	s := string(buf)
	if i := strings.LastIndex(s, "\x1b["); i >= 0 {
		s = strings.TrimPrefix(s[i+2:], "?")
		parts := strings.SplitN(s, ";", 2)
		if row, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil && row > 0 {
			return row
		}
	}
	return -1
}
