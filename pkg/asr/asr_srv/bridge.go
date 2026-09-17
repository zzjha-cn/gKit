package asrsrv

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zzjha-cn/gKit/pkg/asr"
	"github.com/zzjha-cn/gKit/pkg/logger"
	"github.com/zzjha-cn/gKit/pkg/transport/wsx"
)

// Bridge 一条前端 WebSocket 与一路上游识别会话的桥接。
//
// goroutine 分工：
//   - Run（调用方 goroutine）：wsx 读循环，控制指令走状态机，音频进 audioCh
//   - upWrite：audioCh -> 上游
//   - upRead：上游事件 -> 前端
//
// 心跳、读写 deadline、空连接闸、最长时长闸、写串行化全部由 wsx.Conn 承担，
// 所以这里既没有 heartbeat/guard goroutine，也没有写锁。
type Bridge struct {
	mgr   *Manager
	conn  *wsx.Conn // 与业务端的连接
	reqID string
	log   logger.Logger

	ctx    context.Context
	cancel context.CancelFunc

	audioCh chan []byte
	dropped atomic.Int64

	startOnce    sync.Once
	stopOnce     sync.Once
	finishedOnce sync.Once
	shutdownOnce sync.Once

	stopCh chan struct{} // 前端发来 stop
	// finishedCh 上游给出终结事件（task-finished / task-failed）后关闭。
	finishedCh chan struct{}

	sessMu sync.Mutex
	sess   *asr.Session
}

// NewBridge 创建桥接。调用方需保证已经通过 Manager.Acquire 拿到名额。
func NewBridge(m *Manager, conn *wsx.Conn, reqID string) *Bridge {
	ctx, cancel := context.WithCancel(m.rootCtx)
	return &Bridge{
		mgr:        m,
		conn:       conn,
		reqID:      reqID,
		log:        m.log.With("request_id", reqID, "conn_id", conn.Info().ID),
		ctx:        ctx,
		cancel:     cancel,
		audioCh:    make(chan []byte, m.cfg.AudioBufferFrames),
		stopCh:     make(chan struct{}),
		finishedCh: make(chan struct{}),
	}
}

// drainWait 发出 finish-task 后等待上游 task-finished 的兜底时间。
// 上游不回就得自己把会话拖走，否则会一直挂在那里。
func (b *Bridge) drainWait() time.Duration {
	return time.Duration(b.mgr.cfg.DrainWaitSeconds) * time.Second
}

// Run 阻塞运行直到连接结束。
func (b *Bridge) Run() {
	defer func() {
		if p := recover(); p != nil {
			b.log.Errorw("asr bridge panic", logger.LogField{"panic": p})
		}
		b.Shutdown()
		b.log.Infow("asr 会话结束", logger.LogField{"dropped_frames": b.dropped.Load()})
	}()

	// 注意：这里必须用进程级 ctx 派生的 b.ctx。
	// 若传 HTTP 请求 ctx，连接会在框架的请求超时到点后被无故关闭。
	b.conn.Serve(b.ctx, b.handler())
}

// handler 组装读循环回调：文本帧按 action 路由，二进制帧当音频。
func (b *Bridge) handler() wsx.Handler {
	router := &wsx.Router{EventKey: ActionKey}
	router.On(ActionStart, func(_ *wsx.Conn, data []byte) { b.handleStart(data) })
	router.On(ActionStop, func(_ *wsx.Conn, _ []byte) { b.handleStop() })

	// 非 JSON / 无 action 字段 / 二进制帧都走到这里。
	base := router.Handler(func(_ *wsx.Conn, mt int, data []byte) {
		if mt == websocket.BinaryMessage {
			b.pushAudio(data)
		}
	})

	return wsx.Callbacks{
		Open:    base.OnOpen,
		Message: base.OnMessage,
		Disconnect: func(c *wsx.Conn, d wsx.Disconnect) {
			base.OnDisconnect(c, d) // 唤醒 Router 上的等待者
			b.onDisconnect(d)
		},
	}
}

