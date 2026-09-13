// settings.go — 运行时可变设置（线程安全 + data/settings.json 持久化 + 热生效）。
//
// 对齐参考项目 zai2api-http 的 settingsMu 模式：启动时先按 config.json（+环境
// 变量覆盖）填默认值，再读 data/settings.json 覆盖存在的字段（文件不存在则用
// 默认，不报错）；运行期由面板「设置中心」POST /admin/settings 改写并立即热
// 生效（读写统一经 settingsMu），同时写回 settings.json，重启不丢失。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var settingsPath string // main() 里按运行目录确定：<base>/data/settings.json

// persistedSettings settings.json 的磁盘结构。指针字段区分「字段存在」与「未设置」——
// JSON 里缺字段时沿用启动默认；qwen_key/nai_key 显式写空 = 不校验/用占位符。
type persistedSettings struct {
	QwenURL      *string `json:"qwen_url"`
	QwenKey      *string `json:"qwen_key"`
	QwenModel    *string `json:"qwen_model"`
	DefaultSize  *string `json:"default_size"`
	NaiKey       *string `json:"nai_key"`
	ChatFallback *string `json:"chat_fallback"`
	Listen       *string `json:"listen"`
	Style        *string `json:"style"`
	StyleCustom  *string `json:"style_custom"`
}

// runtimeSettings 运行期可变设置（全部经 settingsMu 读写）。
type runtimeSettings struct {
	qwenURL      string
	qwenKey      string
	qwenModel    string
	defaultSize  string
	naiKey       string
	chatFallback string // auto / off / chat_only
	listen       string
	style        string // 画风预设 id；"" = 不启用；"custom" = 用 styleCustom
	styleCustom  string // 自定义画风提示词（style=custom 时生效）
}

var (
	settingsMu sync.RWMutex
	rt         runtimeSettings
)

// initSettings 启动初始化：先按 config.json(+env) 填默认，再读 settings.json 覆盖。
func initSettings(cfg *startupConfig) {
	rt = runtimeSettings{
		qwenURL:      strings.TrimSpace(cfg.QwenURL),
		qwenKey:      strings.TrimSpace(cfg.QwenKey),
		qwenModel:    strings.TrimSpace(cfg.QwenModel),
		defaultSize:  normalizeSizeOrDefault(cfg.DefaultSize),
		naiKey:       strings.TrimSpace(cfg.NaiKey),
		chatFallback: normalizeChatFallback(cfg.ChatFallback),
		listen:       normalizeListenOrDefault(cfg.Listen),
		style:        "",
		styleCustom:  "",
	}
	var p persistedSettings
	if raw, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(raw, &p)
	}
	settingsMu.Lock()
	applyPersisted(&p)
	settingsMu.Unlock()
}

// applyPersisted 把磁盘快照覆盖进运行期设置（调用方需持锁）。
func applyPersisted(p *persistedSettings) {
	if p.QwenURL != nil && strings.TrimSpace(*p.QwenURL) != "" {
		rt.qwenURL = strings.TrimSpace(*p.QwenURL)
	}
	if p.QwenKey != nil {
		rt.qwenKey = strings.TrimSpace(*p.QwenKey)
	}
	if p.QwenModel != nil && strings.TrimSpace(*p.QwenModel) != "" {
		rt.qwenModel = strings.TrimSpace(*p.QwenModel)
	}
	if p.DefaultSize != nil {
		if s, ok := normalizeSizeStr(*p.DefaultSize); ok {
			rt.defaultSize = s
		}
	}
	if p.NaiKey != nil {
		rt.naiKey = strings.TrimSpace(*p.NaiKey)
	}
	if p.ChatFallback != nil {
		rt.chatFallback = normalizeChatFallback(*p.ChatFallback)
	}
	if p.Listen != nil {
		if s, ok := normalizeListen(*p.Listen); ok {
			rt.listen = s
		}
	}
	if p.Style != nil {
		rt.style = normalizeStyleID(*p.Style)
	}
	if p.StyleCustom != nil {
		rt.styleCustom = strings.TrimSpace(*p.StyleCustom)
	}
}

