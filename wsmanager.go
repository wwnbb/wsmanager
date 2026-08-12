package wsmanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/coder/websocket"

	"github.com/jeremiah-masters/dlht"

	pp "github.com/wwnbb/pprint"
	"github.com/wwnbb/wsmanager/bpool"
	"github.com/wwnbb/wsmanager/states"
)

// DefaultReadLimit is the default maximum size in bytes of a complete inbound
// WebSocket message (4 MiB).
const DefaultReadLimit int64 = 4 << 20

type WSManager struct {
	logger atomic.Pointer[slog.Logger]

	parentCtx context.Context
	ctx       context.Context
	ctxCancel context.CancelFunc
	ctxMu     sync.RWMutex // guards ctx / ctxCancel

	connState states.ConnectionState
	conn      *WSConnection
	url       string

	// DataCh preserves frame order for delivered frames and blocks the reader when
	// its buffer is full. Queued frames remain readable after Close. An in-flight
	// frame blocked on send is dropped when the connection context is canceled;
	// callers need not drain DataCh for Wait to return.
	DataCh        chan ReceivedFrame
	DisconnectSig chan struct{}
	disconnectWg  sync.WaitGroup

	requestIds *dlht.Map[string, int]
	reqIDMu    sync.Mutex
	connMu     sync.RWMutex
	connectMu  sync.Mutex
	cfgMu      sync.RWMutex
	readLimit  int64 // complete inbound message limit; guarded by cfgMu

	nextConnID atomic.Uint64

	Pinger Pinger
}

// Wait blocks until the current connection's reader and pinger have stopped.
// Queued DataCh frames remain readable; draining is not required for Wait to return.
func (m *WSManager) Wait() {
	// Serialize with an in-flight Connect so its disconnectWg.Add is registered
	// before the counter is observed. Acquire/release only: refreshContext
	// waits on disconnectWg directly and must not hold connectMu (deadlock).
	m.connectMu.Lock()
	m.connectMu.Unlock()
	m.disconnectWg.Wait()
}

func (m *WSManager) getConn() *WSConnection {
	m.connMu.RLock()
	defer m.connMu.RUnlock()
	return m.conn
}

func (m *WSManager) setConn(conn *WSConnection) {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	if conn != nil {
		conn.id = m.nextConnID.Add(1)
	}
	m.conn = conn
}

func (m *WSManager) isCurrentConn(conn *WSConnection) bool {
	if conn == nil {
		return false
	}
	m.connMu.RLock()
	defer m.connMu.RUnlock()
	return m.conn != nil && m.conn.id == conn.id
}

func (m *WSManager) closeConn(closeCode websocket.StatusCode, reason string) {
	m.connMu.Lock()
	conn := m.conn
	m.conn = nil
	m.connMu.Unlock()

	closeDetachedConn(conn, closeCode, reason, true)
}

// closeConnIfCurrent detaches and closes conn only when it is still the active connection.
func (m *WSManager) closeConnIfCurrent(conn *WSConnection, closeCode websocket.StatusCode, reason string) bool {
	if conn == nil {
		return false
	}
	m.connMu.Lock()
	if m.conn == nil || m.conn.id != conn.id {
		m.connMu.Unlock()
		return false
	}
	current := m.conn
	m.conn = nil
	m.connMu.Unlock()

	closeDetachedConn(current, closeCode, reason, true)
	return true
}

func (m *WSManager) SetPinger(pinger Pinger) {
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	m.Pinger = pinger
}

func (m *WSManager) getPinger() Pinger {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.Pinger
}

