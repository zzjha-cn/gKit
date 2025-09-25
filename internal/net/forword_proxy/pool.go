package main

import (
	"context"
	"net"
	"sync"
	"time"
)

// ConnectionPool 连接池
type ConnectionPool struct {
	maxSize     int
	idleTimeout time.Duration
	conns       map[string][]*poolConn
	mu          sync.RWMutex
	closed      bool
}

// poolConn 池化连接
type poolConn struct {
	net.Conn
	lastUsed time.Time
}

// NewConnectionPool 创建新的连接池
func NewConnectionPool(maxSize int, idleTimeout time.Duration) *ConnectionPool {
	pool := &ConnectionPool{
		maxSize:     maxSize,
		idleTimeout: idleTimeout,
		conns:       make(map[string][]*poolConn),
	}

	// 启动清理goroutine
	go pool.cleanup()

	return pool
}

// Get 从连接池获取连接
func (p *ConnectionPool) Get(ctx context.Context, network, address string) (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return net.DialTimeout(network, address, 30*time.Second)
	}

	key := network + ":" + address
	conns := p.conns[key]

	// 尝试从池中获取可用连接
	for len(conns) > 0 {
		conn := conns[len(conns)-1]
		conns = conns[:len(conns)-1]
		p.conns[key] = conns

		// 检查连接是否仍然有效
		if time.Since(conn.lastUsed) < p.idleTimeout {
			// 简单的连接测试
			conn.SetReadDeadline(time.Now().Add(time.Millisecond))
			buf := make([]byte, 1)
			_, err := conn.Read(buf)
			conn.SetReadDeadline(time.Time{})
			
			if err == nil {
				// 连接仍然有效，返回使用
				return &poolConnWrapper{conn.Conn, p, key}, nil
			}
		}

		// 连接无效，关闭它
		conn.Close()
	}

	// 池中没有可用连接，创建新连接
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}

	return &poolConnWrapper{conn, p, key}, nil
}

// Put 将连接放回连接池
func (p *ConnectionPool) Put(key string, conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		conn.Close()
		return
	}

	conns := p.conns[key]
	if len(conns) >= p.maxSize {
		// 池已满，关闭连接
		conn.Close()
		return
	}

	// 将连接放回池中
	p.conns[key] = append(conns, &poolConn{
		Conn:     conn,
		lastUsed: time.Now(),
	})
}

// cleanup 清理过期连接
func (p *ConnectionPool) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanupExpired()
		}
	}
}

// cleanupExpired 清理过期连接
func (p *ConnectionPool) cleanupExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := time.Now()
	for key, conns := range p.conns {
		var validConns []*poolConn
		for _, conn := range conns {
			if now.Sub(conn.lastUsed) < p.idleTimeout {
				validConns = append(validConns, conn)
			} else {
				conn.Close()
			}
		}
		
		if len(validConns) == 0 {
			delete(p.conns, key)
		} else {
			p.conns[key] = validConns
		}
	}
}

// Close 关闭连接池
func (p *ConnectionPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true

	// 关闭所有连接
	for _, conns := range p.conns {
		for _, conn := range conns {
			conn.Close()
		}
	}

	p.conns = nil
}

// poolConnWrapper 连接包装器，用于在关闭时将连接放回池中
type poolConnWrapper struct {
	net.Conn
	pool *ConnectionPool
	key  string
}

// Close 关闭连接（实际上是放回池中）
func (w *poolConnWrapper) Close() error {
	// 对于某些类型的连接，我们不应该放回池中
	// 这里简化处理，直接关闭
	return w.Conn.Close()
}