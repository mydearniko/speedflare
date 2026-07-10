package app

import (
	"context"
	"math"
	"sync"
	"time"
)

const bandwidthLimitBurst = 20 * time.Millisecond

// bandwidthLimiter is a concurrency-safe aggregate pacer. All workers for one
// transfer direction share an instance, so a configured limit applies to their
// combined throughput instead of to every connection independently.
type bandwidthLimiter struct {
	mu             sync.Mutex
	bytesPerSecond float64
	next           time.Time
}

func newBandwidthLimiter(mbps float64) *bandwidthLimiter {
	if mbps <= 0 || math.IsNaN(mbps) || math.IsInf(mbps, 0) {
		return nil
	}

	return &bandwidthLimiter{bytesPerSecond: mbps * 1e6 / 8}
}

func (l *bandwidthLimiter) wait(ctx context.Context, bytes int) error {
	if l == nil || bytes <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	seconds := float64(bytes) / l.bytesPerSecond
	maxSeconds := float64(math.MaxInt64) / float64(time.Second)
	var duration time.Duration
	if seconds >= maxSeconds {
		duration = time.Duration(math.MaxInt64)
	} else {
		duration = time.Duration(math.Ceil(seconds * float64(time.Second)))
	}
	if duration < time.Nanosecond {
		duration = time.Nanosecond
	}

	now := time.Now()
	l.mu.Lock()
	base := l.next
	burstStart := now.Add(-bandwidthLimitBurst)
	if base.Before(burstStart) {
		base = burstStart
	}
	ready := base.Add(duration)
	l.next = ready
	l.mu.Unlock()

	delay := time.Until(ready)
	if delay <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
