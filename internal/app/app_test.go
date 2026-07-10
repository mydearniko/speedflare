package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idanyas/speedflare/internal/data"
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

func TestUploadLiveSpeedEstimatorDampensShortBursts(t *testing.T) {
	now := time.Now()
	recorder := &throughputRecorder{
		start:       now.Add(-2500 * time.Millisecond),
		warmup:      0,
		measure:     speedTestMeasure,
		firstByteCh: make(chan struct{}),
	}
	recorder.firstByteNs.Store(0)

	estimator := newLiveSpeedEstimatorWithConfig(recorder, uploadLiveSpeedEstimatorConfig)

	recorder.totalBytes.Store(100_000_000)
	recorder.measuredBytes.Store(100_000_000)
	first := estimator.current(now)

	recorder.totalBytes.Store(180_000_000)
	recorder.measuredBytes.Store(180_000_000)
	second := estimator.current(now.Add(100 * time.Millisecond))

	if first <= 0 {
		t.Fatalf("expected positive first estimate, got %.2f", first)
	}
	if second > first*1.25 {
		t.Fatalf("expected upload live estimate to dampen a short burst, got first %.2f second %.2f", first, second)
	}
}

func TestBidirectionalModeRunsLatencyAndBothThroughputDirections(t *testing.T) {
	if !TestModeBidirectional.runsLatency() {
		t.Fatal("expected bidirectional mode to run latency")
	}
	if !TestModeBidirectional.runsDownload() {
		t.Fatal("expected bidirectional mode to run download")
	}
	if !TestModeBidirectional.runsUpload() {
		t.Fatal("expected bidirectional mode to run upload")
	}
}

func TestUploadStreamCountsBytesRead(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, time.Second)
	stream := newUploadStream(context.Background(), recorder, 32*1024, nil)
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

func TestUploadStreamStopsAtConfiguredSize(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, time.Second)
	stream := newUploadStream(context.Background(), recorder, 40*1024, nil)
	buf := make([]byte, 32*1024)

	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("unexpected first read error: %v", err)
	}
	if n != 32*1024 {
		t.Fatalf("first read length mismatch: got %d want %d", n, 32*1024)
	}

	n, err = stream.Read(buf)
	if err != nil {
		t.Fatalf("unexpected second read error: %v", err)
	}
	if n != 8*1024 {
		t.Fatalf("second read length mismatch: got %d want %d", n, 8*1024)
	}

	n, err = stream.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected EOF after configured size, got n=%d err=%v", n, err)
	}
	if got, want := recorder.totalBytes.Load(), int64(40*1024); got != want {
		t.Fatalf("total bytes mismatch: got %d want %d", got, want)
	}
}

func TestUploadStreamStopsBeforeRecordingWhenLimitWaitIsCanceled(t *testing.T) {
	recorder := newThroughputRecorder(time.Now(), 0, time.Second)
	limiter := newBandwidthLimiter(0.008) // 1 KB/s
	ctx, cancel := context.WithCancel(context.Background())
	stream := newUploadStream(ctx, recorder, 32*1024, limiter)
	buf := make([]byte, 32*1024)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	n, err := stream.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got n=%d err=%v", n, err)
	}
	if n != 0 {
		t.Fatalf("expected no bytes after canceled limiter wait, got %d", n)
	}
	if got := recorder.totalBytes.Load(); got != 0 {
		t.Fatalf("expected canceled bytes not to be recorded, got %d", got)
	}
}

