package config

import (
	"strings"
	"testing"
)

func TestParseRemoteHosts(t *testing.T) {
	raw := "nuc5=192.0.2.10:8018:yourkey1, aml=http://192.0.2.11:8018 ," +
		"bare=198.51.100.9;full=https://example.com|8443|secret123"
	hosts := parseRemoteHosts(raw)

	cases := []struct {
		alias, host, scheme, key string
		port                     int
	}{
		{"nuc5", "192.0.2.10", "http", "yourkey1", 8018},
		{"aml", "192.0.2.11", "http", "", 8018},
		{"bare", "198.51.100.9", "http", "", 0},
		{"full", "example.com", "https", "secret123", 8443},
	}
	for _, c := range cases {
		tgt, ok := hosts[c.alias]
		if !ok {
			t.Fatalf("别名 %s 未解析出来：%v", c.alias, hosts)
		}
		if tgt.Host != c.host || tgt.Port != c.port || tgt.Scheme != c.scheme || tgt.Key != c.key {
			t.Fatalf("%s 解析错误：%+v，期望 host=%s port=%d scheme=%s key=%s",
				c.alias, tgt, c.host, c.port, c.scheme, c.key)
		}
	}
}

func TestRemoteTargetURL(t *testing.T) {
	cases := []struct {
		tgt  RemoteTarget
		want string
	}{
		{RemoteTarget{Host: "192.0.2.10", Port: 8018}, "http://192.0.2.10:8018"},
		{RemoteTarget{Host: "192.0.2.10"}, "http://192.0.2.10:8000"},
		{RemoteTarget{Host: "192.0.2.10:9000", Port: 8018}, "http://192.0.2.10:9000"},
		{RemoteTarget{Host: "example.com", Port: 443, Scheme: "https"}, "https://example.com:443"},
	}
	for _, c := range cases {
		if got := c.tgt.BaseURL(); got != c.want {
			t.Fatalf("%+v → %s，期望 %s", c.tgt, got, c.want)
		}
	}
}

func TestResolveRemote(t *testing.T) {
	oldHosts, oldHost, oldPort, oldKey := RemoteHosts, RemoteHost, RemotePort, RemoteKey
	t.Cleanup(func() {
		RemoteHosts, RemoteHost, RemotePort, RemoteKey = oldHosts, oldHost, oldPort, oldKey
	})

	RemoteHosts = parseRemoteHosts("nuc5=192.0.2.10:8018:abcd1234")
	RemoteHost, RemotePort, RemoteKey = "", 8000, ""

	// 别名（带不带 + 都可以）
	for _, name := range []string{"nuc5", "+nuc5", "NUC5"} {
		tgt, err := ResolveRemote(name)
		if err != nil {
			t.Fatalf("ResolveRemote(%q) 报错：%v", name, err)
		}
		if tgt.Host != "192.0.2.10" || tgt.Port != 8018 || tgt.Key != "abcd1234" {
			t.Fatalf("ResolveRemote(%q) → %+v", name, tgt)
		}
	}

	// 裸地址：沿用默认端口与默认访问码
	RemoteKey = "defkey"
	tgt, err := ResolveRemote("192.0.2.9:7000")
	if err != nil || tgt.Host != "192.0.2.9" || tgt.Port != 7000 || tgt.Key != "defkey" {
		t.Fatalf("裸地址解析错误：%+v %v", tgt, err)
	}
	// 带访问码的裸地址：提示语里不能把访问码打出来
	tgt, err = ResolveRemote("192.0.2.9:7000:secret123")
	if err != nil {
		t.Fatalf("带访问码的裸地址解析失败：%v", err)
	}
	if tgt.Key != "secret123" {
		t.Fatalf("访问码没解析出来：%+v", tgt)
	}
	if strings.Contains(tgt.Label(), "secret123") {
		t.Fatalf("提示语不应包含访问码：%s", tgt.Label())
	}

	// 未知别名要报错并列出可用别名
	if _, err := ResolveRemote("nope"); err == nil {
		t.Fatal("未知别名应报错")
	}

	// 默认目标为空时明确报错
	RemoteHosts = map[string]RemoteTarget{}
	if _, err := ResolveRemote("default"); err == nil {
		t.Fatal("没有默认远程主机时应报错")
	}
}
