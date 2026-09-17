// Package asr_api 是 ASR 网关的接入适配层。
//
// 分工：pkg/asr/asr_srv 只做「网桥与中转」——一条前端连接对应一路上游会话，
// 以及这条桥的生命周期。本包负责「怎么被调用」：鉴权、请求标识、名额与升级的
// 先后顺序、协议入口。后续要挂 gRPC 或别的接入方式，在本包再加一个适配器即可，
// 中转层不用动。
package asr_api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/google/uuid"
	asrsrv "github.com/zzjha-cn/gKit/pkg/asr/asr_srv"
	"github.com/zzjha-cn/gKit/pkg/logger"
	"github.com/zzjha-cn/gKit/pkg/transport/wsx"
)

// 请求标识可能来自上游网关的这几个头，取到哪个用哪个。
var requestIDHeaders = []string{"X-Request-Id", "X-Trace-Id", "X-Correlation-Id"}

// WSHandler 把 asrsrv.Manager 适配成 http.Handler。
type WSHandler struct {
	mgr *asrsrv.Manager
	log logger.Logger
}

var _ http.Handler = (*WSHandler)(nil)

// NewWSHandler 绑定一个中转层实例。mgr 为 nil（ASR 未配置）时接口一律返回 503。
func NewWSHandler(mgr *asrsrv.Manager) *WSHandler {
	return &WSHandler{mgr: mgr, log: logger.With("mod", "asr.api")}
}

// StreamV1 是挂在全局 Manager 上的现成入口，签名即 http.HandlerFunc。
//
//	mux.HandleFunc("/asr/v1/stream", asr_api.StreamV1)
func StreamV1(w http.ResponseWriter, r *http.Request) {
	NewWSHandler(asrsrv.GetManager()).ServeHTTP(w, r)
}

// ServeHTTP 处理一次前端接入：鉴权 -> 名额 -> 升级 -> 交给中转层。
//
// 顺序是刻意的：鉴权和名额都在升级**之前**做完，失败直接回 HTTP 状态码。
// 升级成功后再关连接，前端会先看到连接成功再看到断开，比拿到 401/503 难排查得多。
func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.mgr == nil {
		http.Error(w, "asr 未启用", http.StatusServiceUnavailable)
		return
	}
	cfg := h.mgr.Config()
	reqID := requestID(r)

	if !checkToken(cfg.Token, r) {
		h.log.Warnw("asr 握手 token 校验失败", logger.LogField{
			"request_id": reqID, "remote": r.RemoteAddr,
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	release, ok := h.mgr.Acquire()
	if !ok {
		h.log.Warnw("asr 并发已满，拒绝接入", logger.LogField{
			"request_id":     reqID,
			"max_concurrent": cfg.MaxConcurrent,
			"active":         h.mgr.ActiveCount(),
		})
		http.Error(w, "too many sessions", http.StatusServiceUnavailable)
		return
	}

	conn, err := wsx.Upgrade(w, r, wsx.UpgradeOptions{
		AllowedOrigins:  cfg.AllowOrigins,
		ReadBufferSize:  8 * 1024,
		WriteBufferSize: 8 * 1024,
		Conn:            h.mgr.ConnOptions(),
	})
	if err != nil {
		// gorilla 已经写过响应，这里只需记录并把名额还回去。
		release()
		h.log.Warnw("asr 升级 WebSocket 失败", logger.LogField{"err": err, "request_id": reqID})
		return
	}
	// 名额等连接真正收敛才释放，否则会出现短暂的超发窗口。
	h.mgr.Track(conn, release)

	// 阻塞直到连接结束。
	//
	// 注意不要把 r.Context() 递进去：升级之后请求已被 Hijack，请求 ctx 在语义上
	// 已不适用，但仍会按 HTTP 框架的超时被取消，结果是长连接固定几秒后无故关闭。
	// 会话的 ctx 由中转层从进程级 ctx 派生。
	asrsrv.NewBridge(h.mgr, conn, reqID).Run()
}

// checkToken 校验前端握手 token，配置为空表示不校验。
//
// 支持 ?token= 与 Authorization: Bearer —— 浏览器的 WebSocket API 不能自定义
// 请求头，所以 query 这条路必须留着。用常量时间比较避免时序侧信道。
func checkToken(want string, r *http.Request) bool {
	if want == "" {
		return true
	}
	got := r.URL.Query().Get("token")
	if got == "" {
		got = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// requestID 复用上游网关传下来的请求标识，没有就自己生成一个。
// 它会贯穿网关与上游 ASR 的全部日志。
func requestID(r *http.Request) string {
	for _, h := range requestIDHeaders {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return uuid.NewString()
}
