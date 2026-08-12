package wsmanager

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/wwnbb/wsmanager/states"
)

// pingerStopTimeout bounds Pinger.Stop during teardown. A Stop that exceeds it
// is logged once and teardown proceeds; the leaked Stop goroutine is unblocked
// whenever the pinger implementation eventually returns.
const pingerStopTimeout = 5 * time.Second

type WSConnection struct {
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex

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

func (c *WSConnection) WriteJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(c.ctx, time.Minute)
	defer cancel()

	return wsjson.Write(ctx, c.conn, v)
}

// writeJSONWithCtx is the pinger entry point: it serializes payload under
// writeMu but uses the caller-supplied ctx as the per-attempt deadline budget
// instead of WriteJSON's fixed time.Minute. Not exported: pinger-only.
func (c *WSConnection) writeJSONWithCtx(ctx context.Context, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.conn, v)
}

func (m *WSManager) GetConnState() states.ConnectionState {
	return states.ConnectionState(atomic.LoadInt32((*int32)(&m.connState)))
}

// closeDetachedConn tears down a connection that has already been detached from
// m.conn (or is otherwise unshared). Locks must not be held by the caller.
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
		// Stop may block; bound it with a watchdog so a hung Stop cannot jam
		// Close/reconnect teardown. No manager logger is reachable here.
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn.pinger.Stop()
		}()
		select {
		case <-done:
		case <-time.After(pingerStopTimeout):
			slog.Default().Error("pinger Stop exceeded watchdog timeout", "timeout", pingerStopTimeout)
		}
	}
	if conn.conn != nil {
		_ = conn.conn.Close(closeCode, reason)
	}
}
