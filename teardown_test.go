package wsmanager_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wwnbb/wsmanager"
	"github.com/wwnbb/wsmanager/states"
)

// assertTornDownAfterParentCancel polls for Disconnected and drains DisconnectSig.
func assertTornDownAfterParentCancel(t *testing.T, wsm *wsmanager.WSManager) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for wsm.GetConnState() != states.StateDisconnected {
		if time.Now().After(deadline) {
			t.Fatalf("state = %v, want Disconnected after parent cancel", wsm.GetConnState())
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-wsm.DisconnectSig:
	case <-time.After(time.Second):
		t.Fatal("DisconnectSig did not fire after parent cancel")
	}
	select {
	case <-wsm.DisconnectSig:
		t.Fatal("duplicate DisconnectSig after parent cancel")
	default:
	}
}

// reconnectFreshManager proves teardown completed cleanly by connecting again.
// The canceled parent ctx forbids same-manager Connect (refreshContext refuses
// a done parent), so the reconnect leg uses a fresh manager and fresh ctx.
func reconnectFreshManager(t *testing.T, url string) {
	t.Helper()

	wsm := wsmanager.NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), 1)
	if err := wsm.Connect(); err != nil {
		t.Fatalf("reconnect after parent-cancel teardown: %v", err)
	}
	if wsm.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v after reconnect, want Connected", wsm.GetConnState())
	}
	if err := wsm.Close(); err != nil {
		t.Fatalf("Close after reconnect: %v", err)
	}
	wsm.Wait()
}

// Regression: reader blocked on a full DataCh must not suppress teardown on
// parent cancel. Pre-fix the dropped in-flight frame path returned from
// readMessages without failConnection, leaving state Connected forever.
func TestParentCancelWithActiveConnectionBlockedOnDataCh(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		// Read side drains the client's close frame so the close handshake
		// (and with it DisconnectSig, signaled after conn teardown) completes.
		go func() {
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		}()
		for {
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"tick":true}`)); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	parent, cancel := context.WithCancel(context.Background())
	wsm := wsmanager.NewWSManager(url, parent, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), 1)

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Let the reader fill the DataCh buffer and block on the next send;
	// intentionally never drained.
	time.Sleep(300 * time.Millisecond)

	cancel()
	assertTornDownAfterParentCancel(t, wsm)
	reconnectFreshManager(t, url)

	// Teardown must have released both workers.
	waitDone := make(chan struct{})
	go func() {
		wsm.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after parent-cancel teardown")
	}
}

// Regression lock for the already-correct path: reader blocked inside Reader()
// must be interrupted by parent cancel and torn down.
func TestParentCancelWithActiveConnectionBlockedInReader(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		// Idle: send nothing, hold until the client goes away.
		_, _, _ = conn.Read(context.Background())
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	parent, cancel := context.WithCancel(context.Background())
	wsm := wsmanager.NewWSManager(url, parent, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Give the reader time to block inside Reader().
	time.Sleep(100 * time.Millisecond)

	cancel()
	assertTornDownAfterParentCancel(t, wsm)
	reconnectFreshManager(t, url)

	waitDone := make(chan struct{})
	go func() {
		wsm.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after parent-cancel teardown")
	}
}

// hangPinger blocks in Stop forever; the manager must bound it with the watchdog.
type hangPinger struct {
	stopEntered chan struct{}
	stopOnce    sync.Once
}

func (p *hangPinger) Start(ctx context.Context, conn *wsmanager.WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	<-ctx.Done()
}

func (p *hangPinger) HandleMessage(ctx context.Context, conn *wsmanager.WSConnection, frame wsmanager.ReceivedFrame, logger *slog.Logger) bool {
	return false
}

func (p *hangPinger) Stop() {
	p.stopOnce.Do(func() { close(p.stopEntered) })
	select {} // block forever
}

func TestBlockingPingerStopDoesNotHangClose(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	wsm := wsmanager.NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	pinger := &hangPinger{stopEntered: make(chan struct{})}
	wsm.SetPinger(pinger)

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		_ = wsm.Close()
		close(closed)
	}()

	// pingerStopTimeout (5s, unexported) + 2s slack.
	select {
	case <-closed:
	case <-time.After(7 * time.Second):
		t.Fatal("Close blocked by hung Pinger.Stop beyond watchdog timeout")
	}

	// Teardown may be won by the reader/pinger worker rather than Close itself;
	// either way Stop is invoked inside a watchdog goroutine, so wait for it.
	select {
	case <-pinger.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Pinger.Stop was never entered")
	}

	// If the reader worker won the teardown CAS it runs the same watchdog, so
	// Wait may be blocked up to pingerStopTimeout too: bound with the same slack.
	waitDone := make(chan struct{})
	go func() {
		wsm.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(7 * time.Second):
		t.Fatal("Wait did not return after Close with hung pinger Stop")
	}
}
