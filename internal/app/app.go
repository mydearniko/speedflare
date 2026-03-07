package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"

	"github.com/idanyas/speedflare/internal/client"
	"github.com/idanyas/speedflare/internal/data"
	"github.com/idanyas/speedflare/internal/location"
	"github.com/idanyas/speedflare/internal/output"
)

const (
	speedTestWarmup             = 1500 * time.Millisecond
	speedTestMeasure            = 8 * time.Second
	speedTestHardTimeout        = 20 * time.Second
	speedTestLiveWindow         = 1200 * time.Millisecond
	speedTestLiveDenominatorMin = 500 * time.Millisecond
	speedTestLiveBlendHorizon   = 2 * time.Second
	speedTestLiveSmoothingAlpha = 0.35
)

type throughputRecorder struct {
	start         time.Time
	warmup        time.Duration
	measure       time.Duration
	totalBytes    int64
	measuredBytes int64
	firstByteNs   int64
	firstByteCh   chan struct{}
}

type throughputSummary struct {
	mbps   float64
	dataMB float64
}

type liveSpeedSample struct {
	at         time.Time
	totalBytes int64
}

type liveSpeedEstimator struct {
	recorder *throughputRecorder
	samples  []liveSpeedSample
	lastMbps float64
	hasLast  bool
}

func newThroughputRecorder(start time.Time, warmup, measure time.Duration) *throughputRecorder {
	return &throughputRecorder{
		start:       start,
		warmup:      warmup,
		measure:     measure,
		firstByteNs: -1,
		firstByteCh: make(chan struct{}),
	}
}

func (r *throughputRecorder) Add(n int) {
	if n <= 0 {
		return
	}

	atomic.AddInt64(&r.totalBytes, int64(n))

	elapsedNs := time.Since(r.start).Nanoseconds()
	firstByteNs := atomic.LoadInt64(&r.firstByteNs)
	if firstByteNs < 0 {
		if atomic.CompareAndSwapInt64(&r.firstByteNs, -1, elapsedNs) {
			close(r.firstByteCh)
			firstByteNs = elapsedNs
		} else {
			firstByteNs = atomic.LoadInt64(&r.firstByteNs)
		}
	}

	measurementStartNs := firstByteNs + r.warmup.Nanoseconds()
	if elapsedNs < measurementStartNs {
		return
	}

	measurementEndNs := measurementStartNs + r.measure.Nanoseconds()
	if elapsedNs > measurementEndNs {
		return
	}

	atomic.AddInt64(&r.measuredBytes, int64(n))
}

func (r *throughputRecorder) summarize(now time.Time) throughputSummary {
	totalBytes := atomic.LoadInt64(&r.totalBytes)
	dataMB := float64(totalBytes) / 1e6

	mbps := 0.0
	measuredBytes := atomic.LoadInt64(&r.measuredBytes)
	measuredDuration := r.measuredDuration(now)
	if measuredDuration >= 500*time.Millisecond && measuredBytes > 0 {
		mbps = (float64(measuredBytes) * 8) / (measuredDuration.Seconds() * 1e6)
	} else {
		activeDuration := r.activeDuration(now)
		if activeDuration > 0 && totalBytes > 0 {
			mbps = (float64(totalBytes) * 8) / (activeDuration.Seconds() * 1e6)
		}
	}

	return throughputSummary{
		mbps:   mbps,
		dataMB: dataMB,
	}
}

func (r *throughputRecorder) measuredDuration(now time.Time) time.Duration {
	firstByteNs := atomic.LoadInt64(&r.firstByteNs)
	if firstByteNs < 0 {
		return 0
	}

	elapsedNs := now.Sub(r.start).Nanoseconds()
	measurementStartNs := firstByteNs + r.warmup.Nanoseconds()
	if elapsedNs <= measurementStartNs {
		return 0
	}

	measurementEndNs := measurementStartNs + r.measure.Nanoseconds()
	if elapsedNs > measurementEndNs {
		elapsedNs = measurementEndNs
	}

	return time.Duration(elapsedNs - measurementStartNs)
}

func (r *throughputRecorder) activeDuration(now time.Time) time.Duration {
	firstByteNs := atomic.LoadInt64(&r.firstByteNs)
	if firstByteNs < 0 {
		return 0
	}

	elapsedNs := now.Sub(r.start).Nanoseconds()
	if elapsedNs <= firstByteNs {
		return 0
	}

	return time.Duration(elapsedNs - firstByteNs)
}

