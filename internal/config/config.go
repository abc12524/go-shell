// Package config：.env 加载 + 全局配置（对应 Python core/config.py）
//
// 配置解析优先级与 python-dotenv 一致：真实环境变量 > .env 文件。
// 新增服务端配置：SERVER_HOST（绑定 IP，127.0.0.1 或 0.0.0.0）、SERVER_PORT。
package config

import (
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ProjectRoot string
	EnvPath     string

	// ConfigOverride 由 CLI -c/--config 指定的配置文件路径（main 在 Load 前设置）。
	// 非空时 EnvPath 用它替代默认的 <二进制目录>/.env；-k/-m 写回也落到该文件。
	ConfigOverride string

	// ===== LLM =====
	APIKey        string
	BaseURL       string
	Model         string
	APIType       string // responses | chat
	Provider      string // deepseek | opencode-go
	SessionHeader string
	UserAgent     string

	// ===== 工具调用 =====
	MaxToolRounds int
	WebSearch     bool

	// ===== 调试 =====
	DebugSendSeq bool

	// ===== 用户自定义提示 =====
	UserTips string

	// ===== OpenViking =====
	OpenVikingURL   string
	OpenVikingKey   string
	OpenVikingUser  string
	OpenVikingAgent string

	OVFindThreshold         float64
	OVFindLimit             int
	OVInject                string // off | question | step | both
	OVProfile               bool
	OVCapture               bool
	OVCaptureAssistantTurns bool
	OVCaptureMaxLength      int
	OVCaptureToolMaxChars   int
	OVMinQueryLength        int
	OVRecallDedupTurns      int
	OVProfileTokenBudget    int
	OVWorkspacePeer         bool
	OVPeerID                string

	// ===== skill 远程仓库 =====
	SkillHubType   string
	SkillHubBase   string
	SkillHubRepo   string
	SkillHubBranch string
	SkillHubToken  string

	// ===== 数据库 =====
	DBOnline   bool
	SQLitePath string
	MySQLHost  string
	MySQLPort  int
	MySQLUser  string
	MySQLPass  string
	MySQLDB    string

	// ===== HTTP 服务（go-shell server）=====
	ServerHost string // 127.0.0.1 或 0.0.0.0
	ServerPort int

	// AccessKey 访问 API 服务的全局验证码：至少 8 位，长度没有上限。
	// 配了就强制校验（只认请求头 Authorization: Bearer <code>，不用 query / 自定义头），
	// 空或短于 8 位 = 不校验（仅本机/内网自用），启动时打印告警。
	AccessKey string

	// InterjectGrace 中途插话等待"递进上下文"的最长秒数（超时回 timeout，客户端自己重发）
	InterjectGrace int

	// ===== skill 运行时 =====
	PythonBin  string // 执行 skill/*.py 用的解释器（默认 python3）
	PythonPath string // 冒号分隔的目录，前置到 skill 子进程的 PATH（默认空=不修改）

	MaxAPIAttempts int
)

// env 是加载后的原始键值（供 UpdateEnv 写回判断）
var envMap map[string]string

// resolveRoot：GO_SHELL_ROOT > 可执行文件目录 > 当前目录
func resolveRoot() string {
	if v := strings.TrimSpace(os.Getenv("GO_SHELL_ROOT")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return filepath.Dir(resolved)
		}
		return filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return wd
}

