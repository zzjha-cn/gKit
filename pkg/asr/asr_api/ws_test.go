package asr_api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	asrsrv "github.com/zzjha-cn/gKit/pkg/asr/asr_srv"
	"github.com/zzjha-cn/gKit/pkg/logger"
)

// 用假的上游 WebSocket 服务端跑通 前端 -> 网关 -> 上游 的完整链路，
// 不需要真实百炼凭据。

type upstreamHeader struct {
	Action string `json:"action"`
	TaskID string `json:"task_id"`
}

type upstreamMsg struct {
	Header upstreamHeader `json:"header"`
}

// newFakeUpstream 模拟百炼的双工协议：
// run-task -> task-started；收到音频回 result-generated；finish-task -> task-finished。
func newFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("上游未收到正确的 Authorization: %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var taskID string
		audioFrames := 0
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}

			if mt == websocket.BinaryMessage {
				audioFrames++
				switch audioFrames {
				case 1:
					// heartbeat 结果必须被网关过滤掉，不能透传给前端
					_ = conn.WriteJSON(resultGenerated(taskID, 0, "", true, false))
				case 2:
					_ = conn.WriteJSON(resultGenerated(taskID, 1, "今天天气", false, false))
				case 3:
					_ = conn.WriteJSON(resultGenerated(taskID, 1, "今天天气不错。", false, true))
				}
				continue
			}

			var msg upstreamMsg
			if err = json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Header.Action {
			case "run-task":
				taskID = msg.Header.TaskID
				_ = conn.WriteJSON(map[string]any{
					"header":  map[string]any{"task_id": taskID, "event": "task-started"},
					"payload": map[string]any{},
				})
			case "finish-task":
				_ = conn.WriteJSON(map[string]any{
					"header": map[string]any{"task_id": taskID, "event": "task-finished"},
					"payload": map[string]any{
						"output": map[string]any{},
						"usage":  map[string]any{"duration": 7},
					},
				})
				return
			}
		}
	}))
}

func resultGenerated(taskID string, sentenceID int, text string, heartbeat, end bool) map[string]any {
	payload := map[string]any{
		"output": map[string]any{
			"sentence": map[string]any{
				"begin_time":   120,
				"end_time":     980,
				"text":         text,
				"heartbeat":    heartbeat,
				"sentence_end": end,
				"sentence_id":  sentenceID,
			},
		},
	}
	if end {
		payload["usage"] = map[string]any{"duration": 3}
	}
	return map[string]any{
		"header":  map[string]any{"task_id": taskID, "event": "result-generated"},
		"payload": payload,
	}
}

type emitEnvelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// withRequestTimeout 复现线上 HTTP 框架的请求超时（如 ehttp.TimeOut）：
// 请求 ctx 在 d 之后被取消。
//
// 这是一道陷阱：接入层若把 r.Context() 递给中转层，连接会在 d 之后被无故关闭。
// 下面的用例特意在这个时长之后才继续说话，走不通就说明 ctx 传错了。
func withRequestTimeout(d time.Duration, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		h(w, r.WithContext(ctx))
	}
}

// startGateway 起一个只挂了 ASR 路由的测试网关，返回前端要连的 ws 地址。
func startGateway(t *testing.T, cfg *asrsrv.ASRCfg) string {
	t.Helper()
	mgr := asrsrv.InitManager(context.Background(), cfg)
	if mgr == nil {
		t.Fatal("InitManager 返回 nil")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.CloseAll(ctx)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/asr/v1/stream", withRequestTimeout(time.Second, StreamV1))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	return "ws" + strings.TrimPrefix(gw.URL, "http") + "/asr/v1/stream"
}

// readEvent 读一条下行事件，超时即失败。
func readEvent(t *testing.T, c *websocket.Conn, within time.Duration) emitEnvelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("没有读到下行事件: %v", err)
	}
	var env emitEnvelope
	if err = json.Unmarshal(data, &env); err != nil {
		t.Fatalf("下行不是合法 JSON: %s", data)
	}
	return env
}

