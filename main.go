// main.go — V. Adapter：NovelAI 协议适配服务。
//
// 对任何支持 NovelAI 协议的客户端，本服务伪装成 NovelAI 官网：
// 接收 /ai/generate-image 等 NovelAI 协议请求，翻译成 OpenAI 兼容生图请求
// 发给上游生图 API，把生成的图打包成 ZIP（内含一张图）按 NovelAI 格式返回。
// 客户端一行代码不改，面板里把 NovelAI 渠道的 URL 指向本服务即可。
//
// 管理面板沿用 zai2api-http 的既有方案：
// settingsMu 热设置 + data/settings.json 持久化 + 首次免密的会话 cookie 登录。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

const version = "v1.1.4"

var (
	cfg       *startupConfig
	startTime = time.Now()

	// baseDir 决定 config.json 与 data/ 的落点（exe 目录优先，其次工作目录）。
	baseDir string
)

// startupConfig 启动默认配置（config.json + 环境变量），随后被 settings.json 覆盖。
type startupConfig struct {
	Listen       string `json:"listen"`
	QwenURL      string `json:"qwen_url"`
	QwenKey      string `json:"qwen_key"`
	QwenModel    string `json:"qwen_model"`
	DefaultSize  string `json:"default_size"`
	NaiKey       string `json:"nai_key"`
	ChatFallback string `json:"chat_fallback"`
}

func defaultConfig() *startupConfig {
	return &startupConfig{
		Listen:       "0.0.0.0:8888",
		QwenURL:      "http://127.0.0.1:4000/v1",
		QwenKey:      "sk-你的上游密钥",
		QwenModel:    "qwen3.8-max",
		DefaultSize:  "1024x1024",
		NaiKey:       "v-adapter-8888",
		ChatFallback: "auto",
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// resolveBaseDir exe 目录里有 config.json 或 data/ 就用 exe 目录（服务器部署友好）；
// 否则用工作目录（go run / 手动启动场景）。
func resolveBaseDir() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		if fileExists(filepath.Join(d, "config.json")) || fileExists(filepath.Join(d, "data")) {
			return d
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// envFirst 按顺序取第一个非空环境变量。
func envFirst(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func loadConfig(base string) *startupConfig {
	c := defaultConfig()
	if raw, err := os.ReadFile(filepath.Join(base, "config.json")); err == nil {
		_ = json.Unmarshal(raw, c) // 存在的字段覆盖默认，缺省字段保留
	}
	// 环境变量优先（VADAPTER_* 为本服务专用，OPENAI_* 为通用名）
	if v := envFirst("VADAPTER_QWEN_URL", "OPENAI_BASE_URL"); v != "" {
		c.QwenURL = v
	}
	if v := envFirst("VADAPTER_QWEN_KEY", "OPENAI_API_KEY"); v != "" {
		c.QwenKey = v
	}
	if v := envFirst("VADAPTER_QWEN_MODEL", "OPENAI_IMAGE_MODEL"); v != "" {
		c.QwenModel = v
	}
	if v := envFirst("VADAPTER_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := envFirst("VADAPTER_DEFAULT_SIZE"); v != "" {
		c.DefaultSize = v
	}
	if v := envFirst("VADAPTER_NAI_KEY"); v != "" {
		c.NaiKey = v
	}
	return c
}

func main() {
	baseDir = resolveBaseDir()
	settingsPath = filepath.Join(baseDir, "data", "settings.json")
	authPath = filepath.Join(baseDir, "data", "auth.json")

	cfg = loadConfig(baseDir)
	initSettings(cfg)
	initAuth()

	listen := settingsListen()
	qwenPort := listen
	if _, port, err := net.SplitHostPort(listen); err == nil {
		qwenPort = port
	}

	printStartupBanner(listen, settingsQwenURL(), settingsQwenModel(), settingsDefaultSize(),
		settingsChatFallback(), settingsNaiKey() != "")
	logf("[Startup] 运行目录 %s | 面板密码：%s",
		baseDir,
		map[bool]string{true: "已设置", false: "未设置（面板可设置）"}[panelPasswordSet()])
	logf("[Startup] 客户端对接：NovelAI 渠道 URL 填 http://<本机IP>:%s（不要带 /ai），Key 填服务端 nai_key，模型随便选", qwenPort)

	mux := http.NewServeMux()
	// ── NovelAI 协议端点（客户端调用）──
	mux.HandleFunc("/ai/generate-image", naiKeyGate(handleGenerateImage))
	mux.HandleFunc("/ai/generate-image/", naiKeyGate(handleGenerateImage))
	mux.HandleFunc("/ai/user/subscription", naiKeyGate(handleSubscription))
	mux.HandleFunc("/ai/user/subscription/", naiKeyGate(handleSubscription))
	mux.HandleFunc("/ai/encode-vibe", handleEncodeVibe)
	mux.HandleFunc("/ai/encode-vibe/", handleEncodeVibe)
	// ── 管理面板 ──
	registerAuthRoutes(mux)
	mux.HandleFunc("/admin/status", handleAdminStatus)
	mux.HandleFunc("/admin/settings", requireAdmin(handleAdminSettings))
	mux.HandleFunc("/admin/test", requireAdmin(handleAdminTest))
	mux.HandleFunc("/admin/translate", requireAdmin(handleAdminTranslate))
	mux.HandleFunc("/admin/styles", requireAdmin(handleAdminStyles))
	mux.HandleFunc("/admin/logs", requireAdmin(handleAdminLogs))
	mux.HandleFunc("/health", handleHealth)
	// ── 内嵌单文件面板 ──
	if indexBytes, err := staticFS.ReadFile("static/index.html"); err == nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(indexBytes)
		})
	} else {
		logf("内嵌面板不可用: %v", err)
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           corsMiddleware(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()

	select {
	case err := <-srvErr:
		if err != nil && err != http.ErrServerClosed {
			fmt.Println("服务退出:", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logf("收到中断信号，退出中 …")
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

// ── 通用小工具 ──

// corsMiddleware 浏览器跨域支持：客户端会从浏览器页面直连本服务，
// 必须允许跨域，否则浏览器拦截 → "Failed to fetch"。
// OPTIONS 预检请求直接放行（预检不带 Authorization，不能走业务 key 校验）。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logf(f string, a ...any) {
	line := fmt.Sprintf(f, a...)
	if ansiEnabled && logHasError(line) {
		// 失败/异常行红色渲染，从绿色横幅里跳出来（终端下才生效）
		fmt.Printf("[%s] %s%s%s\n", time.Now().Format("15:04:05"), ansiRed, line, ansiReset)
		return
	}
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), line)
}

// truncate 按字符数截断（中文安全），超长加 …
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// writeJSON 统一 JSON 输出（NovelAI 端点错误用 {"message": …}，客户端会取 message 展示）。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAdminErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"success": false, "error": msg})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version})
}

// atoiSafe 宽松转 int。
func atoiSafe(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}
