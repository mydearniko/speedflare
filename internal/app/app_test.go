package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestThroughputRecorderSeparatesWarmupFromMeasuredBytes(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 40*time.Millisecond, 120*time.Millisecond)

	recorder.Add(1000)
	time.Sleep(60 * time.Millisecond)
	recorder.Add(4000)
	time.Sleep(40 * time.Millisecond)
	recorder.Add(6000)
	time.Sleep(140 * time.Millisecond)
	recorder.Add(8000)

	if got, want := atomic.LoadInt64(&recorder.totalBytes), int64(19000); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
	if got, want := atomic.LoadInt64(&recorder.measuredBytes), int64(10000); got != want {
		t.Fatalf("measured bytes mismatch: got %d want %d", got, want)
	}
}

func TestThroughputRecorderFallsBackBeforeWarmupCompletes(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), time.Second, time.Second)

	recorder.Add(5000)
	time.Sleep(50 * time.Millisecond)
	summary := recorder.summarize(time.Now())

	if summary.mbps <= 0 {
		t.Fatalf("expected fallback throughput to be positive, got %.2f", summary.mbps)
	}
	if summary.dataMB <= 0 {
		t.Fatalf("expected positive data usage, got %.4f", summary.dataMB)
	}
}

func TestLiveSpeedEstimatorReturnsImmediateNumericEstimate(t *testing.T) {
	now := time.Now()
	recorder := &throughputRecorder{
		start:       now.Add(-700 * time.Millisecond),
		warmup:      speedTestWarmup,
		measure:     speedTestMeasure,
		firstByteNs: 0,
		firstByteCh: make(chan struct{}),
	}
	atomic.StoreInt64(&recorder.totalBytes, 12_000_000)

	estimator := newLiveSpeedEstimator(recorder)
	mbps := estimator.current(now)
	if mbps <= 0 {
		t.Fatalf("expected positive live Mbps estimate, got %.2f", mbps)
	}
}

func TestLiveSpeedEstimatorUsesRollingWindow(t *testing.T) {
	now := time.Now()
	recorder := &throughputRecorder{
		start:       now.Add(-2 * time.Second),
		warmup:      speedTestWarmup,
		measure:     speedTestMeasure,
		firstByteNs: 0,
		firstByteCh: make(chan struct{}),
	}
	estimator := newLiveSpeedEstimator(recorder)

	atomic.StoreInt64(&recorder.totalBytes, 10_000_000)
	first := estimator.current(now.Add(-900 * time.Millisecond))
	atomic.StoreInt64(&recorder.totalBytes, 34_000_000)
	second := estimator.current(now)

	if first <= 0 {
		t.Fatalf("expected first estimate to be positive, got %.2f", first)
	}
	if second <= first {
		t.Fatalf("expected rolling estimate to react to increased throughput, got first %.2f second %.2f", first, second)
	}
}

func TestUploadStreamCountsBytesRead(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, time.Second)
	stream := newUploadStream(context.Background(), recorder)
	buf := make([]byte, 32*1024)

	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("read length mismatch: got %d want %d", n, len(buf))
	}

	if got, want := atomic.LoadInt64(&recorder.totalBytes), int64(len(buf)); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
	if got, want := atomic.LoadInt64(&recorder.measuredBytes), int64(len(buf)); got != want {
		t.Fatalf("measured bytes mismatch: got %d want %d", got, want)
	}
}