// SetReadLimit sets the maximum complete inbound WebSocket message size in bytes.
// limit must be positive. The value is stored for future connections and applied
// immediately to the current connection when one exists.
//
// Lock ordering: cfgMu then connMu (never inverted) so Connect cannot install a
// connection with a stale limit while a concurrent SetReadLimit runs.
func (m *WSManager) SetReadLimit(limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("invalid read limit: %d (must be positive)", limit)
	}

	m.cfgMu.Lock()
	m.readLimit = limit
	m.connMu.RLock()
	conn := m.conn
	m.connMu.RUnlock()
	if conn != nil && conn.conn != nil {
		conn.conn.SetReadLimit(limit)
	}
	m.cfgMu.Unlock()
	return nil
}

// applyReadLimitLocked applies the current read limit to conn.
// Caller must hold cfgMu (read or write).
func (m *WSManager) applyReadLimitLocked(conn *WSConnection) {
	if conn == nil || conn.conn == nil {
		return
	}
	conn.conn.SetReadLimit(m.readLimit)
}

func (m *WSManager) GetReqId(topic string) string {
	m.reqIDMu.Lock()
	defer m.reqIDMu.Unlock()

	if n, exist := m.requestIds.Get(topic); exist {
		id := n + 1
		m.requestIds.Put(topic, id)
		return fmt.Sprintf("%s_%d", topic, id)
	}

	m.requestIds.Insert(topic, 1)
	return fmt.Sprintf("%s_%d", topic, 1)
}

/*
 ******************************
* Connection State Transitions *
 ******************************
*/

func changeState(from, to states.ConnectionState, m *WSManager) bool {
	if m == nil {
		return false
	}
	return atomic.CompareAndSwapInt32((*int32)(&m.connState), int32(from), int32(to))
}

func (m *WSManager) SetConnecting() bool {
	m.Logger().Debug("SetConnecting", "from", m.GetConnState(), "to", states.StateConnecting)
	currentState := m.GetConnState()
	switch currentState {
	case states.StateNew, states.StateDisconnected:
		return changeState(currentState, states.StateConnecting, m)
	default:
		return false
	}
}

func (m *WSManager) SetConnected() bool {
	m.Logger().Debug("SetConnected", "from", m.GetConnState(), "to", states.StateConnected)
	return changeState(states.StateConnecting, states.StateConnected, m)
}

func (m *WSManager) setDisconnected(from states.ConnectionState, reason string) bool {
	m.Logger().Debug("SetDisconnected", "from", m.GetConnState(), "to", states.StateDisconnected, "reason", reason)
	if !changeState(from, states.StateDisconnected, m) {
		return false
	}

	m.closeConn(websocket.StatusNormalClosure, reason)
	m.notifyDisconnect()
	return true
}

// failConnection disconnects only when conn is still the active connection and
// the manager is Connected. Stale failures from older connections are ignored.
// Always stops the snapshotted pinger (reader/send paths).
func (m *WSManager) failConnection(conn *WSConnection, reason string) bool {
	return m.failConnectionLocked(conn, reason, true)
}

// failConnectionFromPinger is the generation-scoped failure path for callbacks
// originating inside Pinger.Start (onError or a recovered panic).
//
// It transitions state, detaches the connection, cancels conn.ctx, closes the
// websocket, and signals DisconnectSig — but does not call Pinger.Stop.
// onError is the terminal callback for that Start invocation; Start must return
// after invoking it. Calling Stop here would deadlock if Stop waits for Start.
func (m *WSManager) failConnectionFromPinger(conn *WSConnection, reason string) bool {
	return m.failConnectionLocked(conn, reason, false)
}

// failConnectionLocked performs generation-aware Connected→Disconnected teardown.
// Manager locks are released before cancel/Close/log/notify.
func (m *WSManager) failConnectionLocked(conn *WSConnection, reason string, stopPinger bool) bool {
	if conn == nil {
		return false
	}

	m.connMu.Lock()
	if m.conn == nil || m.conn.id != conn.id {
		m.connMu.Unlock()
		return false
	}
	if !changeState(states.StateConnected, states.StateDisconnected, m) {
		m.connMu.Unlock()
		return false
	}
	current := m.conn
	m.conn = nil
	m.connMu.Unlock()

	m.Logger().Debug("SetDisconnected", "from", states.StateConnected, "to", states.StateDisconnected, "reason", reason)
	closeDetachedConn(current, websocket.StatusNormalClosure, reason, stopPinger)
	m.notifyDisconnect()
	return true
}

