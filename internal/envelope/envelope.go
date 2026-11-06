// Package envelope：统一的工具结果信封（对应 core/tools/envelope.py）
//
// 成功：{"status": "ok", "result": {...}}
// 失败：{"status": "error", "error": "...", "code": "..."}
package envelope

import (
	"encoding/json"
	"fmt"
)

// Ok 构造成功信封 JSON 字符串
func Ok(result map[string]any) string {
	if result == nil {
		result = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"status": "ok", "result": result})
	return string(b)
}

// Error 构造失败信封 JSON 字符串
func Error(message string, code string) string {
	payload := map[string]any{"status": "error", "error": message}
	if code != "" {
		payload["code"] = code
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// IsError 判断已解析的响应 dict 是否为失败信封
func IsError(value map[string]any) bool {
	if value == nil {
		return false
	}
	s, _ := value["status"].(string)
	return s == "error"
}

// Result 提取成功信封的 result 字段（非信封或失败返回 nil）
func Result(parsed map[string]any) map[string]any {
	if parsed == nil {
		return nil
	}
	s, _ := parsed["status"].(string)
	if s != "ok" {
		return nil
	}
	r, _ := parsed["result"].(map[string]any)
	return r
}

// Fmt 便捷格式化（错误信息里常用）
func Fmt(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
