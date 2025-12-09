package wsmanager_test

import (
	"context"
	"fmt"
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
	pp "github.com/wwnbb/pprint"
	"github.com/wwnbb/wsmanager"
	"github.com/wwnbb/wsmanager/states"
)

func TestNewWSManager(t *testing.T) {
	ctx := context.Background()
	url := "wss://example.com/ws"

	wsm := wsmanager.NewWSManager(url, ctx)

	if wsm == nil {
		t.Fatal("NewWSManager returned nil")
	}

	if wsm.GetConnState() != states.StateNew {
		t.Errorf("Expected initial state to be StateNew, got %v", wsm.GetConnState())
	}

	if wsm.DataCh == nil {
		t.Error("DataCh should not be nil")
	}

	if wsm.Logger == nil {
		t.Error("Logger should not be nil")
	}
}

func TestSetLogger(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	customLogger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	wsm.SetLogger(customLogger)

	if wsm.Logger != customLogger {
		t.Error("Logger was not set correctly")
	}
}

func TestStateTransitions(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	// New -> Connecting
	if !wsm.SetConnecting() {
		t.Error("Expected transition from New to Connecting to succeed")
	}
	if wsm.GetConnState() != states.StateConnecting {
		t.Errorf("Expected StateConnecting, got %v", wsm.GetConnState())
	}

	// Connecting -> Connected
	if !wsm.SetConnected() {
		t.Error("Expected transition from Connecting to Connected to succeed")
	}
	if wsm.GetConnState() != states.StateConnected {
		t.Errorf("Expected StateConnected, got %v", wsm.GetConnState())
	}

	// Connected -> Disconnected
	if !wsm.SetDisconnectedFromConnected() {
		t.Error("Expected transition from Connected to Disconnected to succeed")
	}
	if wsm.GetConnState() != states.StateDisconnected {
		t.Errorf("Expected StateDisconnected, got %v", wsm.GetConnState())
	}

	// Disconnected -> Connecting
	if !wsm.SetConnectingFromDisconnected() {
		t.Error("Expected transition from Disconnected to Connecting to succeed")
	}
	if wsm.GetConnState() != states.StateConnecting {
		t.Errorf("Expected StateConnecting, got %v", wsm.GetConnState())
	}
}

func TestInvalidStateTransitions(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	// Move to Connected state
	wsm.SetConnecting()
	wsm.SetConnected()

	// Try to set connected when already connected (should fail)
	if wsm.SetConnected() {
		t.Error("Expected transition from Connected to Connected to fail")
	}

	// Try to set connecting from disconnected when not disconnected (should fail since we're connected)
	if wsm.SetConnectingFromDisconnected() {
		t.Error("Expected SetConnectingFromDisconnected to fail when in Connected state")
	}

	// State should still be Connected
	if wsm.GetConnState() != states.StateConnected {
		t.Errorf("Expected StateConnected, got %v", wsm.GetConnState())
	}
}

// TestSendRequestWithoutConnection tests sending a request without a connection
func TestSendRequestWithoutConnection(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	msg := map[string]interface{}{
		"op": "test",
	}

	err := wsm.SendRequest(msg)
	if err == nil {
		t.Error("Expected error when sending request without connection")
	}

	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Expected 'not connected' error, got: %v", err)
	}
}

func TestCloseWithoutConnection(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	err := wsm.Close()
	if err != nil {
		t.Errorf("Close should not return error when no connection exists: %v", err)
	}
}

func TestConnectWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	err := wsm.Connect()
	if err == nil {
		t.Error("Expected error when connecting with canceled context")
	}
}

func TestConnectInvalidURL(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("invalid://url", ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err == nil {
		t.Error("Expected error when connecting with invalid URL")
	}
}

func TestConcurrentStateAccess(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	var wg sync.WaitGroup
	iterations := 100

	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = wsm.GetConnState()
		}()
	}

	// Concurrent state transitions
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wsm.SetConnecting()
		}()
	}

	wg.Wait()
}

// TestDataChannelCapacity tests that the data channel has proper capacity
func TestDataChannelCapacity(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	if cap(wsm.DataCh) != 100 {
		t.Errorf("Expected DataCh capacity of 100, got %d", cap(wsm.DataCh))
	}
}

func mockWebSocketServer(t *testing.T, handler func(*websocket.Conn)) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("Failed to accept websocket: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		handler(conn)
	}))
	return server
}

func TestConnectToMockServer(t *testing.T) {
	received := make(chan bool, 1)

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()

		// Read one message
		_, _, err := conn.Read(ctx)
		if err == nil {
			received <- true
		}
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer wsm.Close()

	time.Sleep(100 * time.Millisecond)

	if wsm.GetConnState() != states.StateConnected {
		t.Errorf("Expected StateConnected, got %v", wsm.GetConnState())
	}

	msg := map[string]interface{}{"test": "message"}
	err = wsm.SendRequest(msg)
	if err != nil {
		t.Errorf("Failed to send request: %v", err)
	}

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("Server did not receive message")
	}
}