func (m *WSManager) SetDisconnectedFromConnected() bool {
	return m.setDisconnected(states.StateConnected, "connection lost")
}

func (m *WSManager) SetDisconnectedFromConnecting() bool {
	return m.setDisconnected(states.StateConnecting, "connect failed")
}

func (m *WSManager) SetConnectingFromDisconnected() bool {
	m.Logger().Debug("SetConnecting", "from", m.GetConnState(), "to", states.StateConnecting)
	return changeState(states.StateDisconnected, states.StateConnecting, m)
}

func (m *WSManager) notifyDisconnect() {
	select {
	case m.DisconnectSig <- struct{}{}:
	default:
	}
}

// NewWSManager creates a manager. nil logger becomes slog.Default().
// dataChSize is optional; default buffer is 1000.
func NewWSManager(url string, parentCtx context.Context, logger *slog.Logger, dataChSize ...int) *WSManager {
	ctx, cancel := context.WithCancel(parentCtx)
	chSz := 1000
	if len(dataChSize) > 0 && dataChSize[0] > 0 {
		chSz = dataChSize[0]
	}
	if logger == nil {
		logger = slog.Default()
	}
	wsm := &WSManager{
		requestIds:    dlht.New[string, int](dlht.Options{}),
		url:           url,
		connState:     states.StateNew,
		parentCtx:     parentCtx,
		ctx:           ctx,
		ctxCancel:     cancel,
		readLimit:     DefaultReadLimit,
		DataCh:        make(chan ReceivedFrame, chSz),
		DisconnectSig: make(chan struct{}, 1),
		Pinger:        NewDefaultPinger(),
	}
	wsm.logger.Store(logger)
	return wsm
}

// Logger returns the current logger. Always non-nil.
func (m *WSManager) Logger() *slog.Logger {
	l := m.logger.Load()
	if l == nil {
		return slog.Default()
	}
	return l
}

// SetLogger replaces the logger atomically; nil becomes slog.Default().
// Safe to call concurrently with Connect and the reader/pinger goroutines.
func (m *WSManager) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	m.logger.Store(logger)
}

func (m *WSManager) managerCtx() context.Context {
	m.ctxMu.RLock()
	defer m.ctxMu.RUnlock()
	return m.ctx
}

