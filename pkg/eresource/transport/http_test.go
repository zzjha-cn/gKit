package transport

import (
	"testing"
)

// 白名单而不是黑名单：新格式默认走 attachment。
func TestIsInlineSafe(t *testing.T) {
	cases := map[string]bool{
		"image/png":                  true,
		"image/jpeg":                 true,
		"IMAGE/PNG":                  true,
		"image/webp; charset=binary": true,
		"video/mp4":                  true,
		"audio/mpeg":                 true,
		"application/pdf":            true,

		// SVG 是 image/*，但可以内嵌 <script>，是最典型的图片伪装型 XSS 载荷
		"image/svg+xml":                false,
		"image/svg":                    false,
		"image/svg+xml; charset=utf-8": false,
		"text/html":                    false,
		"text/plain":                   false,
		"application/xhtml+xml":        false,
		"application/octet-stream":     false,
		"application/javascript":       false,
		"":                             false,
	}
	for mimeType, want := range cases {
		if got := isInlineSafe(mimeType); got != want {
			t.Errorf("isInlineSafe(%q) = %t, want %t", mimeType, got, want)
		}
	}
}
