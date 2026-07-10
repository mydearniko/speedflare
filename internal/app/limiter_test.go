package app

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewBandwidthLimiterTreatsZeroAsUnlimited(t *testing.T) {
	for _, value := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if limiter := newBandwidthLimiter(value); limiter != nil {
			t.Fatalf("expected %v Mbps to disable limiting", value)
		}
	}
}

func TestBandwidthLimiterCapsAggregateConcurrentRate(t *testing.T) {
	const (
		limitMbps  = 8.0 // 1 MB/s
		chunkBytes = 20_000
		chunks     = 6
	)

	limiter := newBandwidthLimiter(limitMbps)
	start := time.Now()
	done := make(chan error, chunks)
	for range chunks {
		go func() {
			done <- limiter.wait(context.Background(), chunkBytes)
		}()
	}
	for range chunks {
		if err := <-done; err != nil {
			t.Fatalf("unexpected limiter error: %v", err)
		}
	}

	elapsed := time.Since(start)
	// 120 KB at 1 MB/s takes 120 ms. The limiter permits a 20 ms initial
	// burst, so completing materially sooner than 100 ms would exceed the cap.
	if elapsed < 90*time.Millisecond {
		t.Fatalf("aggregate limit was exceeded: completed in %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("limiter took unexpectedly long: %v", elapsed)
	}
}

func TestBandwidthLimiterWaitStopsOnContextCancellation(t *testing.T) {
	limiter := newBandwidthLimiter(0.008) // 1 KB/s
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- limiter.wait(ctx, 10_000)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("limiter did not stop promptly after cancellation")
	}
}
