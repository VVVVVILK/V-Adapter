// auth.go — 面板登录：首次无密码可进并引导设置密码；设置后管理端点需登录。
//
// 照搬参考项目 zai2api-http 的成熟模式：
//   - data/auth.json 存密码的 SHA-256 哈希；文件缺失/哈希为空 = 未设置密码，
//     此时 /admin/* 全部放行（面板引导设置密码）。
//   - 设置密码后，/admin/* 需携带有效会话 cookie；/ai/* 生图端点与 /health
//     不受影响（客户端用的是 nai_key，与面板登录互不相干）。
//   - 会话为内存 map + HttpOnly cookie，24h 过期。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	panelCookieName = "v_adapter_panel"
	panelSessionTTL = 24 * time.Hour
)

var authPath string // main() 里按运行目录确定：<base>/data/auth.json

type authFileData struct {
	PasswordHash string `json:"password_hash"` // sha256 hex；空 = 未设置
}

type panelSession struct {
	expiry time.Time
}

var (
	authMu   sync.Mutex
	sessions = map[string]panelSession{}
	authHash string // 空 = 未设置密码
)

// initAuth 启动时加载面板密码（在路由注册前调用）。
func initAuth() {
	var d authFileData
	if raw, err := os.ReadFile(authPath); err == nil {
		_ = json.Unmarshal(raw, &d)
	}
	authMu.Lock()
	authHash = strings.TrimSpace(d.PasswordHash)
	authMu.Unlock()
}

func hashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(h[:])
}

func saveAuthHash(h string) error {
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(authFileData{PasswordHash: h}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(authPath, data, 0o600)
}

// panelPasswordSet 是否已设置面板密码。
func panelPasswordSet() bool {
	authMu.Lock()
	defer authMu.Unlock()
	return authHash != ""
}

// panelOK 面板鉴权：未设置密码 → 放行；已设置 → 校验会话 cookie。
func panelOK(r *http.Request) bool {
	if !panelPasswordSet() {
		return true
	}
	c, err := r.Cookie(panelCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	authMu.Lock()
	defer authMu.Unlock()
	s, ok := sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(s.expiry) {
		delete(sessions, c.Value)
		return false
	}
	return true
}

func newSessionToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

func setPanelCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(panelSessionTTL / time.Second),
	})
}

func clearPanelCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requireAdmin 包装需要登录的管理端点（未设置密码时放行，面板引导设置）。
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !panelOK(r) {
			writeAdminErr(w, http.StatusUnauthorized, "面板已锁定：请先登录")
			return
		}
		h(w, r)
	}
}

// ── 路由与处理器 ──

func registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/auth/status", handleAuthStatus)
	mux.HandleFunc("/admin/auth/login", handleAuthLogin)
	mux.HandleFunc("/admin/auth/setup", handleAuthSetup)
	mux.HandleFunc("/admin/auth/logout", handleAuthLogout)
}

// GET /admin/auth/status → {success, setup_required, locked}（公开，登录遮罩用）。
func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	set := panelPasswordSet()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"setup_required": !set,
		"locked":         set && !panelOK(r),
	})
}

// POST /admin/auth/login {password} → 校验通过则种会话 cookie。
func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	authMu.Lock()
	want := authHash
	authMu.Unlock()
	if want == "" {
		writeAdminErr(w, 400, "尚未设置面板密码，请先完成首次设置")
		return
	}
	if hashPassword(body.Password) != want {
		writeAdminErr(w, 401, "密码错误")
		return
	}
	token := newSessionToken()
	authMu.Lock()
	sessions[token] = panelSession{expiry: time.Now().Add(panelSessionTTL)}
	authMu.Unlock()
	setPanelCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// POST /admin/auth/setup {password} → 设置/修改/关闭面板密码：
//   - 未设置（首次）：任意非空密码直接设置；
//   - 已设置：必须已登录才能修改或关闭（password 为空 = 关闭）。
func handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	password := strings.TrimSpace(body.Password)
	set := panelPasswordSet()

	if set && !panelOK(r) {
		writeAdminErr(w, 401, "请先登录后再修改面板密码")
		return
	}
	if password == "" {
		if !set {
			writeAdminErr(w, 400, "密码不能为空")
			return
		}
		// 关闭面板密码
		if err := saveAuthHash(""); err != nil {
			writeAdminErr(w, 500, "保存失败: "+err.Error())
			return
		}
		authMu.Lock()
		authHash = ""
		authMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "closed": true})
		return
	}
	if len(password) < 4 {
		writeAdminErr(w, 400, "密码至少 4 位")
		return
	}
	hash := hashPassword(password)
	if err := saveAuthHash(hash); err != nil {
		writeAdminErr(w, 500, "保存失败: "+err.Error())
		return
	}
	authMu.Lock()
	authHash = hash
	authMu.Unlock()
	// 首次设置成功即视为已登录（无需再输一次）。
	if !set {
		token := newSessionToken()
		authMu.Lock()
		sessions[token] = panelSession{expiry: time.Now().Add(panelSessionTTL)}
		authMu.Unlock()
		setPanelCookie(w, token)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// POST /admin/auth/logout → 销毁会话并清 cookie。
func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(panelCookieName); err == nil && c.Value != "" {
		authMu.Lock()
		delete(sessions, c.Value)
		authMu.Unlock()
	}
	clearPanelCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
