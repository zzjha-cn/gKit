// Package asrsrv 提供前端 WebSocket 与上游语音识别之间的桥接服务。
//
// 接入层用 pkg/transport/wsx：写串行化、读写超时、心跳、空连接闸、
// 最长时长闸、关闭归因都由它负责，本包只剩下「ASR 协议状态机」这一件事。
package asrsrv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zzjha-cn/gKit/pkg/asr"
	"github.com/zzjha-cn/gKit/pkg/asr/dashscope"
	"github.com/zzjha-cn/gKit/pkg/logger"
	"github.com/zzjha-cn/gKit/pkg/transport/wsx"
)

const (
	// pongWait 读超时窗口：这段时间内没有任何入站帧（数据帧 / ping / pong）
	// 就判定链路已死。pingPeriod 必须 <= 它的一半，否则 wsx.Wrap 直接报错。
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second
	// writeWait 单次写超时。
	writeWait = 10 * time.Second
	// maxFrameSize 单帧上限：100ms@16k PCM16 是 3200 字节，留足余量防超大帧打爆内存。
	maxFrameSize = 64 * 1024
)

// Manager 管理全部活跃会话：并发名额、生命周期、优雅退出。
type Manager struct {
	cfg        *ASRCfg
	recognizer asr.Recognizer
	log        logger.Logger

	// rootCtx 来自进程级 ctx，不能用请求 ctx——
	// 连接升级后请求已被 Hijack，请求 ctx 的超时会把长连接无故关掉。
	rootCtx context.Context

	// reg 同时承担并发名额与优雅退出：一路会话持续占用一个上游连接，
	// 所以限的是并发量而不是 QPS。
	reg *wsx.Registry
}

var manager *Manager

// InitManager 在进程启动时初始化，配置缺失时返回 nil（接口直接返回不可用）。
//
// ctx 是进程级 ctx（取消即代表进程退出）；优雅退出请在退出流程里调用
// CloseAll，它会等连接真正收敛——只取消 ctx 不等待的话，正在收尾的会话
// 可能来不及把最后的业务消息和关闭帧发出去。
func InitManager(ctx context.Context, cfg *ASRCfg) *Manager {
	m := NewManager(ctx, cfg)
	manager = m
	return m
}

// NewManager 创建一个独立实例，不写全局变量，便于测试与多实例场景。
func NewManager(ctx context.Context, cfg *ASRCfg) *Manager {
	if cfg == nil {
		logger.Info("asr 未配置，跳过初始化")
		return nil
	}
	cfg.Normalize()
	if ctx == nil {
		ctx = context.Background()
	}

	log := logger.With("mod", "asr.srv")
	m := &Manager{
		cfg: cfg,
		recognizer: dashscope.New(dashscope.Config{
			Region:        cfg.Region,
			WorkspaceID:   cfg.WorkspaceID,
			APIKey:        cfg.APIKey,
			Model:         cfg.Model,
			Endpoint:      cfg.Endpoint,
			DialTimeout:   time.Duration(cfg.UpstreamDialTimeout) * time.Second,
			StartAttempts: cfg.UpstreamStartAttempts,
			StartTimeout:  time.Duration(cfg.UpstreamStartTimeout) * time.Second,
			ReadTimeout:   time.Duration(cfg.UpstreamReadTimeout) * time.Second,
			// 上游不该比前端会话活得更久：ASR 按时长计费，卡住不结束的会话会一直烧钱。
			MaxSessionLifetime: time.Duration(cfg.MaxSessionSeconds) * time.Second,
		}),
		log:     log,
		rootCtx: ctx,
		reg:     wsx.NewRegistry(cfg.MaxConcurrent),
	}

	log.Infow("asr manager 初始化完成", logger.LogField{
		"region":         cfg.Region,
		"model":          cfg.Model,
		"max_concurrent": cfg.MaxConcurrent,
	})
	return m
}

// GetManager 返回全局实例，未初始化时为 nil。
func GetManager() *Manager { return manager }

// Config 返回归一化后的配置。
func (m *Manager) Config() *ASRCfg { return m.cfg }

// Acquire 申请一个并发名额，占满或正在退出时返回 false。
//
// 必须在**升级之前**调用：满了直接回 503，比升级成功再关连接好排查得多。
// release 幂等；升级失败要立刻调用，成功后交给 Track 管理。
func (m *Manager) Acquire() (release func(), ok bool) { return m.reg.Acquire() }

// Track 登记连接，并在连接真正收敛后释放名额。
func (m *Manager) Track(c *wsx.Conn, release func()) { m.reg.Track(c, release) }

// CloseAll 广播关闭所有活跃会话并等待收敛，供进程退出时调用。
func (m *Manager) CloseAll(ctx context.Context) error {
	if m == nil {
		return nil
	}
	n := m.reg.ActiveCount()
	err := m.reg.CloseAll(ctx)
	m.log.Infow("asr 会话已全部关闭", logger.LogField{"count": n, "err": err})
	return err
}

// ActiveCount 当前活跃会话数，用于观测。
func (m *Manager) ActiveCount() int { return m.reg.ActiveCount() }

// ConnOptions 是前端连接的行为配置，由接入层（pkg/asr/asr_api）在升级时带上。
//
// 连接策略属于中转层的职责——超时、心跳、帧大小、两道时间闸决定了这条桥
// 怎么活怎么死；接入层只负责「怎么被调用」，不该自己编一套超时。
//
// 三件原来要自己写 goroutine 的事现在交给 wsx：
//   - PingInterval        心跳（原 Bridge.heartbeat）
//   - ActivateGrace       等 start 指令的上限（原 Bridge.guard 前半段）
//   - MaxLifetime         单次会话最长时长（原 Bridge.guard 后半段）
func (m *Manager) ConnOptions() wsx.Options {
	return wsx.Options{
		WriteTimeout:  writeWait,
		ReadTimeout:   pongWait,
		PingInterval:  pingPeriod,
		ReadLimit:     maxFrameSize,
		ActivateGrace: time.Duration(m.cfg.StartWaitSeconds) * time.Second,
		MaxLifetime:   time.Duration(m.cfg.MaxSessionSeconds) * time.Second,
		GateNotice:    gateNotice,
		Logger:        m.log,
	}
}

// gateNotice 把 wsx 的时间闸关闭翻译成本协议的 error 事件。
//
// 静默断开会让前端以为是网络故障，排查成本极高，所以关之前必须先说一声。
// reason 是 wsx 给出的固定文案，这里只做归类：没按时 start 是客户端问题，
// 超过最长时长是服务端限制。
func gateNotice(reason string) (int, []byte) {
	data := ErrorData{Code: 500, Msg: "会话超过最长时长限制"}
	if reason == "activation grace exceeded" {
		data = ErrorData{Code: 400, Msg: "超时未收到 start 指令"}
	}
	body, _ := json.Marshal(envelope{Event: EventError, Data: data})
	return websocket.TextMessage, body
}
