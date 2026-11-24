package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileAuditSink 把审计事件异步写成一行一条的 JSON 日志。
//
// 这是 AuditSink 的内置实现，对应 Roadmap §4.2 的方案 B：
// 请求线程只做一次 channel 投递，落盘由后台 goroutine 批量完成，
// 下载路径上近似没有额外开销。
//
// 为什么不是同步写库（方案 A）：那会给每次下载加一次写入，
// 且审计后端抖动直接变成下载失败。为什么不是复用 Metrics（方案 C）：
// 指标接口不返回错误，丢了就是丢了，不满足合规。
//
// 缓冲满时**拒绝**而不是丢弃——由上层的 access.audit_fail_closed
// 决定这次拒绝是让请求失败还是降级成丢弃。这个决策不该埋在这里：
// 同一个实现要同时服务合规与非合规两种部署。
type FileAuditSink struct {
	events chan *AuditEvent
	done   chan struct{}
	closed chan struct{}

	mu      sync.Mutex
	file    *os.File
	writer  *bufio.Writer
	dropped int64

	flushEvery  time.Duration
	syncOnFlush bool
}

// FileAuditOptions 文件审计的参数。
type FileAuditOptions struct {
	// Path 日志文件路径，必填。
	Path string
	// BufferSize 缓冲的事件条数，<=0 取 DefaultAuditBuffer。
	//
	// 这个值直接决定 fail-closed 部署的可用性：缓冲越小，
	// 磁盘一卡顿就越快开始拒绝下载。
	BufferSize int
	// FlushEvery 批量落盘的间隔，<=0 取 DefaultAuditFlushEvery。
	FlushEvery time.Duration
	// SyncOnFlush 每次落盘是否 fsync。
	//
	// 打开则进程崩溃最多丢一个 flush 周期内的事件，代价是每个周期
	// 一次磁盘同步。合规场景应当打开——"崩溃时丢了最后几条"
	// 在审计语境下不是一句可以接受的解释。
	SyncOnFlush bool
}

const (
	// DefaultAuditBuffer 默认缓冲条数。
	DefaultAuditBuffer = 4096
	// DefaultAuditFlushEvery 默认落盘间隔。
	DefaultAuditFlushEvery = time.Second
)

// NewFileAuditSink 打开（或创建）审计日志并启动后台落盘。
//
// 文件以 append 方式打开，权限 0600：审计日志里有用户 ID、IP 与
// 完整的访问轨迹，它本身就是一份需要保护的数据。
func NewFileAuditSink(opts FileAuditOptions) (*FileAuditSink, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("eresource: audit log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	bufferSize := opts.BufferSize
	if bufferSize <= 0 {
		bufferSize = DefaultAuditBuffer
	}
	flushEvery := opts.FlushEvery
	if flushEvery <= 0 {
		flushEvery = DefaultAuditFlushEvery
	}
	sink := &FileAuditSink{
		events:      make(chan *AuditEvent, bufferSize),
		done:        make(chan struct{}),
		closed:      make(chan struct{}),
		file:        file,
		writer:      bufio.NewWriter(file),
		flushEvery:  flushEvery,
		syncOnFlush: opts.SyncOnFlush,
	}
	go sink.run()
	return sink, nil
}

// Record 把事件投进缓冲。
//
// 非阻塞：满了立刻返回 ErrAuditRejected，而不是等着写进去。
// 阻塞等待会让一次磁盘抖动变成所有下载请求一起卡住——
// 那比拒绝几次下载严重得多。
func (s *FileAuditSink) Record(_ context.Context, event *AuditEvent) error {
	select {
	case <-s.closed:
		return ErrAuditRejected
	default:
	}
	select {
	case s.events <- event:
		return nil
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
		return ErrAuditRejected
	}
}

// Dropped 返回被拒绝的事件数，供监控告警。
//
// 这个数字非零就说明审计正在丢事件（或在 fail-closed 下正在拒绝下载），
// 它应当是一条告警而不是一个需要有人想起来去看的指标。
func (s *FileAuditSink) Dropped() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *FileAuditSink) run() {
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case event := <-s.events:
			s.write(event)
		case <-ticker.C:
			s.flush()
		case <-s.done:
			// 退出前把缓冲里剩下的全部写完：这里不 drain 的话，
			// 一次正常的优雅关闭就会丢掉最后几千条审计。
			for {
				select {
				case event := <-s.events:
					s.write(event)
					continue
				default:
				}
				break
			}
			s.flush()
			s.mu.Lock()
			_ = s.file.Close()
			s.mu.Unlock()
			close(s.closed)
			return
		}
	}
}

func (s *FileAuditSink) write(event *AuditEvent) {
	// 时间戳在这里兜底补一次。Manager.Audit 已经补过，
	// 但这个 sink 是导出的，调用方完全可能直接拿去用——
	// 一条没有时间的审计记录在合规场景里等于没有。
	if event.At.IsZero() {
		event.At = time.Now()
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.writer.Write(raw)
	_ = s.writer.WriteByte('\n')
}

func (s *FileAuditSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writer.Flush(); err != nil {
		return
	}
	if s.syncOnFlush {
		_ = s.file.Sync()
	}
}

// Close 停止后台落盘并把缓冲里剩下的事件全部写完。
// 重复调用是安全的。
func (s *FileAuditSink) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	<-s.closed
	return nil
}

var _ AuditSink = (*FileAuditSink)(nil)
