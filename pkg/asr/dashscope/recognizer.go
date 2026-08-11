// Package dashscope 实现百炼（DashScope）双工 WebSocket 协议的实时语音识别。
package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/zzjha-cn/gKit/pkg/asr"
	"github.com/zzjha-cn/gKit/pkg/logger"
	"github.com/zzjha-cn/gKit/pkg/transport/wsx"
)

const (
	// 上游写超时。音频帧很小，写不出去说明链路已经有问题。
	upstreamWriteTimeout = 5 * time.Second
	userAgent            = "algo-srv-asr/1.0"

	// startRetryBackoff 首次重试前的等待，每次重试翻倍。
	startRetryBackoff = 200 * time.Millisecond
	// minAttemptHeadroom 一次尝试至少要留这么多预算才有意义，
	// 否则退避完就直接超时，白等一场。
	minAttemptHeadroom = time.Second
)

// Config 上游连接参数。
type Config struct {
	Region      string // cn-beijing | ap-southeast-1
	WorkspaceID string
	APIKey      string
	Model       string
	// Endpoint 覆盖默认拼装的 wss 地址，留空表示按 region + workspace 拼。
	// 用于私有化地址或本地联调。
	Endpoint string
	// DialTimeout 单次「拨号 + 等待 task-started」的超时。
	DialTimeout time.Duration
	// StartAttempts 建立会话的总尝试次数（含首次），<=1 表示不重试。
	// 重试只发生在会话建立阶段——此时一帧音频都还没发出去，重来是幂等的。
	StartAttempts int
	// StartTimeout 建立会话的总预算，覆盖全部重试轮次。
	// 前端在这段时间里一直等不到 ready，所以别放太大。
	StartTimeout time.Duration
	// ReadTimeout 上游静默多久判定链路已死。
	//
	// 依赖 run-task 里开启的 heartbeat：静音期上游会持续下发 heartbeat 结果，
	// 所以正常连接不会长时间没有任何消息。没有这个 deadline 的话，TCP 半开时
	// ReadMessage 会永久阻塞——连接看着健康，但一个字都不会再出来。
	//
	// 刻意不开 wsx 的协议级 ping：pong 是上游 WebSocket 栈自动回的，
	// 上游业务线程假死时 pong 照样回、读 deadline 照样被续期，
	// 结果是把「已经不出字了」的会话一直续着。业务 heartbeat 才是这里的探活主力。
	ReadTimeout time.Duration
	// MaxSessionLifetime 单路会话的硬上限，0 表示不限。
	// ASR 按时长计费，卡住不结束的会话会一直烧钱，建议配一个。
	MaxSessionLifetime time.Duration
}

// 默认值。DialTimeout 取值偏小是为了给重试留出预算：
// 3 次 × 4s + 退避 ≈ 13s，由 StartTimeout 兜底截断到 10s。
const (
	defaultDialTimeout   = 4 * time.Second
	defaultStartAttempts = 3
	defaultStartTimeout  = 10 * time.Second
	defaultReadTimeout   = 60 * time.Second
)

// retryableError 标记「重试有意义」的失败。
// 不带这个标记的错误一律不重试，避免在上游已经异常时继续加压。
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func retryable(err error) error { return &retryableError{err: err} }

func isRetryable(err error) bool {
	var re *retryableError
	return errors.As(err, &re)
}

// Recognizer 百炼实时识别器，可并发复用，每次 Start 建立独立连接。
type Recognizer struct {
	cfg      Config
	dialOpt  wsx.DialOptions
	connOpts wsx.Options
}

var _ asr.Recognizer = (*Recognizer)(nil)

