package wsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultWsPingInterval is the Bybit-documented application ping period.
	DefaultWsPingInterval = 20 * time.Second
)

// Pinger manages application-level keepalive for a single connection generation.
// Stop may block; the manager bounds it with a watchdog timeout of pingerStopTimeout.
type Pinger interface {
	Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func())
	HandleMessage(
		ctx context.Context,
		conn *WSConnection,
		frame ReceivedFrame,
		logger *slog.Logger,
	) bool
	Stop()
}

// DefaultPinger implements Bybit JSON ping/pong liveness.
//
// Heartbeat attempt semantics:
//   - A heartbeat cycle runs on every ticker tick (interval, default 20s).
//   - At cycle start, stale pong notifications from prior cycles are drained once.
//   - Each cycle performs up to (retries+1) attempts (1 initial + retries).
//   - Each attempt is one shared budget of retryInterval covering the JSON ping
//     write and the subsequent pong wait (remaining budget). Defaults therefore
//     cap a full cycle at (retries+1)*retryInterval = 30s after the interval tick.
//   - Before each retry attempt (and once before final timeout failure), any
//     pending pong is accepted so a pong that arrives after a wait timeout keeps
//     the cycle alive even if a retry ping races with it.
//   - A documented pong (success:true, ret_msg:"pong", op:"ping") ends the cycle
//     successfully. Echoed req_id is not required (Spot may omit it).
//   - Active JSON ping write error (not intentional cancel) => onError once and
//     Start returns.
//   - All attempts exhausted without pong => onError once and Start returns.
//   - Manager ctx cancellation or Stop() aborts immediately without onError.
//
// Stop never closes the pong notification channel; Start may replace it.
type DefaultPinger struct {
	interval      time.Duration
	retries       int
	retryInterval time.Duration
	mu            sync.Mutex
	ticker        *time.Ticker
	done          chan struct{}
	pongCh        chan struct{} // buffered; signaled on pong; never closed by Stop
}

func NewDefaultPinger() *DefaultPinger {
	return &DefaultPinger{
		interval:      DefaultWsPingInterval,
		retries:       5,
		retryInterval: 5 * time.Second,
	}
}

func (p *DefaultPinger) Ping(ctx context.Context, conn *WSConnection, reqIdFunc func(topic string) string) error {
	payload := map[string]any{
		"req_id": reqIdFunc("ping"),
		"op":     "ping",
	}
	// ctx is the per-attempt budget (and inherits manager cancellation).
	return conn.writeJSONWithCtx(ctx, payload)
}

func (p *DefaultPinger) GetInterval() time.Duration {
	return p.interval
}

func (p *DefaultPinger) GetRetries() int {
	return p.retries
}

func (p *DefaultPinger) GetRetryInterval() time.Duration {
	return p.retryInterval
}

type pongWaitResult int

const (
	pongGot pongWaitResult = iota
	pongCanceled
	pongTimedOut
)

