//go:build windows

// console_windows.go — Windows 控制台 ANSI 支持。
// 旧版 conhost 默认不解析 ANSI 转义，需 SetConsoleMode 打开
// ENABLE_VIRTUAL_TERMINAL_PROCESSING；stdout 被重定向到文件/管道时
// GetConsoleMode 会失败，据此判定「不是终端」并关闭颜色。
package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandle   = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// detectANSI stdout 是控制台时启用 VT 转义并返回 true，否则 false。
func detectANSI() bool {
	const (
		stdOutputHandle                 = ^uintptr(10) // (DWORD)-11 → STD_OUTPUT_HANDLE
		enableVirtualTerminalProcessing = 0x0004
	)
	h, _, _ := procGetStdHandle.Call(stdOutputHandle)
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false // 重定向到文件/管道，不是终端
	}
	_, _, _ = procSetConsoleMode.Call(h, uintptr(mode)|enableVirtualTerminalProcessing)
	return true
}
