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
		ok      bool
	}{
		{"未配置：不校验", "", "", false, true},
		{"未配置：随便带也放行", "", "whatever", false, true},
		{"长度不足：视为未配置", "1234567", "1234567", false, true},
		{"长度超长：视为未配置", "123456789", "123456789", false, true},
		{"8 位且相符", "FCiwP3Ym", "FCiwP3Ym", true, true},
		{"8 位且首尾空白", "FCiwP3Ym", " FCiwP3Ym ", true, true},
		{"8 位但不符", "FCiwP3Ym", "FCiwP3Yn", true, false},
		{"8 位但为空", "FCiwP3Ym", "", true, false},
		{"8 位但小写化", "FCiwP3Ym", "fciwp3ym", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			AccessKey = c.key
			if got := AccessKeyEnabled(); got != c.enabled {
				t.Fatalf("AccessKeyEnabled()=%v，期望 %v", got, c.enabled)
			}
			if got := CheckAccessKey(c.given); got != c.ok {
				t.Fatalf("CheckAccessKey(%q)=%v，期望 %v", c.given, got, c.ok)
			}
		})
	}
}

func TestAccessKeyLen(t *testing.T) {
	if AccessKeyLen != 8 {
		t.Fatalf("验证码固定 8 位，实际 %d", AccessKeyLen)
	}
}
