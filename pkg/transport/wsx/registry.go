package wsx

import (
	"context"
	"errors"
	"sync"

	"github.com/gorilla/websocket"
)

// ErrRegistryFull 表示并发名额已满。
var ErrRegistryFull = errors.New("wsx: connection limit reached")

// Registry 是进程级连接治理组件：并发上限 + 优雅退出。
type Registry struct {
	max int

	mu       sync.Mutex
	acquired int
	conns    map[*Conn]struct{}
	closing  bool
}

// NewRegistry 创建注册表，max <= 0 表示不限并发数。
func NewRegistry(max int) *Registry {
	return &Registry{max: max, conns: make(map[*Conn]struct{})}
}

// Acquire 申请一个并发名额，占满或进程正在退出时返回 false。
//
// 必须在**升级之前**调用：满了直接返回 HTTP 503。升级成功后再关连接，
// 对端会先看到连接成功再看到断开，比直接拿到 503 难排查得多。
//
// release 幂等；升级失败时要立刻调用，成功后交给 Track 管理。
func (r *Registry) Acquire() (release func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return func() {}, false
	}
	if r.max > 0 && r.acquired >= r.max {
		return func() {}, false
	}
	r.acquired++

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.acquired--
			r.mu.Unlock()
		})
	}, true
}

// Track 登记连接，并在连接真正收敛后释放名额（release 可为 nil）。
//
// 名额必须等到连接结束才释放，否则会出现短暂的超发窗口。
func (r *Registry) Track(c *Conn, release func()) {
	r.mu.Lock()
	closing := r.closing
	r.conns[c] = struct{}{}
	r.mu.Unlock()

	go func() {
		<-c.Done()
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		if release != nil {
			release()
		}
	}()

	// 极端情况：CloseAll 与 Track 并发，补一刀，避免漏关。
	if closing {
		_ = c.Close(websocket.CloseGoingAway, "server shutting down")
	}
}

// ActiveCount 返回当前在册连接数。
func (r *Registry) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// AcquiredCount 返回已占用的名额数（含已升级但尚未收敛的连接）。
func (r *Registry) AcquiredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acquired
}

// CloseAll 广播优雅关闭并**等待**收敛，ctx 控制等待上限。
//
// 只发信号不等待是错的：进程退出时正在收尾的连接可能来不及把最后的业务消息
// 和关闭帧发出去。调用后 Acquire 一律失败。
func (r *Registry) CloseAll(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	conns := make([]*Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	for _, c := range conns {
		_ = c.Close(websocket.CloseGoingAway, "server shutting down")
	}
	for _, c := range conns {
		select {
		case <-c.Done():
			// Track 的清理 goroutine 是异步的，这里同步摘除，
			// 保证 CloseAll 返回时 ActiveCount 已经准确。
			r.mu.Lock()
			delete(r.conns, c)
			r.mu.Unlock()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
