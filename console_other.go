//go:build !windows

// console_other.go — 非 Windows：stdout 是字符设备（终端）就开颜色。
package main

import "os"

// detectANSI 判断 stdout 是否为终端。
func detectANSI() bool {
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
