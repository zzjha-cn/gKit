package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/zzjha-cn/gKit/pkg/tools"
)

// 该模块定义了一套通用的流式事件协议，目标是把“业务事件建模”和“传输写出逻辑”解耦。
//
// 设计要点：
// 1. Event 负责表达一帧流式数据的语义，而不是只表达某个字段片段；
// 2. Writer 负责把 Event 包装成统一响应格式并写出；
// 3. Option 负责以可组合的方式构造 Event，避免大量重复 struct 字面量；
// 4. EventFromLLM / WriteFromLLM 负责把底层 types.StreamEvent 适配到对外协议。
type (
	// Format 表示流式事件的编码输出格式。
	//
	// - FormatNDJSON: 每帧输出一行完整 JSON，适合服务端与通用客户端消费；
	// - FormatSSE: 输出标准 SSE 帧，便于浏览器原生 EventSource 直接消费。
	Format string

	// EventType 表示当前流式帧的内容类型。
	//
	// 它回答的是“这帧是什么内容”，而不是“这帧做了什么动作”。
	// 例如：
	// - status: 状态帧
	// - message: 普通消息帧；
	// - reasoning: 深度思考/推理帧；
	// - card: 卡片帧；
	// - error: 错误帧。
	EventType string

	// EventAction 表示当前流式帧的行为动作。
	//
	// 它回答的是“这帧应该如何作用到客户端状态”。
	// 例如：
	// - new: 创建一条新的消息；
	// - append: 对已有内容追加增量；
	// - replace: 用完整内容覆盖已有内容；
	// - end: 标记当前流结束。
	EventAction string

	// Event 是对外暴露的统一流式事件结构。
	//
	// 一个 Event 表示“一帧有明确语义的流式数据”，而不只是一个字段增量。
	// 其中：
	// - Type/Action 决定这一帧是什么、应该如何应用；
	// - Data 决定这一帧携带什么统一负载；
	// - Sequence 用于客户端去重、排序或断线恢复；
	// - MessageID 用于归并同一条消息的多帧数据；
	// - Usage/Err/Meta 用于承载附加元信息。
	Event struct {
		Type      EventType         `json:"type"`
		Action    EventAction       `json:"action,omitempty"`
		MessageID string            `json:"message_id,omitempty"`
		Sequence  int64             `json:"sequence"`
		Data      any               `json:"data,omitempty"`
		Err       string            `json:"err,omitempty"`
		Usage     *Usage            `json:"usage,omitempty"`
		Meta      map[string]string `json:"meta,omitempty"`
	}

	// Usage 表示一条消息在结束时附带的 token 使用统计。
	//
	// 它通常只会出现在 end 事件中，用于前端展示或链路记录。
	Usage struct {
		PromptTokens     int32 `json:"prompt_tokens,omitempty"`
		CompletionTokens int32 `json:"completion_tokens,omitempty"`
		TotalTokens      int32 `json:"total_tokens,omitempty"`
	}

	// Writer 负责把 Event 编码成单行 JSON 并写出到目标输出流。
	//
	// sequence 由 Writer 内部维护，保证同一个 Writer 输出的帧序号严格递增。
	Writer struct {
		mu        sync.RWMutex
		ctx       *WriterCtx
		out       io.Writer
		flusher   http.Flusher
		format    Format
		messageID string
		sequence  atomic.Int64
	}

	WriterCtx struct {
		Context context.Context
		Request *http.Request
		Writer  http.ResponseWriter
	}
)

const (
	// FormatNDJSON 表示逐行 JSON 输出格式，也是默认格式。
	FormatNDJSON Format = "ndjson"
	// FormatSSE 表示标准 SSE 输出格式。
	FormatSSE Format = "sse"

	// EventTypeStatus 表示状态信息帧，例如 connected / searching / generating。
	EventTypeStatus EventType = "status"
	// EventTypeMessage 表示普通消息帧。
	EventTypeMessage EventType = "message"
	// EventTypeReasoning 表示思考/推理帧。
	EventTypeReasoning EventType = "reasoning"
	// EventTypeCard 表示卡片类结构化内容帧。
	EventTypeCard EventType = "card"
	// EventTypeError 表示错误帧。
	EventTypeError EventType = "error"

	// EventActionNew 表示创建新的消息或内容容器。
	EventActionNew EventAction = "new"
	// EventActionAppend 表示追加增量内容。
	EventActionAppend EventAction = "append"
	// EventActionReplace 表示整体覆盖已有内容。
	EventActionReplace EventAction = "replace"
	// EventActionEnd 表示当前流结束。
	EventActionEnd EventAction = "end"
)

