package wsmanager

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wwnbb/wsmanager/states"
)

func testErrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestDefaultPinger(interval, retryInterval time.Duration, retries int) *DefaultPinger {
	return &DefaultPinger{
		interval:      interval,
		retries:       retries,
		retryInterval: retryInterval,
	}
}

func TestDefaultWsPingInterval(t *testing.T) {
	if DefaultWsPingInterval != 20*time.Second {
		t.Fatalf("DefaultWsPingInterval = %v, want 20s", DefaultWsPingInterval)
	}
	p := NewDefaultPinger()
	if p.GetInterval() != 20*time.Second {
		t.Fatalf("NewDefaultPinger interval = %v, want 20s", p.GetInterval())
	}
	if p.GetRetries() != 5 {
		t.Fatalf("retries = %d, want 5", p.GetRetries())
	}
	if p.GetRetryInterval() != 5*time.Second {
		t.Fatalf("retryInterval = %v, want 5s", p.GetRetryInterval())
	}
}

func TestIsBybitPong(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"valid", `{"success":true,"ret_msg":"pong","op":"ping","conn_id":"x"}`, true},
		{"valid_with_req_id", `{"success":true,"ret_msg":"pong","op":"ping","req_id":"ping_1"}`, true},
		{"success_false", `{"success":false,"ret_msg":"pong","op":"ping"}`, false},
		{"wrong_ret_msg", `{"success":true,"ret_msg":"ok","op":"ping"}`, false},
		{"wrong_op", `{"success":true,"ret_msg":"pong","op":"pong"}`, false},
		{"not_json", `not-json`, false},
		{"trade", `{"topic":"publicTrade.BTCUSDT","data":[]}`, false},
		{"empty", ``, false},
		{"array", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBybitPong([]byte(tc.data)); got != tc.want {
				t.Fatalf("isBybitPong(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

// dialTestConn opens a client WS against handler and returns a WSConnection.
func dialTestConn(t *testing.T, handler func(*websocket.Conn)) (*WSConnection, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		handler(c)
	}))
	t.Cleanup(server.Close)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	wsConn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	connCtx, connCancel := context.WithCancel(context.Background())
	conn := &WSConnection{
		conn:   wsConn,
		ctx:    connCtx,
		cancel: connCancel,
	}
	t.Cleanup(func() {
		connCancel()
		_ = wsConn.Close(websocket.StatusNormalClosure, "")
		wsConn.CloseNow()
	})
	return conn, server
}

func TestDefaultPingerConsumesValidPong(t *testing.T) {
	pingSeen := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(data), `"op":"ping"`) {
				select {
				case pingSeen <- struct{}{}:
				default:
				}
				pong := []byte(`{"success":true,"ret_msg":"pong","op":"ping"}`)
				if err := conn.Write(ctx, websocket.MessageText, pong); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), testErrLogger())
	p := newTestDefaultPinger(30*time.Millisecond, 200*time.Millisecond, 2)
	m.SetPinger(p)

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		_ = m.Close()
		m.Wait()
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-pingSeen:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for ping %d", i)
		}
	}

	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case frame := <-m.DataCh:
			if isBybitPong(frame.Data) {
				t.Fatalf("pong was delivered to DataCh: %s", frame.Data)
			}
		case <-deadline:
			if m.GetConnState() != states.StateConnected {
				t.Fatalf("state = %v, want Connected after successful pongs", m.GetConnState())
			}
			return
		}
	}
}

func TestDefaultPingerPongTimeoutDisconnects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), testErrLogger())
	p := newTestDefaultPinger(20*time.Millisecond, 40*time.Millisecond, 1)
	m.SetPinger(p)

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	select {
	case <-m.DisconnectSig:
	case <-time.After(3 * time.Second):
		t.Fatal("expected disconnect after pong timeout")
	}
	if m.GetConnState() != states.StateDisconnected {
		t.Fatalf("state = %v, want Disconnected", m.GetConnState())
	}
	m.Wait()
}