func newLiveSpeedEstimator(recorder *throughputRecorder) *liveSpeedEstimator {
	return &liveSpeedEstimator{
		recorder: recorder,
	}
}

func (e *liveSpeedEstimator) current(now time.Time) float64 {
	totalBytes := atomic.LoadInt64(&e.recorder.totalBytes)
	if totalBytes <= 0 {
		return 0
	}

	activeDuration := e.recorder.activeDuration(now)
	if activeDuration <= 0 {
		return 0
	}

	e.samples = append(e.samples, liveSpeedSample{
		at:         now,
		totalBytes: totalBytes,
	})
	for len(e.samples) > 1 && now.Sub(e.samples[0].at) > speedTestLiveWindow {
		e.samples = e.samples[1:]
	}

	denominator := activeDuration
	if denominator < speedTestLiveDenominatorMin {
		denominator = speedTestLiveDenominatorMin
	}
	estimate := (float64(totalBytes) * 8) / (denominator.Seconds() * 1e6)

	if rollingMbps, ok := e.rollingMbps(now, totalBytes); ok {
		estimate = rollingMbps
	}

	measuredDuration := e.recorder.measuredDuration(now)
	measuredBytes := atomic.LoadInt64(&e.recorder.measuredBytes)
	if measuredDuration > 0 && measuredBytes > 0 {
		measuredMbps := (float64(measuredBytes) * 8) / (measuredDuration.Seconds() * 1e6)
		confidence := measuredDuration.Seconds() / speedTestLiveBlendHorizon.Seconds()
		if confidence > 1 {
			confidence = 1
		}
		estimate = ((1 - confidence) * estimate) + (confidence * measuredMbps)
	}

	if e.hasLast {
		estimate = (speedTestLiveSmoothingAlpha * estimate) + ((1 - speedTestLiveSmoothingAlpha) * e.lastMbps)
	}
	e.lastMbps = estimate
	e.hasLast = true

	if estimate < 0 {
		return 0
	}
	return estimate
}

func (e *liveSpeedEstimator) rollingMbps(now time.Time, totalBytes int64) (float64, bool) {
	if len(e.samples) < 2 {
		return 0, false
	}

	oldest := e.samples[0]
	window := now.Sub(oldest.at)
	if window <= 0 {
		return 0, false
	}

	deltaBytes := totalBytes - oldest.totalBytes
	if deltaBytes <= 0 {
		return 0, false
	}

	mbps := (float64(deltaBytes) * 8) / (window.Seconds() * 1e6)
	return mbps, true
}

func RunSpeedTest(client *http.Client, latencyAttempts int, workers int, singleConnection bool, jsonOutput bool, suppressIntro bool, hideIP bool, originIP string) (*data.TestResult, error) {
	trace, err := location.GetServerTrace(client)
	if err != nil {
		return nil, fmt.Errorf("failed to get server info: %w", err)
	}

	locs, err := location.FetchLocations(client)
	if err != nil {
		return nil, fmt.Errorf("failed to get locations: %w", err)
	}

	server, err := location.FindServerInfo(trace["colo"], locs)
	if err != nil {
		return nil, err
	}
	server.IP = originIP

	if !suppressIntro {
		output.PrintConnectionInfo(trace, server, jsonOutput, hideIP)
	}

	latency, err := measureLatency(client, latencyAttempts)
	if err != nil {
		return nil, fmt.Errorf("latency test failed: %w", err)
	}
	output.PrintLatencyInfo(latency, jsonOutput)

	download := runTest("Download:", downloadWorker, client, !singleConnection, workers, jsonOutput)
	upload := runTest("Upload:", uploadWorker, client, !singleConnection, workers, jsonOutput)

	resultIP := trace["ip"]
	if hideIP {
		resultIP = "---"
	}

	return &data.TestResult{
		IP:     resultIP,
		Server: server,
		Latency: data.Stats{
			Value:  latency.Avg,
			Jitter: latency.Jitter,
			Min:    latency.Min,
			Max:    latency.Max,
		},
		Download: data.Speed{
			Mbps:   download.mbps,
			DataMB: download.dataMB,
		},
		Upload: data.Speed{
			Mbps:   upload.mbps,
			DataMB: upload.dataMB,
		},
	}, nil
}

