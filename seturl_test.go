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

func seturlTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func wsURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http")
}

// frameSender returns a handler that writes one labeled frame on accept and
// then holds the connection open until the client goes away.
func frameSender(label string) func(*websocket.Conn) {
	return func(conn *websocket.Conn) {
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, []byte(label))
		_, _, _ = conn.Read(ctx)
	}
}

// SetURL in StateNew replaces the URL before any connection; the next
// Connect dials the new URL and frames flow into DataCh.
func TestSetURLFromStateNewConnectsToNewURL(t *testing.T) {
	server := mockWebSocketServer(t, frameSender("from-new-url"))
	defer server.Close()
	newURL := wsURL(server.URL)

	wsm := wsmanager.NewWSManager("wss://example.com/ws", context.Background(), seturlTestLogger())

	if err := wsm.SetURL(newURL); err != nil {
		t.Fatalf("SetURL: %v", err)
	}
	if got := wsm.URL(); got != newURL {
		t.Fatalf("URL() = %q, want %q", got, newURL)
	}

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer wsm.Close()

	select {
	case frame := <-wsm.DataCh:
		if string(frame.Data) != "from-new-url" {
			t.Fatalf("frame = %q, want %q", frame.Data, "from-new-url")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received from the new URL")
	}
}

// SetURL in Disconnected points the next Connect at the new URL; frames
// continue to arrive on the same DataCh created by NewWSManager.
func TestSetURLAfterDisconnectReconnectsToNewURL(t *testing.T) {
	serverA := mockWebSocketServer(t, frameSender("from-old-url"))
	defer serverA.Close()
	serverB := mockWebSocketServer(t, frameSender("from-new-url"))
	defer serverB.Close()

	wsm := wsmanager.NewWSManager(wsURL(serverA.URL), context.Background(), seturlTestLogger())
	dataCh := wsm.DataCh

	if err := wsm.Connect(); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	if err := wsm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wsm.Wait()
	if wsm.GetConnState() != states.StateDisconnected {
		t.Fatalf("state = %v, want Disconnected", wsm.GetConnState())
	}

	// Drain the frame queued by the first connection, if any.
	select {
	case <-dataCh:
	default:
	}

	if err := wsm.SetURL(wsURL(serverB.URL)); err != nil {
		t.Fatalf("SetURL: %v", err)
	}
	if got := wsm.URL(); got != wsURL(serverB.URL) {
		t.Fatalf("URL() = %q, want %q", got, wsURL(serverB.URL))
	}
	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect after SetURL: %v", err)
	}
	defer wsm.Close()

	select {
	case frame := <-dataCh:
		if string(frame.Data) != "from-new-url" {
			t.Fatalf("frame = %q, want %q", frame.Data, "from-new-url")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received on the original DataCh after reconnect")
	}
}

// SetURL in Connected is rejected; state and URL stay unchanged.
func TestSetURLRejectedWhenConnected(t *testing.T) {
	serverA := mockWebSocketServer(t, frameSender("from-old-url"))
	defer serverA.Close()
	urlA := wsURL(serverA.URL)
	const urlB = "wss://example.com/ws"

	wsm := wsmanager.NewWSManager(urlA, context.Background(), seturlTestLogger())
	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer wsm.Close()

	err := wsm.SetURL(urlB)
	if err == nil || !strings.Contains(err.Error(), "SetURL requires") {
		t.Fatalf("SetURL err = %v, want state rejection", err)
	}
	if wsm.GetConnState() != states.StateConnected {
		t.Fatalf("state = %v, want Connected", wsm.GetConnState())
	}
	if got := wsm.URL(); got != urlA {
		t.Fatalf("URL() = %q, want unchanged %q", got, urlA)
	}
}

// SetURL in Connecting is rejected. The hanging dial is produced by a server
// that never upgrades; Close cancels the manager context first, so the
// in-flight Connect fails promptly instead of blocking for the dial timeout.
func TestSetURLRejectedWhileConnecting(t *testing.T) {
	// Connect holds connectMu for the entire dial, so SetURL blocks on the
	// same mutex and can never observe an in-flight Connect's Connecting
	// state; the mutual exclusion itself is covered by the race test below.
	// An external Connecting state is reached via the exported transition,
	// as in TestInvalidStateTransitions.
	const urlA = "wss://example.com/ws"
	wsm := wsmanager.NewWSManager(urlA, context.Background(), seturlTestLogger())
	if !wsm.SetConnecting() {
		t.Fatal("SetConnecting failed")
	}

	err := wsm.SetURL("wss://example.com/ws")
	if err == nil || !strings.Contains(err.Error(), "SetURL requires") {
		t.Fatalf("SetURL err = %v, want state rejection", err)
	}
	if got := wsm.URL(); got != urlA {
		t.Fatalf("URL() = %q, want unchanged %q", got, urlA)
	}
	if wsm.GetConnState() != states.StateConnecting {
		t.Fatalf("state = %v, want Connecting", wsm.GetConnState())
	}
}

// SetURL validates the URL before storing it; failures leave the URL intact
// and the manager usable.
func TestSetURLInvalidURLRejected(t *testing.T) {
	const original = "wss://example.com/ws"
	wsm := wsmanager.NewWSManager(original, context.Background(), seturlTestLogger())

	for _, bad := range []string{
		"",
		"://bad url",
		"http://example.com/ws",
		"https://example.com/ws",
		"ws://",
		"wss://",
	} {
		if err := wsm.SetURL(bad); err == nil {
			t.Errorf("SetURL(%q): want error, got nil", bad)
		}
	}
	if got := wsm.URL(); got != original {
		t.Fatalf("URL() = %q, want unchanged %q", got, original)
	}

	if err := wsm.SetURL("wss://other.example.com/ws"); err != nil {
		t.Fatalf("SetURL after rejections: %v", err)
	}
}

// SetURL never emits a disconnect event, in StateNew or in Disconnected.
func TestSetURLDoesNotEmitDisconnectSig(t *testing.T) {
	wsm := wsmanager.NewWSManager("wss://example.com/ws", context.Background(), seturlTestLogger())
	if err := wsm.SetURL("wss://other.example.com/ws"); err != nil {
		t.Fatalf("SetURL: %v", err)
	}
	select {
	case ev := <-wsm.DisconnectSig:
		t.Fatalf("unexpected DisconnectEvent from SetURL: %+v", ev)
	default:
	}

	// A failed Connect legitimately queues one event; after draining it,
	// SetURL must not queue another.
	wsm2 := wsmanager.NewWSManager("ws://127.0.0.1:1/ws", context.Background(), seturlTestLogger())
	if err := wsm2.Connect(); err == nil {
		t.Fatal("Connect to dead URL: want error, got nil")
	}
	select {
	case <-wsm2.DisconnectSig:
	default:
		t.Fatal("failed Connect did not emit DisconnectEvent")
	}
	if err := wsm2.SetURL("wss://other.example.com/ws"); err != nil {
		t.Fatalf("SetURL: %v", err)
	}
	select {
	case ev := <-wsm2.DisconnectSig:
		t.Fatalf("unexpected DisconnectEvent from SetURL: %+v", ev)
	default:
	}
}

// -race regression: SetURL is serialized with Connect and Close through
// connectMu and leaves the manager usable for every interleaving.
func TestSetURLConcurrentWithConnectAndClose(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
	})
	defer server.Close()
	urlA := wsURL(server.URL)
	const urlB = "wss://example.com/ws"

	const iterations = 50
	for i := 0; i < iterations; i++ {
		wsm := wsmanager.NewWSManager(urlA, context.Background(), seturlTestLogger())

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = wsm.Connect() }()
		go func() { defer wg.Done(); _ = wsm.SetURL(urlB) }()
		go func() { defer wg.Done(); _ = wsm.Close() }()
		wg.Wait()

		if got := wsm.URL(); got != urlA && got != urlB {
			t.Fatalf("iteration %d: URL() = %q, want %q or %q", i, got, urlA, urlB)
		}
		// Connect after Close is legal (refreshContext): if Close won the
		// ordering, Connect may legitimately end Connected.
		if s := wsm.GetConnState(); s != states.StateConnected && s != states.StateDisconnected {
			t.Fatalf("iteration %d: state = %v, want Connected or Disconnected", i, s)
		}

		if err := wsm.Close(); err != nil {
			t.Fatalf("iteration %d: teardown Close: %v", i, err)
		}
		wsm.Wait()
		if err := wsm.SetURL(urlA); err != nil {
			t.Fatalf("iteration %d: final SetURL: %v", i, err)
		}
		if err := wsm.Connect(); err != nil {
			t.Fatalf("iteration %d: final Connect: %v", i, err)
		}
		if err := wsm.Close(); err != nil {
			t.Fatalf("iteration %d: final Close: %v", i, err)
		}
		wsm.Wait()
	}
}