// onDisconnect 由 wsx 保证恰好调用一次（含 panic 路径）。
func (b *Bridge) onDisconnect(d wsx.Disconnect) {
	// 我们自己 Shutdown 之后读循环必然报错，那不是异常，不用告警。
	switch {
	case d.Cause == wsx.CausePeer || b.ctx.Err() != nil:
		b.log.Infow("asr 业务端断开", logger.LogField{
			"cause": d.Cause.String(), "code": d.Code, "reason": d.Reason,
		})
	default:
		b.log.Warnw("asr 业务端连接异常", logger.LogField{
			"cause": d.Cause.String(), "code": d.Code, "err": d.Err,
		})
	}
	b.Shutdown()
}

func (b *Bridge) handleStart(data []byte) {
	var req StartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		b.emitError(400, "invalid start payload", "")
		return
	}
	b.startOnce.Do(func() {
		// 解除 wsx 的 ActivateGrace 闸门：连接已经进入正常工作状态。
		b.conn.MarkActive()
		go b.runSession(req)
	})
}

func (b *Bridge) handleStop() {
	b.stopOnce.Do(func() { close(b.stopCh) })
}

// markFinished 标记上游已经给出终结事件，解除 upWrite 的收尾等待。
func (b *Bridge) markFinished() {
	b.finishedOnce.Do(func() { close(b.finishedCh) })
}

// pushAudio 有界投递：队列满时丢最老的一帧，绝不无界堆积。
//
// 回调跑在 wsx 的读循环上，阻塞它等于阻塞整条连接的读取，所以这里只能丢不能等。
func (b *Bridge) pushAudio(frame []byte) {
	select {
	case b.audioCh <- frame:
		return
	default:
	}
	select {
	case <-b.audioCh:
		b.dropped.Add(1)
	default:
	}
	select {
	case b.audioCh <- frame:
	default:
		b.dropped.Add(1)
	}
}

// runSession 建立上游会话并驱动上下行。
func (b *Bridge) runSession(req StartRequest) {
	defer func() {
		if p := recover(); p != nil {
			b.log.Errorw("asr session panic", logger.LogField{"panic": p})
			b.Shutdown()
		}
	}()

	cfg := b.mgr.cfg
	opt := asr.RecognizeOption{
		Model:               cfg.Model,
		Format:              req.Format,
		SampleRate:          req.SampleRate,
		LanguageHints:       req.LanguageHints,
		SemanticPunctuation: cfg.SemanticPunctuation,
		MaxSentenceSilence:  cfg.MaxSentenceSilence,
		RequestID:           b.reqID,
	}
	if opt.Format == "" {
		opt.Format = "pcm"
	}
	if opt.SampleRate <= 0 {
		opt.SampleRate = cfg.SampleRate
	}
	if req.SemanticPunctuation != nil {
		opt.SemanticPunctuation = *req.SemanticPunctuation
	}
	if req.MaxSentenceSilence > 0 {
		opt.MaxSentenceSilence = req.MaxSentenceSilence
	}

	sess, err := b.mgr.recognizer.Start(b.ctx, opt)
	if err != nil {
		b.log.Errorw("asr 上游会话建立失败", logger.LogField{"err": err})
		b.emitError(500, err.Error(), "")
		b.Shutdown()
		return
	}

	b.sessMu.Lock()
	b.sess = sess
	b.sessMu.Unlock()

	go b.upRead(sess)
	b.upWrite(sess)
}

// upWrite 上游连接的音频写入方。
func (b *Bridge) upWrite(sess *asr.Session) {
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-b.conn.Done():
			return
		case <-b.stopCh:
			b.drainAudio(sess)
			if err := sess.Finish(); err != nil {
				b.log.Warnw("asr finish-task 失败", logger.LogField{"err": err})
				b.emitError(500, "通知上游收尾失败", "")
				b.Shutdown()
				return
			}
			b.awaitFinish()
			return
		case frame := <-b.audioCh:
			if err := sess.Write(frame); err != nil {
				b.log.Warnw("asr 写上游失败", logger.LogField{"err": err})
				// 写失败只 Shutdown 的话，前端只看到连接莫名断开，无从排查。
				b.emitError(500, "音频上送失败，会话已中断", "")
				b.Shutdown()
				return
			}
		}
	}
}