// New 创建一个不绑定具体输出目标的 Writer。
//
// 该构造适合以下场景：
// - 只想生成事件对象，不立即写出；
// - 在测试中手动补充输出；
// - 先构造 Writer，再按需挂接输出目标。
//
// 当 messageID 为空时，会自动生成一个新的消息 ID。
func New(messageID string) *Writer {
	if messageID == "" {
		messageID = tools.GenerateIdWithTime()
	}
	return &Writer{messageID: messageID, format: FormatNDJSON}
}

// NewWriter
//
// 该构造适用于 HTTP 流式输出场景：
// - 自动复用 ctx.Writer 作为输出目标；
// - 自动提取 Flusher，用于每帧立即刷新到客户端；
// - 如果底层 writer 不支持 flush，则返回错误。
func NewWriter(ctx *WriterCtx, messageID string) (*Writer, error) {
	flusher, ok := ctx.Writer.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flush")
	}
	writer := New(messageID)
	writer.ctx = ctx
	writer.out = ctx.Writer
	writer.flusher = flusher
	return writer, nil
}

// NewBufferWriter 创建一个绑定到通用 io.Writer 的 Writer。
//
// 适合单元测试、内存缓冲、文件输出或其他非 HTTP 流式场景。
// flusher 可为空；为空时 Write 只负责写入，不执行 Flush。
func NewBufferWriter(out io.Writer, flusher http.Flusher, messageID string) *Writer {
	writer := New(messageID)
	writer.out = out
	writer.flusher = flusher
	return writer
}

// WithFormat 设置 Writer 的输出编码格式。
//
// 当 format 为空时，会回退到默认的 NDJSON 格式。
func (w *Writer) WithFormat(format Format) *Writer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if format == "" {
		w.format = FormatNDJSON
		return w
	}
	w.format = format
	return w
}

// Init 为 HTTP 流式输出初始化 SSE 相关头信息。
//
// 对于 buffer writer 或未绑定 ctx 的 writer，调用该方法会直接返回。
func (w *Writer) Init() {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.ctx == nil {
		return
	}
	w.applyStreamHeaders()
	w.ctx.Writer.WriteHeader(http.StatusOK)
}

// MessageID 返回当前 Writer 维护的消息 ID。
//
// 该 ID 会在未显式覆盖时自动注入到所有写出的事件中，
// 便于客户端把同一条消息的多帧流式数据进行归并。
func (w *Writer) MessageID() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.messageID
}

func (w *Writer) NewOtherMessageID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.messageID = tools.GenerateIdWithTime()
	return w.messageID
}

// Status 写出一帧状态事件。
//
// 常用于 connected / searching / generating 等状态同步。
func (w *Writer) Status(meta map[string]string) error {
	return w.Write(Event{
		Type:      EventTypeStatus,
		MessageID: w.messageID,
		Meta:      meta,
	})
}

// New 写出一帧“新建消息”事件。
//
// 一般在开始流式回答前调用，用于通知客户端创建一个新的消息槽位。
func (w *Writer) New() error {
	return w.Write(Event{
		Type:      EventTypeMessage,
		Action:    EventActionNew,
		MessageID: w.messageID,
	})
}

// AppendAnswer 写出一帧回答正文的追加事件。
//
// 适合 token-by-token 或 chunk-by-chunk 的主回答输出。
func (w *Writer) AppendAnswer(delta any) error {
	return w.Write(Event{
		Type:      EventTypeMessage,
		Action:    EventActionAppend,
		MessageID: w.messageID,
		Data:      delta,
	})
}

// AppendReasoning 写出一帧思考内容的追加事件。
//
// 适合大模型把 reasoning 单独输出的场景。
func (w *Writer) AppendReasoning(delta any) error {
	return w.Write(Event{
		Type:      EventTypeReasoning,
		Action:    EventActionAppend,
		MessageID: w.messageID,
		Data:      delta,
	})
}

// ReplaceAnswer 写出一帧回答正文的覆盖事件。
//
// 适合“整体替换而不是增量追加”的场景，例如服务端后处理修正全文。
func (w *Writer) ReplaceAnswer(content any) error {
	return w.Write(Event{
		Type:      EventTypeMessage,
		Action:    EventActionReplace,
		MessageID: w.messageID,
		Data:      content,
	})
}

// ReplaceReasoning 写出一帧思考内容的覆盖事件。
func (w *Writer) ReplaceReasoning(content any) error {
	return w.Write(Event{
		Type:      EventTypeReasoning,
		Action:    EventActionReplace,
		MessageID: w.messageID,
		Data:      content,
	})
}

