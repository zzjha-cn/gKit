// Package asr 定义实时语音识别的供应商无关抽象。
// 具体协议实现放在子包（如 asr/dashscope），上层只依赖本包的接口与事件。
package asr

import (
	"context"
	"time"
)

type ASREventType string

const (
	EventASRReady    ASREventType = "ready"    // 上游任务就绪，可以开始送音频
	EventASRPartial  ASREventType = "partial"  // 句子中间结果
	EventASRFinal    ASREventType = "final"    // 句子定稿
	EventASRFinished ASREventType = "finished" // 任务正常结束
	EventASRFailed   ASREventType = "failed"   // 任务失败
)

// Word 词级时间戳。
type Word struct {
	BeginMs     int64  `json:"begin_ms"`
	EndMs       int64  `json:"end_ms"`
	Text        string `json:"text"`
	Punctuation string `json:"punctuation,omitempty"`
}

// ASREvent 识别过程中的事件，形状对齐 pkg/llm/agent 的流式事件。
type ASREvent struct {
	Type       ASREventType
	SessionID  string
	SentenceID int
	Text       string
	BeginMs    int64
	EndMs      int64
	Words      []Word

	// DurationSec 计费时长，仅 finished 事件有值。
	DurationSec int
	// Code 上游错误码，仅 failed 事件有值。
	Code string
	Err  error

	Timestamp time.Time
}

// RecognizeOption 单次识别任务的参数。
type RecognizeOption struct {
	Model               string
	Format              string // pcm / wav / mp3 / opus / speex / aac / amr
	SampleRate          int
	LanguageHints       []string
	SemanticPunctuation bool
	MaxSentenceSilence  int // 200-6000ms
	VocabularyID        string
	RequestID           string // 贯穿日志的请求标识
}

// Recognizer 实时识别器，一次 Start 对应一路上游会话。
type Recognizer interface {
	Start(ctx context.Context, opt RecognizeOption) (*Session, error)
}

// Session 一路已就绪的识别会话。
// Write 送音频，Finish 通知上游收尾并等待结束，Close 强制释放。
type Session struct {
	ID     string
	Events *StreamRecognizeResult

	// Write 送一帧音频。必须由单个 goroutine 调用。
	Write func(pcm []byte) error
	// Finish 通知上游没有更多音频了，立即返回；
	// 结束时机以事件流里的 finished/failed 或 DoneCh 为准。
	Finish func() error
	// Close 立即释放连接，可重复调用。
	Close func()
}
