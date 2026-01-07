package wsmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/dustinxie/lockfree"

	json "github.com/goccy/go-json"

	pp "github.com/wwnbb/pprint"
	"github.com/wwnbb/wsmanager/bpool"
	"github.com/wwnbb/wsmanager/states"
)

type WSConnection struct {
	*websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	lastPing time.Time

	writeMu sync.Mutex
	pingMu  sync.Mutex
}

func (c *WSConnection) GetConn() *websocket.Conn {
	return c.Conn
}

func (c *WSConnection) getLastPing() time.Time {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	return c.lastPing
}

func (c *WSConnection) setLastPing(t time.Time) {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	c.lastPing = t
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

func (c *WSManager) GetConnState() states.ConnectionState {
	return states.ConnectionState(atomic.LoadInt32((*int32)(&c.connState)))
}

type WSManager struct {
	Logger *slog.Logger

	parentCtx context.Context
	ctx       context.Context
	ctxCancel context.CancelFunc

	connState states.ConnectionState
	Conn      *WSConnection
	url       string

	DataCh        chan json.RawMessage
	DisconnectSig chan struct{}
	disconnectWg  sync.WaitGroup

	requestIds lockfree.HashMap
	connMu     sync.RWMutex

	Pinger Pinger
}

func (m *WSManager) getConn() *WSConnection {
	m.connMu.RLock()
	defer m.connMu.RUnlock()
	return m.Conn
}

func (m *WSManager) setConn(conn *WSConnection) {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	m.Conn = conn
}

func (m *WSManager) SetPinger(pinger Pinger) {
	m.Pinger = pinger
}

func (m *WSManager) GetReqId(topic string) string {
	if n, exist := m.requestIds.Get(topic); exist {
		id := n.(int) + 1
		m.requestIds.Set(topic, id)
		return fmt.Sprintf("%s_%d", topic, id)
	}

	m.requestIds.Set(topic, 1)
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
	m.Logger.Debug("SetConnecting", "from", m.GetConnState(), "to", states.StateConnecting)
	return changeState(m.GetConnState(), states.StateConnecting, m)
}

func (m *WSManager) SetConnected() bool {
	m.Logger.Debug("SetConnected", "from", m.GetConnState(), "to", states.StateConnected)
	return changeState(states.StateConnecting, states.StateConnected, m)
}

func (m *WSManager) SetDisconnectedFromConnected() bool {
	m.Logger.Debug("SetDisconnected", "from", m.GetConnState(), "to", states.StateDisconnected)
	defer m.notifyDisconnect()
	return changeState(states.StateConnected, states.StateDisconnected, m)
}

func (m *WSManager) SetDisconnectedFromConnecting() bool {
	m.Logger.Debug("SetDisconnected", "from", m.GetConnState(), "to", states.StateDisconnected)
	defer m.notifyDisconnect()
	return changeState(states.StateConnecting, states.StateDisconnected, m)
}

func (m *WSManager) SetConnectingFromDisconnected() bool {
	m.Logger.Debug("SetConnecting", "from", m.GetConnState(), "to", states.StateConnecting)
	return changeState(states.StateDisconnected, states.StateConnecting, m)
}

func (m *WSManager) notifyDisconnect() {
	select {
	case m.DisconnectSig <- struct{}{}:
	default:
	}
}

func NewWSManager(url string, parentCtx context.Context, DataChSize ...int) *WSManager {
	ctx, cancel := context.WithCancel(parentCtx)
	var chSz int = 1000
	if len(DataChSize) > 0 && DataChSize[0] > 0 {
		chSz = DataChSize[0]
	}
	wsm := &WSManager{
		requestIds: lockfree.NewHashMap(),
		url:        url,
		connState:  states.StateNew,
		parentCtx:  parentCtx,
		ctx:        ctx,
		Logger:     slog.Default(),
		ctxCancel:  cancel,

		DataCh:        make(chan json.RawMessage, chSz),
		DisconnectSig: make(chan struct{}, 1),
		Pinger:        NewDefaultPinger(), // Use default pinger
	}
	return wsm
}

func (m *WSManager) SetLogger(Logger *slog.Logger) {
	m.Logger = Logger
}

func (m *WSManager) SetProductionLogger() {
	m.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (m *WSManager) refreshContext() error {
	m.disconnectWg.Wait()
	select {
	case <-m.parentCtx.Done():
		return fmt.Errorf("parent context done:  %w", m.parentCtx.Err())
	case <-m.ctx.Done():
		m.ctx, m.ctxCancel = context.WithCancel(m.parentCtx)
	default:
		return nil
	}
	return nil
}

func (m *WSManager) Connect() error {
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
		return fmt.Errorf("failed to refresh context: %w", err)
	}

	m.Logger.Info("Connecting to websocket", "url", m.url)

	conn := m.getConn()
	if conn != nil {
		if conn.cancel != nil {
			conn.cancel()
		}
		if conn.Conn != nil {
			conn.Conn.Close(1000, "Done")
		}
	}

	dialCtx, dialCancel := context.WithTimeout(m.ctx, 15*time.Second)
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

	connCtx, cancel := context.WithCancel(m.ctx)

	m.setConn(&WSConnection{
		Conn:     wsConn,
		ctx:      connCtx,
		cancel:   cancel,
		lastPing: time.Now(),
		writeMu:  sync.Mutex{},
	})
	conn = m.getConn()
	conn.Conn.SetReadLimit(-1)

	defer func() {
		if err != nil && conn != nil {
			if conn.cancel != nil {
				conn.cancel()
			}
			if conn.Conn != nil {
				conn.Conn.CloseNow()
			}
		}
	}()

	m.SetConnected()

	m.disconnectWg.Add(1)
	go func() {
		defer m.disconnectWg.Done()
		m.readMessages()
	}()
	m.disconnectWg.Add(1)
	go func() {
		defer m.disconnectWg.Done()
		onError := func() {
			m.SetDisconnectedFromConnected()
		}
		m.Pinger.Start(m.ctx, conn, m.Logger, m.GetReqId, onError)
	}()

	return nil
}

func (m *WSManager) handleReaderError(err error) {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			m.Logger.Debug("connection closed normally", "code", closeErr.Code, "reason", closeErr.Reason)
			return
		case websocket.StatusAbnormalClosure:
			m.Logger.Error("abnormal connection closure", "code", closeErr.Code, "reason", closeErr.Reason)
			m.SetDisconnectedFromConnected()
			return
		default:
			m.Logger.Error("connection closed with error", "code", closeErr.Code, "reason", closeErr.Reason)
			m.SetDisconnectedFromConnected()
			return
		}
	}

	if errors.Is(err, net.ErrClosed) {
		m.Logger.Debug("connection already closed")
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		m.Logger.Debug("read context deadline exceeded")
		return
	}
	if errors.Is(err, context.Canceled) {
		m.Logger.Debug("read context canceled")
		return
	}

	m.Logger.Error("failed to get reader", "error", err, "conn_state", m.GetConnState().String())
	m.SetDisconnectedFromConnected()
	return

}

