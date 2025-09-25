package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProxyConfig 代理服务器配置
type ProxyConfig struct {
	Logger   *log.Logger
	CertFile string
	KeyFile  string
	Verbose  bool
}

// Proxy 代理服务器结构
type Proxy struct {
	config   *ProxyConfig
	logger   *log.Logger
	certMgr  *CertManager
	connPool *ConnectionPool
	mu       sync.RWMutex
	closed   bool
}

// NewProxy 创建新的代理服务器实例
func NewProxy(config *ProxyConfig) (*Proxy, error) {
	if config.Logger == nil {
		config.Logger = log.New(io.Discard, "", 0)
	}

	// 创建证书管理器
	certMgr, err := NewCertManager(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("创建证书管理器失败: %w", err)
	}

	// 创建连接池
	connPool := NewConnectionPool(100, 30*time.Second)

	proxy := &Proxy{
		config:   config,
		logger:   config.Logger,
		certMgr:  certMgr,
		connPool: connPool,
	}

	return proxy, nil
}

// ServeHTTP 实现 http.Handler 接口
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		http.Error(w, "代理服务器已关闭", http.StatusServiceUnavailable)
		return
	}
	p.mu.RUnlock()

	if p.config.Verbose {
		p.logger.Printf("收到请求: %s %s", r.Method, r.URL.String())
	}

	// 根据请求方法分发处理
	switch r.Method {
	case http.MethodConnect:
		p.handleHTTPS(w, r)
	default:
		p.handleHTTP(w, r)
	}
}

// handleHTTP 处理普通HTTP请求
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// 移除代理相关的头部
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authorization")

	// 设置连接头部
	r.Header.Set("Connection", "close")

	// 确保URL是绝对路径
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	// 创建HTTP客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return p.connPool.Get(ctx, network, addr)
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不自动跟随重定向
		},
	}

	// 发送请求
	resp, err := client.Do(r)
	if err != nil {
		p.logger.Printf("HTTP请求失败: %v", err)
		http.Error(w, "请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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
		p.logger.Printf("复制响应体失败: %v", err)
	}

	if p.config.Verbose {
		p.logger.Printf("HTTP请求完成: %s %s -> %d", r.Method, r.URL.String(), resp.StatusCode)
	}
}

// handleHTTPS 处理HTTPS CONNECT请求
func (p *Proxy) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	// 获取目标地址
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	if p.config.Verbose {
		p.logger.Printf("建立HTTPS隧道到: %s", host)
	}

	// 劫持连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持连接劫持", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		p.logger.Printf("连接劫持失败: %v", err)
		http.Error(w, "连接劫持失败", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 连接到目标服务器
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverConn, err := p.connPool.Get(ctx, "tcp", host)
	if err != nil {
		p.logger.Printf("连接目标服务器失败: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer serverConn.Close()

	// 发送连接建立响应
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		p.logger.Printf("发送连接建立响应失败: %v", err)
		return
	}

	// 开始数据转发
	p.relay(clientConn, serverConn)

	if p.config.Verbose {
		p.logger.Printf("HTTPS隧道关闭: %s", host)
	}
}

// relay 在两个连接之间转发数据
func (p *Proxy) relay(conn1, conn2 net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	// 从conn1到conn2
	go func() {
		defer wg.Done()
		defer conn2.Close()
		io.Copy(conn2, conn1)
	}()

	// 从conn2到conn1
	go func() {
		defer wg.Done()
		defer conn1.Close()
		io.Copy(conn1, conn2)
	}()

	wg.Wait()
}

// Close 关闭代理服务器
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true

	if p.connPool != nil {
		p.connPool.Close()
	}

	p.logger.Println("代理服务器资源已清理")
	return nil
}
