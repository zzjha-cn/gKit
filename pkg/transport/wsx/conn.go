// Package wsx 是 github.com/gorilla/websocket 的应用层封装。
//
// 它不试图成为一个 WebSocket 框架，只把 gorilla 刻意留给应用的几个坑一次性填掉：
// 写串行化、读写超时、心跳、关闭归因、幂等收敛。
//
// 分层：
//
//	L3 接入层    Dial / Upgrade
//	L2 语义层    Router / Expect-Wait
//	L1 生命周期  Serve / Disconnect
//	L0 连接层    Conn
//
// 上层可以不用：只想要「写安全 + 有超时」的连接，用到 Conn 就停。
package wsx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 默认参数。
const (
	// DefaultCloseTimeout 是等待对端回应关闭帧的默认上限。
	DefaultCloseTimeout = 3 * time.Second
)

// 包级错误。
var (
	// ErrClosed 表示连接已经进入关闭流程，不再接受新的发送。
	ErrClosed = errors.New("wsx: connection closed")
	// ErrNilConn 表示 Wrap 收到了空连接。
	ErrNilConn = errors.New("wsx: nil websocket connection")
	// ErrPingInterval 表示心跳间隔与读超时配反了，心跳永远救不了读超时。
	ErrPingInterval = errors.New("wsx: PingInterval must be <= ReadTimeout/2")
	// ErrServing 表示同一条连接上重复调用了 Serve。
	ErrServing = errors.New("wsx: Serve already running")
)

// Options 是单连接的行为配置。
//
// 原则：策略可配，行为不可配。超时时长是配置，「写必须加锁」不是。
type Options struct {
	// WriteTimeout 单次写超时，0 表示不限。
	// 写失败即判定连接死亡，不重试（重试会让帧序错乱）。
	WriteTimeout time.Duration
	// ReadTimeout 多久没有收到任何入站帧就判定链路已死，0 表示不限。
	// 任何入站帧（数据帧、ping、pong）都会续期该 deadline。
	//
	// 配它就必须保证静默期有东西可发，二选一：开 PingInterval（协议级探活），
	// 或者协议本身有业务心跳（很多上游服务有可选的 heartbeat 参数，要主动开启）。
	// 两者都没有的话，长静默的正常连接会被误杀。
	ReadTimeout time.Duration
	// PingInterval 心跳间隔，0 表示不发。
	// 必须满足 PingInterval <= ReadTimeout/2，以容忍丢一次 pong。
	//
	// 注意：协议级 ping 只能证明「对端 WebSocket 栈还在响应」。
	// 如果协议自带业务心跳，优先只依赖它——开了 ping 反而会在对端进程假死
	// （GC 长停顿、业务线程死锁，但网络栈照样回 pong）时把连接续住，
	// 让业务级探活失效。
	PingInterval time.Duration
	// ReadLimit 单帧字节上限，0 表示不限。
	ReadLimit int64
	// CloseTimeout 主动关闭后等待对端回应关闭帧的上限，0 使用 DefaultCloseTimeout。
	CloseTimeout time.Duration

	// ActivateGrace 连接建立后多久内必须调用 MarkActive，超时关闭。
	// 防「只连不用」的空连接占用名额，0 表示不限。
	ActivateGrace time.Duration
	// MaxLifetime 单连接最长存活时间，到点强制关闭。
	// 防长连接无限期持有上游资源（含持续计费场景），0 表示不限。
	MaxLifetime time.Duration
	// GateNotice 时间闸触发时用于通知对端的消息，返回 nil 表示不通知。
	// 默认发一条 JSON 文本，让对端能区分「被服务端主动限制」和「网络故障」。
	GateNotice func(reason string) (messageType int, data []byte)

	Logger Logger
	Hooks  Hooks
}

// normalize 填默认值并校验互相矛盾的参数组合。
func (o *Options) normalize() error {
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = DefaultCloseTimeout
	}
	if o.Logger == nil {
		o.Logger = nopLogger{}
	}
	if o.GateNotice == nil {
		o.GateNotice = defaultGateNotice
	}
	// 配反了心跳就永远救不了读超时，连接会周期性莫名断开，
	// 这种错误必须在构造时炸掉，而不是等线上暴露。
	if o.ReadTimeout > 0 && o.PingInterval > 0 && o.PingInterval > o.ReadTimeout/2 {
		return fmt.Errorf("%w (PingInterval=%s, ReadTimeout=%s)", ErrPingInterval, o.PingInterval, o.ReadTimeout)
	}
	return nil
}

