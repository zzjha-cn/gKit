package wsx

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// DialOptions 是客户端接入配置。
//
// 刻意不与 UpgradeOptions 合并：两边关注点差别很大，
// 硬合并只会得到一个两边都有一半字段没用的结构。
type DialOptions struct {
	// Header 握手请求头（鉴权 token、UA 等）。
	Header http.Header
	// Subprotocols 期望的子协议列表。
	Subprotocols []string
	// HandshakeTimeout 握手超时，0 使用 gorilla 默认值。
	HandshakeTimeout time.Duration
	// TLSClientConfig 自定义 TLS 配置。
	TLSClientConfig *tls.Config
	// Proxy 代理选择函数，nil 表示不走代理（注意：不是 http.ProxyFromEnvironment）。
	Proxy func(*http.Request) (*url.URL, error)
	// ReadBufferSize / WriteBufferSize 收发缓冲区大小，0 使用 gorilla 默认值。
	ReadBufferSize  int
	WriteBufferSize int
	// EnableCompression 是否协商 permessage-deflate。
	EnableCompression bool

	// Conn 是握手成功后的连接行为配置。
	Conn Options
}

// HandshakeError 携带握手失败时的 HTTP 响应信息。
//
// gorilla 只给一个 ErrBadHandshake，定位鉴权失败、限流、路径错误都需要状态码和响应体，
// 所以在这里补齐，同时用 Unwrap 保留原始错误类型供 errors.Is 判断。
type HandshakeError struct {
	StatusCode int
	Status     string
	Body       string
	Err        error
}

// Error 实现 error。
func (e *HandshakeError) Error() string {
	return fmt.Sprintf("wsx: handshake failed: %v (status=%s, body=%s)", e.Err, e.Status, e.Body)
}

// Unwrap 保留原始错误，可用 errors.Is(err, websocket.ErrBadHandshake) 判断。
func (e *HandshakeError) Unwrap() error { return e.Err }

// 握手失败时读取的响应体上限，避免异常响应打爆内存。
const handshakeBodyLimit = 4 << 10

// Dial 建立客户端连接并包装成 *Conn。
//
// ctx 只作用于握手阶段；连接建立后的生命周期由 Serve(ctx, h) 的 ctx 控制。
// 握手失败返回 *HandshakeError，可 errors.Is(err, websocket.ErrBadHandshake)。
func Dial(ctx context.Context, rawURL string, opt DialOptions) (*Conn, error) {
	d := &websocket.Dialer{
		HandshakeTimeout:  opt.HandshakeTimeout,
		Subprotocols:      opt.Subprotocols,
		TLSClientConfig:   opt.TLSClientConfig,
		Proxy:             opt.Proxy,
		ReadBufferSize:    opt.ReadBufferSize,
		WriteBufferSize:   opt.WriteBufferSize,
		EnableCompression: opt.EnableCompression,
	}

	raw, resp, err := d.DialContext(ctx, rawURL, opt.Header)
	if err != nil {
		if resp == nil {
			return nil, err
		}
		he := &HandshakeError{StatusCode: resp.StatusCode, Status: resp.Status, Err: err}
		if resp.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, handshakeBodyLimit))
			_ = resp.Body.Close()
			he.Body = string(body)
		}
		return nil, he
	}

	c, err := Wrap(raw, opt.Conn)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return c, nil
}
