// 远程主机配置（客户端模式）：REMOTE_HOSTS 别名表 + REMOTE_HOST 默认目标
//
// REMOTE_HOSTS 每项格式：`别名=地址[:端口[:访问码]]`，多项用逗号/分号分隔，例如
//
//	REMOTE_HOSTS=nuc5=192.0.2.10:8018:yourkey1, aml=192.0.2.11:8018
//
// 也可以写 URL（http://、https://）。`-h +别名` 与 `-h 别名` 等价；
// `-h` 不带值、或 `-h +default` 用 REMOTE_HOST/REMOTE_PORT/REMOTE_KEY。
package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RemoteTarget 一台可连的 go-shell 服务端
type RemoteTarget struct {
	Alias  string
	Scheme string // http（默认）| https
	Host   string
	Port   int
	Key    string // 访问码，对应服务端 API_ACCESS_KEY
}

// Addr host:port
func (t RemoteTarget) Addr() string {
	host := t.Host
	port := t.Port
	if port <= 0 {
		port = 8000
	}
	if strings.Contains(host, ":") && !strings.Contains(host, "]") {
		// 已是 host:port 形态（IPv6 不在此列，忽略）
		if _, _, err := splitHostPort(host); err == nil {
			return host
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// BaseURL 拼出 http(s)://host:port
func (t RemoteTarget) BaseURL() string {
	scheme := t.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + t.Addr()
}

// Label 用于提示：别名 + 地址
func (t RemoteTarget) Label() string {
	if t.Alias != "" && t.Alias != t.Host {
		return fmt.Sprintf("%s（%s）", t.Alias, t.Addr())
	}
	return t.Addr()
}

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return s, 0, fmt.Errorf("no port")
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return s, 0, err
	}
	return s[:i], port, nil
}

// parseRemoteHosts 解析 REMOTE_HOSTS 为 别名 → 目标
func parseRemoteHosts(raw string) map[string]RemoteTarget {
	out := map[string]RemoteTarget{}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	for _, item := range fields {
		item = strings.TrimSpace(item)
		if item == "" || strings.HasPrefix(item, "#") {
			continue
		}
		alias, spec := "", item
		if eq := strings.Index(item, "="); eq > 0 {
			alias, spec = strings.TrimSpace(item[:eq]), strings.TrimSpace(item[eq+1:])
		}
		t := parseRemoteSpec(spec)
		if t.Host == "" {
			continue
		}
		if alias == "" {
			alias = t.Host
		}
		t.Alias = alias
		out[strings.ToLower(alias)] = t
	}
	return out
}

// parseRemoteSpec 解析 地址[:端口[:访问码]] 或 地址|端口|访问码
func parseRemoteSpec(spec string) RemoteTarget {
	t := RemoteTarget{Scheme: "http"}
	s := strings.TrimSpace(spec)
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"):
		t.Scheme = "https"
		s = s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		s = s[len("http://"):]
	}
	s = strings.TrimRight(s, "/")

	var parts []string
	if strings.Contains(s, "|") {
		parts = strings.SplitN(s, "|", 3)
	} else {
		parts = strings.SplitN(s, ":", 3)
	}
	t.Host = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		if p, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil && p > 0 {
			t.Port = p
		}
	}
	if len(parts) > 2 {
		t.Key = strings.TrimSpace(parts[2])
	}
	return t
}

// looksLikeAddress 判断 `-h` 给的是地址而不是别名
func looksLikeAddress(s string) bool {
	if strings.Contains(s, "/") || strings.Contains(s, ":") {
		return true
	}
	if strings.EqualFold(s, "localhost") {
		return true
	}
	return strings.Count(s, ".") == 3
}

// RemoteAliases 已配置的别名（排序后，用于报错提示）
func RemoteAliases() []string {
	names := make([]string, 0, len(RemoteHosts))
	for n := range RemoteHosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolveRemote 把 `-h` 的值解析成目标：别名（大小写不敏感）> 裸地址 > 默认目标
func ResolveRemote(name string) (RemoteTarget, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "+")
	if name == "" || strings.EqualFold(name, "default") {
		if RemoteHost == "" {
			return RemoteTarget{}, fmt.Errorf("没有配置默认远程主机：在配置里设 REMOTE_HOST=地址（或 REMOTE_HOSTS=别名=地址:端口:访问码），再用 -h 别名 指定")
		}
		t := parseRemoteSpec(RemoteHost)
		if RemotePort > 0 && t.Port == 0 {
			t.Port = RemotePort
		}
		if RemoteKey != "" && t.Key == "" {
			t.Key = RemoteKey
		}
		if t.Alias == "" {
			t.Alias = t.Host
		}
		return t, nil
	}
	if t, ok := RemoteHosts[strings.ToLower(name)]; ok {
		if t.Port == 0 {
			t.Port = RemotePort
		}
		if t.Key == "" {
			t.Key = RemoteKey
		}
		return t, nil
	}
	if looksLikeAddress(name) {
		t := parseRemoteSpec(name)
		if t.Port == 0 {
			t.Port = RemotePort
		}
		if t.Key == "" {
			t.Key = RemoteKey
		}
		// 别名只留地址：`-h host:port:访问码` 这种写法里带访问码，原样当别名会打到终端
		t.Alias = t.Host
		return t, nil
	}
	if aliases := RemoteAliases(); len(aliases) > 0 {
		return RemoteTarget{}, fmt.Errorf("没有别名 '%s' 的远程主机（已配置：%s）；也可直接写 -h 192.0.2.10:8018", name, strings.Join(aliases, "、"))
	}
	return RemoteTarget{}, fmt.Errorf("没有别名 '%s' 的远程主机：先在配置里加 REMOTE_HOSTS=%s=地址:端口:访问码，或直接写 -h 192.0.2.10:8018", name, name)
}