func dial(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial 网关失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestStreamV1_EndToEnd(t *testing.T) {
	upstream := newFakeUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		Region:      "cn-beijing",
		WorkspaceID: "ws-test",
		APIKey:      "test-key",
		Model:       "qwen-audio-3.0-asr-flash-streaming",
		Endpoint:    "ws" + strings.TrimPrefix(upstream.URL, "http"),
		Token:       "secret",
	})

	client := dial(t, wsURL+"?token=secret")

	if err := client.WriteJSON(map[string]any{"action": "start", "sample_rate": 16000}); err != nil {
		t.Fatalf("发送 start 失败: %v", err)
	}

	// 等待超过 withRequestTimeout 的时长，确认连接没有被请求 ctx 带走。
	time.Sleep(1500 * time.Millisecond)

	frame := make([]byte, 3200)
	for i := 0; i < 3; i++ {
		if err := client.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("发送音频失败: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := client.WriteJSON(map[string]any{"action": "stop"}); err != nil {
		t.Fatalf("发送 stop 失败: %v", err)
	}

	var events []string
	var lastFinal string
	var doneDuration int

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, data, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("读下行消息失败: %v (已收到 %v)", err, events)
		}
		var env emitEnvelope
		if err = json.Unmarshal(data, &env); err != nil {
			t.Fatalf("下行不是合法 JSON: %s", data)
		}
		events = append(events, env.Event)

		switch env.Event {
		case "partial", "final":
			var s asrsrv.SentenceData
			if err = json.Unmarshal(env.Data, &s); err != nil {
				t.Fatalf("解析句子失败: %v", err)
			}
			if s.Text == "" {
				t.Fatalf("收到空文本，heartbeat 结果没有被过滤: %s", data)
			}
			if env.Event == "final" {
				lastFinal = s.Text
			}
			logger.Infof("内容 %s", s.Text)
		case "error":
			t.Fatalf("收到 error 事件: %s", data)
		case "done":
			var d asrsrv.DoneData
			_ = json.Unmarshal(env.Data, &d)
			doneDuration = d.DurationSec
		}
		if env.Event == "done" {
			break
		}
	}

	if events[0] != "ready" {
		t.Errorf("首个事件应为 ready，实际 %v", events)
	}
	if !contains(events, "partial") {
		t.Errorf("缺少 partial 事件: %v", events)
	}
	if lastFinal != "今天天气不错。" {
		t.Errorf("final 文本不对: %q", lastFinal)
	}
	if doneDuration != 7 {
		t.Errorf("done 的计费时长不对: %d", doneDuration)
	}
}

// token 不对必须在**握手阶段**就拒掉，而不是升级成功后再关连接。
func TestStreamV1_BadToken(t *testing.T) {
	upstream := newFakeUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		WorkspaceID: "ws-test",
		APIKey:      "test-key",
		Endpoint:    "ws" + strings.TrimPrefix(upstream.URL, "http"),
		Token:       "secret",
	})

	_, resp, err := websocket.DefaultDialer.Dial(wsURL+"?token=wrong", nil)
	if err == nil {
		t.Fatal("错误 token 不应该握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %v", resp)
	}
}

// Authorization: Bearer 与 ?token= 等价，两条路都得通。
func TestStreamV1_BearerToken(t *testing.T) {
	upstream := newFakeUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(upstream.URL, "http"),
		Token:    "secret",
	})

	header := http.Header{}
	header.Set("Authorization", "Bearer secret")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("Bearer token 应该握手成功: %v", err)
	}
	defer client.Close()

	if err = client.WriteJSON(map[string]any{"action": "start"}); err != nil {
		t.Fatalf("发送 start 失败: %v", err)
	}
	if env := readEvent(t, client, 5*time.Second); env.Event != "ready" {
		t.Fatalf("期望 ready，实际 %s", env.Event)
	}
}

// 名额占满时必须在升级前回 503：升级成功再关连接，前端只看到「连上又断了」。
func TestStreamV1_ConcurrencyLimit(t *testing.T) {
	upstream := newFakeUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		APIKey:        "test-key",
		Endpoint:      "ws" + strings.TrimPrefix(upstream.URL, "http"),
		MaxConcurrent: 1,
	})

	first := dial(t, wsURL)
	if err := first.WriteJSON(map[string]any{"action": "start"}); err != nil {
		t.Fatalf("发送 start 失败: %v", err)
	}
	if env := readEvent(t, first, 5*time.Second); env.Event != "ready" {
		t.Fatalf("期望 ready，实际 %s", env.Event)
	}

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("名额已满时不应该握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %v", resp)
	}
}

// newSilentUpstream 只回 task-started，收到 finish-task 后装死不回 task-finished。
// 用来验证网关的收尾兜底：上游不应答时必须自己把会话拖走。
func newSilentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				continue
			}
			var msg upstreamMsg
			if err = json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.Header.Action == "run-task" {
				_ = conn.WriteJSON(map[string]any{
					"header":  map[string]any{"task_id": msg.Header.TaskID, "event": "task-started"},
					"payload": map[string]any{},
				})
			}
			// finish-task 故意不应答。
		}
	}))
}

