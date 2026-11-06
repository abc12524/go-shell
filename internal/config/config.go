// Package config：.env 加载 + 全局配置（对应 Python core/config.py）
//
// 配置解析优先级与 python-dotenv 一致：真实环境变量 > .env 文件。
//
// 配置根（ConfigHome）决定 go.config、data/、skill/ 的默认位置，查找顺序：
//
//	-c/--config 指定 > GO_SHELL_HOME > XDG_CONFIG_HOME > ~/.config/go-shell（存在 go.config）
//	> <二进制目录>/.env（老部署）> 都没有则在 ~/.config/go-shell 生成
//
// 即 ~/.config/go-shell/{go.config, data/, skill/} 优先；老部署（配置和数据都在
// 二进制目录）行为不变，不会被"搬家"。DATA_DIR / SKILL_DIR 可单独覆盖。
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
	// ProjectRoot 二进制所在目录（相对路径基准、recall peer 标识用）。
	ProjectRoot string
	// ConfigHome 配置根：go.config / data / skill 默认都放这里。
	ConfigHome string
	// EnvPath 实际加载的配置文件路径。
	EnvPath string

	// ConfigOverride 由 CLI -c/--config 指定的配置文件路径（main 在 Load 前设置）。
	// 非空时 EnvPath 用它替代默认位置；-k/-m 写回也落到该文件。
	ConfigOverride string

	// exampleConfig 由 main 通过 go:embed 注入的配置模板：全新机器只有二进制时
	// 也能生成一份完整可用的 go.config（不再依赖 exe 旁边有 .env.example）。
	exampleConfig string

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

	// ===== 远程客户端（go-shell -h <别名>）=====
	// RemoteHosts 别名 → 目标（REMOTE_HOSTS 解析结果）
	RemoteHosts map[string]RemoteTarget
	// RemoteHost/RemotePort/RemoteKey 是 `-h` 不带别名时用的默认远程目标
	RemoteHost string
	RemotePort int
	RemoteKey  string
	// RemoteTimeout 建连与健康检查的超时秒数（流式对话本身不设总超时）
	RemoteTimeout int

	// AccessKey 访问 API 服务的全局验证码：至少 8 位，长度没有上限。
	// 配了就强制校验（只认请求头 Authorization: Bearer <code>，不用 query / 自定义头），
	// 空或短于 8 位 = 不校验（仅本机/内网自用），启动时打印告警。
	AccessKey string

	// InterjectGrace 中途插话等待"递进上下文"的最长秒数（超时回 timeout，客户端自己重发）
	InterjectGrace int

	// ===== skill 运行时 =====
	// PythonBin 执行 skill/*.py 用的解释器（默认 python3）。
	// 值含目录（如 /root/.venv/bin/python3）时，该目录自动前置到 skill 子进程的
	// PATH——venv 里 pip 装的 CLI 工具对 skill 直接可见，无需再单独配路径。
	PythonBin string

	// ===== 目录（相对路径按 ProjectRoot 解析）=====
	DataDir     string // 数据目录（SQLite 默认落这里；默认 <配置根>/data）
	SkillDirCfg string // skill 脚本目录（默认 <配置根>/skill）

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

// resolveHome 默认配置根：GO_SHELL_HOME > XDG_CONFIG_HOME/go-shell > ~/.config/go-shell
func resolveHome() string {
	if v := strings.TrimSpace(os.Getenv("GO_SHELL_HOME")); v != "" {
		return absPath(v)
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(absPath(v), "go-shell")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "go-shell")
	}
	return ProjectRoot
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// SetExampleConfig 注入配置模板（main 里 //go:embed .env.example）。
// 必须在 Load 之前调用；不注入时退回内置最小模板。
func SetExampleConfig(text string) { exampleConfig = text }

const minimalConfig = `DEEPSEEK_API_KEY=
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_MODEL=deepseek-v4-flash

# 服务端：绑定的 IP 与端口（0.0.0.0 可被局域网访问）
SERVER_HOST=127.0.0.1
SERVER_PORT=8000
API_ACCESS_KEY=

# 远程主机（客户端模式 -h 使用）：别名=地址:端口:访问码，逗号分隔多个
REMOTE_HOSTS=
REMOTE_HOST=
REMOTE_PORT=8000
REMOTE_KEY=
`

// ensureConfigFile 写出配置模板
func ensureConfigFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := exampleConfig
	if strings.TrimSpace(content) == "" {
		content = minimalConfig
	}
	return os.WriteFile(path, []byte(content), 0o644)
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

// resolveDir 目录类配置：相对路径按 ProjectRoot 解析为绝对路径，空值取默认。
func resolveDir(m map[string]string, key, def string) string {
	v := strings.TrimSpace(str(m, key, ""))
	if v == "" {
		v = def
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(ProjectRoot, v)
	}
	return v
}