// TestReconnect tests reconnecting after closing
func TestReconnect(t *testing.T) {
	messageCount := 0
	mu := sync.Mutex{}

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			messageCount++
			mu.Unlock()

			// Echo back
			conn.Write(ctx, websocket.MessageText, msg)
		}
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// First connection
	err := wsm.Connect()
	if err != nil {
		t.Fatalf("First connect failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	msg := map[string]interface{}{"test": "message1"}
	wsm.SendRequest(msg)

	time.Sleep(100 * time.Millisecond)

	// Close
	wsm.Close()
	time.Sleep(100 * time.Millisecond)

	if wsm.GetConnState() != states.StateDisconnected {
		t.Errorf("Expected StateDisconnected after close, got %v", wsm.GetConnState())
	}

	// Reconnect
	err = wsm.Connect()
	if err != nil {
		t.Fatalf("Reconnect failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if wsm.GetConnState() != states.StateConnected {
		t.Errorf("Expected StateConnected after reconnect, got %v", wsm.GetConnState())
	}

	msg2 := map[string]interface{}{"test": "message2"}
	wsm.SendRequest(msg2)

	time.Sleep(100 * time.Millisecond)
	wsm.Close()

	mu.Lock()
	defer mu.Unlock()
	if messageCount < 2 {
		t.Errorf("Expected at least 2 messages, got %d", messageCount)
	}
}

// TestReceiveMessages tests receiving messages from server
func TestReceiveMessages(t *testing.T) {
	testMessage := `{"type":"test","data":"hello"}`

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		time.Sleep(100 * time.Millisecond)

		// Send test message
		err := conn.Write(ctx, websocket.MessageText, []byte(testMessage))
		if err != nil {
			t.Logf("Failed to write message: %v", err)
			return
		}

		// Keep connection alive
		time.Sleep(1 * time.Second)
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer wsm.Close()

	// Wait for message
	select {
	case msg := <-wsm.DataCh:
		if msg == nil {
			t.Error("Received nil message")
		}
		// Verify message is a map
		if _, ok := msg.(map[string]interface{}); !ok {
			t.Errorf("Expected map[string]interface{}, got %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Error("Did not receive message from server")
	}
}

// TestMultipleConcurrentConnects tests that multiple concurrent connect attempts are handled
func TestMultipleConcurrentConnects(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		time.Sleep(2 * time.Second)
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	var wg sync.WaitGroup
	errors := make([]error, 3)

	// Try to connect 3 times concurrently
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errors[idx] = wsm.Connect()
		}(i)
	}

	wg.Wait()

	// At least one should succeed
	successCount := 0
	for _, err := range errors {
		if err == nil {
			successCount++
		}
	}

	if successCount == 0 {
		t.Error("Expected at least one successful connection")
	}

	wsm.Close()
}

// TestConnectFromConnectingState tests attempting to connect when already connecting
func TestConnectFromConnectingState(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// Manually set to connecting state
	wsm.SetConnecting()

	// Try to connect when already connecting (should fail)
	err := wsm.Connect()
	if err == nil {
		t.Error("Expected error when connecting from Connecting state")
	}

	if !strings.Contains(err.Error(), "state transition failed") {
		t.Errorf("Expected state transition error, got: %v", err)
	}
}

// TestConnectFromConnectedState tests attempting to connect when already connected
func TestConnectFromConnectedState(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		time.Sleep(2 * time.Second)
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// First connection
	err := wsm.Connect()
	if err != nil {
		t.Fatalf("First connect failed: %v", err)
	}
	defer wsm.Close()

	time.Sleep(100 * time.Millisecond)

	// Try to connect again (should fail)
	err = wsm.Connect()
	if err == nil {
		t.Error("Expected error when connecting from Connected state")
	}
}

// TestSendRequestNilConnection tests sending request when connection is nil
func TestSendRequestNilConnection(t *testing.T) {
	ctx := context.Background()
	wsm := wsmanager.NewWSManager("wss://example.com/ws", ctx)

	// Manually set to connected state without actual connection
	wsm.SetConnecting()
	wsm.SetConnected()

	msg := map[string]interface{}{"test": "data"}
	err := wsm.SendRequest(msg)
	if err == nil {
		t.Error("Expected error when connection is nil")
	}

	if !strings.Contains(err.Error(), "connection is nil") {
		t.Errorf("Expected 'connection is nil' error, got: %v", err)
	}
}

// TestServerClosesConnection tests handling when server closes the connection
func TestServerClosesConnection(t *testing.T) {
	serverClosed := make(chan bool, 1)

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		// Close immediately after accepting
		time.Sleep(100 * time.Millisecond)
		conn.Close(websocket.StatusNormalClosure, "server closing")
		serverClosed <- true
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer wsm.Close()

	// Wait for server to close
	select {
	case <-serverClosed:
		// Server closed
	case <-time.After(2 * time.Second):
		t.Error("Server did not close connection")
	}

	// Give time for client to detect closure
	time.Sleep(200 * time.Millisecond)

	// Try to send a message (should fail or connection should be disconnected)
	msg := map[string]interface{}{"test": "data"}
	err = wsm.SendRequest(msg)

	// Either SendRequest fails or state changes to disconnected
	if err == nil && wsm.GetConnState() == states.StateConnected {
		t.Error("Expected error or state change after server closure")
	}
}

// TestDataChannelFull tests behavior when data channel is full
func TestDataChannelFull(t *testing.T) {
	messagesSent := 0
	mu := sync.Mutex{}

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		// Send many messages quickly
		for i := 0; i < 150; i++ {
			msg := fmt.Sprintf(`{"id":%d}`, i)
			err := conn.Write(ctx, websocket.MessageText, []byte(msg))
			if err != nil {
				return
			}
			mu.Lock()
			messagesSent++
			mu.Unlock()
			time.Sleep(1 * time.Millisecond)
		}
		time.Sleep(1 * time.Second)
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer wsm.Close()

	// Don't read from channel to make it fill up
	time.Sleep(2 * time.Second)

	mu.Lock()
	sent := messagesSent
	mu.Unlock()

	if sent == 0 {
		t.Error("No messages were sent by server")
	}
	// Test passes if no crash occurs when channel is full
}

// TestConcurrentSendRequests tests sending requests concurrently
func TestConcurrentSendRequests(t *testing.T) {
	messageCount := int32(0)

	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
			atomic.AddInt32(&messageCount, 1)
		}
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer wsm.Close()

	time.Sleep(100 * time.Millisecond)

	// Send messages concurrently
	var wg sync.WaitGroup
	numMessages := 10

	for i := 0; i < numMessages; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			msg := map[string]interface{}{"id": id}
			wsm.SendRequest(msg)
		}(i)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	count := atomic.LoadInt32(&messageCount)
	if count != int32(numMessages) {
		t.Errorf("Expected %d messages, got %d", numMessages, count)
	}
}

