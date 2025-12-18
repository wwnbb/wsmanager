package wsmanager

import (
	"context"
	"github.com/coder/websocket/wsjson"
	"log/slog"
	"sync"
	"time"
)

const (
	DefaultWsPingInterval = 10 * time.Second
)

type Pinger interface {
	Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func())
	HandleMessage(
		ctx context.Context,
		conn *WSConnection,
		data any,
		logger *slog.Logger,
	) bool
	Stop()
}

type DefaultPinger struct {
	interval      time.Duration
	retries       int
	retryInterval time.Duration
	mu            sync.Mutex
	ticker        *time.Ticker
	done          chan struct{}
}

func NewDefaultPinger() *DefaultPinger {
	return &DefaultPinger{
		interval:      DefaultWsPingInterval,
		retries:       5,
		retryInterval: 5 * time.Second,
	}
}

func (p *DefaultPinger) Ping(ctx context.Context, conn *WSConnection, reqIdFunc func(topic string) string) error {
	payload := map[string]interface{}{
		"req_id": reqIdFunc("ping"),
		"op":     "ping",
	}

	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()

	writeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	return wsjson.Write(writeCtx, conn.Conn, payload)
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

func (p *DefaultPinger) Start(ctx context.Context, conn *WSConnection, logger *slog.Logger, reqIdFunc func(topic string) string, onError func()) {
	p.mu.Lock()
	p.ticker = time.NewTicker(p.interval)
	p.done = make(chan struct{})
	ticker := p.ticker
	done := p.done
	p.mu.Unlock()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if err := p.Ping(ctx, conn, reqIdFunc); err != nil {
					logger.Error("ping failed", "error", err)
					if onError != nil {
						onError()
					}
				}
			}
		}
	}()
}

func (p *DefaultPinger) HandleMessage(ctx context.Context, conn *WSConnection, data any, logger *slog.Logger) bool {
	return false
}

func (p *DefaultPinger) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

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
}
