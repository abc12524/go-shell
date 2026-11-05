package config

import "testing"

func TestAccessKeyGate(t *testing.T) {
	old := AccessKey
	t.Cleanup(func() { AccessKey = old })

	cases := []struct {
		name    string
		key     string
		given   string
		enabled bool
		short   bool
		ok      bool
	}{
		{"未配置：不校验", "", "", false, false, true},
		{"未配置：随便带也放行", "", "whatever", false, false, true},
		{"太短：视为未配置", "1234567", "1234567", false, true, true},
		{"短一位且不符：也放行（没生效）", "1234567", "7654321", false, true, true},
		{"刚好 8 位：生效", "12345678", "12345678", true, false, true},
		{"超过 8 位：照常生效", "FCiwP3Ym9z", "FCiwP3Ym9z", true, false, true},
		{"很长也生效", "a-very-long-access-code-2026", "a-very-long-access-code-2026", true, false, true},
		{"8 位且首尾空白", "FCiwP3Ym", " FCiwP3Ym ", true, false, true},
		{"8 位但不符", "FCiwP3Ym", "FCiwP3Yn", true, false, false},
		{"8 位但为空", "FCiwP3Ym", "", true, false, false},
		{"8 位但小写化", "FCiwP3Ym", "fciwp3ym", true, false, false},
		{"长码只对前缀", "FCiwP3Ym9z", "FCiwP3Ym", true, false, false},
		{"中文 8 个字符：生效", "访问验证码八个字", "访问验证码八个字", true, false, true},
		{"中文 4 个字符（12 字节）：按字符算仍太短", "访问验证", "访问验证", false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			AccessKey = c.key
			if got := AccessKeyEnabled(); got != c.enabled {
				t.Fatalf("AccessKeyEnabled()=%v，期望 %v", got, c.enabled)
			}
			if got := AccessKeyTooShort(); got != c.short {
				t.Fatalf("AccessKeyTooShort()=%v，期望 %v", got, c.short)
			}
			if got := CheckAccessKey(c.given); got != c.ok {
				t.Fatalf("CheckAccessKey(%q)=%v，期望 %v", c.given, got, c.ok)
			}
		})
	}
}

func TestAccessKeyMinLen(t *testing.T) {
	if AccessKeyMinLen != 8 {
		t.Fatalf("最低 %d 位，实际 %d", AccessKeyMinLen, AccessKeyMinLen)
	}
}
