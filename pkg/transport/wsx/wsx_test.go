package wsx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testServer 启动一条连接的测试服务端，serve 在独立 goroutine 中运行 Serve。
type testServer struct {
	*httptest.Server
	url    string
	result chan Disconnect
}

func newTestServer(t *testing.T, opt Options, h func(c *Conn) Handler) *testServer {
	t.Helper()
	ts := &testServer{result: make(chan Disconnect, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, UpgradeOptions{Conn: opt})
		if err != nil {
			return
		}
		ts.result <- c.Serve(context.Background(), h(c))
	}))
	ts.Server = srv
	ts.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(srv.Close)
	return ts
}

func echoHandler(c *Conn) Handler {
	return Callbacks{Message: func(c *Conn, mt int, data []byte) { _ = c.Send(mt, data) }}
}

func dial(t *testing.T, url string, opt Options) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), url, DialOptions{Conn: opt})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func waitDisconnect(t *testing.T, ch <-chan Disconnect) Disconnect {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("等待 Disconnect 超时")
		return Disconnect{}
	}
}

func TestWrapValidatesPingInterval(t *testing.T) {
	// 心跳间隔大于读超时的一半，配反了必须在构造时炸掉。
	_, err := Wrap(&websocket.Conn{}, Options{ReadTimeout: 10 * time.Second, PingInterval: 9 * time.Second})
	if !errors.Is(err, ErrPingInterval) {
		t.Fatalf("期望 ErrPingInterval，得到 %v", err)
	}
	if _, err := Wrap(nil, Options{}); !errors.Is(err, ErrNilConn) {
		t.Fatalf("期望 ErrNilConn，得到 %v", err)
	}
}

func TestEchoRoundTrip(t *testing.T) {
	ts := newTestServer(t, Options{WriteTimeout: time.Second}, echoHandler)

	got := make(chan string, 1)
	cli := dial(t, ts.url, Options{WriteTimeout: time.Second})
	go cli.Serve(context.Background(), Callbacks{
		Message: func(c *Conn, mt int, data []byte) { got <- string(data) },
	})

	if err := cli.SendText("ping"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case s := <-got:
		if s != "ping" {
			t.Fatalf("期望 ping，得到 %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到回显")
	}
	_ = cli.Close(websocket.CloseNormalClosure, "bye")
}

func TestCausePeerOnNormalClose(t *testing.T) {
	// 对端发 1000 关闭帧也必须归因为 CausePeer，而不是「正常」被吞掉。
	ts := newTestServer(t, Options{}, echoHandler)

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})
	_ = cli.Close(websocket.CloseNormalClosure, "client done")

	d := waitDisconnect(t, ts.result)
	if d.Cause != CausePeer {
		t.Fatalf("期望 CausePeer，得到 %s (%+v)", d.Cause, d)
	}
	if d.Code != websocket.CloseNormalClosure || d.Reason != "client done" {
		t.Fatalf("关闭码/原因不对: %+v", d)
	}
}

func TestCauseLocal(t *testing.T) {
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		return Callbacks{Open: func(c *Conn) {
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = c.Close(websocket.CloseNormalClosure, "server done")
			}()
		}}
	})

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})

	d := waitDisconnect(t, ts.result)
	if d.Cause != CauseLocal || d.Reason != "server done" {
		t.Fatalf("期望 CauseLocal/server done，得到 %s %+v", d.Cause, d)
	}
	if d.Code != websocket.CloseNormalClosure {
		t.Fatalf("关闭码不对: %+v", d)
	}
}

func TestCauseTimeout(t *testing.T) {
	// 客户端不发任何东西也不回 ping：模拟半开连接，服务端必须在 ReadTimeout 内判死。
	ts := newTestServer(t, Options{ReadTimeout: 200 * time.Millisecond}, echoHandler)

	raw, _, err := websocket.DefaultDialer.Dial(ts.url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	// 吃掉 ping 但不回 pong：gorilla 默认 ping handler 会自动回 pong，
	// 所以这里干脆不读，让服务端收不到任何入站帧。

	d := waitDisconnect(t, ts.result)
	if d.Cause != CauseTimeout {
		t.Fatalf("期望 CauseTimeout，得到 %s (%+v)", d.Cause, d)
	}
}

func TestCauseProtocolOnReadLimit(t *testing.T) {
	ts := newTestServer(t, Options{ReadLimit: 16}, echoHandler)

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})
	if err := cli.SendText(strings.Repeat("x", 128)); err != nil {
		t.Fatalf("send: %v", err)
	}

	d := waitDisconnect(t, ts.result)
	if d.Cause != CauseProtocol || d.Code != websocket.CloseMessageTooBig {
		t.Fatalf("期望 CauseProtocol/1009，得到 %s %+v", d.Cause, d)
	}
}