func (m *WSManager) cancelManagerCtx() {
	m.ctxMu.RLock()
	cancel := m.ctxCancel
	m.ctxMu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (m *WSManager) refreshContext() error {
	m.disconnectWg.Wait()

	m.ctxMu.Lock()
	defer m.ctxMu.Unlock()

	select {
	case <-m.parentCtx.Done():
		return fmt.Errorf("parent context done:  %w", m.parentCtx.Err())
	default:
	}

	select {
	case <-m.ctx.Done():
		m.ctx, m.ctxCancel = context.WithCancel(m.parentCtx)
	default:
	}
	return nil
}

func (m *WSManager) Connect() error {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()

	currentState := m.GetConnState()
	var transitionOk bool

	switch currentState {
	case states.StateNew:
		transitionOk = m.SetConnecting()
	case states.StateDisconnected:
		transitionOk = m.SetConnecting()
	default:
		return fmt.Errorf("state transition failed from %s to %s", currentState, states.StateConnecting)
	}
	if !transitionOk {
		return fmt.Errorf("state transition failed from %s to %s", currentState, states.StateConnecting)
	}

	err := m.refreshContext()
	if err != nil {
		m.SetDisconnectedFromConnecting()
		return fmt.Errorf("failed to refresh context: %w", err)
	}

	// Stable snapshot for dial + conn lifetime (ctxMu-protected).
	mgrCtx := m.managerCtx()

	m.Logger().Info("Connecting to websocket", "url", m.url)

	existing := m.getConn()
	if existing != nil {
		m.closeConn(websocket.StatusNormalClosure, "replacing connection")
	}

	dialCtx, dialCancel := context.WithTimeout(mgrCtx, 15*time.Second)
	defer dialCancel()

	opts := &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				DisableKeepAlives:     false,
				MaxIdleConnsPerHost:   -1,
			},
		},
	}

	wsConn, _, err := websocket.Dial(dialCtx, m.url, opts)
	if err != nil {
		m.SetDisconnectedFromConnecting()
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	if err := mgrCtx.Err(); err != nil {
		// Connection is not published yet; skip the close-handshake (peer may be
		// slow or unresponsive) and tear down the socket immediately.
		_ = wsConn.CloseNow()
		m.SetDisconnectedFromConnecting()
		return fmt.Errorf("connect canceled during dial: %w", err)
	}

	connCtx, cancel := context.WithCancel(mgrCtx)
	pinger := m.getPinger()

	conn := &WSConnection{
		conn:    wsConn,
		ctx:     connCtx,
		cancel:  cancel,
		writeMu: sync.Mutex{},
		pinger:  pinger,
	}
	m.setConn(conn)
	// Apply limit under cfgMu so a concurrent SetReadLimit cannot race to a stale value.
	m.cfgMu.RLock()
	m.applyReadLimitLocked(conn)
	m.cfgMu.RUnlock()

	// Register workers once before Connected is visible; never Add after Start.
	workers := 1
	if pinger != nil {
		workers++
	}
	m.disconnectWg.Add(workers)

	if !m.SetConnected() {
		m.disconnectWg.Add(-workers)
		state := m.GetConnState()
		// Roll back Connecting and drop this attempt without touching a newer conn.
		if !m.SetDisconnectedFromConnecting() {
			m.closeConnIfCurrent(conn, websocket.StatusInternalError, "connect failed")
		}
		return fmt.Errorf("state transition failed from %s to %s", state, states.StateConnected)
	}

	go func() {
		defer m.disconnectWg.Done()
		m.readMessages(conn)
	}()

	if pinger != nil {
		logger := m.Logger()
		go func() {
			defer m.disconnectWg.Done()
			defer func() {
				if r := recover(); r != nil {
					m.Logger().Error("recovered from panic in Pinger.Start", "panic", r)
					// Same no-Stop path as onError: Start is unwinding through recover.
					m.failConnectionFromPinger(conn, "connection lost")
				}
			}()
			// onError is terminal for this Start: do not Stop from that path.
			onError := func() {
				m.failConnectionFromPinger(conn, "connection lost")
			}
			pinger.Start(conn.ctx, conn, logger, m.GetReqId, onError)
			// Start returned without onError: exit-failConnection safety net.
			// Idempotent no-op after onError or intentional Close.
			m.failConnection(conn, "connection lost")
		}()
	}

	return nil
}

func (m *WSManager) handleReaderError(conn *WSConnection, err error) {
	logger := m.Logger()
	if !m.isCurrentConn(conn) || m.GetConnState() != states.StateConnected {
		logger.Debug("reader error after disconnect", "error", err)
		return
	}

	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			logger.Warn("connection closed", "code", closeErr.Code, "reason", closeErr.Reason)
		case websocket.StatusAbnormalClosure:
			logger.Error("abnormal connection closure", "code", closeErr.Code, "reason", closeErr.Reason)
		default:
			logger.Error("connection closed with error", "code", closeErr.Code, "reason", closeErr.Reason)
		}
		m.failConnection(conn, "connection lost")
		return
	}

	if errors.Is(err, net.ErrClosed) {
		logger.Warn("connection already closed")
		m.failConnection(conn, "connection lost")
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("read context deadline exceeded")
		m.failConnection(conn, "connection lost")
		return
	}
	if errors.Is(err, context.Canceled) {
		logger.Warn("read context canceled")
		m.failConnection(conn, "connection lost")
		return
	}

	logger.Error("failed to get reader", "error", err, "conn_state", m.GetConnState().String())
	m.failConnection(conn, "connection lost")
}

