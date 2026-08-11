package asr

import (
	"sync"
	"time"
)

// StreamRecognizeResult 识别事件流。
// 约定：只有 provider 的单条读 goroutine 会 Publish/Finish；
// 消费者放弃接收时必须调用 Abort，否则 provider 可能阻塞在满队列上。
type StreamRecognizeResult struct {
	EventCh chan ASREvent
	DoneCh  chan struct{}

	abort     chan struct{}
	once      sync.Once
	abortOnce sync.Once
}

func NewStreamRecognizeResult() *StreamRecognizeResult {
	return &StreamRecognizeResult{
		EventCh: make(chan ASREvent, 32),
		DoneCh:  make(chan struct{}),
		abort:   make(chan struct{}),
	}
}

// Publish 投递事件到业务端；消费者已放弃时直接丢弃，不阻塞。
func (s *StreamRecognizeResult) Publish(event ASREvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	select {
	case s.EventCh <- event:
	case <-s.abort:
	}
}

// Finish 结束事件流，只能由 provider 调用一次。
func (s *StreamRecognizeResult) Finish() {
	s.once.Do(func() {
		close(s.EventCh)
		close(s.DoneCh)
	})
}

// Abort 由消费者调用，声明不再接收事件。
func (s *StreamRecognizeResult) Abort() {
	s.abortOnce.Do(func() {
		close(s.abort)
	})
}
