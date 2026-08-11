package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zzjha-cn/gKit/pkg/asr"
)

// 这里只验证「会话建立阶段」的重试与上游存活检测，不需要真实百炼凭据。

type fakeUpstream struct {
	srv *httptest.Server
	// attempts 收到 run-task 的次数，即 Start 的实际尝试次数。
	attempts atomic.Int32
	// heartbeatOn 记录最后一次 run-task 的 parameters.heartbeat。
	heartbeatOn atomic.Bool
}

// newUpstream 起一个假上游。onRunTask 决定第 n 次 run-task 如何应答，
// 返回 false 表示直接断开连接（模拟网络抖动）。
func newUpstream(t *testing.T, onRunTask func(conn *websocket.Conn, taskID string, attempt int32) bool) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg struct {
				Header struct {
					Action string `json:"action"`
					TaskID string `json:"task_id"`
				} `json:"header"`
				Payload struct {
					Parameters struct {
						Heartbeat bool `json:"heartbeat"`
					} `json:"parameters"`
				} `json:"payload"`
			}
			if json.Unmarshal(data, &msg) != nil || msg.Header.Action != "run-task" {
				continue
			}
			f.heartbeatOn.Store(msg.Payload.Parameters.Heartbeat)
			if !onRunTask(conn, msg.Header.TaskID, f.attempts.Add(1)) {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func writeTaskStarted(conn *websocket.Conn, taskID string) bool {
	_ = conn.WriteJSON(map[string]any{
		"header":  map[string]any{"task_id": taskID, "event": "task-started"},
		"payload": map[string]any{},
	})
	return true
}

func writeTaskFailed(conn *websocket.Conn, taskID, code string) bool {
	_ = conn.WriteJSON(map[string]any{
		"header": map[string]any{
			"task_id": taskID, "event": "task-failed",
			"error_code": code, "error_message": "boom",
		},
		"payload": map[string]any{},
	})
	return false
}

func testRecognizer(endpoint string, tune func(*Config)) *Recognizer {
	cfg := Config{
		APIKey:      "test-key",
		Endpoint:    endpoint,
		DialTimeout: time.Second,
	}
	if tune != nil {
		tune(&cfg)
	}
	return New(cfg)
}

// 上游瞬时故障（InternalError）应该退避重来，并在恢复后成功。
func TestStart_RetryOnTransientUpstreamFailure(t *testing.T) {
	up := newUpstream(t, func(conn *websocket.Conn, taskID string, attempt int32) bool {
		if attempt < 3 {
			return writeTaskFailed(conn, taskID, "InternalError.Algo")
		}
		return writeTaskStarted(conn, taskID)
	})

	r := testRecognizer(up.wsURL(), nil)
	sess, err := r.Start(context.Background(), asr.RecognizeOption{SampleRate: 16000})
	if err != nil {
		t.Fatalf("重试后仍失败: %v", err)
	}
	defer sess.Close()

	if got := up.attempts.Load(); got != 3 {
		t.Errorf("期望尝试 3 次，实际 %d", got)
	}
	// heartbeat 必须开启，否则上游读 deadline 会误杀静音期的正常会话。
	if !up.heartbeatOn.Load() {
		t.Error("run-task 没有开启 heartbeat")
	}
}

// 确定性错误（参数非法）重试没有意义，必须一次就放弃。
func TestStart_NoRetryOnDeterministicFailure(t *testing.T) {
	up := newUpstream(t, func(conn *websocket.Conn, taskID string, _ int32) bool {
		return writeTaskFailed(conn, taskID, "InvalidParameter")
	})

	r := testRecognizer(up.wsURL(), nil)
	if _, err := r.Start(context.Background(), asr.RecognizeOption{SampleRate: 16000}); err == nil {
		t.Fatal("期望失败")
	}
	if got := up.attempts.Load(); got != 1 {
		t.Errorf("不可重试的错误只应尝试 1 次，实际 %d", got)
	}
}

// 连接建立后就一直不说话，读 deadline 必须把这路会话判死，
// 否则 TCP 半开时 ReadMessage 会永久阻塞。
func TestSession_UpstreamReadTimeout(t *testing.T) {
	up := newUpstream(t, func(conn *websocket.Conn, taskID string, _ int32) bool {
		writeTaskStarted(conn, taskID)
		// 之后一个字都不发，也不关连接。
		return true
	})

	r := testRecognizer(up.wsURL(), func(c *Config) {
		c.ReadTimeout = 300 * time.Millisecond
	})
	sess, err := r.Start(context.Background(), asr.RecognizeOption{SampleRate: 16000})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer sess.Close()

	deadline := time.After(3 * time.Second)
	var failed bool
	for !failed {
		select {
		case event, ok := <-sess.Events.EventCh:
			if !ok {
				goto done
			}
			if event.Type == asr.EventASRFailed {
				failed = true
			}
		case <-deadline:
			t.Fatal("读 deadline 没有生效，会话一直挂着")
		}
	}
done:
	if !failed {
		t.Fatal("期望收到 failed 事件")
	}
}

// 总预算必须能截断重试，不能让前端无限期等 ready。
func TestStart_RespectsStartTimeout(t *testing.T) {
	up := newUpstream(t, func(conn *websocket.Conn, taskID string, _ int32) bool {
		return writeTaskFailed(conn, taskID, "InternalError")
	})

	r := testRecognizer(up.wsURL(), func(c *Config) {
		c.StartAttempts = 100 // 次数放开，全靠总预算截断
		c.StartTimeout = 3 * time.Second
	})

	begin := time.Now()
	if _, err := r.Start(context.Background(), asr.RecognizeOption{SampleRate: 16000}); err == nil {
		t.Fatal("期望失败")
	}
	cost := time.Since(begin)
	if cost > 4*time.Second {
		t.Errorf("超出总预算太多: %v", cost)
	}
	// 退避是递增的，预算内不可能跑满 100 次；也不该一次就放弃。
	attempts := up.attempts.Load()
	if attempts < 2 || attempts > 10 {
		t.Errorf("重试次数不合理: %d（耗时 %v）", attempts, cost)
	}
}