// awaitFinish 发出 finish-task 后等上游的终结事件，超时就自己把会话拖走。
// 没有这个兜底的话，上游不回 task-finished 时整条链路会一直挂着——
// 前端心跳还在续命，名额也一直占着，谁都发现不了。
func (b *Bridge) awaitFinish() {
	timer := time.NewTimer(b.drainWait())
	defer timer.Stop()

	select {
	case <-b.finishedCh:
	case <-b.ctx.Done():
	case <-b.conn.Done():
	case <-timer.C:
		b.log.Warnw("asr 等待上游收尾超时")
		b.emitError(500, "等待上游收尾超时", "")
		b.Shutdown()
	}
}

// drainAudio 收到 stop 后把缓冲里剩余的音频送完再收尾。
func (b *Bridge) drainAudio(sess *asr.Session) {
	for {
		select {
		case frame := <-b.audioCh:
			if err := sess.Write(frame); err != nil {
				return
			}
		default:
			return
		}
	}
}

// upRead 把上游事件翻译成前端事件。
func (b *Bridge) upRead(sess *asr.Session) {
	// terminal 记录是否已经给前端一个明确的结局（done 或 error）。
	terminal := false
	defer func() {
		if p := recover(); p != nil {
			b.log.Errorw("asr upRead panic", logger.LogField{"panic": p})
		}
		// 兜底：事件流结束了却没给出终结事件（上游直接断开、或将来换 provider
		// 出现没预料到的路径），也必须让前端拿到结果。
		// 少了这一条，前端只会看到连接莫名关闭，既没有 done 也没有 error。
		// b.ctx 已取消说明是我们自己在收尾（前端断开、超时等），那条路径上
		// 该报的错已经报过了，不用重复。
		if !terminal && b.ctx.Err() == nil {
			b.log.Warnw("asr 上游未给出终结事件就结束")
			b.emitError(500, "上游会话异常结束", "")
		}
		// 放在这里：上游事件流因为任何原因结束，都要解除 awaitFinish 的等待。
		b.markFinished()
		b.Shutdown()
	}()

	for event := range sess.Events.EventCh {
		switch event.Type {
		case asr.EventASRReady:
			b.emit(EventReady, ReadyData{SessionID: event.SessionID})

		case asr.EventASRPartial, asr.EventASRFinal:
			name := EventPartial
			if event.Type == asr.EventASRFinal {
				name = EventFinal
			}
			b.emit(name, SentenceData{
				SentenceID: event.SentenceID,
				Text:       event.Text,
				BeginMs:    event.BeginMs,
				EndMs:      event.EndMs,
			})

		case asr.EventASRFinished:
			terminal = true
			b.emit(EventDone, DoneData{DurationSec: event.DurationSec})
			return

		case asr.EventASRFailed:
			terminal = true
			msg := "upstream failed"
			if event.Err != nil {
				msg = event.Err.Error()
			}
			b.emitError(500, msg, event.Code)
			return
		}
	}
}

// emit 向前端发送事件。加锁与写 deadline 由 wsx.Conn.Send 内部成对完成。
func (b *Bridge) emit(event string, data any) {
	err := b.conn.SendJSON(envelope{Event: event, Data: data})
	if err == nil || errors.Is(err, wsx.ErrClosed) {
		return
	}
	b.log.Warnw("asr 下行写失败", logger.LogField{"err": err, "event": event})
}

func (b *Bridge) emitError(code int, msg, upstreamCode string) {
	b.emit(EventError, ErrorData{Code: code, Msg: msg, UpstreamCode: upstreamCode})
}

// Shutdown 幂等收尾：取消 ctx、释放上游连接、关闭前端连接。
//
// 顺序是先关上游再关前端：反过来的话，上游最后几个事件会写到一条已关的连接上。
func (b *Bridge) Shutdown() {
	b.shutdownOnce.Do(func() {
		b.cancel()
		b.sessMu.Lock()
		sess := b.sess
		b.sessMu.Unlock()
		if sess != nil {
			sess.Close()
		}
		_ = b.conn.Close(websocket.CloseNormalClosure, "session closed")
	})
}
