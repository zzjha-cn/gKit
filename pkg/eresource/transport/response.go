package transport

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

func (h *HTTPHandler) ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
}

func (h *HTTPHandler) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	slog.ErrorContext(r.Context(), "eresource: HTTP request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, status, map[string]any{"code": status, "message": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Error("eresource: marshal response", "err", err)
		status = http.StatusInternalServerError
		payload = []byte(`{"code":500,"message":"internal server error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func storeStatus(err error) int {
	// fs.ErrNotExist 来自 Provider.Open：底层对象没了（已被清理、或元数据与存储不同步），
	// 与「元数据查不到」一样应当是 404 而不是 500。LocalProvider 与 AliOSSProvider
	// 都会返回该哨兵错误。
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
