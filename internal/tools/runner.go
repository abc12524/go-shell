// runner 内嵌：runner.py 经 //go:embed 打进二进制，执行 skill 时写入临时目录，
// 进程结束后删除——部署目录无需携带 runner.py。
package tools

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed runner.py
var runnerScript string

// writeRunner 把内嵌 runner.py 落到独立临时目录并返回路径与清理函数
func writeRunner() (string, func(), error) {
	dir, err := os.MkdirTemp("", "go-shell-runner-")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "runner.py")
	if err := os.WriteFile(path, []byte(runnerScript), 0o644); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { os.RemoveAll(dir) }, nil
}