func defaultGateNotice(reason string) (int, []byte) {
	data, _ := json.Marshal(map[string]string{"event": "wsx.closing", "reason": reason})
	return websocket.TextMessage, data
}

var connSeq uint64

// Conn 是线程安全的 WebSocket 连接。
//
// 所有写操作经 Send 串行化并自带 deadline；关闭幂等且带归因。
// 刻意不导出 SetWriteDeadline / SetReadDeadline / WriteMessage：
// 前两者由 Options 接管，后者由 Send 取代，把「每次写前记得设 deadline，
// 并且要和写在同一把锁里」这条约定从注释变成结构保证。
type Conn struct {
	raw  *websocket.Conn
	opt  Options
	log  Logger
	info ConnInfo

	// wmu 保护所有写操作，含 SetWriteDeadline —— 二者必须在同一把锁内。
	wmu sync.Mutex

	closeOnce sync.Once     // 优雅关闭只发一次关闭帧
	shutOnce  sync.Once     // 底层 Close 只执行一次
	done      chan struct{} // 收敛信号：底层已关闭
	readDone  chan struct{} // 读循环已退出
	readOnce  sync.Once

	closedByUs atomic.Bool
	activated  atomic.Bool
	serving    atomic.Bool

	mu          sync.Mutex // 保护下面几个归因字段
	localCode   int
	localReason string
	termErr     error // 写/心跳失败等「先于读错误发现」的终止原因
}

// Wrap 包装一条已经建立的连接。
//
// 会校验 PingInterval <= ReadTimeout/2，并设置 ReadLimit、初始读 deadline
// 以及续期用的 ping/pong handler，因此不调用 Serve 也能得到正确的超时行为
// （但读 deadline 只有在有人读的时候才有意义）。
func Wrap(raw *websocket.Conn, opt Options) (*Conn, error) {
	if raw == nil {
		return nil, ErrNilConn
	}
	if err := opt.normalize(); err != nil {
		return nil, err
	}
	c := &Conn{
		raw:      raw,
		opt:      opt,
		log:      opt.Logger,
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	c.info = ConnInfo{
		ID:          "wsx-" + strconv.FormatUint(atomic.AddUint64(&connSeq, 1), 10),
		Subprotocol: raw.Subprotocol(),
		ConnectedAt: time.Now(),
	}
	if a := raw.RemoteAddr(); a != nil {
		c.info.RemoteAddr = a.String()
	}
	if a := raw.LocalAddr(); a != nil {
		c.info.LocalAddr = a.String()
	}

	if opt.ReadLimit > 0 {
		raw.SetReadLimit(opt.ReadLimit)
	}
	c.renewReadDeadline()
	// pong 与 ping 都是入站帧，都算「链路还活着」，都续期读 deadline。
	raw.SetPongHandler(func(string) error {
		c.renewReadDeadline()
		return nil
	})
	raw.SetPingHandler(func(appData string) error {
		c.renewReadDeadline()
		if err := c.writeControl(websocket.PongMessage, []byte(appData)); err != nil {
			if errors.Is(err, websocket.ErrCloseSent) {
				return nil
			}
			return err
		}
		return nil
	})

	opt.Hooks.connect(c.info)
	return c, nil
}

// Info 返回连接元信息快照。
func (c *Conn) Info() ConnInfo { return c.info }

// Done 在连接收敛（底层已关闭）后被 close。
//
// 所有等待者都必须同时监听它，否则连接断开后会继续挂到自己的 ctx 超时。
func (c *Conn) Done() <-chan struct{} { return c.done }

// IsClosed 报告连接是否已经收敛。
func (c *Conn) IsClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// ClosedByUs 报告连接是否由本端关闭。
//
// 读循环在本端关闭后必然报错，这个标志用来把它和「对端断开」区分开。
func (c *Conn) ClosedByUs() bool { return c.closedByUs.Load() }

// MarkActive 由业务在连接进入正常工作状态时调用，用于解除 ActivateGrace 闸门。
//
// 「什么算工作状态」是业务定义（收到首个指令、完成鉴权、上游就绪等），
// 封装只提供闸门，不猜语义。
func (c *Conn) MarkActive() { c.activated.Store(true) }

// Raw 是逃生舱。
//
// 使用前请确认你清楚 gorilla 的并发约束：绕过 Send 直接写会破坏写串行化。
func (c *Conn) Raw() *websocket.Conn { return c.raw }

// Send 发送一帧。内部完成「加锁 → SetWriteDeadline → 写」，全程持锁。
//
// 写失败即判定连接死亡并触发收敛，调用方不应重试。
func (c *Conn) Send(mt int, data []byte) error {
	if c.IsClosed() {
		return ErrClosed
	}
	c.wmu.Lock()
	deadline := time.Time{}
	if c.opt.WriteTimeout > 0 {
		deadline = time.Now().Add(c.opt.WriteTimeout)
	}
	if err := c.raw.SetWriteDeadline(deadline); err != nil {
		c.wmu.Unlock()
		c.failTerminal(err)
		return err
	}
	err := c.raw.WriteMessage(mt, data)
	c.wmu.Unlock()

	if err != nil {
		// 写超时是最快的死连接探测器：对端不再 ACK → 发送缓冲填满 → 写阻塞 → 超时，
		// 这比等满一个 ReadTimeout 周期快得多。
		if !errors.Is(err, websocket.ErrCloseSent) {
			c.failTerminal(err)
		}
		return err
	}
	c.opt.Hooks.frame(c.info, mt, len(data), true)
	return nil
}

// SendText 发送文本帧。
func (c *Conn) SendText(s string) error { return c.Send(websocket.TextMessage, []byte(s)) }

// SendBinary 发送二进制帧。
func (c *Conn) SendBinary(b []byte) error { return c.Send(websocket.BinaryMessage, b) }

// SendJSON 序列化后发送文本帧。
func (c *Conn) SendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Send(websocket.TextMessage, data)
}

