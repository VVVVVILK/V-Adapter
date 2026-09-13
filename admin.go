// admin.go — 管理面板端点（运行总览 / 测试连接 / 生成记录）。
//
// /admin/status 公开（与 /health 同级，只给脱敏信息，不含任何明文 key）；
// /admin/settings、/admin/test、/admin/logs 需面板登录（auth.go 的 requireAdmin）。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleAdminStatus GET /admin/status → 运行状态 + 脱敏配置 + 最近记录。
func handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	success, fail := genLog.Counters()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        version,
		"started_at":     startTime.Format("2006-01-02 15:04:05"),
		"uptime_seconds": int(time.Since(startTime).Seconds()),
		"listen":         settingsListen(),
		"settings":       settingsView(),
		"counters": map[string]any{
			"success": success,
			"fail":    fail,
			"total":   success + fail,
		},
		"recent": genLog.Snapshot(10),
	})
}

// handleAdminTest POST /admin/test → 实调一次上游生图，返回耗时/链路/预览图。
//
// body 可选覆盖（不落盘、不影响线上配置）：{url, key, model, size, prompt}
func handleAdminTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		body = map[string]any{}
	}
	tgt := targetFromSettings()
	if s := strings.TrimSpace(toStr(body["url"])); s != "" {
		tgt.URL = s
	}
	if s := strings.TrimSpace(toStr(body["key"])); s != "" {
		tgt.Key = s
	}
	if s := strings.TrimSpace(toStr(body["model"])); s != "" {
		tgt.Model = s
	}
	size := settingsDefaultSize()
	if s, ok := normalizeSizeStr(toStr(body["size"])); ok {
		size = s
	}
	prompt := strings.TrimSpace(toStr(body["prompt"]))
	if prompt == "" {
		prompt = "一只红色的苹果放在木桌上，柔和光线，静物摄影，测试图"
	}

	start := time.Now()
	res, err := generateImage(r.Context(), tgt, prompt, "", size, settingsChatFallback())
	latency := time.Since(start).Milliseconds()

	rec := GenRecord{
		Kind: "test", Endpoint: "/admin/test", Model: tgt.Model,
		Prompt: truncate(prompt, 80), Size: size, LatencyMs: latency,
	}
	if err != nil {
		rec.Status = http.StatusBadGateway
		rec.Error = truncate(err.Error(), 300)
		genLog.Add(rec)
		logf("[Test] 测试连接失败（%dms）：%v", latency, err)
		writeAdminErr(w, http.StatusBadGateway, truncate(err.Error(), 500))
		return
	}
	rec.OK = true
	rec.Status = http.StatusOK
	rec.Via = res.Via
	genLog.Add(rec)
	logf("[Test] 测试连接成功（%dms via %s）：%s %s %dKB", latency, res.Via, size, res.Ext, len(res.Data)/1024)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"latency_ms": latency,
		"model":      tgt.Model,
		"size":       size,
		"via":        res.Via,
		"ext":        res.Ext,
		"bytes":      len(res.Data),
		"preview":    "data:" + mimeForExt(res.Ext) + ";base64," + base64.StdEncoding.EncodeToString(res.Data),
		"message":    "生成成功：耗时 " + strconv.FormatInt(latency, 10) + "ms，链路 " + res.Via,
	})
}