// ── 归一化工具 ──

// normalizeSizeStr 校验「宽x高」字符串（也容忍 × 与空格），返回规范形式。
func normalizeSizeStr(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "×", "x")))
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return "", false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w < 16 || h < 16 || w > 4096 || h > 4096 {
		return "", false
	}
	return fmt.Sprintf("%dx%d", w, h), true
}

func normalizeSizeOrDefault(s string) string {
	if v, ok := normalizeSizeStr(s); ok {
		return v
	}
	return "1024x1024"
}

// normalizeListen 校验 host:port（缺 host 时补 :）。
func normalizeListen(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, ":") {
		s = ":" + s
	}
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", false
	}
	return s, true
}

func normalizeListenOrDefault(s string) string {
	if v, ok := normalizeListen(s); ok {
		return v
	}
	return "0.0.0.0:8888"
}

func normalizeChatFallback(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off":
		return "off"
	case "chat_only":
		return "chat_only"
	case "openai":
		return "openai"
	default:
		return "auto"
	}
}

// maskKey Key 脱敏：短 key 只留首字符（如 1 → 1***），长 key 留头 3 尾 2。
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 4 {
		return string(r[0]) + "***"
	}
	return string(r[:3]) + "***" + string(r[len(r)-2:])
}

// ── 线程安全 getter（热生效读取点统一走这里） ──

func settingsQwenURL() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.qwenURL
}

func settingsQwenKey() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.qwenKey
}

func settingsQwenModel() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.qwenModel
}

func settingsDefaultSize() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.defaultSize
}

func settingsNaiKey() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.naiKey
}

func settingsChatFallback() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.chatFallback
}

func settingsListen() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.listen
}

func settingsStyle() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.style
}

func settingsStyleCustom() string {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return rt.styleCustom
}

// settingsView 设置中心展示快照。Key 一律脱敏（验收要求：GET /admin/settings
// 不返回明文 key），只给 *_set 布尔让前端知道是否已配置。
func settingsView() map[string]any {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return map[string]any{
		"qwen_url":         rt.qwenURL,
		"qwen_key_masked":  maskKey(rt.qwenKey),
		"qwen_key_set":     rt.qwenKey != "",
		"qwen_model":       rt.qwenModel,
		"default_size":     rt.defaultSize,
		"nai_key_masked":   maskKey(rt.naiKey),
		"nai_key_required": rt.naiKey != "",
		"chat_fallback":    rt.chatFallback,
		"listen":           rt.listen,
		"style":            rt.style,
		"style_custom":     rt.styleCustom,
	}
}

// snapshotPersisted 把运行期设置快照成磁盘结构（指针全量填充，完整覆盖旧文件）。
func snapshotPersisted() persistedSettings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	qwenURL, qwenKey, qwenModel := rt.qwenURL, rt.qwenKey, rt.qwenModel
	defSize, naiKey, chatFB, listen := rt.defaultSize, rt.naiKey, rt.chatFallback, rt.listen
	style, styleCustom := rt.style, rt.styleCustom
	return persistedSettings{
		QwenURL:      &qwenURL,
		QwenKey:      &qwenKey,
		QwenModel:    &qwenModel,
		DefaultSize:  &defSize,
		NaiKey:       &naiKey,
		ChatFallback: &chatFB,
		Listen:       &listen,
		Style:        &style,
		StyleCustom:  &styleCustom,
	}
}

func saveSettingsFile(p persistedSettings) {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		logf("创建设置目录失败: %v", err)
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
		logf("写入设置失败: %v", err)
	}
}

