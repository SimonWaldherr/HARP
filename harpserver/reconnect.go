package harpserver

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const (
	defaultReconnectInterval = 5 * time.Second
	maxReconnectDelay        = time.Minute
	stableConnectionDuration = 30 * time.Second
)

type reconnectBackoff struct {
	base    time.Duration
	current time.Duration
	max     time.Duration
	state   uint64
}

func newReconnectBackoff(base time.Duration, identity string) *reconnectBackoff {
	if base <= 0 {
		base = defaultReconnectInterval
	}
	state := uint64(time.Now().UnixNano())
	for i := range identity {
		state = state*1099511628211 ^ uint64(identity[i])
	}
	if state == 0 {
		state = 1
	}
	maxDelay := maxReconnectDelay
	if base > maxDelay {
		maxDelay = base
	}
	return &reconnectBackoff{base: base, current: base, max: maxDelay, state: state}
}

func (b *reconnectBackoff) Next() time.Duration {
	delay := b.current
	b.state ^= b.state << 13
	b.state ^= b.state >> 7
	b.state ^= b.state << 17
	// Apply ±20% jitter without using the globally locked math/rand source.
	jitterPermille := int64(b.state%401) - 200
	delay += time.Duration(int64(delay) * jitterPermille / 1000)

	if b.current < b.max {
		b.current *= 2
		if b.current > b.max || b.current < 0 {
			b.current = b.max
		}
	}
	return delay
}

func (b *reconnectBackoff) Reset() {
	b.current = b.base
}

func waitForReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newHarpClientConn(target string, reconnectInterval time.Duration) (*grpc.ClientConn, error) {
	baseDelay := reconnectInterval
	if baseDelay <= 0 || baseDelay > defaultReconnectInterval {
		baseDelay = defaultReconnectInterval
	}
	return grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  baseDelay,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   maxReconnectDelay,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
	)
}