func chanClosed(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func drainPongCh(pongCh chan struct{}) {
	if pongCh == nil {
		return
	}
	for {
		select {
		case <-pongCh:
		default:
			return
		}
	}
}

// acceptPendingPong consumes one queued pong notification if present.
func acceptPendingPong(pongCh chan struct{}) bool {
	if pongCh == nil {
		return false
	}
	select {
	case <-pongCh:
		return true
	default:
		return false
	}
}

func waitPong(ctx context.Context, done <-chan struct{}, pongCh <-chan struct{}, timeout time.Duration) pongWaitResult {
	// Prefer a pong already queued before observing timeout/cancel.
	select {
	case <-pongCh:
		return pongGot
	default:
	}

	if timeout <= 0 {
		select {
		case <-ctx.Done():
			return pongCanceled
		case <-done:
			return pongCanceled
		default:
			return pongTimedOut
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return pongCanceled
	case <-done:
		return pongCanceled
	case <-pongCh:
		return pongGot
	case <-timer.C:
		return pongTimedOut
	}
}

// isBybitPong reports whether data is a documented Bybit application pong.
// Required fields: success=true, ret_msg="pong", op="ping". req_id is optional.
func isBybitPong(data []byte) bool {
	// Cheap prefilter: high-volume market frames skip full JSON decode.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	if !bytes.Contains(data, []byte(`"pong"`)) {
		return false
	}
	if !bytes.Contains(data, []byte(`"ping"`)) {
		return false
	}
	if !bytes.Contains(data, []byte(`success`)) {
		return false
	}

	var msg struct {
		Success bool   `json:"success"`
		RetMsg  string `json:"ret_msg"`
		Op      string `json:"op"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return false
	}
	return msg.Success && msg.RetMsg == "pong" && msg.Op == "ping"
}

// intentionalCancel reports whether a Ping error is due to manager ctx cancel or Stop.
func intentionalCancel(err error, ctx context.Context, done <-chan struct{}) bool {
	if err == nil {
		return false
	}
	if chanClosed(done) {
		return true
	}
	if ctx.Err() == nil {
		return false
	}
	// Parent ctx finished; attempt child surfaces Canceled or the parent's error.
	return errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err())
}

// heartbeat runs one liveness cycle. Returns false when Start should exit.
func (p *DefaultPinger) heartbeat(
	ctx context.Context,
	done <-chan struct{},
	pongCh chan struct{},
	conn *WSConnection,
	logger *slog.Logger,
	reqIdFunc func(topic string) string,
	onError func(),
	retries int,
	retryInterval time.Duration,
) bool {
	// Drop pongs left over from a previous successful/failed cycle only once.
	drainPongCh(pongCh)

	// attempts = 1 initial + retries follow-ups
	for attempt := 0; attempt <= retries; attempt++ {
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return false
		default:
		}

		// Pong may land after a prior wait timeout while we prepare a retry.
		if attempt > 0 && acceptPendingPong(pongCh) {
			return true
		}

		// One shared budget for write + pong wait.
		attemptCtx, attemptCancel := context.WithTimeout(ctx, retryInterval)

		// Stop() closes done without canceling manager ctx; fold it into the attempt.
		stopWatchExit := make(chan struct{})
		go func() {
			select {
			case <-done:
				attemptCancel()
			case <-stopWatchExit:
			}
		}()

		err := p.Ping(attemptCtx, conn, reqIdFunc)
		close(stopWatchExit)

		if err != nil {
			attemptCancel()
			if intentionalCancel(err, ctx, done) {
				return false
			}
			// Active write failure (including attempt-budget deadline): onError once.
			logger.Error("ping failed", "error", err)
			if onError != nil {
				onError()
			}
			return false
		}

		remaining := retryInterval
		if dl, ok := attemptCtx.Deadline(); ok {
			remaining = time.Until(dl)
		}
		// Keep attemptCtx live only for deadline math; wait uses parent ctx + done.
		attemptCancel()

		switch waitPong(ctx, done, pongCh, remaining) {
		case pongGot:
			return true
		case pongCanceled:
			return false
		case pongTimedOut:
			logger.Debug("pong wait timed out", "attempt", attempt+1, "max_attempts", retries+1)
		}
	}

	// Final chance: pong arrived while finishing the last attempt.
	if acceptPendingPong(pongCh) {
		return true
	}

	logger.Error("pong timeout exhausted", "retries", retries, "retry_interval", retryInterval)
	if onError != nil {
		onError()
	}
	return false
}

func (p *DefaultPinger) Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	p.mu.Lock()
	if p.ticker != nil {
		p.ticker.Stop()
	}
	if p.done != nil {
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}

	p.ticker = time.NewTicker(p.interval)
	p.done = make(chan struct{})
	// Replace pongCh each Start so reconnects do not see stale signals.
	// Never closed by Stop (callers may still hold a reference briefly).
	p.pongCh = make(chan struct{}, 1)

	ticker := p.ticker
	done := p.done
	pongCh := p.pongCh
	retries := p.retries
	retryInterval := p.retryInterval
	p.mu.Unlock()

	defer func() {
		ticker.Stop()

		p.mu.Lock()
		if p.ticker == ticker {
			p.ticker = nil
		}
		if p.done == done {
			p.done = nil
		}
		// Leave pongCh in place for HandleMessage; next Start replaces it.
		p.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if !p.heartbeat(ctx, done, pongCh, conn, logger, reqIdFunc, onError, retries, retryInterval) {
				return
			}
		}
	}
}

func (p *DefaultPinger) HandleMessage(ctx context.Context, conn *WSConnection, frame ReceivedFrame, logger *slog.Logger) bool {
	if !isBybitPong(frame.Data) {
		return false
	}

	p.mu.Lock()
	pongCh := p.pongCh
	p.mu.Unlock()

	if pongCh != nil {
		select {
		case pongCh <- struct{}{}:
		default:
			// Already have a pending pong notification; keep it.
		}
	}
	return true
}

func (p *DefaultPinger) Stop() {
	p.mu.Lock()
	ticker := p.ticker
	done := p.done
	p.ticker = nil
	p.done = nil
	// Intentionally do not close or nil pongCh.
	p.mu.Unlock()

	if ticker != nil {
		ticker.Stop()
	}
	if done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}