func measureLatency(c *http.Client, latencyAttempts int) (*data.LatencyResult, error) {
	attempts := latencyAttempts
	var latencies []float64

	var transport *http.Transport
	if t, ok := c.Transport.(*http.Transport); ok {
		transport = t
	} else if t, ok := c.Transport.(*client.BrowserTransport); ok {
		transport = t.Transport
	} else {
		return nil, fmt.Errorf("invalid transport type, expected *http.Transport")
	}

	for i := 0; i < attempts; i++ {
		// Increased timeout significantly to 15s.
		// This handles slow DNS resolution and multiple IP failovers in the custom dialer.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		conn, err := transport.DialContext(ctx, "tcp", "speed.cloudflare.com:443")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to dial server: %w", err)
		}
		conn.Close()
		cancel()
		latency := time.Since(start).Seconds() * 1000 // Convert to milliseconds
		latencies = append(latencies, latency)

		// Introduce random jitter between attempts (0-50ms)
		if i < attempts-1 {
			time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
		}
	}

	sum := 0.0
	min := math.MaxFloat64
	max := 0.0
	for _, l := range latencies {
		sum += l
		if l < min {
			min = l
		}
		if l > max {
			max = l
		}
	}
	avg := sum / float64(len(latencies))

	jitterSum := 0.0
	for _, l := range latencies {
		jitterSum += math.Abs(l - avg)
	}
	jitter := jitterSum / float64(len(latencies))

	return &data.LatencyResult{
		Avg:    avg,
		Jitter: jitter,
		Min:    min,
		Max:    max,
	}, nil
}

type testResult struct {
	mbps   float64
	dataMB float64
}

func runTest(name string, worker func(context.Context, *throughputRecorder, *http.Client), client *http.Client, multiple bool, workers int, jsonOutput bool) testResult {
	start := time.Now()
	recorder := newThroughputRecorder(start, speedTestWarmup, speedTestMeasure)
	liveEstimator := newLiveSpeedEstimator(recorder)

	ctx, cancel := context.WithTimeout(context.Background(), speedTestHardTimeout)
	defer cancel()

	done := make(chan struct{})

	var wg sync.WaitGroup
	numWorkers := 1
	if multiple {
		numWorkers = workers
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, recorder, client)
		}()
	}

	go func() {
		select {
		case <-ctx.Done():
			return
		case <-recorder.firstByteCh:
		}

		timer := time.NewTimer(speedTestWarmup + speedTestMeasure)
		defer timer.Stop()

		select {
		case <-ctx.Done():
		case <-timer.C:
			cancel()
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	go output.ProgressReporter(name, done, jsonOutput, func() string {
		return fmt.Sprintf("%.2f Mbps", liveEstimator.current(time.Now()))
	})

	<-done
	summary := recorder.summarize(time.Now())

	if !jsonOutput {
		green := color.New(color.FgGreen).SprintFunc()
		fmt.Printf("\r%s %s %.2f Mbps (Used: %.2f MB)    \n",
			green("✓"),
			name,
			summary.mbps,
			summary.dataMB,
		)
	}

	return testResult{summary.mbps, summary.dataMB}
}

func downloadWorker(ctx context.Context, recorder *throughputRecorder, client *http.Client) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://speed.cloudflare.com/__down?bytes=2147483648", nil)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 256*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			recorder.Add(n)
		}
		if err != nil {
			return
		}
	}
}

func uploadWorker(ctx context.Context, recorder *throughputRecorder, client *http.Client) {
	req, _ := http.NewRequest("POST", "https://speed.cloudflare.com/__up", newUploadStream(ctx, recorder))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/octet-stream")
	_, err := client.Do(req)
	if err != nil {
		return
	}
}

type uploadStream struct {
	ctx      context.Context
	recorder *throughputRecorder
	payload  []byte
	offset   int
}

func newUploadStream(ctx context.Context, recorder *throughputRecorder) io.Reader {
	payload := make([]byte, 1<<20)
	rand.Read(payload)

	return &uploadStream{
		ctx:      ctx,
		recorder: recorder,
		payload:  payload,
	}
}

func (s *uploadStream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	written := 0
	for written < len(p) {
		copied := copy(p[written:], s.payload[s.offset:])
		written += copied
		s.offset += copied
		if s.offset == len(s.payload) {
			s.offset = 0
		}
	}

	s.recorder.Add(written)
	return written, nil
}
