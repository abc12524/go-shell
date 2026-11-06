//go:build windows

// Windows 进程树控制：无 Setpgid/进程组，超时用 taskkill /T 整树击杀。
package tools

import (
	"os"
	"os/exec"
	"strconv"
)

func setProcGroup(cmd *exec.Cmd) {}

func killTree(p *os.Process) {
	if p != nil {
		exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(p.Pid)).Run()
	}
}
