package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestThroughputRecorderSeparatesWarmupFromMeasuredBytes(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 40*time.Millisecond, 120*time.Millisecond)

	recorder.Add(1000)
	time.Sleep(60 * time.Millisecond)
	recorder.Add(4000)
	time.Sleep(40 * time.Millisecond)
	recorder.Add(6000)
	time.Sleep(140 * time.Millisecond)
	recorder.Add(8000)

	if got, want := recorder.totalBytes.Load(), int64(19000); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
	if got, want := recorder.measuredBytes.Load(), int64(10000); got != want {
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

func TestThroughputRecorderUnlimitedMeasureKeepsAccumulating(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 20*time.Millisecond, 0)

	recorder.Add(1000)
	time.Sleep(30 * time.Millisecond)
	recorder.Add(4000)
	time.Sleep(50 * time.Millisecond)
	recorder.Add(6000)

	if got, want := recorder.totalBytes.Load(), int64(11000); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
	if got, want := recorder.measuredBytes.Load(), int64(10000); got != want {
		t.Fatalf("measured bytes mismatch: got %d want %d", got, want)
	}

	if got := recorder.measuredDuration(time.Now()); got < 50*time.Millisecond {
		t.Fatalf("expected measured duration to keep growing in continuous mode, got %v", got)
	}
}

func TestLiveSpeedEstimatorReturnsImmediateNumericEstimate(t *testing.T) {
	now := time.Now()
	recorder := &throughputRecorder{
		start:       now.Add(-700 * time.Millisecond),
		warmup:      speedTestWarmup,
		measure:     speedTestMeasure,
		firstByteCh: make(chan struct{}),
	}
	recorder.firstByteNs.Store(0)
	recorder.totalBytes.Store(12_000_000)

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
		firstByteCh: make(chan struct{}),
	}
	estimator := newLiveSpeedEstimator(recorder)

	recorder.firstByteNs.Store(0)
	recorder.totalBytes.Store(10_000_000)
	first := estimator.current(now.Add(-900 * time.Millisecond))
	recorder.totalBytes.Store(34_000_000)
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

	if got, want := recorder.totalBytes.Load(), int64(len(buf)); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
	if got, want := recorder.measuredBytes.Load(), int64(len(buf)); got != want {
		t.Fatalf("measured bytes mismatch: got %d want %d", got, want)
	}
}

func TestThroughputClientDisablesClientTimeout(t *testing.T) {
	transport := &http.Transport{}
	base := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	got := throughputClient(base)
	if got == base {
		t.Fatalf("expected throughputClient to clone timed client")
	}
	if got.Timeout != 0 {
		t.Fatalf("expected throughput client timeout to be disabled, got %v", got.Timeout)
	}
	if got.Transport != transport {
		t.Fatalf("expected throughput client to reuse transport")
	}
	if base.Timeout != 30*time.Second {
		t.Fatalf("expected base client timeout to remain unchanged, got %v", base.Timeout)
	}
}

func TestRunTestPassesTimeoutlessClientToWorker(t *testing.T) {
	base := &http.Client{
		Transport: &http.Transport{},
		Timeout:   30 * time.Second,
	}

	observedTimeout := make(chan time.Duration, 1)
	runTest("Upload:", func(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool) {
		observedTimeout <- client.Timeout
		recorder.Add(128 * 1024)
	}, base, false, 1, true, false)

	select {
	case got := <-observedTimeout:
		if got != 0 {
			t.Fatalf("expected worker to receive timeoutless client, got %v", got)
		}
	default:
		t.Fatal("expected worker to observe throughput client")
	}

	if base.Timeout != 30*time.Second {
		t.Fatalf("expected base client timeout to remain unchanged, got %v", base.Timeout)
	}
}

func TestDownloadWorkerRestartsRequestsUntilContextCanceled(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	payload := bytes.Repeat([]byte("d"), 32*1024)
	var calls int32
	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			call := atomic.AddInt32(&calls, 1)
			if call == 3 {
				cancel()
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(payload)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	downloadWorker(ctx, recorder, client, true)

	if got, want := atomic.LoadInt32(&calls), int32(3); got != want {
		t.Fatalf("request count mismatch: got %d want %d", got, want)
	}
	if got, want := recorder.totalBytes.Load(), int64(3*len(payload)); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
}

func TestUploadWorkerRestartsRequestsUntilContextCanceled(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chunkSize := 32 * 1024
	var calls int32
	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			call := atomic.AddInt32(&calls, 1)

			buf := make([]byte, chunkSize)
			if _, err := io.ReadFull(req.Body, buf); err != nil {
				return nil, err
			}
			req.Body.Close()

			if call == 3 {
				cancel()
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	uploadWorker(ctx, recorder, client, true)

	if got, want := atomic.LoadInt32(&calls), int32(3); got != want {
		t.Fatalf("request count mismatch: got %d want %d", got, want)
	}
	if got, want := recorder.totalBytes.Load(), int64(3*chunkSize); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
}

func TestUploadWorkerRetriesDoErrorsInContinuousMode(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			call := atomic.AddInt32(&calls, 1)
			if call == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			cancel()
			req.Body.Close()
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	uploadWorker(ctx, recorder, client, true)

	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("request count mismatch: got %d want %d", got, want)
	}
}

func TestUploadWorkerRetriesHTTPFailuresInContinuousMode(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			call := atomic.AddInt32(&calls, 1)
			req.Body.Close()
			if call == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(bytes.NewReader(nil)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}

			cancel()
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	uploadWorker(ctx, recorder, client, true)

	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("request count mismatch: got %d want %d", got, want)
	}
}
