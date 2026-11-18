// Package billing：DeepSeek/OpenCode Go 计费（对应 core/billing.py）
package billing

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"go-shell/internal/config"
)

// 北京时间（UTC+8）
var beijingTZ = time.FixedZone("CST", 8*3600)

// 工作日高峰时段（北京时间）：周一至周五 09:00-12:00、14:00-18:00
var peakRanges = [][2]int{{9 * 60, 12 * 60}, {14 * 60, 18 * 60}}

// OpenCode Go 峰谷时段（UTC）：周一至周五 01:00-04:00、06:00-10:00
var opencodeGoPeakRanges = [][2]int{{1 * 60, 4 * 60}, {6 * 60, 10 * 60}}

type price struct{ Hit, Miss, Out float64 }

var pricing = map[string]map[string]price{
	"flash": {
		"peak": {0.04, 2.0, 8.0}, "off": {0.02, 1.0, 4.0},
	},
	"pro": {
		"peak": {0.30, 9.0, 27.0}, "off": {0.15, 4.5, 13.5},
	},
}

var opencodeGoPricing = map[string]map[string]price{
	"flash": {
		"peak": {0.006, 0.30, 1.20}, "off": {0.003, 0.15, 0.60},
	},
	"pro": {
		"peak": {0.044, 1.32, 3.96}, "off": {0.022, 0.66, 1.98},
	},
}

func IsOpenCodeGo() bool { return config.Provider == "opencode-go" }

func Currency() string {
	if IsOpenCodeGo() {
		return "USD"
	}
	return "CNY"
}

func CurrencySymbol() string {
	if IsOpenCodeGo() {
		return "$"
	}
	return "¥"
}

func PeriodLabel(peak bool) string {
	span := "北京时间 周一至周五 9:00-12:00、14:00-18:00"
	if IsOpenCodeGo() {
		span = "UTC 周一至周五 01:00-04:00、06:00-10:00"
	}
	if peak {
		return "高峰时段（" + span + "）"
	}
	return "空闲时段（" + span + "）"
}

// ModelFamily 按模型名归族：含 pro 记为 pro，其余按 flash 计费
func ModelFamily(model string) string {
	if strings.Contains(strings.ToLower(model), "pro") {
		return "pro"
	}
	return "flash"
}

// IsPeak 按服务商时区判断是否为工作日高峰时段
func IsPeak(when time.Time) bool {
	var d time.Time
	ranges := peakRanges
	if IsOpenCodeGo() {
		d = when.UTC()
		ranges = opencodeGoPeakRanges
	} else {
		d = when.In(beijingTZ)
	}
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		return false
	}
	minutes := d.Hour()*60 + d.Minute()
	for _, r := range ranges {
		if minutes >= r[0] && minutes < r[1] {
			return true
		}
	}
	return false
}

// Costs 分项费用
type Costs struct{ Hit, Miss, Out float64 }

// ComputeCost 计算本次用量费用，返回 (总费用, 是否高峰, 分项)
func ComputeCost(hit, miss, out int64, model string, when time.Time) (float64, bool, Costs) {
	table := pricing
	if IsOpenCodeGo() {
		table = opencodeGoPricing
	}
	p := table[ModelFamily(model)]["off"]
	peak := false
	if IsPeak(when) {
		p = table[ModelFamily(model)]["peak"]
		peak = true
	}
	c := Costs{
		Hit:  float64(hit) / 1e6 * p.Hit,
		Miss: float64(miss) / 1e6 * p.Miss,
		Out:  float64(out) / 1e6 * p.Out,
	}
	return c.Hit + c.Miss + c.Out, peak, c
}

// balanceURL 余额网关地址：BALANCE_URL 作基址拼 /api/balance；未配置返回空串。
func balanceURL() string {
	base := strings.TrimRight(strings.TrimSpace(config.BalanceURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/api/balance") {
		return base
	}
	return base + "/api/balance"
}

type balanceInfo struct {
	TotalBalance string `json:"total_balance"`
	Currency     string `json:"currency"`
}

// balanceBody 兼容网关信封 {"status":"ok","result":{...}} 与裸 {"balance_infos":[...]}
type balanceBody struct {
	Result *struct {
		BalanceInfos []balanceInfo `json:"balance_infos"`
	} `json:"result"`
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

// gatewayClient 查内网网关用：自签名证书跳过校验，且不走 HTTP_PROXY
// （内网地址送进代理会直接失败）。
func gatewayClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func decodeBalance(data []byte) (string, string) {
	var body balanceBody
	if err := json.Unmarshal(data, &body); err != nil {
		return "", ""
	}
	infos := body.BalanceInfos
	if body.Result != nil {
		infos = body.Result.BalanceInfos
	}
	if len(infos) == 0 {
		return "", ""
	}
	return infos[0].TotalBalance, infos[0].Currency
}

// FetchBalance 查询账户余额，返回 (金额, 币种)；失败返回 ("","")。
//
// 两种模式：
//   - 未配置 BALANCE_URL：用 DEEPSEEK_API_KEY 查官方 {DEEPSEEK_BASE_URL}/user/balance
//   - 配置了 BALANCE_URL：查自建网关 {BALANCE_URL}/api/balance（无需 Key）
//
// OpenCode Go 不显示余额，直接返回空。
func FetchBalance() (string, string) {
	if IsOpenCodeGo() {
		return "", ""
	}
	if url := balanceURL(); url != "" {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return "", ""
		}
		req.Header.Set("Accept", "application/json")
		resp, err := gatewayClient().Do(req)
		if err != nil {
			return "", ""
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", ""
		}
		return decodeBalance(data)
	}
	if config.APIKey == "" {
		return "", ""
	}
	url := strings.TrimRight(config.BaseURL, "/") + "/user/balance"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.APIKey)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", ""
	}
	return decodeBalance(data)
}
