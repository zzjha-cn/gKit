package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MITMProxy MITM代理结构
type MITMProxy struct {
	*Proxy
	enableMITM bool
}

// NewMITMProxy 创建新的MITM代理实例
func NewMITMProxy(config *ProxyConfig, enableMITM bool) (*MITMProxy, error) {
	proxy, err := NewProxy(config)
	if err != nil {
		return nil, err
	}

	return &MITMProxy{
		Proxy:      proxy,
		enableMITM: enableMITM,
	}, nil
}

// ServeHTTP 实现http.Handler接口，重写代理请求处理
func (mp *MITMProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mp.mu.RLock()
	if mp.closed {
		mp.mu.RUnlock()
		http.Error(w, "代理服务器已关闭", http.StatusServiceUnavailable)
		return
	}
	mp.mu.RUnlock()

	if mp.config.Verbose {
		mp.logger.Printf("收到请求: %s %s", r.Method, r.URL.String())
	}

	// 根据请求方法分发处理
	switch r.Method {
	case http.MethodConnect:
		mp.handleHTTPS(w, r)  // 调用MITMProxy的handleHTTPS
	default:
		mp.handleHTTP(w, r)   // 调用继承的handleHTTP
	}
}

// handleHTTPS 重写HTTPS处理，支持MITM
func (mp *MITMProxy) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	if !mp.enableMITM {
		// 如果未启用MITM，使用普通隧道代理
		mp.Proxy.handleHTTPS(w, r)
		return
	}

	// 启用MITM模式
	mp.handleMITM(w, r)
}

// handleMITM 处理MITM拦截
func (mp *MITMProxy) handleMITM(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	hostname := strings.Split(host, ":")[0]

	if mp.config.Verbose {
		mp.logger.Printf("MITM拦截HTTPS请求: %s", host)
	}

	// 劫持连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持连接劫持", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		mp.logger.Printf("连接劫持失败: %v", err)
		return
	}
	defer clientConn.Close()

	// 发送连接建立响应
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		mp.logger.Printf("发送连接建立响应失败: %v", err)
		return
	}

	// 获取服务器证书
	cert, err := mp.certMgr.GetCertificate(hostname)
	if err != nil {
		mp.logger.Printf("获取证书失败: %v", err)
		return
	}

	// 创建TLS配置
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		ServerName:   hostname,
	}

	// 将客户端连接包装为TLS连接
	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		mp.logger.Printf("TLS握手失败: %v", err)
		return
	}

	// 创建MITM监听器
	listener := &mitmListener{
		conn: tlsConn,
		done: make(chan struct{}),
	}

	// 创建HTTP处理器
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 确保URL格式正确
		r.URL.Scheme = "https"
		r.URL.Host = r.Host

		if mp.config.Verbose {
			mp.logger.Printf("MITM拦截请求: %s %s", r.Method, r.URL.String())
		}

		// 这里可以添加请求分析、修改等逻辑
		mp.analyzeRequest(r)

		// 转发请求到真实服务器
		mp.forwardMITMRequest(w, r)
	})

	// 创建HTTP服务器
	server := &http.Server{
		Handler: handler,
	}

	// 创建context用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 在goroutine中处理连接
	go func() {
		defer listener.Close()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			mp.logger.Printf("MITM服务器错误: %v", err)
		}
	}()

	// 监听连接关闭信号
	go func() {
		<-listener.done
		// 连接关闭时，优雅关闭server
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			mp.logger.Printf("MITM服务器关闭失败: %v", err)
			server.Close() // 强制关闭
		}
		cancel() // 通知主goroutine
	}()

	// 等待服务器完全关闭
	<-ctx.Done()
}

// analyzeRequest 分析请求（可以在这里添加自定义逻辑）
func (mp *MITMProxy) analyzeRequest(r *http.Request) {
	if mp.config.Verbose {
		mp.logger.Printf("分析请求: %s %s", r.Method, r.URL.String())

		// 记录请求头
		for key, values := range r.Header {
			for _, value := range values {
				mp.logger.Printf("请求头: %s: %s", key, value)
			}
		}
	}
}

// forwardMITMRequest 转发MITM请求
func (mp *MITMProxy) forwardMITMRequest(w http.ResponseWriter, r *http.Request) {
	// 创建到真实服务器的连接
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // 在生产环境中应该验证证书
			},
		},
	}

	// 发送请求
	resp, err := client.Do(r)
	if err != nil {
		mp.logger.Printf("转发请求失败: %v", err)
		http.Error(w, "请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 分析响应
	mp.analyzeResponse(resp)

	// 复制响应头
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// 设置状态码
	w.WriteHeader(resp.StatusCode)

	// 复制响应体
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		mp.logger.Printf("复制响应体失败: %v", err)
	}
}

// analyzeResponse 分析响应
func (mp *MITMProxy) analyzeResponse(resp *http.Response) {
	if mp.config.Verbose {
		mp.logger.Printf("分析响应: %d %s", resp.StatusCode, resp.Status)
		// 记录响应头
		for key, values := range resp.Header {
			for _, value := range values {
				mp.logger.Printf("响应头: %s: %s", key, value)
			}
		}
	}
}

// mitmListener MITM监听器
type mitmListener struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

// Accept 接受连接
func (l *mitmListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, io.EOF
	default:
		if l.conn != nil {
			conn := l.conn
			l.conn = nil
			return conn, nil
		}
		return nil, io.EOF
	}
}

// Close 关闭监听器
func (l *mitmListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		if l.conn != nil {
			l.conn.Close()
		}
	})
	return nil
}

// Addr 获取地址
func (l *mitmListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}
