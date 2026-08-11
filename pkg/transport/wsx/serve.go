package wsx

import (
	"context"
	"errors"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Cause 是断开归因。
//
// 判定依据不是关闭码：对端发 1000 正常关闭帧，从传输层看是「正常」，
// 但业务约定的终结消息可能还没到。正常与否是业务语义，由上层结合协议状态判断。
type Cause int

const (
	// CauseLocal 本端主动关闭（含 ctx 取消、时间闸触发）。
	CauseLocal Cause = iota
	// CausePeer 对端发来关闭帧。只说明「对端关的」，不判断是否正常。
	CausePeer
	// CauseTimeout 读超时，链路已死。
	CauseTimeout
	// CauseProtocol 帧格式错误 / 超出 ReadLimit / 协议违规。
	CauseProtocol
	// CauseFatal 其余 I/O 错误（含写失败、心跳失败、1006 异常断开）。
	CauseFatal
)

// String 实现 fmt.Stringer，便于按 Cause 分组打点。
func (c Cause) String() string {
	switch c {
	case CauseLocal:
		return "local"
	case CausePeer:
		return "peer"
	case CauseTimeout:
		return "timeout"
	case CauseProtocol:
		return "protocol"
	case CauseFatal:
		return "fatal"
	default:
		return "unknown(" + strconv.Itoa(int(c)) + ")"
	}
}

// Disconnect 描述连接为什么结束。
//
// Serve 返回它而不是 error，是为了强迫调用方面对「为什么断的」——
// error 太容易被 if err != nil { log } 一笔带过，而不同 Cause 的处理天差地别。
type Disconnect struct {
	Cause  Cause
	Code   int // WebSocket 关闭码；没有关闭帧时为 1006
	Reason string
	Err    error // 原始错误，保留类型供上层判断
}

// Handler 是连接生命周期回调。
//
// OnMessage 运行在读循环 goroutine 上：handler 里做耗时操作会阻塞整条连接的读取。
// 要投递到别的 goroutine，同步责任在应用侧（队列长度、满了丢弃还是阻塞都是业务决策）。
type Handler interface {
	OnOpen(c *Conn)
	OnMessage(c *Conn, messageType int, data []byte)
	// OnDisconnect 保证恰好被调用一次，含 panic 路径。
	OnDisconnect(c *Conn, d Disconnect)
}

// Callbacks 是 Handler 的函数式实现，字段全部可选。
type Callbacks struct {
	Open       func(c *Conn)
	Message    func(c *Conn, messageType int, data []byte)
	Disconnect func(c *Conn, d Disconnect)
}

// OnOpen 实现 Handler。
func (h Callbacks) OnOpen(c *Conn) {
	if h.Open != nil {
		h.Open(c)
	}
}

// OnMessage 实现 Handler。
func (h Callbacks) OnMessage(c *Conn, mt int, data []byte) {
	if h.Message != nil {
		h.Message(c, mt, data)
	}
}

// OnDisconnect 实现 Handler。
func (h Callbacks) OnDisconnect(c *Conn, d Disconnect) {
	if h.Disconnect != nil {
		h.Disconnect(c, d)
	}
}

// Serve 阻塞运行直到连接结束，返回断开归因。
//
// 内部负责：心跳 ticker、pong 续期读 deadline、时间闸、读循环、逐条消息 recover。
// ctx 取消时主动优雅关闭，归因为 CauseLocal。
//
// 高危陷阱：绝不要传 HTTP 请求的 ctx。连接升级后请求已被 Hijack，
// 请求 ctx 在语义上已不适用，但仍会按 HTTP 框架的超时被取消，
// 结果是连接在固定秒数后被无故关闭。检测到带 deadline 的 ctx 会打 warn。
func (c *Conn) Serve(ctx context.Context, h Handler) Disconnect {
	if h == nil {
		h = Callbacks{}
	}
	if !c.serving.CompareAndSwap(false, true) {
		return Disconnect{Cause: CauseLocal, Code: websocket.CloseAbnormalClosure, Reason: ErrServing.Error(), Err: ErrServing}
	}
	if dl, ok := ctx.Deadline(); ok {
		c.log.Warnw("wsx: Serve 收到带 deadline 的 ctx，长连接通常不该有 deadline（是否误传了 HTTP 请求 ctx？）",
			"conn_id", c.info.ID, "deadline", dl)
	}

	stop := make(chan struct{})
	go c.supervise(ctx, stop)

	c.safe("OnOpen", func() { h.OnOpen(c) })

	d := c.readLoop(h)

	close(stop)      // 停掉心跳与时间闸
	c.markReadDone() // 让等待对端回应的 Close 立刻收尾
	c.shutdown()     // 收敛点：底层 Close 只执行一次

	c.opt.Hooks.disconnect(c.info, d)
	c.safe("OnDisconnect", func() { h.OnDisconnect(c, d) })
	return d
}

// readLoop 是唯一的读者，返回时连接必然已经结束。
func (c *Conn) readLoop(h Handler) Disconnect {
	for {
		mt, data, err := c.raw.ReadMessage()
		if err != nil {
			return c.classify(err)
		}
		c.renewReadDeadline()
		c.opt.Hooks.frame(c.info, mt, len(data), false)
		// 一个 handler panic 不能打穿整条连接。
		c.safe("OnMessage", func() { h.OnMessage(c, mt, data) })
	}
}

// supervise 承担心跳与两道时间闸，随读循环退出而结束。
func (c *Conn) supervise(ctx context.Context, stop <-chan struct{}) {
	var pingC <-chan time.Time
	if c.opt.PingInterval > 0 {
		t := time.NewTicker(c.opt.PingInterval)
		defer t.Stop()
		pingC = t.C
	}
	var graceC, lifeC <-chan time.Time
	if c.opt.ActivateGrace > 0 {
		t := time.NewTimer(c.opt.ActivateGrace)
		defer t.Stop()
		graceC = t.C
	}
	if c.opt.MaxLifetime > 0 {
		t := time.NewTimer(c.opt.MaxLifetime)
		defer t.Stop()
		lifeC = t.C
	}

	for {
		select {
		case <-stop:
			return
		case <-c.done:
			return
		case <-ctx.Done():
			_ = c.Close(websocket.CloseGoingAway, "context canceled")
			return
		case <-pingC:
			if err := c.ping(); err != nil {
				if errors.Is(err, websocket.ErrCloseSent) {
					return
				}
				// 心跳写失败是独立信号，立即判死，不用等读超时。
				c.log.Warnw("wsx: 心跳发送失败，判定连接死亡", "conn_id", c.info.ID, "error", err)
				c.failTerminal(err)
				return
			}
		case <-graceC:
			if c.activated.Load() {
				graceC = nil
				continue
			}
			c.gate(websocket.ClosePolicyViolation, "activation grace exceeded")
			return
		case <-lifeC:
			c.gate(websocket.CloseGoingAway, "max lifetime exceeded")
			return
		}
	}
}

// gate 时间闸关闭：先发一条业务可理解的消息，再关。
// 静默断开会让对端以为是网络问题，排查成本极高。
func (c *Conn) gate(code int, reason string) {
	c.log.Infow("wsx: 时间闸触发，关闭连接", "conn_id", c.info.ID, "reason", reason)
	if mt, data := c.opt.GateNotice(reason); data != nil {
		_ = c.Send(mt, data)
	}
	_ = c.Close(code, reason)
}

// classify 把读错误翻译成归因。
//
// 顺序很关键：本端关闭优先（读循环在本端关闭后必然报错），
// 其次是写/心跳失败记录的终止原因，最后才看读错误本身。
func (c *Conn) classify(readErr error) Disconnect {
	if c.ClosedByUs() {
		c.mu.Lock()
		code, reason := c.localCode, c.localReason
		c.mu.Unlock()
		if code == 0 {
			code = websocket.CloseNormalClosure
		}
		return Disconnect{Cause: CauseLocal, Code: code, Reason: reason, Err: readErr}
	}

	c.mu.Lock()
	err := c.termErr
	c.mu.Unlock()
	if err == nil {
		err = readErr
	}

	// 真正的关闭帧才算 CausePeer；1006 是 gorilla 造出来的「异常断开」，没有关闭帧。
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure && ce.Code != websocket.CloseTLSHandshake {
		return Disconnect{Cause: CausePeer, Code: ce.Code, Reason: ce.Text, Err: err}
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		return Disconnect{Cause: CauseProtocol, Code: websocket.CloseMessageTooBig, Reason: err.Error(), Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Disconnect{Cause: CauseTimeout, Code: websocket.CloseAbnormalClosure, Reason: err.Error(), Err: err}
	}
	// gorilla 的协议违规错误统一形如 "websocket: <message>"（invalid opcode、
	// unexpected reserved bits 等），和底层 I/O 错误由此区分。
	if !errors.As(err, &ce) && strings.HasPrefix(err.Error(), "websocket: ") {
		return Disconnect{Cause: CauseProtocol, Code: websocket.CloseProtocolError, Reason: err.Error(), Err: err}
	}
	return Disconnect{Cause: CauseFatal, Code: websocket.CloseAbnormalClosure, Reason: err.Error(), Err: err}
}

// safe 执行回调并拦截 panic，保证一个 handler 的错误不会打穿连接。
func (c *Conn) safe(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Errorw("wsx: handler panic",
				"conn_id", c.info.ID, "where", where, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}
