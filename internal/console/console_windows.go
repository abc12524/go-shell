//go:build windows

// Package console：Windows 控制台的编码与 ANSI 初始化。
//
// 中文 Windows 的控制台默认代码页是 936（GBK），而本程序所有输出都是 UTF-8；
// 两条字节流对不上，整段中文就成了乱码。老 conhost 还默认不解析 ANSI 转义，
// 彩色输出会露出 "←[38;5;203m" 这类残字。Init 在启动时把这两件事一并处理，
// 非控制台环境（输出被重定向到管道/文件）下什么都不做。
package console

import "golang.org/x/sys/windows"

// codePageUTF8 是 UTF-8 代码页
const codePageUTF8 = 65001

// Init 把控制台代码页切到 UTF-8、打开 VT 转义处理，返回恢复函数。
// 恢复函数在进程正常返回时把原代码页还回控制台：cmd.exe 的提示符仍按原代码页
// 写字，不还原会让退出后的命令行看着别扭。
func Init() func() {
	noop := func() {}
	out, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return noop
	}
	var mode uint32
	if err := windows.GetConsoleMode(out, &mode); err != nil {
		return noop // 不是控制台（管道/重定向/服务），无需处理
	}
	// 打开 VT 处理，让 lipgloss/glamour 的 ANSI 着色在 conhost 里也生效
	_ = windows.SetConsoleMode(out, mode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)

	oldOutCP, errOut := windows.GetConsoleOutputCP()
	oldInCP, errIn := windows.GetConsoleCP()
	if errOut != nil && errIn != nil {
		return noop
	}
	_ = windows.SetConsoleOutputCP(codePageUTF8)
	// 输入侧同样切 UTF-8：不然中文提问按 GBK 进来，模型收到的是乱码
	_ = windows.SetConsoleCP(codePageUTF8)

	return func() {
		if errOut == nil {
			_ = windows.SetConsoleOutputCP(oldOutCP)
		}
		if errIn == nil {
			_ = windows.SetConsoleCP(oldInCP)
		}
	}
}