func New(cfg Config) *Recognizer {
	//if cfg.Region == "" {
	//	cfg.Region = "cn-beijing"
	//}
	if cfg.Model == "" {
		cfg.Model = "qwen-audio-3.0-asr-flash-streaming"
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.StartAttempts <= 0 {
		cfg.StartAttempts = defaultStartAttempts
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	return &Recognizer{
		cfg: cfg,
		dialOpt: wsx.DialOptions{
			HandshakeTimeout: cfg.DialTimeout,
			ReadBufferSize:   8 * 1024,
			WriteBufferSize:  8 * 1024,
		},
		connOpts: wsx.Options{
			WriteTimeout: upstreamWriteTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			// PingInterval 故意留 0，见 Config.ReadTimeout 的说明。
			MaxLifetime: cfg.MaxSessionLifetime,
			// 上游没什么好通知的，到点直接关。
			GateNotice: func(string) (int, []byte) { return 0, nil },
			// 关闭帧发出后不必久等上游回应：结果流已经收尾，连接留着只是浪费。
			CloseTimeout: time.Second,
			Logger:       logger.With("mod", "asr.dashscope"),
		},
	}
}

// endpoint 拼装 wss 地址，workspace id 是地址的子域。
func (r *Recognizer) endpoint() string {
	if r.cfg.Endpoint != "" {
		return r.cfg.Endpoint
	}
	if r.cfg.WorkspaceID != "" {
		return fmt.Sprintf("wss://%s.%s.maas.aliyuncs.com/api-ws/v1/inference", r.cfg.WorkspaceID, r.cfg.Region)
	}
	return "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
}

// session 单路上游会话的内部状态。
//
// 写串行化、读写 deadline、幂等关闭、关闭归因都由 wsx.Conn 负责，
// 这里只剩下 dashscope 协议本身的状态。
type session struct {
	conn   *wsx.Conn
	taskID string
	events *asr.StreamRecognizeResult

	finishOnce sync.Once
	startedCh  chan error
	started    bool

	// terminal 标记上游已经给出终局（task-finished / task-failed），
	// 结局事件已经投递过。连接随后的断开是预期收尾，不能再报一次失败。
	//
	// 这个标记就是「会话是否正常结束」的唯一依据——不看关闭码。
	// 上游发 1000 正常关闭帧却没给 task-finished，那也是异常终止。
	terminal atomic.Bool
}

// Start 建立一路上游会话，失败时按可重试性退避重来。
//
// 重试只在这里做：此刻还没有发过任何音频，换个 task_id 重来是完全幂等的。
// 会话建立之后的失败一律不重试——上游连接一旦异常，音频流的位置就对不上了，
// 重发只会让识别结果错乱，不如直接判定会话死亡。
func (r *Recognizer) Start(ctx context.Context, opt asr.RecognizeOption) (*asr.Session, error) {
	if r.cfg.APIKey == "" { // || r.cfg.WorkspaceID == "" {
		return nil, fmt.Errorf("dashscope asr: api_key/workspace_id 未配置")
	}

	budgetCtx, cancel := context.WithTimeout(ctx, r.cfg.StartTimeout)
	defer cancel()

	backoff := startRetryBackoff
	var lastErr error
	for attempt := 1; attempt <= r.cfg.StartAttempts; attempt++ {
		sess, err := r.dialAndStart(budgetCtx, opt)
		if err == nil {
			if attempt > 1 {
				logger.Infow("asr 重试后建立成功",
					"attempt", attempt,
					"request_id", opt.RequestID,
				)
			}
			return sess, nil
		}
		lastErr = err

		if !isRetryable(err) || attempt == r.cfg.StartAttempts {
			break
		}
		// 预算不够走完下一轮就别浪费这次退避了，直接把错误交出去。
		if deadline, ok := budgetCtx.Deadline(); ok && time.Until(deadline) < backoff+minAttemptHeadroom {
			break
		}
		logger.Errorw("asr 建立会话失败，准备重试",
			"attempt", attempt,
			"backoff_ms", backoff.Milliseconds(),
			"err", err,
			"request_id", opt.RequestID,
		)
		select {
		case <-time.After(backoff):
		case <-budgetCtx.Done():
			return nil, lastErr
		}
		backoff *= 2
	}
	return nil, lastErr
}

// dialAndStart 单次尝试：拨号 -> run-task -> 等 task-started。
func (r *Recognizer) dialAndStart(ctx context.Context, opt asr.RecognizeOption) (*asr.Session, error) {
	model := opt.Model
	if model == "" {
		model = r.cfg.Model
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	if r.cfg.WorkspaceID != "" {
		header.Set("X-DashScope-WorkSpace", r.cfg.WorkspaceID)
	}
	header.Set("user-agent", userAgent)

	dialCtx, cancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
	defer cancel()

	dialOpt := r.dialOpt // 逐次拷贝：Header 因请求而异，不能共享一份
	dialOpt.Header = header
	dialOpt.Conn = r.connOpts

	conn, err := wsx.Dial(dialCtx, r.endpoint(), dialOpt)
	if err != nil {
		// 401/403 是鉴权问题，和网络错误分开报，便于排查。
		status, body := 0, ""
		var he *wsx.HandshakeError
		if errors.As(err, &he) {
			status, body = he.StatusCode, he.Body
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			logger.Errorw("asr dial unauthorized", logger.LogField{
				"status":     status,
				"body":       body,
				"request_id": opt.RequestID,
			})
			return nil, fmt.Errorf("dashscope asr: 鉴权失败 (http %d): %w", status, err)
		}
		logger.Errorw("asr dial fail", logger.LogField{
			"status":     status,
			"body":       body,
			"err":        err,
			"request_id": opt.RequestID,
		})
		dialErr := fmt.Errorf("dashscope asr: 连接上游失败: %w", err)
		// status==0 是纯网络错误（DNS / 拨不通 / 超时），5xx 是上游自身故障，
		// 这两类换个时间点大概率能成。其余 4xx（含 429 限流）重试没有意义。
		if status == 0 || status >= http.StatusInternalServerError {
			return nil, retryable(dialErr)
		}
		return nil, dialErr
	}

	s := &session{
		conn:      conn,
		taskID:    uuid.NewString(),
		events:    asr.NewStreamRecognizeResult(),
		startedCh: make(chan error, 1),
	}

	// 读循环先起来，run-task 的响应（task-started / task-failed）由它接管。
	//
	// ctx 必须是 Background：会话的生命周期由 finish-task / Close 决定，
	// 而不是由调用方那个可能带 HTTP 请求超时的 ctx 决定——
	// 传进来的 ctx 只用于「建立阶段」的预算（见 Start）。
	go s.conn.Serve(context.Background(), s.handler(opt.RequestID))

	if err = s.conn.SendJSON(buildRunTask(s.taskID, model, opt)); err != nil {
		s.close()
		return nil, fmt.Errorf("dashscope asr: 发送 run-task 失败: %w", err)
	}

	// 必须等到 task-started 才能发音频。
	select {
	case err = <-s.startedCh:
		if err != nil {
			s.close()
			return nil, err
		}
	case <-dialCtx.Done():
		s.close()
		// 连都连上了却等不到 task-started，多半是上游这一路实例有问题，值得换一条连接重来。
		return nil, retryable(fmt.Errorf("dashscope asr: 等待 task-started 超时: %w", dialCtx.Err()))
	}

	logger.Infow("asr task started", logger.LogField{
		"task_id":    s.taskID,
		"model":      model,
		"request_id": opt.RequestID,
	})

	// ready 事件由读循环在收到 task-started 时投递，这里不能再发（见 dispatch）。
	return &asr.Session{
		ID:     s.taskID,
		Events: s.events,
		Write:  s.writeAudio,
		Finish: s.finish,
		Close:  s.close,
	}, nil
}

func buildRunTask(taskID, model string, opt asr.RecognizeOption) runTaskRequest {
	format := opt.Format
	if format == "" {
		format = "pcm"
	}
	sampleRate := opt.SampleRate
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	return runTaskRequest{
		Header: clientHeader{
			Action:    ActionRunTask,
			TaskID:    taskID,
			Streaming: StreamingDuplex,
		},
		Payload: runTaskPayload{
			TaskGroup: TaskGroupAudio,
			Task:      TaskASR,
			Function:  FunctionRecognition,
			Model:     model,
			Parameters: taskParameter{
				Format:                    format,
				SampleRate:                sampleRate,
				LanguageHints:             opt.LanguageHints,
				SemanticPunctuationEnable: opt.SemanticPunctuation,
				MaxSentenceSilence:        opt.MaxSentenceSilence,
				VocabularyID:              opt.VocabularyID,
				// 必须开：静音期上游靠它持续下发心跳结果，
				// wsx 的读 deadline 才有续期依据。心跳结果在 dispatch 里被丢弃。
				Heartbeat: true,
			},
		},
	}
}

// writeAudio 发送一帧音频。加锁与写 deadline 由 wsx.Conn.Send 内部完成，
// 写失败即判定链路死亡（wsx 会立刻收敛并触发 OnDisconnect），这里不重试。
func (s *session) writeAudio(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return s.conn.SendBinary(pcm)
}

// finish 发送 finish-task，不阻塞等待结果；结束事件由 Events 流给出。
func (s *session) finish() error {
	var err error
	s.finishOnce.Do(func() {
		err = s.conn.SendJSON(finishTaskRequest{
			Header: clientHeader{
				Action:    ActionFinishTask,
				TaskID:    s.taskID,
				Streaming: StreamingDuplex,
			},
		})
	})
	return err
}

// close 由业务侧调用，可重复调用（wsx.Conn.Close 幂等）。
func (s *session) close() {
	// 先让可能阻塞在投递上的读循环解开，再关连接，顺序不能反。
	s.events.Abort()
	_ = s.conn.Close(websocket.CloseNormalClosure, "client close")
}

// handler 是上游消息的处理入口，全部回调都跑在 wsx 的读循环 goroutine 上，
// 因此「只有一条 goroutine 会 Publish/Finish」这条事件流约定天然成立。
//
// 读 deadline 由 wsx 在每个入站帧上续期。run-task 已开启 heartbeat，静音期上游
// 会持续下发心跳结果，所以「ReadTimeout 内一条消息都没有」只可能是链路已死
// （典型是 TCP 半开：不报错也不返回，会一直阻塞下去）。
//
// 不做重发/重连：上游异常后音频流的位置已经不确定，重发同一帧会让识别结果错乱
// 且无从发现——宁愿失败，也不要给出错的文本。重试只在 Start 阶段做，
// 那时还没发过任何音频。
func (s *session) handler(requestID string) wsx.Handler {
	return wsx.Callbacks{
		Message: func(c *wsx.Conn, _ int, data []byte) {
			var ev serverEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				logger.Warnw("asr decode server event fail", logger.LogField{
					"err":        err,
					"raw":        string(data),
					"task_id":    s.taskID,
					"request_id": requestID,
				})
				return
			}
			if done := s.dispatch(ev, requestID); done {
				// 先立标记再关连接：OnDisconnect 靠它判断结局是否已经给过。
				s.terminal.Store(true)
				_ = c.Close(websocket.CloseNormalClosure, "task terminated")
			}
		},
		// OnDisconnect 由 wsx 保证恰好调用一次（含 panic 路径），
		// 所以「必须给出一个结局」的逻辑可以安全地挂在这里。
		Disconnect: func(_ *wsx.Conn, d wsx.Disconnect) {
			defer s.events.Finish()

			// 上游已经给过 task-finished / task-failed，后续断开是预期收尾。
			if s.terminal.Load() {
				s.markStarted(fmt.Errorf("dashscope asr: 会话已终结"))
				return
			}
			// 业务侧主动 Close（上层放弃会话），同样是预期收尾，不报失败。
			if d.Cause == wsx.CauseLocal {
				s.markStarted(fmt.Errorf("dashscope asr: 连接已关闭: %w", d.Err))
				return
			}

			// 其余一律算异常终止，**包括上游发正常关闭帧（1000/1001）就走人**。
			// 判定依据不是关闭码，而是有没有拿到终结事件（terminal 标记）。
			// 早先按关闭码放行会让这种断开变成静默失败——前端只看到连接断了，
			// 既没有 done 也没有 error，完全没法排查。
			logger.Errorw("asr upstream read fail", logger.LogField{
				"cause":      d.Cause.String(),
				"code":       d.Code,
				"err":        d.Err,
				"task_id":    s.taskID,
				"request_id": requestID,
			})
			// 还在等 task-started 时链路就断了（含读超时），换条连接重来是值得的。
			s.markStarted(retryable(d.Err))
			// 包一层可读的描述：底层错误可能只是 "close 1000 (normal)" 之类，
			// 直接透传到前端毫无信息量。
			s.events.Publish(asr.ASREvent{
				Type:      asr.EventASRFailed,
				SessionID: s.taskID,
				Err:       fmt.Errorf("dashscope asr: 上游连接中断且未返回结果: %w", d.Err),
			})
		},
	}
}

// dispatch 返回 true 表示会话已终结。
func (s *session) dispatch(ev serverEvent, requestID string) bool {
	switch ev.Header.Event {
	case EventTaskStarted:
		// ready 必须在这里发，不能在 Start 里发：
		// 事件流约定只有读循环这一条 goroutine 能 Publish/Finish，
		// 从调用方 goroutine 投递会和这里的 Finish 撞车（send on closed channel）。
		// 先 Publish 再 markStarted，保证 Start 返回时 ready 已经在队列里。
		s.events.Publish(asr.ASREvent{Type: asr.EventASRReady, SessionID: s.taskID})
		s.markStarted(nil)

	case EventResultGenerated:
		st := ev.Payload.Output.Sentence
		if st == nil || st.Heartbeat {
			// heartbeat 结果只用于保活（sentence_id=0），不能透传给业务。
			return false
		}
		evt := asr.ASREvent{
			Type:       asr.EventASRPartial,
			SessionID:  s.taskID,
			SentenceID: st.SentenceID,
			Text:       st.Text,
			BeginMs:    st.BeginTime,
			EndMs:      st.EndTime,
			Words:      convertWords(st.Words),
		}
		if st.SentenceEnd {
			evt.Type = asr.EventASRFinal
		}
		s.events.Publish(evt)

	case EventTaskFinished:
		evt := asr.ASREvent{Type: asr.EventASRFinished, SessionID: s.taskID}
		if ev.Payload.Usage != nil {
			evt.DurationSec = ev.Payload.Usage.Duration
		}
		s.events.Publish(evt)
		return true

	case EventTaskFailed:
		err := fmt.Errorf("dashscope asr: %s %s", ev.Header.ErrorCode, ev.Header.ErrorMessage)
		logger.Errorw("asr task failed", logger.LogField{
			"code":       ev.Header.ErrorCode,
			"msg":        ev.Header.ErrorMessage,
			"task_id":    s.taskID,
			"request_id": requestID,
		})
		// 会话建立阶段就失败的话，按错误码决定 Start 要不要换条连接重来。
		startErr := error(err)
		if isRetryableUpstreamCode(ev.Header.ErrorCode) {
			startErr = retryable(err)
		}
		s.markStarted(startErr)
		s.events.Publish(asr.ASREvent{
			Type:      asr.EventASRFailed,
			SessionID: s.taskID,
			Code:      ev.Header.ErrorCode,
			Err:       err,
		})
		return true
	}
	return false
}

// markStarted 只对第一次生效：Start 还在等 task-started 时唤醒它。
//
// 无锁的 started 字段是安全的：调用点全在 wsx 的读循环 goroutine 上
// （dispatch 与 OnDisconnect 都是）。
func (s *session) markStarted(err error) {
	if s.started {
		return
	}
	s.started = true
	s.startedCh <- err
}

func convertWords(ws []word) []asr.Word {
	if len(ws) == 0 {
		return nil
	}
	res := make([]asr.Word, 0, len(ws))
	for _, w := range ws {
		res = append(res, asr.Word{
			BeginMs:     w.BeginTime,
			EndMs:       w.EndTime,
			Text:        w.Text,
			Punctuation: w.Punctuation,
		})
	}
	return res
}
