package wsx

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

// UpgradeOptions 是服务端接入配置。
type UpgradeOptions struct {
	// AllowedOrigins 允许的 Origin 白名单，支持 "https://a.com" 或 "a.com"，
	// 以及前缀通配 "*.a.com"。留空表示只允许同源请求。
	AllowedOrigins []string
	// CheckOrigin 完全自定义校验逻辑，设置后 AllowedOrigins 被忽略。
	// 需要放行所有来源时显式写 func(*http.Request) bool { return true }，
	// 让「不安全」这件事出现在业务代码里而不是藏在默认值里。
	CheckOrigin func(r *http.Request) bool
	// ReadBufferSize / WriteBufferSize 收发缓冲区大小，0 使用 gorilla 默认值。
	ReadBufferSize  int
	WriteBufferSize int
	// Subprotocols 服务端支持的子协议，按顺序与客户端协商。
	Subprotocols []string
	// ResponseHeader 握手响应附加头。
	ResponseHeader http.Header
	// EnableCompression 是否协商 permessage-deflate。
	EnableCompression bool
	// ErrorWriter 自定义握手失败响应，nil 使用 gorilla 默认。
	ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, reason error)

	// Conn 是升级成功后的连接行为配置。
	Conn Options
}

// Upgrade 把 HTTP 请求升级为 WebSocket 连接并包装成 *Conn。
//
// 与 gorilla 的关键差异：**默认拒绝跨域**。gorilla 默认放行所有 Origin，
// 是个常见的安全脚印，封装层把默认值扳回安全的一侧，跨域来源必须显式白名单。
//
// 注意：升级成功后请求已被 Hijack，不要把 r.Context() 传给 Serve（见 Serve 文档）。
func Upgrade(w http.ResponseWriter, r *http.Request, opt UpgradeOptions) (*Conn, error) {
	check := opt.CheckOrigin
	if check == nil {
		check = func(r *http.Request) bool { return checkOrigin(r, opt.AllowedOrigins) }
	}

	u := &websocket.Upgrader{
		ReadBufferSize:    opt.ReadBufferSize,
		WriteBufferSize:   opt.WriteBufferSize,
		Subprotocols:      opt.Subprotocols,
		CheckOrigin:       check,
		EnableCompression: opt.EnableCompression,
		Error:             opt.ErrorWriter,
	}

	raw, err := u.Upgrade(w, r, opt.ResponseHeader)
	if err != nil {
		// gorilla 已经写过响应，这里只需把错误交给上层记录。
		return nil, err
	}

	c, err := Wrap(raw, opt.Conn)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return c, nil
}

// checkOrigin 默认策略：无 Origin（非浏览器客户端）放行，同源放行，其余按白名单。
func checkOrigin(r *http.Request, allowed []string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == strings.ToLower(r.Host) {
		return true
	}
	for _, a := range allowed {
		if originMatch(origin, host, a) {
			return true
		}
	}
	return false
}

func originMatch(origin, host, pattern string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	if p == "*" {
		return true
	}
	if strings.EqualFold(p, origin) {
		return true
	}
	// 允许只写 host（含端口）。
	if pu, err := url.Parse(p); err == nil && pu.Host != "" {
		p = strings.ToLower(pu.Host)
	}
	if p == host {
		return true
	}
	// 前缀通配：*.example.com 匹配 a.example.com，但不匹配 example.com。
	if suffix, ok := strings.CutPrefix(p, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return false
}