// InferProvider 按 base_url 推断服务商
func InferProvider(baseURL string) string {
	u := strings.ToLower(baseURL)
	if strings.Contains(u, "opencode.ai") && strings.Contains(u, "/go") {
		return "opencode-go"
	}
	return "deepseek"
}

// Load 加载配置到全局变量。幂等，可重复调用。
//
// 配置根查找：-c > GO_SHELL_HOME/XDG 下的 go.config > <二进制目录>/.env > 生成 go.config。
// 老部署（二进制目录有 .env）继续用它，配置与数据都不搬家。
func Load() {
	ProjectRoot = resolveRoot()
	home := resolveHome()
	exeEnv := filepath.Join(ProjectRoot, ".env")

	switch {
	case ConfigOverride != "":
		// 配置与数据不一定同处（如 -c /etc/go-shell/go.conf + 二进制在 /opt/go-shell）：
		// 只有配置文件所在目录里确实有 data/ 或 skill/ 才把它当配置根，否则跟二进制目录
		dir := filepath.Dir(absPath(ConfigOverride))
		if dirExists(filepath.Join(dir, "data")) || dirExists(filepath.Join(dir, "skill")) {
			ConfigHome = dir
		} else {
			ConfigHome = ProjectRoot
		}
	case fileExists(filepath.Join(home, "go.config")):
		ConfigHome = home
	case fileExists(exeEnv):
		ConfigHome = ProjectRoot
	default:
		ConfigHome = home
	}

	switch {
	case ConfigOverride != "":
		EnvPath = absPath(ConfigOverride)
		if _, err := os.Stat(EnvPath); err != nil {
			fmt.Fprintf(os.Stderr, "配置文件不存在或不可读：%s\n", EnvPath)
			os.Exit(1)
		}
	case ConfigHome == home:
		EnvPath = filepath.Join(ConfigHome, "go.config")
	default:
		EnvPath = filepath.Join(ConfigHome, ".env")
	}

	if !fileExists(EnvPath) {
		_ = os.MkdirAll(filepath.Join(ConfigHome, "data"), 0o755)
		_ = os.MkdirAll(filepath.Join(ConfigHome, "skill"), 0o755)
		if err := ensureConfigFile(EnvPath); err != nil {
			fmt.Fprintf(os.Stderr, "生成默认配置失败：%v\n", err)
		} else if ConfigHome == home {
			fmt.Fprintf(os.Stderr, "已生成默认配置：%s（数据与 skill 同级）\n", EnvPath)
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
	DataDir = resolveDir(envMap, "DATA_DIR", filepath.Join(ConfigHome, "data"))
	SQLitePath = str(envMap, "SQLITE_DB_PATH", filepath.Join(DataDir, "shell_tool.db"))

	MySQLHost = str(envMap, "MYSQL_HOST", "")
	MySQLPort = integer(envMap, "MYSQL_PORT", 3306)
	MySQLUser = str(envMap, "MYSQL_USER", "")
	MySQLPass = str(envMap, "MYSQL_PASSWORD", "")
	MySQLDB = str(envMap, "MYSQL_DB", "")

	ServerHost = str(envMap, "SERVER_HOST", "127.0.0.1")
	ServerPort = integer(envMap, "SERVER_PORT", 8000)

	// 远程客户端：别名表 + 默认目标
	RemoteHosts = parseRemoteHosts(str(envMap, "REMOTE_HOSTS", ""))
	RemoteHost = strings.TrimSpace(str(envMap, "REMOTE_HOST", ""))
	RemotePort = integer(envMap, "REMOTE_PORT", 8000)
	RemoteKey = strings.TrimSpace(str(envMap, "REMOTE_KEY", ""))
	RemoteTimeout = integer(envMap, "REMOTE_TIMEOUT", 15)
	if RemoteTimeout < 1 {
		RemoteTimeout = 1
	}

	// 访问口令：API 服务的全局验证码，至少 8 位（太短等于没配，避免弱口令半开）
	AccessKey = strings.TrimSpace(str(envMap, "API_ACCESS_KEY", ""))
	InterjectGrace = integer(envMap, "INTERJECT_GRACE", 20)
	if InterjectGrace < 1 {
		InterjectGrace = 1
	}

	PythonBin = str(envMap, "PYTHON_BIN", "python3")
	SkillDirCfg = resolveDir(envMap, "SKILL_DIR", filepath.Join(ConfigHome, "skill"))

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

// UpdateEnv 把配置写回配置文件（已有则替换该行，否则追加），并同步内存配置。
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