// TestCloseMultipleTimes tests calling Close multiple times
func TestCloseMultipleTimes(t *testing.T) {
	server := mockWebSocketServer(t, func(conn *websocket.Conn) {
		time.Sleep(2 * time.Second)
	})
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()
	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Close multiple times
	for i := 0; i < 3; i++ {
		err = wsm.Close()
		if err != nil {
			t.Errorf("Close call %d failed: %v", i+1, err)
		}
	}
}

// Integration test with real server (kept from original)
func TestCreateWsManager(t *testing.T) {
	url := "wss://stream.bybit.com/v5/public/spot"
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := context.Background()

	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(logger)
	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				fmt.Println("websocket: Exiting listener goroutine")
				return
			case msg := <-wsm.DataCh:
				fmt.Printf("websocket: Recieved msg \n----------\n%s\n----------\n", pp.PrettyFormat(msg))
			}
		}
	}()

	subscribeMsg := map[string]interface{}{
		"req_id": "test",
		"op":     "subscribe",
		"args":   []string{"orderbook.1.BTCUSDT"},
	}
	err = wsm.SendRequest(subscribeMsg)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	time.Sleep(2 * time.Second)

	wsm.Close()
	time.Sleep(1 * time.Second)

	// Test reconnection
	err = wsm.Connect()
	if err != nil {
		t.Fatalf("Reconnect failed: %v", err)
	}

	time.Sleep(2 * time.Second)

	err = wsm.SendRequest(subscribeMsg)
	if err != nil {
		t.Fatalf("SendRequest after reconnect failed: %v", err)
	}

	time.Sleep(2 * time.Second)
	wsm.Close()
}

func TestConnectDisconnect(t *testing.T) {
	url := "wss://stream.bybit.com/v5/public/spot"
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := context.Background()

	wsm := wsmanager.NewWSManager(url, ctx)
	wsm.SetLogger(logger)
	err := wsm.Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	go func() {
		for {
			select {
			case <-wsm.DataCh:
			}
		}
	}()

	subscribeMsg := map[string]interface{}{
		"req_id": "test",
		"op":     "subscribe",
		"args":   []string{"orderbook.1.BTCUSDT"},
	}
	err = wsm.SendRequest(subscribeMsg)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}

	time.Sleep(1 * time.Second)
	wsm.Close()
	time.Sleep(1 * time.Second)
	err = wsm.Connect()
	if err != nil {
		t.Fatalf("Reconnect failed: %v", err)
	}
	time.Sleep(1 * time.Second)
	err = wsm.SendRequest(subscribeMsg)
	if err != nil {
		t.Fatalf("SendRequest after reconnect failed: %v", err)
	}

	time.Sleep(1 * time.Second)
	wsm.Close()
}