// Close 幂等关闭：发关闭帧并标记「本端主动」，等对端回应（上限 CloseTimeout）后关闭底层连接。
//
// 任意 goroutine 任意次数调用都安全；只有首次调用会真正发出关闭帧并返回其错误。
func (c *Conn) Close(code int, reason string) error {
	c.closedByUs.Store(true)

	var (
		first bool
		err   error
	)
	c.closeOnce.Do(func() {
		first = true
		c.mu.Lock()
		c.localCode, c.localReason = code, reason
		c.mu.Unlock()

		err = c.writeControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))

		// Draining：发出关闭帧后不能立即关 TCP，要给对端回应的机会；但也不能无限等。
		go func() {
			select {
			case <-c.readDone:
			case <-c.done:
			case <-time.After(c.opt.CloseTimeout):
			}
			c.shutdown()
		}()
	})
	if !first || errors.Is(err, websocket.ErrCloseSent) {
		return nil
	}
	return err
}

// writeControl 在写锁内发送控制帧，deadline 独立于数据帧。
func (c *Conn) writeControl(mt int, data []byte) error {
	deadline := time.Now().Add(c.opt.CloseTimeout)
	if c.opt.WriteTimeout > 0 {
		deadline = time.Now().Add(c.opt.WriteTimeout)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.raw.WriteControl(mt, data, deadline)
}

// ping 发送一个心跳帧。写失败是独立信号，直接判死，不等读超时。
func (c *Conn) ping() error { return c.writeControl(websocket.PingMessage, nil) }

// renewReadDeadline 由任何入站帧触发，把「读 deadline 续期」这条规则定死，
// 数据帧、业务心跳、pong 自然统一成同一个存活信号。
func (c *Conn) renewReadDeadline() {
	if c.opt.ReadTimeout <= 0 {
		_ = c.raw.SetReadDeadline(time.Time{})
		return
	}
	_ = c.raw.SetReadDeadline(time.Now().Add(c.opt.ReadTimeout))
}

// failTerminal 记录先于读错误发现的终止原因，并触发收敛。
func (c *Conn) failTerminal(err error) {
	c.mu.Lock()
	if c.termErr == nil {
		c.termErr = err
	}
	c.mu.Unlock()
	c.shutdown()
}

// shutdown 关闭底层连接，保证只执行一次，并唤醒所有等待者。
func (c *Conn) shutdown() {
	c.shutOnce.Do(func() {
		_ = c.raw.Close()
		close(c.done)
	})
}

func (c *Conn) markReadDone() {
	c.readOnce.Do(func() { close(c.readDone) })
}
