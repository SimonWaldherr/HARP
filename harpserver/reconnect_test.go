package harpserver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReconnectBackoffGrowthCapAndReset(t *testing.T) {
	retry := &reconnectBackoff{
		base:    100 * time.Millisecond,
		current: 100 * time.Millisecond,
		max:     400 * time.Millisecond,
		state:   1,
	}

	assertJitterRange(t, retry.Next(), 80*time.Millisecond, 120*time.Millisecond)
	assertJitterRange(t, retry.Next(), 160*time.Millisecond, 240*time.Millisecond)
	assertJitterRange(t, retry.Next(), 320*time.Millisecond, 480*time.Millisecond)
	if retry.current != 400*time.Millisecond {
		t.Fatalf("current delay = %s, want capped 400ms", retry.current)
	}

	retry.Reset()
	if retry.current != 100*time.Millisecond {
		t.Fatalf("reset delay = %s, want 100ms", retry.current)
	}
}

func TestWaitForReconnectIsContextCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := waitForReconnect(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled reconnect wait took %s", elapsed)
	}
}

func assertJitterRange(t *testing.T, got, minimum, maximum time.Duration) {
	t.Helper()
	if got < minimum || got > maximum {
		t.Fatalf("delay %s outside [%s, %s]", got, minimum, maximum)
	}
}

func BenchmarkReconnectBackoffNext(b *testing.B) {
	retry := newReconnectBackoff(time.Second, "benchmark")
	b.ReportAllocs()
	for b.Loop() {
		_ = retry.Next()
		if retry.current == retry.max {
			retry.Reset()
		}
	}
}
