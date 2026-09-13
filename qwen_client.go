// qwen_client.go — 调用上游 OpenAI 兼容生图接口。
//
// 核心逻辑：
//  1. 优先 response_format=b64_json 取 data[0].b64_json 解码；
//  2. 没有 b64 则取 data[0].url 下载；
//  3. 带 negative_prompt 请求失败（服务端不支持该非标准字段）时自动去掉重试一次；
//  4. 返回非图片内容（风控验证码页/HTML 错误页）时给出人话错误，不把乱码当图，
//     并（auto 模式）自动转聊天接口生图兜底；
//  5. 上游拒绝尺寸时回退到配置的默认尺寸重试一次。
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// qwenTarget 一次调用所需的上游上游参数（面板热设置快照 / /admin/test 覆盖值）。
type qwenTarget struct {
	URL         string
	Key         string
	Model       string
	DefaultSize string
}

func targetFromSettings() qwenTarget {
	return qwenTarget{
		URL:         settingsQwenURL(),
		Key:         settingsQwenKey(),
		Model:       settingsQwenModel(),
		DefaultSize: settingsDefaultSize(),
	}
}

// imageHTTP 出图上游客户端：不做整体超时，超时由每请求 context 控制。
var imageHTTP = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// upstreamError 上游返回了非 2xx。
type upstreamError struct {
	StatusCode int
	Body       string
	Msg        string
}

func (e *upstreamError) Error() string { return e.Msg }

// notImageError 上游 2xx 但拿到的不是图片（风控页/HTML/错误文本），可触发聊天兜底。
type notImageError struct{ Msg string }

func (e *notImageError) Error() string { return e.Msg }

// imageResult 生成结果：图片字节 + 真实扩展名 + 走的链路。
type imageResult struct {
	Data []byte
	Ext  string
	Via  string // images / chat / images→chat兜底
}

// chatImageInstruction 聊天生图指令。
const chatImageInstruction = "请直接生成一张图片，不要输出多余文字。"