// handleAdminTranslate POST /admin/translate → 角色转译 + 直接出图。
//
// 这个功能的目的是「拿一句话描述直接出图」，转译出来的提示词只是中间产物（同时展示出来供参考/复制）。
//
//	body: {
//	  text     string  描述（必填；若给了 prompt 则跳过转译，直接用 prompt 出图）
//	  prompt   string  可选：直接指定总提示词（面板「再出一张」用，避免重新转译导致画风漂移）
//	  negative string  可选：直接指定负面词
//	  generate bool    可选：转译后立刻出图
//	  style    bool    可选：出图时套用面板「画风预设」
//	  size     string  可选："宽x高"，覆盖转译推荐的尺寸
//	}
//
// 返回 data 为转译结果；generate=true 时额外带 preview/via/ext/bytes/latency_ms/used_size；
// 若转译成功但出图失败，仍是 200，错误放在 image_error 里（避免抹掉已经拿到的提示词）。
func handleAdminTranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	var req struct {
		Text     string `json:"text"`
		Prompt   string `json:"prompt"`
		Negative string `json:"negative"`
		Generate bool   `json:"generate"`
		Style    bool   `json:"style"`
		Size     string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}

	timeout := 200 * time.Second
	if req.Generate {
		timeout = 320 * time.Second // 转译 + 出图（聊天链路单张可能 30~80s）
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var res *translateResult
	if p := strings.TrimSpace(req.Prompt); p != "" {
		// 「再出一张」：沿用上一次的提示词，不再走转译
		neg := strings.TrimSpace(req.Negative)
		if neg == "" {
			neg = mergeNegative(translateDefaultNegative)
		}
		dw, dh := defaultSizeWH()
		res = &translateResult{Prompt: p, NegativePrompt: neg, Width: dw, Height: dh, Steps: 28, CfgScale: 7.0}
	} else {
		var err error
		res, err = translateCharacter(ctx, targetFromSettings(), req.Text)
		if err != nil {
			logf("[Translate] 转译失败：%v", err)
			writeAdminErr(w, http.StatusBadGateway, truncate(err.Error(), 300))
			return
		}
		logf("[Translate] 转译成功：%s → 总提示词 %d 字",
			truncate(strings.TrimSpace(req.Text), 40), len([]rune(res.Prompt)))
	}

	out := map[string]any{"success": true, "data": res}
	if !req.Generate {
		writeJSON(w, http.StatusOK, out)
		return
	}

	tgt := targetFromSettings()
	prompt := strings.TrimSpace(res.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(res.CharacterPrompt + "，" + res.MainPrompt)
	}
	neg := res.NegativePrompt
	if req.Style {
		prompt, neg = applyStyle(prompt, neg)
	}
	size := fmt.Sprintf("%dx%d", res.Width, res.Height)
	if s, ok := normalizeSizeStr(req.Size); ok {
		size = s
	}

	start := time.Now()
	img, gerr := generateImage(ctx, tgt, prompt, neg, size, settingsChatFallback())
	latency := time.Since(start).Milliseconds()
	rec := GenRecord{
		Kind: "translate", Endpoint: "/admin/translate", Model: tgt.Model,
		Prompt: truncate(prompt, 80), Size: size, LatencyMs: latency,
	}
	out["latency_ms"] = latency
	out["used_size"] = size
	out["used_prompt"] = prompt
	if gerr != nil {
		rec.Status = http.StatusBadGateway
		rec.Error = truncate(gerr.Error(), 300)
		genLog.Add(rec)
		logf("[Translate] 出图失败（%dms）：%v", latency, gerr)
		out["image_error"] = truncate(gerr.Error(), 500)
		writeJSON(w, http.StatusOK, out)
		return
	}
	rec.OK = true
	rec.Status = http.StatusOK
	rec.Via = img.Via
	genLog.Add(rec)
	logf("[Translate] 出图成功（%dms via %s）：%s %s %dKB",
		latency, img.Via, size, img.Ext, len(img.Data)/1024)

	out["preview"] = "data:" + mimeForExt(img.Ext) + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
	out["via"] = img.Via
	out["ext"] = img.Ext
	out["bytes"] = len(img.Data)
	out["used_negative"] = neg
	writeJSON(w, http.StatusOK, out)
}

// handleAdminStyles GET /admin/styles → 画风预设列表 + 当前选择（面板「画风预设」页用）。
func handleAdminStyles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminErr(w, http.StatusMethodNotAllowed, "方法不支持")
		return
	}
	list := make([]stylePreset, 0, len(styleOrder))
	for _, id := range styleOrder {
		if p, ok := stylePresets[id]; ok {
			list = append(list, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"presets": list,
		"current": map[string]any{
			"style":        settingsStyle(),
			"style_custom": settingsStyleCustom(),
		},
	})
}

// handleAdminLogs GET /admin/logs（?limit=N） / DELETE /admin/logs（清空历史）。
func handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := 100
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 {
				limit = n
			}
		}
		success, fail := genLog.Counters()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"logs":    genLog.Snapshot(limit),
			"cap":     genLogCap,
			"counters": map[string]any{
				"success": success, "fail": fail, "total": success + fail,
			},
		})

	case http.MethodDelete:
		genLog.Clear()
		writeJSON(w, http.StatusOK, map[string]any{"success": true})

	default:
		writeAdminErr(w, http.StatusMethodNotAllowed, "方法不支持")
	}
}
