//go:build !windows

// Unix 进程组控制：execute_system_command 的子进程独立成组，超时可整树击杀。
package tools

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(p *os.Process) {
	if p != nil {
		syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}
