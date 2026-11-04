// skillhub：从远程技能仓库（GitHub / Gitea）查看 / 下载 / 卸载 skill（对应 core/tools/skill_hub.py）
package tools

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go-shell/internal/config"
	"go-shell/internal/envelope"
)

const hubTimeout = 30 * time.Second

var skillNameRe2 = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

func hubRepo() string {
	if v := strings.Trim(strings.TrimSpace(config.SkillHubRepo), "/"); v != "" {
		return v
	}
	return "abc12524/shell-skills"
}

func hubBranch() string {
	if v := strings.TrimSpace(config.SkillHubBranch); v != "" {
		return v
	}
	return "main"
}

func hubType() string {
	t := strings.ToLower(strings.TrimSpace(config.SkillHubType))
	if t == "github" || t == "gitea" {
		return t
	}
	if config.SkillHubBase != "" {
		return "gitea"
	}
	return "github"
}

// HubSource 仓库来源展示文案
func HubSource() string {
	if hubType() == "gitea" {
		base := config.SkillHubBase
		if base == "" {
			base = "https://gitea.com"
		}
		return fmt.Sprintf("%s/%s@%s (gitea)", base, hubRepo(), hubBranch())
	}
	return fmt.Sprintf("%s@%s (github)", hubRepo(), hubBranch())
}