func TestDefaultPingerReusableAcrossReconnects(t *testing.T) {
	var accepts atomic.Int32
	pingSeen := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(data), `"op":"ping"`) {
				select {
				case pingSeen <- struct{}{}:
				default:
				}
				pong := []byte(`{"success":true,"ret_msg":"pong","op":"ping","conn_id":"c"}`)
				_ = conn.Write(ctx, websocket.MessageText, pong)
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), testErrLogger())
	p := newTestDefaultPinger(25*time.Millisecond, 150*time.Millisecond, 2)
	m.SetPinger(p)

	if err := m.Connect(); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	select {
	case <-pingSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("first generation: timed out waiting for ping")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	m.Wait()

	if err := m.Connect(); err != nil {
		t.Fatalf("reconnect with same pinger: %v", err)
	}
	defer func() {
		_ = m.Close()
		m.Wait()
	}()

	select {
	case <-pingSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("second generation: timed out waiting for ping")
	}
	if m.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", m.GetConnState())
	}
	if accepts.Load() < 2 {
		t.Fatalf("accepts = %d, want >= 2", accepts.Load())
	}
}

func TestDefaultPingerStopDoesNotClosePongCh(t *testing.T) {
	p := newTestDefaultPinger(time.Hour, time.Hour, 0)
	p.mu.Lock()
	p.done = make(chan struct{})
	p.pongCh = make(chan struct{}, 1)
	p.mu.Unlock()

	p.Stop()

	p.mu.Lock()
	pongCh := p.pongCh
	done := p.done
	p.mu.Unlock()
	if pongCh == nil {
		t.Fatal("Stop nilled pongCh; must leave it open")
	}
	if done != nil {
		t.Fatal("Stop should clear done")
	}

	frame := ReceivedFrame{Data: []byte(`{"success":true,"ret_msg":"pong","op":"ping"}`)}
	if !p.HandleMessage(context.Background(), nil, frame, slog.Default()) {
		t.Fatal("HandleMessage should consume valid pong after Stop")
	}
	select {
	case <-pongCh:
	default:
		t.Fatal("expected pong signal on open pongCh")
	}
}