func (m *WSManager) readMessages(conn *WSConnection) {
	logger := m.Logger()
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in readMessages", "panic", r)
			m.failConnection(conn, "connection lost")
			return
		}
	}()

	if conn == nil {
		logger.Error("readMessages: connection is nil, exiting")
		return
	}

	logger.Debug("Starting ReadMessages")
	defer logger.Debug("Exiting ReadMessages")

loop:
	for {
		select {
		case <-conn.ctx.Done():
			logger.Debug("Read messages context done, exiting")
			break loop
		default:
		}

		if !m.isCurrentConn(conn) || m.GetConnState() != states.StateConnected {
			break loop
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("recovered from panic in ReadMessage", "panic", r)
					m.failConnection(conn, "connection lost")
					return
				}
			}()

			messageType, reader, err := conn.conn.Reader(conn.ctx)
			if err != nil {
				m.handleReaderError(conn, err)
				return
			}

			b := bpool.Get()
			defer bpool.Put(b)

			_, err = b.ReadFrom(reader)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					logger.Error("read timeout")
				} else {
					logger.Error("failed to read message", "error", err)
				}
				m.failConnection(conn, "connection lost")
				return
			}
			receivedAt := time.Now()
			frame := ReceivedFrame{
				Type:       messageType,
				Data:       append([]byte(nil), b.Bytes()...),
				ReceivedAt: receivedAt,
			}

			if conn.pinger != nil && conn.pinger.HandleMessage(conn.ctx, conn, frame, logger) {
				return
			}

			// Cancellation wins over a blocked DataCh send: drop this in-flight frame.
			select {
			case m.DataCh <- frame:
				if logger.Enabled(context.Background(), slog.LevelDebug) {
					logger.Debug("received message", "type", frame.Type, "data", pp.PrettyFormat(frame.Data))
				}
			case <-conn.ctx.Done():
				logger.Debug("dropping in-flight frame on connection cancel")
				return
			}
		}()
	}

	// Every loop exit lands here. Generation-guarded and idempotent: no-op on
	// intentional Close (already detached/Disconnected) and on stale generations.
	m.failConnection(conn, "connection lost")
}

// Close stops the current connection. DataCh remains open for reconnects.
// Manager context is canceled before connectMu is acquired so an in-flight
// Connect's dial (derived from mgrCtx) fails promptly and Close does not
// block for up to the dial timeout. Reader/pinger observe cancellation before
// the connection is detached and closed.
func (m *WSManager) Close() error {
	m.cancelManagerCtx()

	m.connectMu.Lock()
	defer m.connectMu.Unlock()

	m.SetDisconnectedFromConnected()
	m.SetDisconnectedFromConnecting()
	m.closeConn(websocket.StatusNormalClosure, "done")
	return nil
}

func (m *WSManager) SendRequest(v any) error {
	// Coherent snapshot: capture the connection pointer once, then validate state.
	conn := m.getConn()
	if state := m.GetConnState(); state != states.StateConnected {
		return fmt.Errorf("websocket not connected, current state: %s", state.String())
	}
	if conn == nil {
		return fmt.Errorf("websocket connection is nil")
	}

	err := conn.WriteJSON(v)
	if err != nil {
		m.Logger().Error("failed to send message", "error", err)
		// Only tear down this connection generation; never a newer one.
		m.failConnection(conn, "connection lost")
		return fmt.Errorf("failed to send message: %w", err)
	}

	logger := m.Logger()
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		logger.Debug("sent message", "data", pp.PrettyFormat(v))
	}
	return nil
}