func hubIndexURL() string {
	if hubType() == "gitea" {
		return fmt.Sprintf("%s/%s/raw/branch/%s/index.json", config.SkillHubBase, hubRepo(), hubBranch())
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/index.json", hubRepo(), hubBranch())
}

func hubTarballURL() string {
	if hubType() == "gitea" {
		return fmt.Sprintf("%s/%s/archive/%s.tar.gz", config.SkillHubBase, hubRepo(), hubBranch())
	}
	return fmt.Sprintf("https://codeload.github.com/%s/tar.gz/refs/heads/%s", hubRepo(), hubBranch())
}

func hubHeaders() map[string]string {
	h := map[string]string{"User-Agent": "shell-tool-skill-hub"}
	token := config.SkillHubToken
	if hubType() == "gitea" {
		h["Accept"] = "application/json"
		if token == "" {
			token = os.Getenv("GITEA_TOKEN")
		}
		if token != "" {
			h["Authorization"] = "token " + token
		}
		return h
	}
	h["Accept"] = "application/vnd.github+json"
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return h
}

func httpGet(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hubHeaders() {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: hubTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// FetchIndex 拉取根索引，返回 skills 列表
func FetchIndex() ([]map[string]any, error) {
	body, err := httpGet(hubIndexURL())
	if err != nil {
		return nil, fmt.Errorf("拉取索引失败 - %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("索引不是合法 JSON - %v", err)
	}
	skills, _ := data["skills"].([]any)
	if skills == nil {
		return nil, fmt.Errorf("索引缺少 skills 列表")
	}
	out := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func localInstalled(name string) bool {
	st, err := os.Stat(filepath.Join(SkillDir(), name))
	return err == nil && st.IsDir()
}

// RemoteSkills 远程技能列表（含 installed 标记）
func RemoteSkills() ([]map[string]any, error) {
	index, err := FetchIndex()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(index))
	for _, s := range index {
		name, _ := s["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		platform, _ := s["platform"].([]any)
		requires, _ := s["requires"].([]any)
		out = append(out, map[string]any{
			"name":        name,
			"description": s["description"],
			"version":     s["version"],
			"platform":    platform,
			"requires":    requires,
			"installed":   localInstalled(name),
		})
	}
	return out, nil
}

// SearchSkills 按名称前缀筛选远程技能
func SearchSkills(skills []map[string]any, keyword string) []map[string]any {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	if kw == "" {
		return skills
	}
	var out []map[string]any
	for _, s := range skills {
		if name, _ := s["name"].(string); strings.HasPrefix(strings.ToLower(name), kw) {
			out = append(out, s)
		}
	}
	return out
}

func safeRelPath(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.Trim(rel, "/")
	if rel == "" || strings.HasPrefix(rel, "/") {
		return ""
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return ""
		}
	}
	return rel
}

// contentsAPI 列出仓库某目录条目
func contentsAPI(path string) ([]map[string]any, error) {
	var url string
	if hubType() == "gitea" {
		url = fmt.Sprintf("%s/api/v1/repos/%s/contents/%s?ref=%s", config.SkillHubBase, hubRepo(), path, hubBranch())
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", hubRepo(), path, hubBranch())
	}
	body, err := httpGet(url)
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil || list == nil {
		return nil, fmt.Errorf("'%s' 不是目录", path)
	}
	return list, nil
}

// collectViaAPI 仅下载 <path>/ 子树
func collectViaAPI(path string) ([][2]string, error) { // [rel, data]
	var files [][2]string
	var walk func(relDir string) error
	walk = func(relDir string) error {
		full := path
		if relDir != "" {
			full = path + "/" + relDir
		}
		entries, err := contentsAPI(full)
		if err != nil {
			return err
		}
		for _, e := range entries {
			etype, _ := e["type"].(string)
			name, _ := e["name"].(string)
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}
			if etype == "dir" {
				if err := walk(rel); err != nil {
					return err
				}
			} else if etype == "file" {
				dl, _ := e["download_url"].(string)
				if dl == "" {
					continue
				}
				data, err := httpGet(dl)
				if err != nil {
					return err
				}
				files = append(files, [2]string{rel, string(data)})
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return files, nil
}

// collectViaTarball 回退：整仓 tarball 取子树
func collectViaTarball(path string) ([][2]string, error) {
	body, err := httpGet(hubTarballURL())
	if err != nil {
		return nil, fmt.Errorf("下载技能包失败 - %v", err)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("技能包不是合法压缩包 - %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files [][2]string
	prefix := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("技能包不是合法压缩包 - %v", err)
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		if prefix == "" {
			parts := strings.SplitN(hdr.Name, "/", 2)
			prefix = parts[0] + "/" + path + "/"
		}
		if !strings.HasPrefix(hdr.Name, prefix) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files = append(files, [2]string{hdr.Name[len(prefix):], string(data)})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("技能包为空")
	}
	return files, nil
}

// Install 下载并安装指定 skill
func Install(name string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if !skillNameRe2.MatchString(name) {
		return nil, fmt.Errorf("非法 skill名 '%s'", name)
	}
	index, err := FetchIndex()
	if err != nil {
		return nil, err
	}
	var entry map[string]any
	for _, s := range index {
		if n, _ := s["name"].(string); strings.TrimSpace(n) == name {
			entry = s
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("远程仓库没有 skill '%s'", name)
	}
	path, _ := entry["path"].(string)
	if path == "" {
		path = name
	}
	path = safeRelPath(path)
	if path == "" {
		return nil, fmt.Errorf("skill '%s' 的 path 非法", name)
	}

	destDir := filepath.Join(SkillDir(), name)
	// skill/ 目录不存在时自动创建：全新部署无需预置 skill/，下载即生成
	if err := os.MkdirAll(SkillDir(), 0o755); err != nil {
		return nil, err
	}
	tmpDir := destDir + ".tmp"
	os.RemoveAll(tmpDir)

	source := "api"
	files, err := collectViaAPI(path)
	if err != nil {
		source = "tarball"
		files, err = collectViaTarball(path)
		if err != nil {
			return nil, fmt.Errorf("下载技能 '%s' 失败 - %v", name, err)
		}
	}

	var written []string
	total := 0
	for _, f := range files {
		rel := safeRelPath(f[0])
		if rel == "" {
			continue
		}
		target := filepath.Join(tmpDir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(target), 0o755)
		if err := os.WriteFile(target, []byte(f[1]), 0o644); err != nil {
			return nil, err
		}
		written = append(written, rel)
		total += len(f[1])
	}
	if len(written) == 0 {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("未找到 '%s/' 目录内容", path)
	}

	// 保留本地 config.json / *.conf
	preserved := map[string][]byte{}
	if entries, err := os.ReadDir(destDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fn := e.Name()
			if fn == "config.json" || strings.HasSuffix(fn, ".conf") {
				if data, err := os.ReadFile(filepath.Join(destDir, fn)); err == nil {
					preserved[fn] = data
				}
			}
		}
	}
	os.RemoveAll(destDir)
	if err := os.Rename(tmpDir, destDir); err != nil {
		return nil, err
	}
	var preservedNames []string
	for fn, data := range preserved {
		os.WriteFile(filepath.Join(destDir, fn), data, 0o644)
		preservedNames = append(preservedNames, fn)
	}
	sort.Strings(preservedNames)

	configCreated := false
	example := filepath.Join(destDir, "config.example.json")
	real := filepath.Join(destDir, "config.json")
	if _, err := os.Stat(example); err == nil {
		if _, err := os.Stat(real); os.IsNotExist(err) {
			data, _ := os.ReadFile(example)
			os.WriteFile(real, data, 0o644)
			configCreated = true
		}
	}
	requires, _ := entry["requires"].([]any)
	return map[string]any{
		"name": name, "path": destDir, "source": source, "files": written,
		"bytes": total, "config_created": configCreated,
		"config_preserved": preservedNames, "requires": requires,
	}, nil
}

// Remove 卸载已安装的 skill
func Remove(name string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if !skillNameRe2.MatchString(name) {
		return nil, fmt.Errorf("非法 skill名 '%s'", name)
	}
	destDir := filepath.Join(SkillDir(), name)
	if st, err := os.Stat(destDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("未安装 skill '%s'", name)
	}
	if err := os.RemoveAll(destDir); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "removed": true}, nil
}

// SkillHub skill_hub 入口：查看远程技能 / 安装 / 卸载
func SkillHub(action, name string) string {
	act := strings.ToLower(strings.TrimSpace(action))
	if act == "" {
		act = "list"
	}
	switch act {
	case "list":
		skills, err := RemoteSkills()
		if err != nil {
			return envelope.Error(err.Error(), "hub_failed")
		}
		return envelope.Ok(map[string]any{
			"type": hubType(), "base": config.SkillHubBase, "repo": hubRepo(),
			"branch": hubBranch(), "count": len(skills), "skills": skills,
		})
	case "install":
		if strings.TrimSpace(name) == "" {
			return envelope.Error("install 需要提供 name", "missing_argument")
		}
		res, err := Install(name)
		if err != nil {
			return envelope.Error(err.Error(), "hub_failed")
		}
		return envelope.Ok(res)
	case "remove":
		if strings.TrimSpace(name) == "" {
			return envelope.Error("remove 需要提供 name", "missing_argument")
		}
		res, err := Remove(name)
		if err != nil {
			return envelope.Error(err.Error(), "hub_failed")
		}
		return envelope.Ok(res)
	default:
		return envelope.Error(fmt.Sprintf("未知 action '%s'（应为 list/install/remove）", action), "unknown_action")
	}
}
