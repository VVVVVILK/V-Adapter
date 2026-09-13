// nai_handler.go — NovelAI 协议兼容端点（客户端 NovelAI 渠道的调用面）。
//
// 端点（客户端会把配置的 url 末尾去斜杠后补 /ai/<path>）：
//
//	POST /ai/generate-image    生图：请求是 NovelAI 格式 JSON，响应必须是 ZIP（内含一张图）
//	GET  /ai/user/subscription 测试连接：返回 200+订阅 JSON（客户端显示「连接正常:Free」）
//	POST /ai/encode-vibe       vibe 编码：本服务不支持，返回 404（客户端提示 vibe 编码失败，可接受）
//
// 错误一律用非 2xx + {"message": "…"}：客户端会取 message 字段展示给用户，
// 所以错误必须是人话，不能是裸状态码。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// naiGenerateReq 生图请求体（宽容解析：只取实际要用的字段，其余丢弃）。
type naiGenerateReq struct {
	Input  string         `json:"input"`
	Model  string         `json:"model"`
	Action string         `json:"action"`
	Params map[string]any `json:"parameters"`
}

// naiKeyGate 校验客户端带来的 Bearer key（服务端 nai_key 为空 = 不校验）。
func naiKeyGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := settingsNaiKey()
		if want != "" {
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if got == "" {
				got = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "bearer "))
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"message":    "API Key 不正确：请在客户端 NovelAI 渠道把 Key 填成与服务端一致的值（可在管理面板查看/修改）",
					"statusCode": http.StatusUnauthorized,
				})
				return
			}
		}
		h(w, r)
	}
}

// handleGenerateImage POST /ai/generate-image → ZIP
func handleGenerateImage(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "仅支持 POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "读取请求体失败：" + err.Error()})
		return
	}
	var req naiGenerateReq
	// 容忍个别客户端/工具带来的 UTF-8 BOM（客户端本身不带）
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "请求体不是合法 JSON：" + err.Error()})
		return
	}

	prompt := strings.TrimSpace(req.Input)
	params := req.Params
	neg := strFromMap(params, "negative_prompt")
	if neg == "" {
		neg = v4BaseNegative(params)
	}
	// 画风预设：把面板选中的画风提示词拼到 prompt 前、负向词合并进 neg
	prompt, neg = applyStyle(prompt, neg)
	defW, defH := defaultSizeWH()
	width, height := defW, defH
	if v := numFromMap(params, "width"); v != nil && *v >= 16 {
		width = clampInt(int(*v), 64, 2048)
	}
	if v := numFromMap(params, "height"); v != nil && *v >= 16 {
		height = clampInt(int(*v), 64, 2048)
	}
	size := fmt.Sprintf("%dx%d", width, height)
	tgt := targetFromSettings()

	rec := GenRecord{
		Kind: "generate", Endpoint: "/ai/generate-image", Model: tgt.Model,
		Prompt: truncate(prompt, 80), Size: size,
	}

	if prompt == "" {
		rec.Status = http.StatusBadRequest
		rec.Error = "正向提示词（input）为空"
		genLog.Add(rec)
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "正向提示词（input）为空，客户端未拼出正向词"})
		return
	}

	logf("[Gen] 生图请求 model=%q size=%s steps=%s seed=%s 负向词=%d字 正向词=%d字",
		req.Model, size, numStr(params, "steps"), numStr(params, "seed"),
		len([]rune(neg)), len([]rune(prompt)))

	res, gerr := generateImage(r.Context(), tgt, prompt, neg, size, settingsChatFallback())
	rec.LatencyMs = time.Since(start).Milliseconds()
	if gerr != nil {
		rec.OK = false
		rec.Status = http.StatusBadGateway
		rec.Via = "images"
		rec.Error = truncate(gerr.Error(), 300)
		genLog.Add(rec)
		logf("[Gen] 生图失败（%dms）：%v", rec.LatencyMs, gerr)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"message":    truncate(gerr.Error(), 500),
			"statusCode": http.StatusBadGateway,
		})
		return
	}
	rec.OK = true
	rec.Status = http.StatusOK
	rec.Via = res.Via
	genLog.Add(rec)
	logf("[Gen] 生图成功（%dms via %s）：%s %s %dKB",
		rec.LatencyMs, res.Via, size, res.Ext, len(res.Data)/1024)

	if err := writeImageZip(w, "image_0."+res.Ext, res.Data); err != nil {
		logf("[Gen] 打包 ZIP 失败：%v", err)
	}
}

// writeImageZip 把图片字节打包成客户端期望的 ZIP（unzipSync 后找第一个图片文件）。
func writeImageZip(w http.ResponseWriter, name string, data []byte) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create(name)
	if err == nil {
		_, err = fw.Write(data)
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "打包 ZIP 失败：" + err.Error()})
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="image_0.zip"`)
	w.WriteHeader(http.StatusOK)
	_, werr := w.Write(buf.Bytes())
	return werr
}

// handleSubscription GET /ai/user/subscription → 订阅信息（客户端「测试连接」用）。
func handleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "仅支持 GET"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tier":   0,
		"active": true,
		"subscription": map[string]any{
			"tier":      0,
			"active":    true,
			"expiresAt": 0,
		},
	})
}

// handleEncodeVibe POST /ai/encode-vibe → 明确不支持（404）。
func handleEncodeVibe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"message":    "本服务不支持 vibe 参考图编码（后端是上游生图，无 vibe 能力）；请在客户端里关闭 vibe 参考图",
		"statusCode": http.StatusNotFound,
	})
}

// ── 请求体取值小工具 ──

func strFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func numFromMap(m map[string]any, key string) *float64 {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case float64:
		return &v
	case string:
		if f, err := json.Number(strings.TrimSpace(v)).Float64(); err == nil {
			return &f
		}
	}
	return nil
}

func numStr(m map[string]any, key string) string {
	if v := numFromMap(m, key); v != nil {
		return fmt.Sprintf("%.0f", *v)
	}
	return "-"
}

// v4BaseNegative 部分模型把负向词放在 v4_negative_prompt.caption.base_caption。
func v4BaseNegative(params map[string]any) string {
	if params == nil {
		return ""
	}
	v4, ok := params["v4_negative_prompt"].(map[string]any)
	if !ok {
		return ""
	}
	capObj, ok := v4["caption"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := capObj["base_caption"].(string)
	return strings.TrimSpace(s)
}

// defaultSizeWH 解析默认尺寸，兜底 1024x1024。
func defaultSizeWH() (int, int) {
	s := settingsDefaultSize()
	if n, ok := normalizeSizeStr(s); ok {
		s = n
	} else {
		s = "1024x1024"
	}
	parts := strings.SplitN(s, "x", 2)
	w, _ := atoiSafe(parts[0])
	h, _ := atoiSafe(parts[1])
	if w <= 0 || h <= 0 {
		return 1024, 1024
	}
	return w, h
}
