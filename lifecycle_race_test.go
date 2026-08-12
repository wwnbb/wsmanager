package wsmanager_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wwnbb/wsmanager"
	"github.com/wwnbb/wsmanager/states"
)

// Regression: Wait racing Connect must not return once Connect has
// authoritatively started (state left StateNew: connectMu held, workers
// registered before Connected becomes visible). A Wait that starts before
// Connect reaches its critical section legitimately sees an empty wait group;
// that API race is out of scope, so this test gates Wait on the state poll.
func TestConcurrentWaitAndConnect(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		// Hold the connection open until the client goes away.
		_, _, _ = conn.Read(context.Background())
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")

	const iterations = 200
iter:
	for i := 0; i < iterations; i++ {
		parent, cancel := context.WithCancel(context.Background())
		wsm := wsmanager.NewWSManager(url, parent, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		wsm.SetPinger(nil)

		connectErr := make(chan error, 1)
		go func() { connectErr <- wsm.Connect() }()

		// Gate on an authoritatively started Connect: Wait may be launched
		// only after state leaves StateNew. Once Connecting is visible,
		// Connect holds connectMu; once Connected is visible, workers are
		// registered. A Wait launched earlier has no authoritative Connect to
		// wait for and may legally see an empty wait group.
		startDeadline := time.Now().Add(2 * time.Second)
		state := wsm.GetConnState()
		for state == states.StateNew {
			if time.Now().After(startDeadline) {
				cancel()
				t.Fatalf("iteration %d: Connect never left StateNew", i)
			}
			state = wsm.GetConnState()
		}
		if state == states.StateDisconnected {
			// Dial flake: Connect failed before we could gate on it.
			cancel()
			if err := <-connectErr; err == nil {
				t.Fatalf("iteration %d: Disconnected but Connect reported success", i)
			}
			continue iter
		}

		waitReturned := make(chan struct{})
		go func() {
			wsm.Wait()
			close(waitReturned)
		}()

		// While Connecting/Connected, Wait must not have returned.
		pollDeadline := time.Now().Add(2 * time.Second)
		for wsm.GetConnState() != states.StateConnected {
			select {
			case <-waitReturned:
				cancel()
				t.Fatalf("iteration %d: Wait returned while connection workers were active", i)
			default:
			}
			if wsm.GetConnState() == states.StateDisconnected {
				// Dial flake observed late; skip this iteration.
				cancel()
				<-connectErr
				continue iter
			}
			if time.Now().After(pollDeadline) {
				cancel()
				t.Fatalf("iteration %d: Connect stuck before Connected", i)
			}
		}
		select {
		case <-waitReturned:
			cancel()
			t.Fatalf("iteration %d: Wait returned while connection workers were active", i)
		default:
		}

		if err := <-connectErr; err != nil {
			cancel()
			t.Fatalf("iteration %d: Connect: %v", i, err)
		}

		cancel()

		select {
		case <-waitReturned:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Wait did not return after parent cancel", i)
		}
		select {
		case <-wsm.DisconnectSig:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: DisconnectSig did not fire after parent cancel", i)
		}

		if err := wsm.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
	}
}

func TestWaitAfterTeardownIsDeterministic(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	wsm := wsmanager.NewWSManager(url, context.Background(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	if err := wsm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := wsm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitDone := make(chan struct{})
	go func() {
		wsm.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return promptly after Close")
	}
}