func TestThroughputClientDisablesClientTimeout(t *testing.T) {
	transport := &http.Transport{ForceAttemptHTTP2: true}
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
	gotTransport, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected throughput transport to be *http.Transport, got %T", got.Transport)
	}
	if gotTransport == transport {
		t.Fatal("expected throughput transport to be cloned")
	}
	if gotTransport.ForceAttemptHTTP2 {
		t.Fatal("expected throughput transport to disable HTTP/2")
	}
	if gotTransport.TLSNextProto == nil {
		t.Fatal("expected throughput transport to disable automatic HTTP/2 with an empty TLSNextProto map")
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
	runTest("Upload:", func(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
		observedTimeout <- client.Timeout
		recorder.Add(128 * 1024)
	}, base, false, 1, true, false, 0)

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

func TestRunTestSharesOneBandwidthLimiterAcrossWorkers(t *testing.T) {
	base := &http.Client{Transport: &http.Transport{}}
	observed := make(chan *bandwidthLimiter, 3)

	runTest("Download:", func(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
		observed <- limiter
		recorder.Add(128 * 1024)
	}, base, true, 3, true, false, 40)

	var shared *bandwidthLimiter
	for range 3 {
		limiter := <-observed
		if limiter == nil {
			t.Fatal("expected a configured limiter")
		}
		if shared == nil {
			shared = limiter
		} else if limiter != shared {
			t.Fatal("expected all workers to share one aggregate limiter")
		}
	}
	if got, want := shared.bytesPerSecond, 5_000_000.0; got != want {
		t.Fatalf("limiter rate mismatch: got %.0f bytes/s want %.0f", got, want)
	}
}

func TestRunBidirectionalTestStartsBothDirectionsTogether(t *testing.T) {
	base := &http.Client{
		Transport: &http.Transport{},
		Timeout:   30 * time.Second,
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	worker := func(name string) throughputWorker {
		return func(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
			if client.Timeout != 0 {
				t.Errorf("%s worker received client timeout %v, want 0", name, client.Timeout)
			}
			started <- name
			select {
			case <-ctx.Done():
				return
			case <-release:
				recorder.Add(128 * 1024)
			}
		}
	}

	type result struct {
		download data.Speed
		upload   data.Speed
	}
	resultCh := make(chan result, 1)
	go func() {
		download, upload := runBidirectionalTest(
			worker("download"),
			worker("upload"),
			base,
			false,
			1,
			true,
			false,
			0,
			0,
		)
		resultCh <- result{download: download, upload: upload}
	}()

	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("upload and download workers did not start concurrently")
		}
	}
	close(release)

	select {
	case got := <-resultCh:
		if got.download.DataMB <= 0 {
			t.Fatalf("expected positive download data, got %.4f", got.download.DataMB)
		}
		if got.upload.DataMB <= 0 {
			t.Fatalf("expected positive upload data, got %.4f", got.upload.DataMB)
		}
	case <-time.After(time.Second):
		t.Fatal("bidirectional test did not finish after workers completed")
	}

	if base.Timeout != 30*time.Second {
		t.Fatalf("expected base client timeout to remain unchanged, got %v", base.Timeout)
	}
}

func TestRunBidirectionalTestSupportsContinuousMode(t *testing.T) {
	base := &http.Client{
		Transport: &http.Transport{},
		Timeout:   30 * time.Second,
	}

	observed := make(chan string, 2)
	worker := func(name string) throughputWorker {
		return func(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
			if !continuous {
				t.Errorf("%s worker did not receive continuous mode", name)
			}
			if recorder.measure != 0 {
				t.Errorf("%s recorder measure window = %v, want unlimited", name, recorder.measure)
			}
			wantBytesPerSecond := map[string]float64{
				"download": 10_000_000,
				"upload":   2_500_000,
			}[name]
			if limiter == nil || limiter.bytesPerSecond != wantBytesPerSecond {
				t.Errorf("%s limiter = %#v, want %.0f bytes/s", name, limiter, wantBytesPerSecond)
			}
			recorder.Add(128 * 1024)
			observed <- name
		}
	}

	download, upload := runBidirectionalTest(
		worker("download"),
		worker("upload"),
		base,
		false,
		1,
		true,
		true,
		80,
		20,
	)

	if download.DataMB <= 0 || upload.DataMB <= 0 {
		t.Fatalf("expected data from both directions, got download %.4f MB upload %.4f MB", download.DataMB, upload.DataMB)
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-observed:
			seen[name] = true
		default:
			t.Fatal("expected both continuous workers to run")
		}
	}
	if !seen["download"] || !seen["upload"] {
		t.Fatalf("expected both directions, got %v", seen)
	}
}

func TestMeasureLatencyUsesHTTPProbeWhenProxyConfigured(t *testing.T) {
	var requests int32
	errCh := make(chan error, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if got, want := r.URL.String(), "http://speed.cloudflare.test/cdn-cgi/trace"; got != want {
			select {
			case errCh <- fmt.Errorf("unexpected proxied request URL: got %q want %q", got, want):
			default:
			}
			http.Error(w, "unexpected URL", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(proxy.Close)

	originalProbeURL := latencyProbeURL
	latencyProbeURL = "http://speed.cloudflare.test/cdn-cgi/trace"
	t.Cleanup(func() {
		latencyProbeURL = originalProbeURL
	})

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	result, err := measureLatency(httpClient, 2)
	if err != nil {
		select {
		case handlerErr := <-errCh:
			t.Fatal(handlerErr)
		default:
		}
		t.Fatalf("measureLatency returned error: %v", err)
	}
	if result.Avg <= 0 {
		t.Fatalf("expected positive average latency, got %.2f", result.Avg)
	}
	if got, want := atomic.LoadInt32(&requests), int32(2); got != want {
		t.Fatalf("proxy request count mismatch: got %d want %d", got, want)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
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

	downloadWorker(ctx, recorder, client, true, nil)

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
			if req.ContentLength != uploadRequestBytes {
				t.Fatalf("content length mismatch: got %d want %d", req.ContentLength, uploadRequestBytes)
			}

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

	uploadWorker(ctx, recorder, client, true, nil)

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

	uploadWorker(ctx, recorder, client, true, nil)

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

	uploadWorker(ctx, recorder, client, true, nil)

	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("request count mismatch: got %d want %d", got, want)
	}
}
