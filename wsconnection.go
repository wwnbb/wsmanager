package wsmanager

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/wwnbb/wsmanager/states"
)

type WSConnection struct {
	*websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	lastPing time.Time

	writeMu sync.Mutex
	pingMu  sync.Mutex

	// id uniquely identifies this connection instance for stale-teardown guards.
	id uint64
	// pinger is snapshotted at connect time so teardown stops the correct instance.
	pinger Pinger
}

// ReceivedFrame is one complete WebSocket message. ReceivedAt is captured
// immediately after the message reader reaches EOF and before delivery.
type ReceivedFrame struct {
	Type       websocket.MessageType
	Data       []byte
	ReceivedAt time.Time
}

func (c *WSConnection) GetConn() *websocket.Conn {
	return c.Conn
}

func (c *WSConnection) WriteJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	return wsjson.Write(ctx, c.Conn, v)
}

func (c *WSConnection) WritePing(ctx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.Ping(ctx)
}

func (m *WSManager) GetConnState() states.ConnectionState {
	return states.ConnectionState(atomic.LoadInt32((*int32)(&m.connState)))
}

// closeDetachedConn tears down a connection that has already been detached from
// m.Conn (or is otherwise unshared). Locks must not be held by the caller.
//
// stopPinger controls whether conn.pinger.Stop is invoked:
//   - true: reader/SendRequest/Close/connect-failure paths (cancel then Stop).
//   - false: pinger-originated onError/panic path. onError is terminal for that
//     Start invocation; calling Stop here would deadlock if Stop waits for Start.
func closeDetachedConn(conn *WSConnection, closeCode websocket.StatusCode, reason string, stopPinger bool) {
	if conn == nil {
		return
	}
	// Cancel first so Stop (when used) may wait for Start to observe ctx done.
	if conn.cancel != nil {
		conn.cancel()
	}
	if stopPinger && conn.pinger != nil {
		conn.pinger.Stop()
	}
	if conn.Conn != nil {
		_ = conn.Conn.Close(closeCode, reason)
	}
}
