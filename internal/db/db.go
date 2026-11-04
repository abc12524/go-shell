// Package db：存储层（对应 core/db.py）
//
// 单一读写源 + 在线副本：
//   1. 本地 SQLite 是唯一读写源；
//   2. 在线 MySQL 仅作同步副本，按 uid 幂等去重；
//   3. 每条记录带 synced 标记，失败保持未同步下次重试。
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"go-shell/internal/config"
	"go-shell/internal/events"
)

// InsertUserRole 合成注入的用户消息角色（skill 名单 / OV 块）
const InsertUserRole = "insert_user"

var (
	mu          sync.Mutex
	sqliteDB    *sql.DB
	mysqlDB     *sql.DB
	syncEnabled bool
	ready       bool
)

func uid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func chatRole(role string) string {
	if role == InsertUserRole {
		return "user"
	}
	return role
}

func openSQLite() (*sql.DB, error) {
	if sqliteDB != nil {
		return sqliteDB, nil
	}
	dsn := "file:" + config.SQLitePath + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)"
	// data/ 目录不存在时自动创建（全新部署只带二进制 + .env 即可启动）
	_ = os.MkdirAll(filepath.Dir(config.SQLitePath), 0o755)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1) // 串行化写入，避免 SQLITE_BUSY
	sqliteDB = conn
	return conn, nil
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS chat_sessions (
    id         TEXT PRIMARY KEY,
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    synced     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS chat_messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    uid               TEXT NULL,
    session_id        TEXT NOT NULL,
    role              TEXT NOT NULL,
    content           TEXT NULL,
    tool_calls_json   TEXT NULL,
    tool_call_id      TEXT NULL,
    reasoning_content TEXT NULL,
    output_items_json TEXT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    synced            INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sync_meta (
    key   TEXT PRIMARY KEY,
    value TEXT
);
`

// ResolveBackend 初始化 SQLite，并在配置允许时开启 MySQL 同步
func ResolveBackend(sink events.Sink) error {
	mu.Lock()
	defer mu.Unlock()
	if ready {
		return nil
	}
	conn, err := openSQLite()
	if err != nil {
		return err
	}
	if _, err := conn.Exec(sqliteSchema); err != nil {
		return fmt.Errorf("SQLite 建表失败: %w", err)
	}
	// 旧数据补 uid + 唯一索引
	conn.Exec("UPDATE chat_messages SET uid = 'legacy-' || id WHERE uid IS NULL OR uid = ''")
	conn.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_msg_uid ON chat_messages (uid)")
	conn.Exec("CREATE INDEX IF NOT EXISTS idx_session_id ON chat_messages (session_id, id)")
	ready = true

	if config.DBOnline && config.MySQLConfigured() {
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=false&loc=UTC",
			config.MySQLUser, config.MySQLPass, config.MySQLHost, config.MySQLPort, config.MySQLDB)
		mconn, err := sql.Open("mysql", dsn)
		if err == nil {
			if err = mconn.Ping(); err == nil {
				if err = initSchemaMySQL(mconn); err == nil {
					mysqlDB = mconn
					syncEnabled = true
				}
			}
		}
		if err != nil {
			events.Warning(sink, "在线 MySQL 不可用，仅使用本地 SQLite："+err.Error(), "mysql_unavailable")
			syncEnabled = false
			if mconn != nil {
				mconn.Close()
			}
		}
	}
	if syncEnabled {
		baselineOnlineSync()
		go SyncPending(sink)
	} else {
		markOnlineDisabled()
	}
	return nil
}

func initSchemaMySQL(conn *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chat_sessions (
			id         VARCHAR(64) NOT NULL PRIMARY KEY,
			status     ENUM('active','closed') NOT NULL DEFAULT 'active',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			uid            VARCHAR(64) NULL,
			session_id     VARCHAR(64) NOT NULL,
			role           VARCHAR(20) NOT NULL,
			content        MEDIUMTEXT NULL,
			tool_calls_json TEXT NULL,
			tool_call_id   VARCHAR(64) NULL,
			reasoning_content MEDIUMTEXT NULL,
			output_items_json TEXT NULL,
			created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			KEY idx_session_id (session_id, id),
			UNIQUE KEY uidx_msg_uid (uid)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func metaGet(conn *sql.DB, key string) (string, bool) {
	var v string
	err := conn.QueryRow("SELECT value FROM sync_meta WHERE key=?", key).Scan(&v)
	return v, err == nil
}

func metaSet(conn *sql.DB, key, value string) {
	conn.Exec("INSERT INTO sync_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
}

// baselineOnlineSync 开启在线同步时取基线：之前的历史不回传
func baselineOnlineSync() {
	if v, _ := metaGet(sqliteDB, "online_epoch"); v == "1" {
		return
	}
	var msgMax, sessMax int
	sqliteDB.QueryRow("SELECT COALESCE(MAX(id), 0) FROM chat_messages").Scan(&msgMax)
	sqliteDB.QueryRow("SELECT COALESCE(MAX(rowid), 0) FROM chat_sessions").Scan(&sessMax)
	sqliteDB.Exec("UPDATE chat_messages SET synced=1 WHERE id<=?", msgMax)
	sqliteDB.Exec("UPDATE chat_sessions SET synced=1 WHERE rowid<=?", sessMax)
	metaSet(sqliteDB, "online_epoch", "1")
	metaSet(sqliteDB, "online_baseline_msg", fmt.Sprint(msgMax))
	metaSet(sqliteDB, "online_baseline_sess", fmt.Sprint(sessMax))
}

func markOnlineDisabled() {
	if v, _ := metaGet(sqliteDB, "online_epoch"); v != "0" {
		metaSet(sqliteDB, "online_epoch", "0")
	}
}

// OnlineEnabled 在线 MySQL 同步是否开启
func OnlineEnabled() bool { return syncEnabled }

// ---------- 会话 ----------

func GetActiveSessionID() string {
	mu.Lock()
	defer mu.Unlock()
	var id string
	err := sqliteDB.QueryRow(
		"SELECT id FROM chat_sessions WHERE status='active' ORDER BY updated_at DESC LIMIT 1").Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

func SessionExists(id string) bool {
	mu.Lock()
	defer mu.Unlock()
	var one int
	return sqliteDB.QueryRow("SELECT 1 FROM chat_sessions WHERE id=?", id).Scan(&one) == nil
}

func CreateSession(id string) {
	mu.Lock()
	sqliteDB.Exec(
		"INSERT INTO chat_sessions (id, synced) VALUES (?, 0) "+
			"ON CONFLICT(id) DO UPDATE SET status='active', updated_at=CURRENT_TIMESTAMP, synced=0", id)
	mu.Unlock()
	go SyncPending(nil)
}

func CloseSession(id string) {
	mu.Lock()
	sqliteDB.Exec("UPDATE chat_sessions SET status='closed', synced=0 WHERE id=?", id)
	mu.Unlock()
	go SyncPending(nil)
}

func CloseAllActiveSessions() {
	mu.Lock()
	sqliteDB.Exec("UPDATE chat_sessions SET status='closed', synced=0 WHERE status='active'")
	mu.Unlock()
	go SyncPending(nil)
}

func CountUserTurns(sessionID string) int {
	mu.Lock()
	defer mu.Unlock()
	var n int
	sqliteDB.QueryRow(
		"SELECT COUNT(*) FROM chat_messages WHERE session_id=? AND role='user'", sessionID).Scan(&n)
	return n
}

// RecentSession 最近会话列表条目
type RecentSession struct {
	ID        string
	Status    string
	UpdatedAt string
	Count     int
}

func ListRecentSessions(limit int) []RecentSession {
	mu.Lock()
	defer mu.Unlock()
	rows, err := sqliteDB.Query(
		"SELECT s.id, s.status, s.updated_at, "+
			"(SELECT COUNT(*) FROM chat_messages m WHERE m.session_id = s.id) "+
			"FROM chat_sessions s ORDER BY s.updated_at DESC, s.id DESC LIMIT ?", limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []RecentSession
	for rows.Next() {
		var s RecentSession
		if rows.Scan(&s.ID, &s.Status, &s.UpdatedAt, &s.Count) == nil {
			out = append(out, s)
		}
	}
	return out
}

// ---------- 消息 ----------

// AppendMessages 批量追加消息（map 格式：role/content/tool_calls/tool_call_id/...）
func AppendMessages(sessionID string, msgs []map[string]any) {
	if len(msgs) == 0 {
		return
	}
	mu.Lock()
	for _, m := range msgs {
		toolCallsJSON := marshalOpt(m["tool_calls"])
		outputItemsJSON := marshalOpt(m["output_items"])
		content := stringOf(m["content"])
		reasoning := stringOf(m["reasoning_content"])
		sqliteDB.Exec(
			"INSERT INTO chat_messages (uid, session_id, role, content, tool_calls_json, tool_call_id, reasoning_content, output_items_json, synced) "+
				"VALUES (?,?,?,?,?,?,?,?,0)",
			orDefault(stringOf(m["uid"]), uid()), sessionID, stringOf(m["role"]),
			content, toolCallsJSON, stringOf(m["tool_call_id"]), reasoning, outputItemsJSON)
	}
	sqliteDB.Exec("UPDATE chat_sessions SET updated_at=CURRENT_TIMESTAMP, synced=0 WHERE id=?", sessionID)
	mu.Unlock()
	go SyncPending(nil)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func stringOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func marshalOpt(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

// LoadMessages 加载会话全部消息（配平工具调用后）
func LoadMessages(sessionID string) []map[string]any {
	mu.Lock()
	defer mu.Unlock()
	rows, err := sqliteDB.Query(
		"SELECT role, content, tool_calls_json, tool_call_id, reasoning_content, output_items_json "+
			"FROM chat_messages WHERE session_id=? ORDER BY id", sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var msgs []map[string]any
	for rows.Next() {
		var role string
		var content, toolCallsJSON, toolCallID, reasoning, outputItemsJSON sql.NullString
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID, &reasoning, &outputItemsJSON); err != nil {
			continue
		}
		r := chatRole(role)
		m := map[string]any{"role": r, "content": content.String}
		if r == "assistant" {
			if toolCallsJSON.Valid && toolCallsJSON.String != "" {
				var tcs []map[string]any
				if json.Unmarshal([]byte(toolCallsJSON.String), &tcs) == nil && len(tcs) > 0 {
					m["tool_calls"] = tcs
				}
			}
			if reasoning.Valid && reasoning.String != "" {
				m["reasoning_content"] = reasoning.String
			}
			if outputItemsJSON.Valid && outputItemsJSON.String != "" {
				var items []map[string]any
				if json.Unmarshal([]byte(outputItemsJSON.String), &items) == nil && len(items) > 0 {
					m["output_items"] = items
				}
			}
		}
		if r == "tool" && toolCallID.Valid {
			m["tool_call_id"] = toolCallID.String
		}
		msgs = append(msgs, m)
	}
	msgs = SanitizeToolPairs(msgs)
	// 兜底：开头不能是孤立的 tool 结果或未配对的 tool_calls
	for len(msgs) > 0 {
		first := msgs[0]
		role, _ := first["role"].(string)
		if role == "tool" {
			msgs = msgs[1:]
			continue
		}
		if role == "assistant" && first["tool_calls"] != nil {
			msgs = msgs[1:]
			continue
		}
		break
	}
	return msgs
}

// HasMessageWithPrefix 会话中是否已有以 prefix 开头的内容
func HasMessageWithPrefix(sessionID, prefix string) bool {
	if prefix == "" {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	var one int
	return sqliteDB.QueryRow(
		"SELECT 1 FROM chat_messages WHERE session_id=? AND substr(content, 1, ?) = ? LIMIT 1",
		sessionID, len([]rune(prefix)), prefix).Scan(&one) == nil
}

// LoadInjectedContents 按 id 升序返回会话内以任一 prefix 开头的消息内容
func LoadInjectedContents(sessionID string, prefixes []string) []string {
	var ps []string
	for _, p := range prefixes {
		if p != "" {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	cond := strings.TrimSuffix(strings.Repeat(" substr(content, 1, ?) = ? OR", len(ps)), " OR")
	args := []any{sessionID}
	for _, p := range ps {
		args = append(args, len([]rune(p)), p)
	}
	mu.Lock()
	defer mu.Unlock()
	rows, err := sqliteDB.Query(
		"SELECT content FROM chat_messages WHERE session_id=? AND ("+cond+") ORDER BY id", args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c sql.NullString
		if rows.Scan(&c) == nil {
			out = append(out, c.String)
		}
	}
	return out
}

// ---------- 工具调用配对（历史合法性） ----------

func isFunctionCallItem(item map[string]any) bool {
	t, _ := item["type"].(string)
	return t == "function_call"
}

func toolCallID(tc map[string]any) string {
	if s, ok := tc["id"].(string); ok {
		return s
	}
	return ""
}

// StripToolCalls 摘掉单条 assistant 消息里的工具调用（含 output_items 中的 function_call）
func StripToolCalls(msg map[string]any) map[string]any {
	if msg == nil {
		return msg
	}
	calls, _ := msg["tool_calls"].([]map[string]any)
	items, _ := msg["output_items"].([]map[string]any)
	hasOrphan := false
	for _, it := range items {
		if isFunctionCallItem(it) {
			hasOrphan = true
			break
		}
	}
	if len(calls) == 0 && !hasOrphan {
		return msg
	}
	out := map[string]any{}
	for k, v := range msg {
		out[k] = v
	}
	if len(calls) > 0 {
		delete(out, "tool_calls")
	}
	if hasOrphan {
		var kept []map[string]any
		for _, it := range items {
			if !isFunctionCallItem(it) {
				kept = append(kept, it)
			}
		}
		if len(kept) > 0 {
			out["output_items"] = kept
		} else {
			delete(out, "output_items")
		}
	}
	return out
}

// SanitizeToolPairs 配平消息序列：摘掉"无人应答的调用"与"无对应调用的工具结果"
func SanitizeToolPairs(msgs []map[string]any) []map[string]any {
	declared := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range msgs {
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			for _, tc := range toCallList(m["tool_calls"]) {
				if cid := toolCallID(tc); cid != "" {
					declared[cid] = true
				}
			}
			for _, it := range toItemList(m["output_items"]) {
				if isFunctionCallItem(it) {
					if cid, _ := it["call_id"].(string); cid != "" {
						declared[cid] = true
					}
				}
			}
		case "tool":
			if cid, _ := m["tool_call_id"].(string); cid != "" {
				answered[cid] = true
			}
		}
	}
	paired := map[string]bool{}
	for cid := range declared {
		if answered[cid] {
			paired[cid] = true
		}
	}
	var out []map[string]any
	for _, m := range msgs {
		role, _ := m["role"].(string)
		if role == "tool" {
			if paired[m["tool_call_id"].(string)] {
				out = append(out, m)
			}
			continue
		}
		if role != "assistant" {
			out = append(out, m)
			continue
		}
		var keptCalls []map[string]any
		for _, tc := range toCallList(m["tool_calls"]) {
			if paired[toolCallID(tc)] {
				keptCalls = append(keptCalls, tc)
			}
		}
		var keptItems []map[string]any
		hasPayload := false
		for _, it := range toItemList(m["output_items"]) {
			if !isFunctionCallItem(it) {
				keptItems = append(keptItems, it)
				hasPayload = true
			} else if cid, _ := it["call_id"].(string); paired[cid] {
				keptItems = append(keptItems, it)
			}
		}
		content, _ := m["content"].(string)
		if len(keptCalls) == 0 && !hasPayload && strings.TrimSpace(content) == "" {
			continue
		}
		newMsg := map[string]any{}
		for k, v := range m {
			newMsg[k] = v
		}
		if len(keptCalls) > 0 {
			newMsg["tool_calls"] = keptCalls
		} else {
			delete(newMsg, "tool_calls")
		}
		if len(keptItems) > 0 {
			newMsg["output_items"] = keptItems
		} else {
			delete(newMsg, "output_items")
		}
		out = append(out, newMsg)
	}
	return out
}

func toCallList(v any) []map[string]any {
	if list, ok := v.([]map[string]any); ok {
		return list
	}
	return nil
}

func toItemList(v any) []map[string]any {
	if list, ok := v.([]map[string]any); ok {
		return list
	}
	return nil
}

// ---------- 在线同步（SQLite → MySQL，幂等） ----------

// SyncPending 把 SQLite 中 synced=0 的会话/消息推送到 MySQL
func SyncPending(sink events.Sink) {
	if !syncEnabled || mysqlDB == nil || sqliteDB == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	sessRows, err := sqliteDB.Query("SELECT id, status, created_at, updated_at FROM chat_sessions WHERE synced=0")
	if err != nil {
		return
	}
	type sessRow struct{ ID, Status, Created, Updated string }
	var sessions []sessRow
	for sessRows.Next() {
		var s sessRow
		if sessRows.Scan(&s.ID, &s.Status, &s.Created, &s.Updated) == nil {
			sessions = append(sessions, s)
		}
	}
	sessRows.Close()

	msgRows, err := sqliteDB.Query(
		"SELECT id, uid, session_id, role, content, tool_calls_json, tool_call_id, reasoning_content, output_items_json, created_at "+
			"FROM chat_messages WHERE synced=0 ORDER BY id")
	if err != nil {
		return
	}
	type msgRow struct {
		ID        int64
		UID       string
		SessionID string
		Role      string
		Content   sql.NullString
		ToolCalls sql.NullString
		CallID    sql.NullString
		Reasoning sql.NullString
		OutItems  sql.NullString
		Created   string
	}
	var messages []msgRow
	for msgRows.Next() {
		var m msgRow
		if msgRows.Scan(&m.ID, &m.UID, &m.SessionID, &m.Role, &m.Content, &m.ToolCalls, &m.CallID, &m.Reasoning, &m.OutItems, &m.Created) == nil {
			messages = append(messages, m)
		}
	}
	msgRows.Close()

	if len(sessions) == 0 && len(messages) == 0 {
		return
	}

	tx, err := mysqlDB.Begin()
	if err != nil {
		events.Warning(sink, "同步到在线 MySQL 失败（数据已安全保存于本地 SQLite）： "+err.Error(), "mysql_sync_failed")
		return
	}
	for _, s := range sessions {
		tx.Exec("INSERT INTO chat_sessions (id, status, created_at, updated_at) VALUES (?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE status=VALUES(status), updated_at=VALUES(updated_at)",
			s.ID, s.Status, s.Created, s.Updated)
	}
	for _, m := range messages {
		tx.Exec("INSERT INTO chat_messages (uid, session_id, role, content, tool_calls_json, tool_call_id, reasoning_content, output_items_json, created_at) "+
			"VALUES (?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE session_id=VALUES(session_id)",
			m.UID, m.SessionID, m.Role, nullIfEmpty(m.Content), nullIfEmpty(m.ToolCalls),
			nullIfEmpty(m.CallID), nullIfEmpty(m.Reasoning), nullIfEmpty(m.OutItems), m.Created)
	}
	if err := tx.Commit(); err != nil {
		events.Warning(sink, "同步到在线 MySQL 失败（数据已安全保存于本地 SQLite）： "+err.Error(), "mysql_sync_failed")
		return
	}
	for _, s := range sessions {
		sqliteDB.Exec("UPDATE chat_sessions SET synced=1 WHERE id=?", s.ID)
	}
	for _, m := range messages {
		sqliteDB.Exec("UPDATE chat_messages SET synced=1 WHERE id=?", m.ID)
	}
}

func nullIfEmpty(v sql.NullString) any {
	if v.Valid && v.String != "" {
		return v.String
	}
	return nil
}

// nowStamp SQLite CURRENT_TIMESTAMP 格式（UTC）
func NowStamp() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}