// newBailingUpstream 模拟「上游中途退出但没发错误帧」：
// 回完 task-started 就发一个正常关闭帧（1000）走人，始终不给 task-finished。
// 按关闭码判断的话这看起来是正常收尾，实际上是异常终止。
func newBailingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var msg upstreamMsg
			if err = json.Unmarshal(data, &msg); err != nil || msg.Header.Action != "run-task" {
				continue
			}
			_ = conn.WriteJSON(map[string]any{
				"header":  map[string]any{"task_id": msg.Header.TaskID, "event": "task-started"},
				"payload": map[string]any{},
			})
			time.Sleep(100 * time.Millisecond)
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}))
}

// 只连不发 start 的空连接必须被关掉，否则会永久占用一个并发名额。
func TestStreamV1_IdleWithoutStart(t *testing.T) {
	upstream := newFakeUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		APIKey:           "test-key",
		Endpoint:         "ws" + strings.TrimPrefix(upstream.URL, "http"),
		StartWaitSeconds: 1,
	})

	client := dial(t, wsURL)

	// 什么都不发，等网关自己收场。
	env := readEvent(t, client, 5*time.Second)
	if env.Event != "error" {
		t.Fatalf("期望 error 事件，实际 %s", env.Event)
	}
	var e asrsrv.ErrorData
	if err := json.Unmarshal(env.Data, &e); err != nil {
		t.Fatalf("解析 error 失败: %v", err)
	}
	// 没按时 start 是客户端问题，别报成 500 让人去查服务端。
	if e.Code != 400 {
		t.Errorf("期望 code 400，实际 %d (%s)", e.Code, e.Msg)
	}
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("网关应该在报错后关闭连接")
	}
}

// 上游收到 finish-task 后不应答时，网关必须自己兜底结束，不能一直挂着。
func TestStreamV1_UpstreamDrainTimeout(t *testing.T) {
	upstream := newSilentUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		APIKey:           "test-key",
		Endpoint:         "ws" + strings.TrimPrefix(upstream.URL, "http"),
		DrainWaitSeconds: 1,
	})

	client := dial(t, wsURL)

	if err := client.WriteJSON(map[string]any{"action": "start", "sample_rate": 16000}); err != nil {
		t.Fatalf("发送 start 失败: %v", err)
	}
	if env := readEvent(t, client, 5*time.Second); env.Event != "ready" {
		t.Fatalf("期望 ready，实际 %s", env.Event)
	}
	if err := client.WriteJSON(map[string]any{"action": "stop"}); err != nil {
		t.Fatalf("发送 stop 失败: %v", err)
	}

	begin := time.Now()
	if env := readEvent(t, client, 5*time.Second); env.Event != "error" {
		t.Fatalf("期望 error 事件，实际 %s", env.Event)
	}
	if cost := time.Since(begin); cost > 4*time.Second {
		t.Errorf("兜底触发太慢: %v", cost)
	}
}

// 上游中途退出且没发错误帧时，前端必须收到 error，不能静默断开。
//
// 这里客户端故意在 ready 之后一帧音频都不发：有音频的话 upWrite 写失败会顺手
// 报错，掩盖掉真正要验的那条路径（读侧发现上游走人）。
func TestStreamV1_UpstreamVanishes(t *testing.T) {
	upstream := newBailingUpstream(t)
	defer upstream.Close()

	wsURL := startGateway(t, &asrsrv.ASRCfg{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(upstream.URL, "http"),
	})

	client := dial(t, wsURL)

	if err := client.WriteJSON(map[string]any{"action": "start", "sample_rate": 16000}); err != nil {
		t.Fatalf("发送 start 失败: %v", err)
	}
	if env := readEvent(t, client, 5*time.Second); env.Event != "ready" {
		t.Fatalf("期望 ready，实际 %s", env.Event)
	}

	env := readEvent(t, client, 5*time.Second)
	if env.Event != "error" {
		t.Fatalf("上游静默退出后必须收到 error，实际 %s", env.Event)
	}
	var e asrsrv.ErrorData
	if err := json.Unmarshal(env.Data, &e); err != nil {
		t.Fatalf("解析 error 失败: %v", err)
	}
	if e.Code != 500 {
		t.Errorf("期望 code 500，实际 %d", e.Code)
	}
	t.Logf("前端收到的错误: %s", e.Msg)
}

// ASR 未配置时接口应明确回 503，而不是 panic。
func TestStreamV1_NotEnabled(t *testing.T) {
	h := NewWSHandler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/asr/v1/stream", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d", w.Code)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