func (m *WSManager) readMessages() {
	defer func() {
		if r := recover(); r != nil {
			m.Logger.Error("recovered from panic in readMessages", "panic", r)
			m.SetDisconnectedFromConnected()
			return
		}
	}()
	conn := m.getConn()

	if conn == nil {
		m.Logger.Error("readMessages: connection is nil, exiting")
		return
	}

	m.Logger.Debug("Starting ReadMessages")
	defer m.Logger.Debug("Exiting ReadMessages")

	for {
		select {
		case <-m.ctx.Done():
			m.Logger.Debug("Read messages context done, exiting")
			return
		default:
			if m.GetConnState() != states.StateConnected {
				return
			}

			func() {
				defer func() {
					if r := recover(); r != nil {
						m.Logger.Error("recovered from panic in ReadMessage", "panic", r)
						m.SetDisconnectedFromConnected()
						return
					}
				}()

				_, reader, err := conn.Conn.Reader(m.ctx)
				if err != nil {
					m.handleReaderError(err)
					return
				}

				b := bpool.Get()
				defer bpool.Put(b)

				_, err = b.ReadFrom(reader)
				if err != nil {
					if strings.Contains(err.Error(), "context deadline exceeded") {
						m.Logger.Error("read timeout")
						m.SetDisconnectedFromConnected()
						return
					}
					m.Logger.Error("failed to read message", "error", err)
					return
				}

				var data json.RawMessage
				if err := json.Unmarshal(b.Bytes(), &data); err != nil {
					m.Logger.Error("failed to unmarshal message", "error", err)
					return
				}

				if m.Pinger != nil && m.Pinger.HandleMessage(m.ctx, conn, data, m.Logger) {
					return
				}

				select {
				case m.DataCh <- data:
					m.Logger.Debug("received message", "data", pp.PrettyFormat(data))
				default:
					m.Logger.Error("message buffer full, dropping message")
				}
			}()
		}
	}
}

func (m *WSManager) Close() error {
	fmt.Println("Closing WSManager")
	m.SetDisconnectedFromConnected()

	if m.Pinger != nil {
		m.Pinger.Stop()
	}

	m.ctxCancel()
	conn := m.getConn()
	if conn != nil {
		if conn.cancel != nil {
			conn.cancel()
		}
		if conn.Conn != nil {
			fmt.Println("Closing websocket connection")
			conn.Conn.Close(1000, "done")
		}
	}
	return nil
}

func (m *WSManager) SendRequest(v interface{}) error {
	conn := m.getConn()
	if state := m.GetConnState(); state != states.StateConnected {
		return fmt.Errorf("websocket not connected, current state: %s", state.String())
	}

	if conn == nil {
		return fmt.Errorf("websocket connection is nil")
	}

	err := conn.WriteJSON(v)
	if err != nil {
		m.Logger.Error("failed to send message", "error", err)
		m.SetDisconnectedFromConnected()
		return fmt.Errorf("failed to send message: %w", err)
	} else {
		m.Logger.Debug("sent message", "data", pp.PrettyFormat(v))
	}
	return nil
}
