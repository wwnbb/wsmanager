package wsmanager_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wwnbb/wsmanager"
	"github.com/wwnbb/wsmanager/states"
)

func quietErrorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// muteWebSocketServer accepts the upgrade and then blocks forever: it never
// reads the client's close frame, so the close handshake can only complete
// via the watchdog's CloseNow fallback. unblock releases the handler so
// server.Close() does not hang at test cleanup.
func muteWebSocketServer(t *testing.T, unblock <-chan struct{}) *httptest.Server {
	return mockWebSocketServer(t, func(conn *websocket.Conn) {
		<-unblock
	})
}

// Regression: a peer that never answers the WebSocket close handshake must
// not block teardown. Close and DisconnectSig are bounded by
// closeHandshakeTimeout (5s, unexported) + 2s slack; pre-fix Close hung
// indefinitely and DisconnectSig never fired.
func TestMutePeerCloseHandshakeIsBounded(t *testing.T) {
	unblock := make(chan struct{})
	server := muteWebSocketServer(t, unblock)
	defer server.Close()
	defer close(unblock)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	wsm := wsmanager.NewWSManager(url, context.Background(), quietErrorLogger(), 1)
	wsm.SetPinger(nil)

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if wsm.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", wsm.GetConnState())
	}

	bound := 5*time.Second + 2*time.Second // closeHandshakeTimeout + slack
	start := time.Now()

	closed := make(chan struct{})
	go func() {
		_ = wsm.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(bound):
		t.Fatal("Close blocked by mute peer beyond close handshake watchdog timeout")
	}

	select {
	case <-wsm.DisconnectSig:
	case <-time.After(bound - time.Since(start)):
		t.Fatal("DisconnectSig did not fire within bound against mute peer")
	}
	select {
	case <-wsm.DisconnectSig:
		t.Fatal("duplicate DisconnectSig against mute peer")
	default:
	}
}

// After a mute-peer teardown, a fresh manager on a fresh parent ctx can still
// connect to a healthy server (teardown released the manager cleanly).
func TestReconnectAfterMutePeerTeardownSucceeds(t *testing.T) {
	unblock := make(chan struct{})
	muteServer := muteWebSocketServer(t, unblock)
	defer muteServer.Close()
	defer close(unblock)

	muteURL := "ws" + strings.TrimPrefix(muteServer.URL, "http")
	wsm := wsmanager.NewWSManager(muteURL, context.Background(), quietErrorLogger(), 1)
	wsm.SetPinger(nil)

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		_ = wsm.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5*time.Second + 2*time.Second): // closeHandshakeTimeout + slack
		t.Fatal("Close blocked by mute peer beyond close handshake watchdog timeout")
	}

	waitDone := make(chan struct{})
	go func() {
		wsm.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(7 * time.Second):
		t.Fatal("Wait did not return after mute-peer teardown")
	}

	healthyServer := mockWebSocketServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
	})
	defer healthyServer.Close()

	reconnectFreshManager(t, "ws"+strings.TrimPrefix(healthyServer.URL, "http"))
}
