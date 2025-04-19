package tools

import (
	"fmt"
	"strings"
)

// 控制台打印进度条
func ProgressBar(current, total int) {
	const barLength = 50
	percent := float32(current) / float32(total) * 100
	complete := int(percent / 100 * barLength)
	remaining := barLength - complete

	fmt.Printf("\r[%s%s] %.2f%%", strings.Repeat("=", complete), strings.Repeat(" ", remaining), percent)
}