func TestHandlerPanicDoesNotKillConn(t *testing.T) {
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		return Callbacks{Message: func(c *Conn, mt int, data []byte) {
			if string(data) == "boom" {
				panic("boom")
			}
			_ = c.Send(mt, data)
		}}
	})

	got := make(chan string, 1)
	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{
		Message: func(c *Conn, mt int, data []byte) { got <- string(data) },
	})

	if err := cli.SendText("boom"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := cli.SendText("alive"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case s := <-got:
		if s != "alive" {
			t.Fatalf("期望 alive，得到 %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic 打穿了连接")
	}
}

func TestConcurrentSend(t *testing.T) {
	ts := newTestServer(t, Options{WriteTimeout: 2 * time.Second}, func(c *Conn) Handler {
		return Callbacks{}
	})

	cli := dial(t, ts.url, Options{WriteTimeout: 2 * time.Second})
	go cli.Serve(context.Background(), Callbacks{})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = cli.SendText(strings.Repeat("a", 64))
			}
		}()
	}
	wg.Wait()
	_ = cli.Close(websocket.CloseNormalClosure, "done")
}

func TestOnDisconnectCalledOnce(t *testing.T) {
	var count int
	var mu sync.Mutex
	done := make(chan struct{})
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		return Callbacks{
			Open: func(c *Conn) {
				go func() {
					// 三方同时结束连接，收敛只能发生一次。
					_ = c.Close(websocket.CloseNormalClosure, "a")
					_ = c.Close(websocket.CloseNormalClosure, "b")
					c.shutdown()
				}()
			},
			Disconnect: func(c *Conn, d Disconnect) {
				mu.Lock()
				count++
				mu.Unlock()
				close(done)
			},
		}
	})

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect 未被调用")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("OnDisconnect 调用了 %d 次", count)
	}
}

func TestRouterExpectWait(t *testing.T) {
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		return Callbacks{Message: func(c *Conn, mt int, data []byte) {
			_ = c.SendJSON(map[string]string{"event": "task-started", "id": "1"})
		}}
	})

	cli := dial(t, ts.url, Options{})
	r := &Router{}
	go cli.Serve(context.Background(), r.Handler(nil))

	w := r.Expect("task-started", "task-failed")
	defer w.Cancel()
	if err := cli.SendJSON(map[string]string{"event": "run"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	name, data, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if name != "task-started" || !strings.Contains(string(data), `"id":"1"`) {
		t.Fatalf("命中错误: %s %s", name, data)
	}
	_ = cli.Close(websocket.CloseNormalClosure, "done")
}

func TestWaitWokenByDisconnect(t *testing.T) {
	// 连接断开时等待者必须立刻返回，不能挂到自己的 ctx 超时。
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		return Callbacks{Open: func(c *Conn) {
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = c.Close(websocket.CloseGoingAway, "server gone")
			}()
		}}
	})

	cli := dial(t, ts.url, Options{})
	r := &Router{}
	go cli.Serve(context.Background(), r.Handler(nil))

	w := r.Expect("never")
	defer w.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := w.Wait(ctx); !errors.Is(err, ErrWaitClosed) {
		t.Fatalf("期望 ErrWaitClosed，得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("等待者被挂到 ctx 超时才醒：%s", elapsed)
	}
}

func TestRouterFallback(t *testing.T) {
	r := &Router{EventKey: "type"}
	hit := make(chan string, 4)
	r.On("a", func(c *Conn, data []byte) { hit <- "handler" })
	h := r.Handler(func(c *Conn, mt int, data []byte) { hit <- "fallback" })

	h.OnMessage(nil, websocket.TextMessage, []byte(`{"type":"a"}`))
	h.OnMessage(nil, websocket.TextMessage, []byte(`{"type":"b"}`))
	h.OnMessage(nil, websocket.TextMessage, []byte(`not json`))

	want := []string{"handler", "fallback", "fallback"}
	for _, w := range want {
		select {
		case got := <-hit:
			if got != w {
				t.Fatalf("期望 %s，得到 %s", w, got)
			}
		default:
			t.Fatalf("缺少一次分发：%s", w)
		}
	}
}

