package transport

import (
	"net/http"
	"strings"
)

// boolForm 解析表单里的布尔值，口径与管理员标记头一致。
func boolForm(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func (h *HTTPHandler) formValue(r *http.Request, key string) string {
	values := postFormValues(r, key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (h *HTTPHandler) formValues(r *http.Request, key string) []string {
	values := postFormValues(r, key)
	if len(values) == 0 {
		return nil
	}
	return values
}

func postFormValues(r *http.Request, key string) []string {
	_ = r.ParseMultipartForm(uploadFormMemory)
	return r.PostForm[key]
}
