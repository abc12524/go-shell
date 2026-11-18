package billing

import (
	"testing"

	"go-shell/internal/config"
)

func TestBalanceURL(t *testing.T) {
	old := config.BalanceURL
	t.Cleanup(func() { config.BalanceURL = old })

	cases := []struct{ name, cfg, want string }{
		{"未配置", "", ""},
		{"只有空白", "   ", ""},
		{"基址补 /api/balance", "https://192.168.30.181:8083/", "https://192.168.30.181:8083/api/balance"},
		{"基址带尾斜杠", "https://192.168.30.181:8083", "https://192.168.30.181:8083/api/balance"},
		{"已含完整路径不重复拼", "https://gw/api/balance", "https://gw/api/balance"},
		{"基址带子路径", "https://gw/litellm/", "https://gw/litellm/api/balance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config.BalanceURL = c.cfg
			if got := balanceURL(); got != c.want {
				t.Fatalf("balanceURL()=%q，期望 %q", got, c.want)
			}
		})
	}
}

func TestDecodeBalance(t *testing.T) {
	cases := []struct{ name, body, amount, currency string }{
		{
			"官方裸结构",
			`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.60"}]}`,
			"12.60", "CNY",
		},
		{
			"网关信封结构",
			`{"status":"ok","result":{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.60"}]}}`,
			"12.60", "CNY",
		},
		{"空列表", `{"balance_infos":[]}`, "", ""},
		{"无 balance_infos", `{"status":"ok","result":{}}`, "", ""},
		{"非法 JSON", `not json`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			amount, currency := decodeBalance([]byte(c.body))
			if amount != c.amount || currency != c.currency {
				t.Fatalf("decodeBalance()=(%q,%q)，期望 (%q,%q)", amount, currency, c.amount, c.currency)
			}
		})
	}
}