func TestCtxCancelClosesGracefully(t *testing.T) {
	ts := newTestServer(t, Options{}, echoHandler)

	cli := dial(t, ts.url, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan Disconnect, 1)
	go func() { res <- cli.Serve(ctx, Callbacks{}) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	d := waitDisconnect(t, res)
	if d.Cause != CauseLocal {
		t.Fatalf("期望 CauseLocal，得到 %s %+v", d.Cause, d)
	}
	// 服务端应看到对端关闭。
	if sd := waitDisconnect(t, ts.result); sd.Cause != CausePeer {
		t.Fatalf("服务端期望 CausePeer，得到 %s", sd.Cause)
	}
}

func TestActivateGrace(t *testing.T) {
	notice := make(chan string, 1)
	ts := newTestServer(t, Options{ActivateGrace: 150 * time.Millisecond}, echoHandler)

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{
		Message: func(c *Conn, mt int, data []byte) { notice <- string(data) },
	})

	d := waitDisconnect(t, ts.result)
	if d.Cause != CauseLocal || !strings.Contains(d.Reason, "activation grace") {
		t.Fatalf("期望时间闸触发，得到 %s %+v", d.Cause, d)
	}
	// 闸门关闭前必须先给对端一条可理解的消息，而不是静默断开。
	select {
	case msg := <-notice:
		if !strings.Contains(msg, "wsx.closing") {
			t.Fatalf("通知内容不对: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("时间闸静默断开，没有发通知")
	}
}

func TestMarkActiveDefersGrace(t *testing.T) {
	ts := newTestServer(t, Options{ActivateGrace: 150 * time.Millisecond}, func(c *Conn) Handler {
		return Callbacks{Message: func(c *Conn, mt int, data []byte) {
			c.MarkActive()
			_ = c.Send(mt, data)
		}}
	})

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})
	if err := cli.SendText("hello"); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case d := <-ts.result:
		t.Fatalf("MarkActive 后仍被闸门关闭: %+v", d)
	case <-time.After(400 * time.Millisecond):
	}
	_ = cli.Close(websocket.CloseNormalClosure, "done")
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry(1)
	rel, ok := reg.Acquire()
	if !ok {
		t.Fatal("首个名额应当可用")
	}
	if _, ok := reg.Acquire(); ok {
		t.Fatal("名额已满仍然放行")
	}
	rel()
	rel() // 幂等
	if reg.AcquiredCount() != 0 {
		t.Fatalf("释放后名额未归零: %d", reg.AcquiredCount())
	}
}

func TestRegistryCloseAllWaits(t *testing.T) {
	reg := NewRegistry(4)
	ts := newTestServer(t, Options{}, func(c *Conn) Handler {
		release, _ := reg.Acquire()
		reg.Track(c, release)
		return Callbacks{}
	})

	cli := dial(t, ts.url, Options{})
	go cli.Serve(context.Background(), Callbacks{})

	deadline := time.Now().Add(2 * time.Second)
	for reg.ActiveCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reg.ActiveCount() != 1 {
		t.Fatalf("期望 1 条在册连接，得到 %d", reg.ActiveCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.CloseAll(ctx); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if reg.ActiveCount() != 0 {
		t.Fatalf("CloseAll 返回时仍有 %d 条连接未收敛", reg.ActiveCount())
	}
	if _, ok := reg.Acquire(); ok {
		t.Fatal("退出中仍然放行新连接")
	}
}

func TestCheckOriginDefaultsToSameOrigin(t *testing.T) {
	req := func(origin, host string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/ws", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	cases := []struct {
		origin, host string
		allowed      []string
		want         bool
	}{
		{"", "a.com", nil, true},                                    // 非浏览器客户端
		{"http://a.com", "a.com", nil, true},                        // 同源
		{"http://evil.com", "a.com", nil, false},                    // 默认拒绝跨域
		{"http://evil.com", "a.com", []string{"evil.com"}, true},    // 白名单（host 形式）
		{"https://x.a.com", "a.com", []string{"*.a.com"}, true},     // 前缀通配
		{"https://a.com.evil", "a.com", []string{"*.a.com"}, false}, // 通配不能被后缀欺骗
	}
	for _, c := range cases {
		if got := checkOrigin(req(c.origin, c.host), c.allowed); got != c.want {
			t.Fatalf("checkOrigin(%q, host=%q, allowed=%v) = %v，期望 %v", c.origin, c.host, c.allowed, got, c.want)
		}
	}
}
