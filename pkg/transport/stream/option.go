package stream

// Option 表示对 Event 的一项可组合变更。
//
// 通过 Option 模式，可以让事件构造调用更清晰，避免不同场景重复拼装 Event 结构。
type Option func(*Event)

// WithMessageID 显式指定事件的消息 ID。
//
// 当需要覆盖 Writer 默认 messageID 时使用。
func WithMessageID(messageID string) Option {
	return func(event *Event) {
		event.MessageID = messageID
	}
}

// WithData 为事件设置统一负载数据。
func WithData(data any) Option {
	return func(event *Event) {
		event.Data = data
	}
}

// WithDelta 为事件设置增量内容。
func WithDelta(delta string) Option {
	return func(event *Event) {
		event.Data = delta
	}
}

// WithContent 为事件设置完整正文内容。
func WithContent(content string) Option {
	return func(event *Event) {
		event.Data = content
	}
}

// WithReasoning 为事件设置完整 reasoning 内容。
func WithReasoning(reasoning string) Option {
	return func(event *Event) {
		event.Data = reasoning
	}
}

// WithMeta 为事件设置附加键值元信息。
func WithMeta(meta map[string]string) Option {
	return func(event *Event) {
		event.Meta = meta
	}
}

// WithError 从 error 对象提取错误信息并写入事件。
func WithError(err error) Option {
	return func(event *Event) {
		if err != nil {
			event.Err = err.Error()
		}
	}
}

// WithErrorMessage 直接设置错误消息字符串。
func WithErrorMessage(msg string) Option {
	return func(event *Event) {
		event.Err = msg
	}
}
