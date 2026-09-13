// banner.go — 启动横幅：绿色像素标题 + 配置信息框 + 系统状态行。
// 样式沿用 zai2api 的 printBanner（同一作者的既定风格）。
// 颜色只在 stdout 是真实终端时启用；重定向到文件（nohup / PM2 日志）时自动退化为纯文本，
// 制表符（─│┌┐└┘█）本身不是转义序列，日志文件里仍然可读。
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// ── ANSI 颜色（裸 ANSI；Windows 侧由 console_windows.go 启用 VT）──
const (
	ansiGreen = "\033[32m"
	ansiRed   = "\033[31m"
	ansiReset = "\033[0m"
)

// ansiEnabled 进程启动时探测一次：stdout 为真实终端才启用颜色。
// 设 VADAPTER_FORCE_ANSI=1 可强制启用（在 winpty/部分终端包装器下有用）。
var ansiEnabled = detectANSI() || os.Getenv("VADAPTER_FORCE_ANSI") == "1"

// bannerGlyphs 像素字模（ANSI Shadow 风格，统一 6 行高）。
var bannerGlyphs = map[rune][]string{
	'V': {
		"██╗   ██╗",
		"██║   ██║",
		"██║   ██║",
		"╚██╗ ██╔╝",
		" ╚████╔╝ ",
		"  ╚═══╝  ",
	},
	'A': {
		" █████╗ ",
		"██╔══██╗",
		"███████║",
		"██╔══██║",
		"██║  ██║",
		"╚═╝  ╚═╝",
	},
	'D': {
		"██████╗ ",
		"██╔══██╗",
		"██║  ██║",
		"██║  ██║",
		"██████╔╝",
		"╚═════╝ ",
	},
	'P': {
		"██████╗ ",
		"██╔══██╗",
		"██████╔╝",
		"██╔═══╝ ",
		"██║     ",
		"╚═╝     ",
	},
	'T': {
		"████████╗",
		"╚══██╔══╝",
		"   ██║   ",
		"   ██║   ",
		"   ██║   ",
		"   ╚═╝   ",
	},
	'E': {
		"███████╗",
		"██╔════╝",
		"█████╗  ",
		"██╔══╝  ",
		"███████╗",
		"╚══════╝",
	},
	'R': {
		"██████╗ ",
		"██╔══██╗",
		"██████╔╝",
		"██╔══██╗",
		"██║  ██║",
		"╚═╝  ╚═╝",
	},
}

// bannerTitle 把文本拼成 6 行像素大字（支持字母、空格与「.」）。
func bannerTitle(text string) []string {
	rows := make([]string, 6)
	sep := ""
	for _, ch := range text {
		switch ch {
		case '.':
			rows[4] = strings.TrimRight(rows[4], " ") + "."
			continue
		case ' ':
			for i := range rows {
				rows[i] += "  "
			}
			sep = ""
			continue
		}
		g, ok := bannerGlyphs[ch]
		if !ok {
			continue
		}
		for i := 0; i < 6; i++ {
			rows[i] += sep + g[i]
		}
		sep = " "
	}
	return rows
}

// dispWidth 终端显示宽度（CJK 全角按 2 列算）。
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F,
			r >= 0x2E80 && r <= 0xA4CF,
			r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF,
			r >= 0xFE30 && r <= 0xFE4F,
			r >= 0xFF00 && r <= 0xFF60,
			r >= 0xFFE0 && r <= 0xFFE6:
			w += 2
		default:
			w++
		}
	}
	return w
}

// padCell 右侧补空格，使 s 恰好占 width 列。
func padCell(s string, width int) string {
	if n := width - dispWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// printStartupBanner 打印大号像素标题 + 配置信息框 + 系统状态行。
// 数据全部来自当前运行实例的真实配置。
func printStartupBanner(listen, upstream, model, size, fallback string, keyRequired bool) {
	g, rr := "", ""
	if ansiEnabled {
		g, rr = ansiGreen, ansiReset
	}

	port := listen
	if _, p, err := net.SplitHostPort(listen); err == nil {
		port = p
	}

	title := bannerTitle("V. ADAPTER")
	titleWidth := 0
	for _, l := range title {
		if w := dispWidth(strings.TrimRight(l, " ")); w > titleWidth {
			titleWidth = w
		}
	}

	fmt.Println()
	for _, l := range title {
		fmt.Println(g + "  " + l + rr)
	}
	fmt.Println()

	keyLine := "关闭（客户端填任意 key 均可调用）"
	if keyRequired {
		keyLine = "开启（客户端须填与服务端一致的 nai_key）"
	}
	lines := []string{
		"  运行模式:     NovelAI 协议适配服务",
		"  管理控制台:   http://localhost:" + port,
		"  生图端点:     POST /ai/generate-image（NovelAI 协议，返回 ZIP）",
		"  上游模型:     " + model + " @ " + upstream,
		"  出图链路:     " + fallback + "（标准接口优先，失败转聊天兜底）",
		"  默认尺寸:     " + size,
		"  Key 校验:     " + keyLine,
		"  监听地址:     " + listen,
	}
	inner := 63
	if titleWidth > inner {
		inner = titleWidth // 信息框至少与标题等宽，视觉上包得住
	}
	for _, l := range lines {
		if w := dispWidth(l) + 2; w > inner {
			inner = w
		}
	}
	fmt.Println(g + "  ┌" + strings.Repeat("─", inner) + "┐" + rr)
	for _, l := range lines {
		fmt.Println(g + "  │" + padCell(l, inner) + "│" + rr)
	}
	fmt.Println(g + "  └" + strings.Repeat("─", inner) + "┘" + rr)
	fmt.Println()

	fmt.Printf("  %s[ 系统状态 ] 已成功启动 | 开发者: VILK | 版本: %s%s\n\n", g, version, rr)
}

// logHasError 判断一行日志是否属于失败/异常，红色渲染让它从绿色横幅里跳出来。
func logHasError(line string) bool {
	for _, kw := range []string{
		"error", "Error", "ERROR", "failed", "Failed", "failure",
		"失败", "错误", "不可用", "超时", "拒绝", "风控", "denied", "timeout",
	} {
		if strings.Contains(line, kw) {
			return true
		}
	}
	return false
}
