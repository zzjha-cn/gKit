package wsx

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// DefaultEventKey 是默认的事件路由字段名。
const DefaultEventKey = "event"

// ErrWaitClosed 表示等待被 Cancel 或所属连接在等待期间终止。
var ErrWaitClosed = errors.New("wsx: connection closed while waiting")

// Router 按 JSON 字段把消息分发到事件处理函数，并支持请求-响应关联。
//
// 一个 Router 服务一条连接：它持有该连接上的等待者，连接终止时统一唤醒。
// 需要多条连接时每条连接建一个 Router。
type Router struct {
	// EventKey 路由字段名，空值等价于 DefaultEventKey。
	EventKey string

	mu       sync.Mutex
	handlers map[string]func(c *Conn, data []byte)
	waiters  map[*Waiter]struct{}
	dead     bool
}

func (r *Router) eventKey() string {
	if r.EventKey == "" {
		return DefaultEventKey
	}
	return r.EventKey
}

// On 注册事件处理函数，重复注册同一事件会覆盖。
func (r *Router) On(event string, fn func(c *Conn, data []byte)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = make(map[string]func(*Conn, []byte))
	}
	r.handlers[event] = fn
}

// Off 注销事件处理函数。
func (r *Router) Off(events ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range events {
		delete(r.handlers, e)
	}
}

// Waiter 是一次「等待首个匹配响应」的注册。
type Waiter struct {
	r      *Router
	events map[string]struct{}
	ch     chan waitResult
	dead   chan struct{}
	once   sync.Once
}

type waitResult struct {
	event string
	data  []byte
}

// Expect 注册对若干事件的等待，必须在发出请求**之前**调用。
//
// 两段式（先 Expect 再发请求再 Wait）是为了消除一个真实的竞态：
// 响应可能在你开始等之前就已经到达。单函数式的 Await 会诱导人写成
// 「先发再等」，中间存在丢消息的窗口。
//
//	w := router.Expect("task-started", "task-failed")
//	defer w.Cancel()
//	conn.SendJSON(runTask)
//	name, data, err := w.Wait(ctx)
func (r *Router) Expect(events ...string) *Waiter {
	w := &Waiter{
		r:      r,
		events: make(map[string]struct{}, len(events)),
		ch:     make(chan waitResult, 1),
		dead:   make(chan struct{}),
	}
	for _, e := range events {
		w.events[e] = struct{}{}
	}

	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		close(w.dead)
		return w
	}
	if r.waiters == nil {
		r.waiters = make(map[*Waiter]struct{})
	}
	r.waiters[w] = struct{}{}
	r.mu.Unlock()
	return w
}

// Wait 阻塞到命中任一注册事件（返回后自动注销）、ctx 结束，或连接终止。
//
// 连接终止必须立刻返回：等待者若只监听自己的 ctx，连接断开后会继续挂到超时，
// 资源迟迟不释放，日志上还会看到一堆莫名其妙的超时错误。
func (w *Waiter) Wait(ctx context.Context) (string, []byte, error) {
	select {
	case res := <-w.ch:
		w.detach()
		return res.event, res.data, nil
	case <-w.dead:
		w.detach()
		// dead 既可能来自连接终止，也可能来自 Cancel；缓冲里可能还有已投递的结果。
		select {
		case res := <-w.ch:
			return res.event, res.data, nil
		default:
		}
		return "", nil, ErrWaitClosed
	case <-ctx.Done():
		w.detach()
		return "", nil, ctx.Err()
	}
}

// Cancel 注销等待，可重复调用。defer w.Cancel() 是推荐用法。
func (w *Waiter) Cancel() {
	w.kill()
	w.detach()
}

func (w *Waiter) kill() { w.once.Do(func() { close(w.dead) }) }

func (w *Waiter) detach() {
	w.r.mu.Lock()
	delete(w.r.waiters, w)
	w.r.mu.Unlock()
}

// Handler 把 Router 适配成 Serve 的 Handler，未命中的消息交给 fallback（可为 nil）。
//
// 非 JSON 消息、缺少事件字段的消息、二进制帧都会走 fallback。
func (r *Router) Handler(fallback func(c *Conn, messageType int, data []byte)) Handler {
	return Callbacks{
		Open: func(c *Conn) {
			// 连接收敛即唤醒全部等待者，无论 Serve 是否走到 OnDisconnect。
			go func() {
				<-c.Done()
				r.wakeAll()
			}()
		},
		Message: func(c *Conn, mt int, data []byte) {
			event, ok := r.parseEvent(data)
			if !ok {
				if fallback != nil {
					fallback(c, mt, data)
				}
				return
			}
			if r.deliver(c, event, data) {
				return
			}
			if fallback != nil {
				fallback(c, mt, data)
			}
		},
		Disconnect: func(c *Conn, d Disconnect) {
			r.wakeAll()
		},
	}
}

// parseEvent 只解析路由字段，不整体反序列化业务负载。
func (r *Router) parseEvent(data []byte) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", false
	}
	raw, ok := fields[r.eventKey()]
	if !ok {
		return "", false
	}
	var event string
	if err := json.Unmarshal(raw, &event); err != nil {
		return "", false
	}
	return event, true
}

// deliver 先喂等待者（请求-响应优先），再走常规事件处理函数。
func (r *Router) deliver(c *Conn, event string, data []byte) bool {
	r.mu.Lock()
	var hit []*Waiter
	for w := range r.waiters {
		if _, ok := w.events[event]; ok {
			hit = append(hit, w)
			delete(r.waiters, w)
		}
	}
	fn := r.handlers[event]
	r.mu.Unlock()

	for _, w := range hit {
		select {
		case w.ch <- waitResult{event: event, data: data}:
		default:
		}
	}
	if len(hit) > 0 {
		return true
	}
	if fn != nil {
		fn(c, data)
		return true
	}
	return false
}

// wakeAll 唤醒所有等待者，用于连接终止时的资源回收。
func (r *Router) wakeAll() {
	r.mu.Lock()
	r.dead = true
	waiters := make([]*Waiter, 0, len(r.waiters))
	for w := range r.waiters {
		waiters = append(waiters, w)
	}
	r.waiters = nil
	r.mu.Unlock()

	for _, w := range waiters {
		w.kill()
	}
}