// 聊天回复里的图片链接提取（Markdown 图片 + 裸链接）。
var (
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^)\s]+)\)`)
	bareURLRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
)

// ── 对外主入口 ──

// 标准生图接口熔断（auto 模式专用）：
// 上游 /images/generations 长期故障（如 HTTP 500）时，连续失败达到阈值即进入熔断窗口，
// 窗口内请求直接走聊天接口，避免每次生图都先白等一次标准接口。
// 换上游（改 qwen_url/qwen_key/qwen_model）或标准接口恢复成功后自动解除。
var (
	imagesBrokenCount atomic.Int32
	imagesBrokenUntil atomic.Int64 // UnixNano；0 = 未熔断
)

const (
	imagesBrokenThreshold = 3
	imagesBrokenCooldown  = 30 * time.Minute
)

// resetImagesBroken 清除熔断计数与窗口（换上游/设置变更时调用）。
func resetImagesBroken() {
	imagesBrokenCount.Store(0)
	imagesBrokenUntil.Store(0)
}

// imagesBroken 是否处于熔断窗口内。
func imagesBroken() bool {
	u := imagesBrokenUntil.Load()
	return u > 0 && time.Now().UnixNano() < u
}

// markImagesBroken 记录一次标准接口故障，达标后进入熔断窗口。
func markImagesBroken() {
	if imagesBrokenCount.Add(1) >= imagesBrokenThreshold {
		imagesBrokenUntil.Store(time.Now().Add(imagesBrokenCooldown).UnixNano())
		logf("[Upstream] 标准生图接口连续失败已达 %d 次，熔断 %v，期间直接走聊天接口", imagesBrokenThreshold, imagesBrokenCooldown)
	}
}

// markImagesOK 标准接口恢复成功，清除熔断。
func markImagesOK() {
	imagesBrokenCount.Store(0)
	imagesBrokenUntil.Store(0)
}

// generateImage 按模式（auto/off/chat_only）生成一张图。
// auto：先走标准 /images/generations；拿到非图片内容（风控页）或 5xx 时转聊天接口兜底；
//
//	标准接口连续故障时自动熔断（直接走聊天），换上游后自动解除熔断。
func generateImage(ctx context.Context, tgt qwenTarget, prompt, neg, size, mode string) (*imageResult, error) {
	mode = normalizeChatFallback(mode)
	if mode == "chat_only" {
		return generateViaChat(ctx, tgt, prompt, neg)
	}
	if mode == "openai" {
		// openai 模式：纯标准 OpenAI 图生接口（/images/generations），
		// 面向各类 OpenAI 兼容 API（官方/第三方），不做反代聊天兜底；失败即报错。
		res, err := generateViaImages(ctx, tgt, prompt, neg, size)
		if err == nil {
			markImagesOK()
			return res, nil
		}
		return nil, err
	}
	if mode == "auto" && imagesBroken() {
		logf("[Upstream] 标准生图接口处于熔断窗口，直接走聊天接口")
		res, err := generateViaChat(ctx, tgt, prompt, neg)
		if err == nil {
			res.Via = "chat（标准接口熔断）"
		}
		return res, err
	}
	res, err := generateViaImages(ctx, tgt, prompt, neg, size)
	if err == nil {
		markImagesOK()
		return res, nil
	}
	if mode == "auto" && shouldTryChat(err) {
		var ue *upstreamError
		if errors.As(err, &ue) && ue.StatusCode >= 500 {
			markImagesBroken()
		}
		logf("[Upstream] 标准生图未拿到图片（%v），转聊天接口兜底 …", err)
		res2, err2 := generateViaChat(ctx, tgt, prompt, neg)
		if err2 == nil {
			res2.Via = "images→chat兜底"
			return res2, nil
		}
		return nil, fmt.Errorf("%v；聊天生图兜底也失败：%v", err, err2)
	}
	return nil, err
}

// generateViaImages 标准生图接口，按「尺寸回退 + negative_prompt 降级」组合尝试。
func generateViaImages(ctx context.Context, tgt qwenTarget, prompt, neg, size string) (*imageResult, error) {
	if size == "" {
		size = tgt.DefaultSize
	}
	type attempt struct{ size, neg string }
	queue := []attempt{{size: size, neg: neg}}
	tried := map[string]bool{}
	var lastErr error
	// 上限 6 次（去重后实际最多 4 种组合：原尺寸/默认尺寸 × 带/不带 negative_prompt）
	for i := 0; i < len(queue) && i < 6; i++ {
		a := queue[i]
		key := a.size + "|" + a.neg
		if tried[key] {
			continue
		}
		tried[key] = true
		res, err := imagesOnce(ctx, tgt, prompt, a.neg, a.size)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var ue *upstreamError
		if errors.As(err, &ue) {
			// 尺寸不被接受：回退到配置的默认尺寸再试
			if sizeRelated(ue.Body) && a.size != tgt.DefaultSize && tgt.DefaultSize != "" {
				logf("[Upstream] 尺寸 %s 被上游拒绝，回退默认尺寸 %s 重试", a.size, tgt.DefaultSize)
				queue = append(queue, attempt{size: tgt.DefaultSize, neg: a.neg})
			}
			// negative_prompt 非标准字段：失败即去掉重试
			if a.neg != "" {
				logf("[Upstream] 带 negative_prompt 请求失败（HTTP %d），去掉该字段重试", ue.StatusCode)
				queue = append(queue, attempt{size: a.size, neg: ""})
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("上游生图请求失败")
	}
	return nil, lastErr
}

// imagesOnce 单次标准生图请求（一次尝试）。
func imagesOnce(ctx context.Context, tgt qwenTarget, prompt, neg, size string) (*imageResult, error) {
	body := map[string]any{
		"model":           tgt.Model,
		"prompt":          prompt,
		"size":            size,
		"n":               1,
		"response_format": "b64_json",
	}
	if neg != "" {
		body["negative_prompt"] = neg
	}
	raw, status, err := postJSON(ctx, tgt, "/images/generations", body, 300*time.Second)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &upstreamError{
			StatusCode: status,
			Body:       string(raw),
			Msg:        fmt.Sprintf("上游生图接口返回 HTTP %d：%s", status, snippet(raw, 300)),
		}
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &notImageError{Msg: "上游生图接口响应不是合法 JSON（片段：" + snippet(raw, 150) + "）"}
	}
	if len(resp.Data) == 0 {
		return nil, &notImageError{Msg: "上游生图接口响应里没有 data[0]（片段：" + snippet(raw, 150) + "）"}
	}
	item := resp.Data[0]
	if s := strings.TrimSpace(item.B64JSON); s != "" {
		b64 := s
		if strings.HasPrefix(b64, "data:") {
			if i := strings.Index(b64, ","); i >= 0 {
				b64 = b64[i+1:]
			}
		}
		if data, derr := base64.StdEncoding.DecodeString(b64); derr == nil {
			return decodeImageBytes(data, "images")
		}
		if data, derr := base64.RawStdEncoding.DecodeString(b64); derr == nil {
			return decodeImageBytes(data, "images")
		}
		return nil, &notImageError{Msg: "上游生图接口返回的 b64_json 无法 base64 解码"}
	}
	if u := strings.TrimSpace(item.URL); u != "" {
		data, derr := downloadImage(ctx, u)
		if derr != nil {
			return nil, derr
		}
		return decodeImageBytes(data, "images")
	}
	return nil, &notImageError{Msg: "API 响应中既没有 b64_json 也没有 url，请检查兼容服务"}
}

// generateViaChat 聊天接口生图兜底。
func generateViaChat(ctx context.Context, tgt qwenTarget, prompt, neg string) (*imageResult, error) {
	instruction := chatImageInstruction
	if neg != "" {
		instruction += "\n请务必避免以下元素：" + neg
	}
	messages := []map[string]any{{"role": "user", "content": instruction + "\n" + prompt}}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			logf("[Upstream] 聊天接口瞬时故障，6s 后重试 …")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(6 * time.Second):
			}
		}
		body := map[string]any{"model": tgt.Model, "messages": messages, "max_tokens": 2000}
		raw, status, err := postJSON(ctx, tgt, "/chat/completions", body, 320*time.Second)
		if err != nil {
			lastErr = err
			if transientErr(err) {
				continue
			}
			return nil, err
		}
		if status < 200 || status >= 300 {
			ue := &upstreamError{
				StatusCode: status,
				Body:       string(raw),
				Msg:        fmt.Sprintf("上游聊天接口返回 HTTP %d：%s", status, snippet(raw, 300)),
			}
			lastErr = ue
			if transientErr(ue) {
				continue
			}
			return nil, ue
		}
		var cr struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
			return nil, &notImageError{Msg: "上游聊天接口响应缺少 choices（片段：" + snippet(raw, 150) + "）"}
		}
		content := cr.Choices[0].Message.Content
		urls, seenPunish := extractImageURLs(content)
		if len(urls) == 0 {
			return nil, &notImageError{Msg: noImageURLDiag(content, seenPunish)}
		}
		for _, u := range urls {
			data, derr := downloadImage(ctx, u)
			if derr != nil {
				lastErr = derr
				continue
			}
			res, rerr := decodeImageBytes(data, "chat")
			if rerr == nil {
				// 聊天链路来自上游官网的图带右下角 "Qwen" 角标，统一抹除
				res.Data = removeWatermark(res.Data, res.Ext)
				return res, nil
			}
			lastErr = rerr
		}
		return nil, &notImageError{Msg: "聊天生图返回的图片链接均无法下载或解析"}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("聊天生图请求失败")
	}
	return nil, lastErr
}

// ── HTTP 基础 ──

// postJSON 向 tgt.URL+path POST 一段 JSON，返回响应体与状态码。
func postJSON(ctx context.Context, tgt qwenTarget, path string, body map[string]any, timeout time.Duration) ([]byte, int, error) {
	base := strings.TrimRight(strings.TrimSpace(tgt.URL), "/")
	if base == "" {
		return nil, 0, fmt.Errorf("上游 API 地址未配置：请在管理面板「设置中心」填写 qwen_url")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("组装请求失败: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("请求地址无效（%s%s）: %w", base, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	key := strings.TrimSpace(tgt.Key)
	if key == "" {
		key = "EMPTY" // 本地免鉴权服务用占位符
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := imageHTTP.Do(req)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, 0, fmt.Errorf("上游接口超时（%s 未在 %s 内响应）", base+path, timeout)
		}
		return nil, 0, fmt.Errorf("请求上游接口失败（%s）: %w", base+path, err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取上游响应失败: %w", rerr)
	}
	return raw, resp.StatusCode, nil
}

// downloadImage 下载 data[0].url 或聊天回复里的图片链接（不带上游鉴权头，
// 链接可能指向第三方 CDN，避免泄漏 key）。
func downloadImage(ctx context.Context, url string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("图片链接无效: %w", err)
	}
	resp, err := imageHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载生成图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("下载生成图片失败：HTTP %d（%s）", resp.StatusCode, snippet(b, 120))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("下载生成图片失败: %w", err)
	}
	return data, nil
}

// ── 图片识别与诊断 ──

// sniffImage 按魔数识别图片格式，返回扩展名（png/jpg/webp/gif/bmp）。
func sniffImage(b []byte) (string, bool) {
	if len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return "png", true
	}
	if len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF {
		return "jpg", true
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "webp", true
	}
	if len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a") {
		return "gif", true
	}
	if len(b) >= 2 && b[0] == 'B' && b[1] == 'M' {
		return "bmp", true
	}
	return "", false
}

// decodeImageBytes 校验字节确实是图片；不是则给出人话诊断。
func decodeImageBytes(raw []byte, via string) (*imageResult, error) {
	ext, ok := sniffImage(raw)
	if !ok {
		return nil, &notImageError{Msg: nonImageDiag(raw)}
	}
	return &imageResult{Data: raw, Ext: ext, Via: via}, nil
}

// nonImageDiag 非图片内容的诊断文案（措辞面向用户，说人话）。
func nonImageDiag(raw []byte) string {
	head := bytes.TrimLeft(raw, "\r\n \t")
	low := bytes.ToLower(head)
	if bytes.HasPrefix(low, []byte("<!doctype")) || bytes.HasPrefix(low, []byte("<html")) ||
		(len(low) > 0 && low[0] == '<') {
		return "API 返回的不是图片，而是一个网页/HTML（常见原因：中转服务的风控验证码页、" +
			"登录失效页或 502 错误页，请检查该 API 服务本身能否生图）"
	}
	if bytes.Contains(low, []byte("error")) || bytes.Contains(low, []byte("exception")) {
		return fmt.Sprintf("API 返回的不是图片，疑似错误信息：%q", snippet(raw, 200))
	}
	return "API 返回的数据无法解析为图片（内容类型异常，请检查 API 服务）"
}

// extractImageURLs 从聊天回复里提取图片链接，并识别风控 punish 页。
func extractImageURLs(content string) ([]string, bool) {
	raw := mdImageRe.FindAllStringSubmatch(content, -1)
	var all []string
	for _, m := range raw {
		all = append(all, m[1])
	}
	all = append(all, bareURLRe.FindAllString(content, -1)...)

	var urls []string
	seen := map[string]bool{}
	seenPunish := false
	for _, u := range all {
		if seen[u] {
			continue
		}
		seen[u] = true
		lu := strings.ToLower(u)
		if strings.Contains(lu, "punish") || strings.Contains(lu, "captcha") {
			seenPunish = true
			continue
		}
		urls = append(urls, u)
	}
	return urls, seenPunish
}

// noImageURLDiag 聊天拿不到图片链接时的统一诊断。
func noImageURLDiag(content string, seenPunish bool) string {
	if seenPunish {
		return "上游被上游风控拦截（返回 punish 验证页），生图未生成图片。" +
			"风控通常几分钟后自动解除：请稍等再试、放慢连续生图节奏，或换个描述词再试"
	}
	sn := truncate(strings.TrimSpace(content), 150)
	if sn == "" {
		sn = "（模型未返回任何图片链接）"
	}
	for _, m := range []string{"无法生成", "无法直接生成", "不能生成", "无法创建",
		"内容政策", "安全规范", "不适宜", "色情", "裸露", "违反"} {
		if strings.Contains(content, m) {
			return "上游生图模型基于其内容安全策略拒绝了本次请求，模型回复：" + sn +
				"。这是上游服务的策略而非本地工具故障；可调整描述规避敏感元素后再试"
		}
	}
	return "聊天生图未返回图片链接，模型回复：" + sn
}

// ── 判定小工具 ──

// shouldTryChat 标准生图失败后是否值得转聊天兜底：
//   - 拿到非图片内容（风控验证码页/HTML）→ 转；
//   - 上游明确回风控/验证码 → 转；
//   - 上游接口报错（5xx/404/400/429 等）→ 转。实测这个上游反代的
//     /images/generations 常年 500（"Cannot access 'upstreamStream' before
//     initialization"），但 /chat/completions 能正常出图，聊天兜底是唯一可用链路；
//   - 鉴权/订阅类错误（401/402/403）不转：聊天接口同样会失败，只会拖慢报错。
func shouldTryChat(err error) bool {
	var nie *notImageError
	if errors.As(err, &nie) {
		return true
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		switch ue.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
			return false
		}
		low := strings.ToLower(ue.Body)
		if strings.Contains(low, "punish") || strings.Contains(low, "captcha") ||
			strings.Contains(ue.Body, "验证码") || strings.Contains(ue.Body, "风控") {
			return true
		}
		return true
	}
	return false
}

// transientErr 瞬时故障判定。
func transientErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "502") || strings.Contains(msg, "429") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "超时") ||
		strings.Contains(msg, "upstream")
}

// sizeRelated 上游错误是否与尺寸有关（用于回退默认尺寸重试）。
func sizeRelated(body string) bool {
	low := strings.ToLower(body)
	for _, k := range []string{"size", "resolution", "width", "height", "尺寸", "分辨率"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// snippet 压缩空白并截断，用于错误信息里带一小段上游响应。
func snippet(raw []byte, n int) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if s == "" {
		return "（空响应）"
	}
	return truncate(s, n)
}

// mimeForExt 图片扩展名 → MIME（面板测试预览用）。
func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}