func TestHeartbeatCancelBeforePingNoOnError(t *testing.T) {
	conn, _ := dialTestConn(t, func(c *websocket.Conn) {
		_, _, _ = c.Read(context.Background())
	})

	p := newTestDefaultPinger(time.Hour, 100*time.Millisecond, 2)
	pongCh := make(chan struct{}, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var onErr atomic.Int32
	ok := p.heartbeat(ctx, done, pongCh, conn, testErrLogger(), func(string) string { return "ping_1" }, func() {
		onErr.Add(1)
	}, 2, 100*time.Millisecond)
	if ok {
		t.Fatal("heartbeat should fail on canceled ctx")
	}
	if onErr.Load() != 0 {
		t.Fatalf("onError called %d times, want 0", onErr.Load())
	}
}

func TestHeartbeatStopDuringWaitNoOnError(t *testing.T) {
	firstPing := make(chan struct{})
	conn, _ := dialTestConn(t, func(c *websocket.Conn) {
		ctx := context.Background()
		_, _, err := c.Read(ctx)
		if err != nil {
			return
		}
		close(firstPing)
		// Never pong; hold until client goes away.
		_, _, _ = c.Read(ctx)
	})

	p := newTestDefaultPinger(time.Hour, time.Second, 3)
	pongCh := make(chan struct{}, 1)
	done := make(chan struct{})
	ctx := context.Background()

	var onErr atomic.Int32
	finished := make(chan bool, 1)
	go func() {
		finished <- p.heartbeat(ctx, done, pongCh, conn, testErrLogger(), func(string) string { return "ping_1" }, func() {
			onErr.Add(1)
		}, 3, time.Second)
	}()

	select {
	case <-firstPing:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ping write")
	}
	close(done) // Stop signal during pong wait

	select {
	case ok := <-finished:
		if ok {
			t.Fatal("heartbeat should exit on Stop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not observe Stop")
	}
	if onErr.Load() != 0 {
		t.Fatalf("onError called %d times, want 0", onErr.Load())
	}
}

func TestHeartbeatActiveWriteErrorOnErrorOnce(t *testing.T) {
	// Server keeps the connection open so the reader path is not the failure source.
	conn, _ := dialTestConn(t, func(c *websocket.Conn) {
		ctx := context.Background()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	// Force subsequent writes to fail without canceling manager ctx.
	conn.conn.CloseNow()

	p := newTestDefaultPinger(time.Hour, 200*time.Millisecond, 3)
	pongCh := make(chan struct{}, 1)
	done := make(chan struct{})
	var onErr atomic.Int32

	ok := p.heartbeat(context.Background(), done, pongCh, conn, testErrLogger(), func(string) string { return "ping_1" }, func() {
		onErr.Add(1)
	}, 3, 200*time.Millisecond)
	if ok {
		t.Fatal("heartbeat should fail on write error")
	}
	if onErr.Load() != 1 {
		t.Fatalf("onError called %d times, want 1", onErr.Load())
	}
}

func TestHeartbeatPendingPongAcceptedBetweenRetries(t *testing.T) {
	// Server: read first ping, delay a pong until after the first wait window,
	// without relying on a second ping for success.
	firstPing := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		_, _, err = c.Read(ctx)
		if err != nil {
			return
		}
		close(firstPing)
		// Hold; pong is injected client-side via HandleMessage after first timeout.
		_, _, _ = c.Read(ctx)
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer wsConn.CloseNow()

	p := newTestDefaultPinger(time.Hour, 40*time.Millisecond, 2)
	// Install pongCh as Start would.
	p.mu.Lock()
	p.pongCh = make(chan struct{}, 1)
	pongCh := p.pongCh
	p.mu.Unlock()

	conn := &WSConnection{conn: wsConn, ctx: context.Background(), cancel: func() {}, pinger: p}
	done := make(chan struct{})
	var onErr atomic.Int32

	finished := make(chan bool, 1)
	go func() {
		finished <- p.heartbeat(context.Background(), done, pongCh, conn, testErrLogger(), func(string) string {
			return "ping_1"
		}, func() { onErr.Add(1) }, 2, 40*time.Millisecond)
	}()

	select {
	case <-firstPing:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first ping")
	}

	// After first wait times out (~40ms), inject a valid pong before retries exhaust.
	// Use HandleMessage so the same path production uses is covered.
	time.AfterFunc(50*time.Millisecond, func() {
		frame := ReceivedFrame{Data: []byte(`{"success":true,"ret_msg":"pong","op":"ping"}`)}
		if !p.HandleMessage(context.Background(), conn, frame, testErrLogger()) {
			t.Error("HandleMessage rejected valid pong")
		}
	})

	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("heartbeat should succeed via pending pong between retries")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not finish")
	}
	if onErr.Load() != 0 {
		t.Fatalf("onError called %d times, want 0", onErr.Load())
	}
}

func TestDefaultPingerWriteErrorCallsOnErrorOnce(t *testing.T) {
	// Direct heartbeat coverage (not manager reader disconnect).
	TestHeartbeatActiveWriteErrorOnErrorOnce(t)
}

// ctxWaitPinger blocks in Start until ctx is done; Stop waits for Start to exit.
type ctxWaitPinger struct {
	started chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func newCtxWaitPinger() *ctxWaitPinger {
	return &ctxWaitPinger{
		started: make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

func (p *ctxWaitPinger) Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	select {
	case <-p.exited:
	default:
		close(p.exited)
	}
}

func (p *ctxWaitPinger) HandleMessage(ctx context.Context, conn *WSConnection, frame ReceivedFrame, logger *slog.Logger) bool {
	return false
}

func (p *ctxWaitPinger) Stop() {
	// Deadlocks unless connection cancel runs before Stop.
	<-p.exited
}

func TestCloseCancelBeforePingerStopNoDeadlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		// Keep connection open until peer closes; read loop respects client close.
		ctx := context.Background()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	pinger := newCtxWaitPinger()
	m.SetPinger(pinger)

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-pinger.started:
	case <-time.After(3 * time.Second):
		t.Fatal("pinger did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = m.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked: conn.cancel must run before Pinger.Stop")
	}

	waitDone := make(chan struct{})
	go func() {
		m.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait hung after Close; workers did not observe cancellation")
	}
}

func TestConcurrentConnectCloseNoRace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.SetPinger(nil)

	var wg sync.WaitGroup
	const n = 32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = m.Connect()
			_ = m.Close()
		}()
	}
	wg.Wait()
	m.Wait()
}

