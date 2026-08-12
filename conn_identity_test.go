package wsmanager

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wwnbb/wsmanager/states"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func startEchoServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	return server, url
}

// TestStaleSendRequestDoesNotCloseNewConnection verifies that a send failure
// belonging to an older connection generation cannot tear down a newer one.
func TestStaleSendRequestDoesNotCloseNewConnection(t *testing.T) {
	_, url := startEchoServer(t)

	ctx := context.Background()
	m := NewWSManager(url, ctx, testLogger())
	m.SetPinger(nil)

	if err := m.Connect(); err != nil {
		t.Fatalf("first connect: %v", err)
	}

	oldConn := m.getConn()
	if oldConn == nil {
		t.Fatal("expected active connection after first connect")
	}
	oldID := oldConn.id

	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	m.Wait()

	if err := m.Connect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer m.Close()

	newConn := m.getConn()
	if newConn == nil {
		t.Fatal("expected active connection after reconnect")
	}
	if newConn.id == oldID {
		t.Fatal("expected new connection identity after reconnect")
	}
	if m.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", m.GetConnState())
	}

	// Mirror SendRequest's failure path against the stale connection snapshot.
	err := oldConn.WriteJSON(map[string]any{"stale": true})
	if err == nil {
		// Underlying socket may already be fully torn down; force the fail path.
		t.Log("stale WriteJSON unexpectedly succeeded; still exercising failConnection")
	} else {
		m.Logger().Error("failed to send message", "error", err)
	}
	if m.failConnection(oldConn, "connection lost", err) {
		t.Fatal("failConnection on stale conn unexpectedly succeeded")
	}

	if m.GetConnState() != states.StateConnected {
		t.Fatalf("stale failure changed state to %v", m.GetConnState())
	}
	if got := m.getConn(); got == nil || got.id != newConn.id {
		t.Fatal("stale failure replaced or cleared the new connection")
	}

	// New connection must remain usable.
	if err := m.SendRequest(map[string]any{"ok": true}); err != nil {
		t.Fatalf("SendRequest on new connection failed: %v", err)
	}
}

// TestFailConnectionOnlyMatchesCurrentIdentity is a focused unit check of the
// identity CAS path without depending on WriteJSON error timing.
func TestFailConnectionOnlyMatchesCurrentIdentity(t *testing.T) {
	_, url := startEchoServer(t)

	m := NewWSManager(url, context.Background(), testLogger())
	m.SetPinger(nil)

	if err := m.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer m.Close()

	current := m.getConn()
	stale := &WSConnection{id: current.id + 1000}

	if m.failConnection(stale, "stale", nil) {
		t.Fatal("failConnection accepted non-current identity")
	}
	if m.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", m.GetConnState())
	}
	if m.getConn() != current {
		t.Fatal("connection pointer changed after stale fail")
	}

	if !m.failConnection(current, "connection lost", nil) {
		t.Fatal("failConnection on current conn failed")
	}
	if m.GetConnState() != states.StateDisconnected {
		t.Fatalf("state = %v, want Disconnected", m.GetConnState())
	}
	if m.getConn() != nil {
		t.Fatal("expected conn cleared after current fail")
	}
}

// TestSendRequestReturnsErrConnectionReplaced exercises the race where a
// SendRequest snapshot is superseded by a newer connection generation while
// the write is in flight. The stale write must report ErrConnectionReplaced
// and must not tear down the new (authoritative) connection.
func TestSendRequestReturnsErrConnectionReplaced(t *testing.T) {
	_, url := startEchoServer(t)

	ctx := context.Background()
	m := NewWSManager(url, ctx, testLogger())
	m.SetPinger(nil)

	if err := m.Connect(); err != nil {
		t.Fatalf("first connect: %v", err)
	}

	oldConn := m.getConn()
	if oldConn == nil {
		t.Fatal("expected active connection after first connect")
	}
	oldID := oldConn.id

	// Hold writeMu so SendRequest blocks inside WriteJSON on the stale conn.
	oldConn.writeMu.Lock()

	var (
		sendErr  error
		sendDone = make(chan struct{})
	)
	go func() {
		defer close(sendDone)
		sendErr = m.SendRequest(map[string]any{"x": 1})
	}()

	// Let the SendRequest goroutine capture oldConn, pass the state check,
	// and block on oldConn.writeMu (held by the test) inside WriteJSON.
	time.Sleep(50 * time.Millisecond)

	// Replace the generation: Close detaches oldConn, Connect installs gen N+1.
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := m.Connect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	newConn := m.getConn()
	if newConn == nil {
		t.Fatal("expected active connection after reconnect")
	}
	if newConn.id == oldID {
		t.Fatal("expected new connection identity after reconnect")
	}
	if m.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", m.GetConnState())
	}

	// Release the stale write: WriteJSON on the closed oldConn fails, and
	// SendRequest must observe the newer generation and return ErrConnectionReplaced.
	oldConn.writeMu.Unlock()

	select {
	case <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("SendRequest did not return after writeMu unlock")
	}

	if sendErr == nil {
		t.Fatal("expected SendRequest error, got nil")
	}
	if !errors.Is(sendErr, ErrConnectionReplaced) {
		t.Fatalf("sendErr = %v, want ErrConnectionReplaced", sendErr)
	}

	// The new connection must remain alive and usable.
	if m.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected after stale send", m.GetConnState())
	}
	if got := m.getConn(); got == nil || got.id != newConn.id {
		t.Fatal("new connection was replaced or cleared by stale send failure")
	}
	if err := m.SendRequest(map[string]any{"ok": true}); err != nil {
		t.Fatalf("SendRequest on new connection failed: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("cleanup Close: %v", err)
	}
	m.Wait()
}
