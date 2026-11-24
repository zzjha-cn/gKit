package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
)

func TestFileAuditSinkWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "access.log")
	sink, err := manager.NewFileAuditSink(manager.FileAuditOptions{Path: path, SyncOnFlush: true})
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err = sink.Record(ctx, &manager.AuditEvent{
			Action: manager.AuditActionDownload, ResourceID: "r-1", UserID: "u-1",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// Close 必须把缓冲里剩下的写完，否则一次正常的优雅关闭就会丢审计。
	if err = sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer file.Close()
	lines := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event manager.AuditEvent
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("line %d is not valid json: %v", lines+1, err)
		}
		if event.ResourceID != "r-1" || event.At.IsZero() {
			t.Errorf("unexpected event: %+v", event)
		}
		lines++
	}
	if lines != 3 {
		t.Fatalf("wrote %d lines, want 3", lines)
	}
}

// 缓冲满了要立刻拒绝而不是阻塞：阻塞会把一次磁盘抖动
// 变成所有下载一起卡住，比拒绝几次下载严重得多。
func TestFileAuditSinkRejectsWhenBufferIsFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	sink, err := manager.NewFileAuditSink(manager.FileAuditOptions{
		Path:       path,
		BufferSize: 1,
		// 落盘间隔拉到很长，确保后台来不及消费，缓冲必然被填满。
		FlushEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	defer sink.Close()

	ctx := context.Background()
	rejected := 0
	for i := 0; i < 200; i++ {
		if recErr := sink.Record(ctx, &manager.AuditEvent{ResourceID: "r"}); recErr != nil {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("a 1-slot buffer never rejected across 200 events")
	}
	if sink.Dropped() == 0 {
		t.Error("rejected events were not counted for alerting")
	}
}

// 配了路径就自动装配，并且真的把下载审计写进了文件。
func TestAuditLogPathWiresFileSink(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir)
	logPath := filepath.Join(dir, "audit.log")
	cfg.Access.AuditLogPath = logPath

	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	result := upload(t, svc, "doc.png", pngBytes())

	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	if rec := serve(router, req); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	svc.Close() // 冲刷缓冲

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(result.Resource.ID)) {
		t.Fatalf("audit log does not mention the downloaded resource:\n%s", raw)
	}
}

func TestMetricsRecordUploads(t *testing.T) {
	metrics := newRecordingMetrics()
	svc := newServiceWithRoot(t, nil, metrics)
	body := pngBytes()
	upload(t, svc.Service, "tracked.png", body)

	if got := metrics.get(manager.MetricUploadTotal); got != 1 {
		t.Errorf("%s = %d, want 1", manager.MetricUploadTotal, got)
	}
	if got := metrics.get(manager.MetricUploadBytes); got != int64(len(body)) {
		t.Errorf("%s = %d, want %d", manager.MetricUploadBytes, got, len(body))
	}
	if got := metrics.label(manager.MetricUploadTotal, "biz_type"); got != "image" {
		t.Errorf("biz_type label = %q, want image", got)
	}
}

// 幂等重试不该被算成第二次写入——那会让「上传了多少字节」这个数字虚高。
func TestMetricsDistinguishRetriesFromWrites(t *testing.T) {
	metrics := newRecordingMetrics()
	svc := newServiceWithRoot(t, nil, metrics)
	body := pngBytes()

	uploadIdempotent(t, svc.Service, "retry.png", body, "key-1")
	uploadIdempotent(t, svc.Service, "retry.png", body, "key-1")

	if got := metrics.get(manager.MetricUploadTotal); got != 2 {
		t.Errorf("%s = %d, want 2 attempts", manager.MetricUploadTotal, got)
	}
	if got := metrics.get(manager.MetricUploadBytes); got != int64(len(body)) {
		t.Errorf("%s = %d, want only the first write counted (%d)",
			manager.MetricUploadBytes, got, len(body))
	}
}

func TestMetricsRecordDedupHits(t *testing.T) {
	metrics := newRecordingMetrics()
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.EnableDedup = true
	}, metrics)
	body := pngBytes()
	upload(t, svc.Service, "a.png", body)
	upload(t, svc.Service, "b.png", body)

	if got := metrics.get(manager.MetricDedupHit); got != 1 {
		t.Errorf("%s = %d, want 1", manager.MetricDedupHit, got)
	}
}

// 不注入 Metrics 时埋点必须是彻底的空操作，不能 panic。
func TestMetricsAreOptional(t *testing.T) {
	svc := newTestService(t, nil)
	upload(t, svc, "no-metrics.png", pngBytes())
}

func TestAuditRecordsDownloads(t *testing.T) {
	audit := &capturingAudit{}
	svc := newAuditService(t, audit, nil)
	result := upload(t, svc, "doc.png", pngBytes())

	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	events := audit.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Action != manager.AuditActionDownload {
		t.Errorf("action = %q, want %q", events[0].Action, manager.AuditActionDownload)
	}
	if events[0].ResourceID != result.Resource.ID {
		t.Errorf("resource id = %q, want %q", events[0].ResourceID, result.Resource.ID)
	}
}

// fail-closed 的全部意义：审计收不下就不给看。
// 默认关闭时同样一条审计都没记下，但内容照给——这两种行为差别巨大，
// 必须由配置明确表态。
func TestAuditFailClosedBlocksDownload(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failClosed bool
		wantCode   int
	}{
		{"fail_closed", true, http.StatusServiceUnavailable},
		{"drop_and_serve", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := &capturingAudit{reject: true}
			svc := newAuditService(t, audit, func(c *config.Config) {
				c.Access.AuditFailClosed = tc.failClosed
			})
			result := upload(t, svc, "doc.png", pngBytes())
			router := newTestRouter(t, svc)
			req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
			rec := serve(router, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

type recordingMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	labels map[string]map[string]string
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{
		counts: map[string]int64{},
		labels: map[string]map[string]string{},
	}
}

func (r *recordingMetrics) Count(name string, value int64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[name] += value
	r.labels[name] = labels
}

func (r *recordingMetrics) Observe(string, float64, map[string]string) {}

func (r *recordingMetrics) get(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

func (r *recordingMetrics) label(name, key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.labels[name][key]
}

type capturingAudit struct {
	mu     sync.Mutex
	events []*manager.AuditEvent
	reject bool
}

func (a *capturingAudit) Record(_ context.Context, event *manager.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reject {
		return manager.ErrAuditRejected
	}
	a.events = append(a.events, event)
	return nil
}

func (a *capturingAudit) all() []*manager.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*manager.AuditEvent(nil), a.events...)
}

func newAuditService(t *testing.T, sink manager.AuditSink, tune func(*config.Config)) *eresource.Service {
	t.Helper()
	cfg := baseConfig(t.TempDir())
	if tune != nil {
		tune(&cfg)
	}
	svc, err := eresource.New(cfg, eresource.WithAuditSink(sink))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}