// parseDotenv 解析 .env 内容为键值对（支持 export 前缀、引号、双引号内 \n 转义）
func parseDotenv(content string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				q := val[0]
				val = val[1 : len(val)-1]
				if q == '"' {
					val = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`).Replace(val)
				}
			}
		}
		out[key] = val
	}
	return out
}

func str(m map[string]string, key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	if v, ok := m[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func boolean(m map[string]string, key string, def bool) bool {
	v := strings.ToLower(str(m, key, ""))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

func integer(m map[string]string, key string, def int) int {
	v := strings.TrimSpace(str(m, key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func float(m map[string]string, key string, def float64) float64 {
	v := strings.TrimSpace(str(m, key, ""))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// InferProvider 按 base_url 推断服务商
func InferProvider(baseURL string) string {
	u := strings.ToLower(baseURL)
	if strings.Contains(u, "opencode.ai") && strings.Contains(u, "/go") {
		return "opencode-go"
	}
	return "deepseek"
}

// Load 加载 .env 与环境变量到全局配置。幂等，可重复调用。
func Load() {
	ProjectRoot = resolveRoot()
	if ConfigOverride != "" {
		abs, err := filepath.Abs(ConfigOverride)
		if err != nil {
			abs = ConfigOverride
		}
		EnvPath = abs
		if _, err := os.Stat(EnvPath); err != nil {
			fmt.Fprintf(os.Stderr, "配置文件不存在或不可读：%s\n", EnvPath)
			os.Exit(1)
		}
	} else {
		EnvPath = filepath.Join(ProjectRoot, ".env")
		if _, err := os.Stat(EnvPath); os.IsNotExist(err) {
			if example, err2 := os.ReadFile(filepath.Join(ProjectRoot, ".env.example")); err2 == nil {
				_ = os.WriteFile(EnvPath, example, 0o644)
			}
		}
	}
	data, err := os.ReadFile(EnvPath)
	envMap = map[string]string{}
	if err == nil {
		envMap = parseDotenv(string(data))
	}

	APIKey = str(envMap, "DEEPSEEK_API_KEY", "")
	BaseURL = str(envMap, "DEEPSEEK_BASE_URL", "https://api.deepseek.com")
	Model = str(envMap, "DEEPSEEK_MODEL", "deepseek-v4-flash")
	APIType = strings.ToLower(str(envMap, "DEEPSEEK_API_TYPE", "responses"))
	if APIType != "chat" {
		APIType = "responses"
	}

	Provider = strings.ToLower(str(envMap, "LLM_PROVIDER", ""))
	if Provider == "" {
		Provider = InferProvider(BaseURL)
	}
	SessionHeader = str(envMap, "LLM_SESSION_HEADER", "")
	if SessionHeader == "" && strings.HasPrefix(Provider, "opencode") {
		SessionHeader = "x-opencode-session"
	}
	UserAgent = str(envMap, "LLM_USER_AGENT", "")

	MaxToolRounds = integer(envMap, "MAX_TOOL_ROUNDS", 6)
	WebSearch = boolean(envMap, "LLM_WEB_SEARCH", false)
	DebugSendSeq = boolean(envMap, "DEBUG_SEND_SEQ", false)
	UserTips = strings.TrimSpace(str(envMap, "user_tips", ""))

	OpenVikingURL = str(envMap, "OPENVIKING_URL", "")
	OpenVikingKey = str(envMap, "OPENVIKING_KEY", "")
	OpenVikingUser = str(envMap, "OPENVIKING_USER", "")
	OpenVikingAgent = str(envMap, "OPENVIKING_AGENT", "default")

	OVFindThreshold = float(envMap, "OV_FIND_THRESHOLD", 0.4)
	OVFindLimit = integer(envMap, "OV_FIND_LIMIT", 3)
	OVInject = strings.ToLower(str(envMap, "OV_INJECT", "question"))
	if OVInject != "off" && OVInject != "question" && OVInject != "step" && OVInject != "both" {
		OVInject = "question"
	}
	OVProfile = boolean(envMap, "OV_PROFILE", false)
	OVCapture = boolean(envMap, "OV_CAPTURE", false)
	OVCaptureAssistantTurns = boolean(envMap, "OV_CAPTURE_ASSISTANT_TURNS", true)
	OVCaptureMaxLength = integer(envMap, "OV_CAPTURE_MAX_LENGTH", 24000)
	OVCaptureToolMaxChars = integer(envMap, "OV_CAPTURE_TOOL_MAX_CHARS", 1000000)
	OVMinQueryLength = integer(envMap, "OV_MIN_QUERY_LENGTH", 3)
	OVRecallDedupTurns = integer(envMap, "OV_RECALL_DEDUP_TURNS", 5)
	OVProfileTokenBudget = integer(envMap, "OV_PROFILE_TOKEN_BUDGET", 1000)
	OVWorkspacePeer = boolean(envMap, "OV_WORKSPACE_PEER", false)
	OVPeerID = str(envMap, "OV_PEER_ID", "")

	SkillHubType = strings.ToLower(str(envMap, "SKILL_HUB_TYPE", ""))
	SkillHubBase = strings.TrimRight(str(envMap, "SKILL_HUB_BASE", ""), "/")
	SkillHubRepo = strings.Trim(str(envMap, "SKILL_HUB_REPO", "abc12524/shell-skills"), "/")
	SkillHubBranch = str(envMap, "SKILL_HUB_BRANCH", "main")
	if SkillHubBranch == "" {
		SkillHubBranch = "main"
	}
	SkillHubToken = str(envMap, "SKILL_HUB_TOKEN", "")

	DBOnline = boolean(envMap, "DB_ONLINE", true)
	SQLitePath = str(envMap, "SQLITE_DB_PATH", filepath.Join(ProjectRoot, "data", "shell_tool.db"))

	MySQLHost = str(envMap, "MYSQL_HOST", "")
	MySQLPort = integer(envMap, "MYSQL_PORT", 3306)
	MySQLUser = str(envMap, "MYSQL_USER", "")
	MySQLPass = str(envMap, "MYSQL_PASSWORD", "")
	MySQLDB = str(envMap, "MYSQL_DB", "")

	ServerHost = str(envMap, "SERVER_HOST", "127.0.0.1")
	ServerPort = integer(envMap, "SERVER_PORT", 8000)

	// 访问口令：API 服务的全局验证码，至少 8 位（太短等于没配，避免弱口令半开）
	AccessKey = strings.TrimSpace(str(envMap, "API_ACCESS_KEY", ""))
	InterjectGrace = integer(envMap, "INTERJECT_GRACE", 20)
	if InterjectGrace < 1 {
		InterjectGrace = 1
	}

	PythonBin = str(envMap, "PYTHON_BIN", "python3")
	PythonPath = str(envMap, "PYTHON_PATH", "")

	MaxAPIAttempts = integer(envMap, "MAX_API_ATTEMPTS", 4)
	if MaxAPIAttempts < 1 {
		MaxAPIAttempts = 1
	}
}

// MySQLConfigured 是否已配置 MySQL（host/user/database 任一为空视为未配置）
func MySQLConfigured() bool {
	return MySQLHost != "" && MySQLUser != "" && MySQLDB != ""
}

// AccessKeyMinLen 访问验证码的最低长度（按字符数）。短于此值视为未配置，
// 避免「配了个 3 位的」被当成开着——那比不配还危险。
const AccessKeyMinLen = 8

// AccessKeyEnabled 是否启用了 API 访问校验：API_ACCESS_KEY 至少 AccessKeyMinLen 位。
func AccessKeyEnabled() bool { return utf8.RuneCountInString(AccessKey) >= AccessKeyMinLen }

// AccessKeyTooShort 配了但长度不够（启动日志据此明确提示，而不是含糊地当成「没配」）。
func AccessKeyTooShort() bool { return AccessKey != "" && !AccessKeyEnabled() }

// CheckAccessKey 校验客户端带来的验证码（常数时间比对，防时序侧信道）。
// 未配置时返回 true（不校验）；配了就必须一致。
func CheckAccessKey(given string) bool {
	if !AccessKeyEnabled() {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(given)), []byte(AccessKey)) == 1
}

// UpdateEnv 把配置写回 .env（已有则替换该行，否则追加），并同步内存配置。
// 供 CLI -k/-m 使用：本次进程立即生效，后续进程（含 server）也读取新值。
func UpdateEnv(updates map[string]string) error {
	var lines []string
	if data, err := os.ReadFile(EnvPath); err == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	pending := map[string]string{}
	for k, v := range updates {
		pending[k] = v
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(line, "=") {
			continue
		}
		key := strings.TrimSpace(strings.SplitN(line, "=", 2)[0])
		if _, ok := pending[key]; ok {
			lines[i] = fmt.Sprintf("%s=%s", key, pending[key])
			delete(pending, key)
		}
	}
	for k, v := range pending {
		lines = append(lines, fmt.Sprintf("%s=%s", k, v))
	}
	if err := os.WriteFile(EnvPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	// 同步内存配置
	for k, v := range updates {
		envMap[k] = v
		switch k {
		case "DEEPSEEK_API_KEY":
			APIKey = v
		case "DEEPSEEK_MODEL":
			Model = v
		}
	}
	return nil
}
