package tools

import "testing"

// GBK 字节（由 iconv 生成）：中文测试 / 磁盘 已满
const (
	gbkChinese = "\xd6\xd0\xce\xc4\xb2\xe2\xca\xd4"
	gbkDisk    = "\xb4\xc5\xc5\xcc\x20\xd2\xd1\xc2\xfa"
)

func TestToUTF8(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"空串", "", ""},
		{"纯 ASCII 不动", "hello world", "hello world"},
		{"合法 UTF-8 不动", "中文测试", "中文测试"},
		{"混合 UTF-8 不动", "ok 中文 123", "ok 中文 123"},
		{"GBK 转 UTF-8", gbkChinese, "中文测试"},
		{"GBK 带空格", gbkDisk, "磁盘 已满"},
		{"非法字节原样返回", "\xff\xfe\x00\x01", "\xff\xfe\x00\x01"},
		{"GBK 混 ASCII", "err: " + gbkDisk, "err: 磁盘 已满"},
	}
	for _, c := range cases {
		if got := toUTF8(c.in); got != c.want {
			t.Errorf("%s：toUTF8(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}