// applySettings 应用面板提交的键值：锁内校验并热生效，返回 (changed, notes)。
//
// Key 字段特殊语义（面板明文不留存）：
//   - 提交为空 / 与当前脱敏值相同 → 视为「未修改」，跳过；
//   - 真正清空用 body 里的 "clear": ["qwen_key"|"nai_key"]。
func applySettings(body map[string]any) (changed, notes []string) {
	settingsMu.Lock()
	if v, ok := body["qwen_url"]; ok {
		if s := strings.TrimSpace(toStr(v)); s != "" {
			if s != rt.qwenURL {
				rt.qwenURL = s
				changed = append(changed, "qwen_url")
				resetImagesBroken() // 换上游后重新探测标准生图接口
			}
		}
	}
	if v, ok := body["qwen_key"]; ok {
		if s := strings.TrimSpace(toStr(v)); s != "" && s != maskKey(rt.qwenKey) {
			rt.qwenKey = s
			changed = append(changed, "qwen_key")
			resetImagesBroken()
		}
	}
	if v, ok := body["qwen_model"]; ok {
		if s := strings.TrimSpace(toStr(v)); s != "" {
			if s != rt.qwenModel {
				rt.qwenModel = s
				changed = append(changed, "qwen_model")
				resetImagesBroken()
			}
		}
	}
	if v, ok := body["default_size"]; ok {
		s := strings.TrimSpace(toStr(v))
		if s == "" {
			// 空值不动
		} else if n, ok2 := normalizeSizeStr(s); ok2 {
			if n != rt.defaultSize {
				rt.defaultSize = n
				changed = append(changed, "default_size")
			}
		} else {
			notes = append(notes, "default_size 格式无效（应为 宽x高，如 832x1216），已忽略")
		}
	}
	if v, ok := body["nai_key"]; ok {
		if s := strings.TrimSpace(toStr(v)); s != "" && s != maskKey(rt.naiKey) {
			rt.naiKey = s
			changed = append(changed, "nai_key")
		}
	}
	if v, ok := body["chat_fallback"]; ok {
		if s := normalizeChatFallback(toStr(v)); s != rt.chatFallback {
			rt.chatFallback = s
			changed = append(changed, "chat_fallback")
		}
	}
	if v, ok := body["listen"]; ok {
		if s, ok2 := normalizeListen(toStr(v)); ok2 && s != rt.listen {
			rt.listen = s
			changed = append(changed, "listen")
			notes = append(notes, "listen 已保存，重启服务后生效")
		}
	}
	if v, ok := body["style"]; ok {
		if s := normalizeStyleID(toStr(v)); s != rt.style {
			rt.style = s
			changed = append(changed, "style")
		}
	}
	if v, ok := body["style_custom"]; ok {
		if s := strings.TrimSpace(toStr(v)); s != rt.styleCustom {
			rt.styleCustom = s
			changed = append(changed, "style_custom")
		}
	}
	if clears, ok := body["clear"].([]any); ok {
		for _, c := range clears {
			switch toStr(c) {
			case "qwen_key":
				if rt.qwenKey != "" {
					rt.qwenKey = ""
					changed = append(changed, "qwen_key")
				}
			case "nai_key":
				if rt.naiKey != "" {
					rt.naiKey = ""
					changed = append(changed, "nai_key")
					notes = append(notes, "nai_key 已清空：客户端填任意 key 均可调用")
				}
			}
		}
	}
	settingsMu.Unlock()

	if len(changed) > 0 {
		saveSettingsFile(snapshotPersisted())
	}
	return changed, notes
}

// ── /admin/settings 处理器（需面板登录） ──

func handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "settings": settingsView()})

	case http.MethodPost:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
			body = map[string]any{}
		}
		if inner, ok := body["settings"].(map[string]any); ok {
			body = inner
		}
		changed, notes := applySettings(body)
		for _, k := range changed {
			if k == "nai_key" && settingsNaiKey() != "" {
				logf("[Settings] nai_key 已更新（客户端须同步改 Key）")
			}
			if k == "qwen_key" {
				logf("[Settings] qwen_key 已更新")
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "changed": changed, "notes": notes, "settings": settingsView(),
		})

	default:
		writeAdminErr(w, http.StatusMethodNotAllowed, "方法不支持")
	}
}