// onErrorFromStartPinger calls onError from Start; Stop waits for Start to exit.
// Used to prove the manager must not call Stop from the onError path.
type onErrorFromStartPinger struct {
	started   chan struct{}
	exited    chan struct{}
	stopCalls atomic.Int32
	startOnce sync.Once
	exitOnce  sync.Once
}

func newOnErrorFromStartPinger() *onErrorFromStartPinger {
	return &onErrorFromStartPinger{
		started: make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

func (p *onErrorFromStartPinger) Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	p.startOnce.Do(func() { close(p.started) })
	defer p.exitOnce.Do(func() { close(p.exited) })
	if onError != nil {
		onError()
	}
	// Start returns after onError; if Stop were invoked inside onError and waited
	// on exited, the process would deadlock before reaching this point.
}

func (p *onErrorFromStartPinger) HandleMessage(ctx context.Context, conn *WSConnection, frame ReceivedFrame, logger *slog.Logger) bool {
	return false
}

func (p *onErrorFromStartPinger) Stop() {
	p.stopCalls.Add(1)
	<-p.exited
}

func TestPingerOnErrorDoesNotCallStopOrDeadlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := newOnErrorFromStartPinger()
	m.SetPinger(p)

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("pinger Start did not run")
	}

	select {
	case <-m.DisconnectSig:
	case <-time.After(3 * time.Second):
		t.Fatal("expected DisconnectSig after pinger onError")
	}
	if m.GetConnState() != states.StateDisconnected {
		t.Fatalf("state = %v, want Disconnected", m.GetConnState())
	}

	waitDone := make(chan struct{})
	go func() {
		m.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait deadlocked after pinger onError (Stop must not run on that path)")
	}

	if p.stopCalls.Load() != 0 {
		t.Fatalf("Stop called %d times from pinger onError path, want 0", p.stopCalls.Load())
	}

	// External Close still uses cancel-before-Stop; pinger already exited so Stop is a no-op path (conn nil).
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// panicPinger panics from Start to exercise manager recover.
type panicPinger struct {
	started   chan struct{}
	stopCalls atomic.Int32
	once      sync.Once
}

func newPanicPinger() *panicPinger {
	return &panicPinger{started: make(chan struct{})}
}

func (p *panicPinger) Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	p.once.Do(func() { close(p.started) })
	panic("pinger start panic")
}

func (p *panicPinger) HandleMessage(ctx context.Context, conn *WSConnection, frame ReceivedFrame, logger *slog.Logger) bool {
	return false
}

func (p *panicPinger) Stop() {
	p.stopCalls.Add(1)
}

func TestPingerStartPanicRecoveredAndDisconnects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := newPanicPinger()
	m.SetPinger(p)

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("pinger Start did not run")
	}

	select {
	case <-m.DisconnectSig:
	case <-time.After(3 * time.Second):
		t.Fatal("expected DisconnectSig after pinger panic")
	}
	if m.GetConnState() != states.StateDisconnected {
		t.Fatalf("state = %v, want Disconnected", m.GetConnState())
	}

	waitDone := make(chan struct{})
	go func() {
		m.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after pinger panic")
	}

	// Panic path must not synchronously Stop (Start is unwinding via recover).
	if p.stopCalls.Load() != 0 {
		t.Fatalf("Stop called %d times from panic path, want 0", p.stopCalls.Load())
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
