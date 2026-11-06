package tools

import (
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// toUTF8 修正子进程输出的字符编码。
//
// Windows 上 cmd.exe 与 PowerShell 5.1 按控制台代码页写字节（中文系统是 936/GBK），
// 直接当 UTF-8 用会整段乱码——既毁掉终端显示，也让模型读到垃圾。Linux 侧输出本来
// 就是 UTF-8，这里原样返回。
//
// 判断只做兜底，绝不猜：合法 UTF-8 一律不动（GBK 文本几乎不可能恰好是合法 UTF-8）；
// 不合法时按 GBK 解码，再用编码回去做一次往返比对，能对上才采用，否则原样返回——
// 宁可留着乱码，也不要把二进制数据改成替换字符。
func toUTF8(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	decoded, _, err := transform.String(simplifiedchinese.GBK.NewDecoder(), s)
	if err != nil || !utf8.ValidString(decoded) {
		return s
	}
	if back, _, err := transform.String(simplifiedchinese.GBK.NewEncoder(), decoded); err != nil || back != s {
		return s // 不是纯 GBK（或已发生替换），保持原样
	}
	return decoded
}