// End 写出一帧结束事件。
//
// 结束事件通常携带：
// - 最终完整内容；
// - token usage。
func (w *Writer) End(content any, usage *Usage) error {
	return w.Write(Event{
		Type:      EventTypeMessage,
		Action:    EventActionEnd,
		MessageID: w.messageID,
		Data:      content,
		Usage:     usage,
	})
}

func (w *Writer) CardAction(action EventAction, content any, m map[string]string) error {
	return w.Write(Event{
		Type:      EventTypeCard,
		Action:    action,
		MessageID: w.messageID,
		Data:      content,
		Meta:      m,
	})
}

func (w *Writer) Card(content any, usage *Usage) error {
	return w.Write(Event{
		Type:      EventTypeCard,
		Action:    EventActionEnd,
		MessageID: w.messageID,
		Data:      content,
		Usage:     usage,
	})
}

// Error 写出一帧错误事件。
//
// 如果 err 为空，会写出一个 err 为空串的 error 事件。
func (w *Writer) Error(err error) error {
	var msg string
	if err != nil {
		msg = err.Error()
	}
	return w.Write(Event{
		Type:      EventTypeError,
		MessageID: w.messageID,
		Err:       msg,
	})
}

func (w *Writer) ErrorString(msg string) error {
	return w.Write(Event{
		Type:      EventTypeError,
		MessageID: w.messageID,
		Err:       msg,
	})
}

// Write 按当前 Writer 配置的格式编码并写出一个事件。
//
// 行为说明：
// 1. 自动分配递增 sequence；
// 2. 当 event 未指定 MessageID 时自动填充 Writer 自身的 messageID；
// 3. FormatNDJSON 模式直接输出 Event 本体，不额外包裹业务 code/data 外层；
// 4. FormatSSE 模式输出 event/id/data 组成的 SSE 帧；
// 5. 如果配置了 flusher，则每帧写出后立即刷新。
func (w *Writer) Write(event Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil {
		return fmt.Errorf("writer output is nil")
	}
	event.Sequence = w.sequence.Add(1)
	if event.MessageID == "" {
		event.MessageID = w.messageID
	}
	byts, err := w.marshalEvent(event)
	if err != nil {
		return err
	}
	if _, err = w.out.Write(byts); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// Emit 以“事件类型 + 动作 + 选项”的方式构造并直接写出事件。
//
// 该方法适合在业务层快速发射一帧事件，减少手写 Event 字面量。
func (w *Writer) Emit(eventType EventType, action EventAction, opts ...Option) error {
	w.mu.RLock()
	event := Event{
		Type:      eventType,
		Action:    action,
		MessageID: w.messageID,
	}
	w.mu.RUnlock()
	for _, opt := range opts {
		if opt != nil {
			opt(&event)
		}
	}
	return w.Write(event)
}

// Event 仅构造事件对象，不执行写出。
//
// 适合：
// - 需要先构造再缓存；
// - 需要先转换再决定是否发送；
// - 单元测试中仅验证事件内容。
func (w *Writer) Event(eventType EventType, action EventAction, opts ...Option) Event {
	w.mu.RLock()
	event := Event{
		Type:      eventType,
		Action:    action,
		MessageID: w.messageID,
	}
	w.mu.RUnlock()
	for _, opt := range opts {
		if opt != nil {
			opt(&event)
		}
	}
	return event
}

func (w *Writer) marshalEvent(event Event) ([]byte, error) {
	switch w.format {
	case "", FormatNDJSON:
		payload, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		return append(payload, '\n'), nil
	case FormatSSE:
		return marshalSSEEvent(event)
	default:
		return nil, fmt.Errorf("unsupported stream format: %s", w.format)
	}
}

func (w *Writer) applyStreamHeaders() {
	if w.ctx == nil {
		return
	}
	switch w.format {
	case "", FormatNDJSON:
		SetEventStreamHeaders(w.ctx.Writer)
		// w.ctx.Writer.Header().Set("Content-Type", "application/x-ndjson")
		w.ctx.Writer.Header().Set("Content-Type", "application/json")
	case FormatSSE:
		SetEventStreamHeaders(w.ctx.Writer)
	default:
		SetEventStreamHeaders(w.ctx.Writer)
	}
}

func marshalSSEEvent(event Event) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, payload)
	return []byte(frame), nil
}

func SetEventStreamHeaders(c http.ResponseWriter) {
	c.Header().Set("Content-Type", "text/event-stream")
	c.Header().Set("Cache-Control", "no-cache")
	c.Header().Set("Connection", "keep-alive")
	c.Header().Set("Transfer-Encoding", "chunked")
	c.Header().Set("X-Accel-Buffering", "no")
}
