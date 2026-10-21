package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fatih/color"

	"github.com/mydearniko/speedflare/internal/client"
	"github.com/mydearniko/speedflare/internal/data"
	"github.com/mydearniko/speedflare/internal/location"
	"github.com/mydearniko/speedflare/internal/output"
)

const (
	speedTestWarmup             = 1500 * time.Millisecond
	speedTestMeasure            = 8 * time.Second
	speedTestHardTimeout        = 20 * time.Second
	speedTestLiveWindow         = 1200 * time.Millisecond
	speedTestLiveRollingMin     = 500 * time.Millisecond
	speedTestLiveDenominatorMin = 500 * time.Millisecond
	speedTestLiveBlendHorizon   = 2 * time.Second
	speedTestLiveSmoothingAlpha = 0.35
	uploadLiveWindow            = 4 * time.Second
	uploadLiveRollingMin        = 2 * time.Second
	uploadLiveDenominatorMin    = 2 * time.Second
	uploadLiveBlendHorizon      = 5 * time.Second
	uploadLiveSmoothingAlpha    = 0.08
	speedTestRetryDelay         = 200 * time.Millisecond
	uploadRequestBytes          = 32 * 1024 * 1024
)

var latencyProbeURL = "https://speed.cloudflare.com/cdn-cgi/trace"

type TestMode uint8

const (
	TestModeDefault TestMode = iota
	TestModeUploadOnly
	TestModeDownloadOnly
	TestModeLatencyOnly
	TestModeBidirectional
)

type RunOptions struct {
	LatencyAttempts   int
	Workers           int
	SingleConnection  bool
	JSONOutput        bool
	SuppressIntro     bool
	HideIP            bool
	OriginIP          string
	Locations         []data.Location
	Mode              TestMode
	Continuous        bool
	DownloadLimitMbps float64
	UploadLimitMbps   float64
}

type throughputRecorder struct {
	// Typed atomics keep these counters 64-bit aligned on 32-bit ARM/Android,
	// which avoids "unaligned 64-bit atomic operation" panics in Termux.
	totalBytes    atomic.Int64
	measuredBytes atomic.Int64
	firstByteNs   atomic.Int64

	start       time.Time
	warmup      time.Duration
	measure     time.Duration
	firstByteCh chan struct{}
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
	config   liveSpeedEstimatorConfig
	samples  []liveSpeedSample
	lastMbps float64
	hasLast  bool
}

type liveSpeedEstimatorConfig struct {
	window         time.Duration
	rollingMin     time.Duration
	denominatorMin time.Duration
	blendHorizon   time.Duration
	smoothingAlpha float64
}

var defaultLiveSpeedEstimatorConfig = liveSpeedEstimatorConfig{
	window:         speedTestLiveWindow,
	rollingMin:     speedTestLiveRollingMin,
	denominatorMin: speedTestLiveDenominatorMin,
	blendHorizon:   speedTestLiveBlendHorizon,
	smoothingAlpha: speedTestLiveSmoothingAlpha,
}

var uploadLiveSpeedEstimatorConfig = liveSpeedEstimatorConfig{
	window:         uploadLiveWindow,
	rollingMin:     uploadLiveRollingMin,
	denominatorMin: uploadLiveDenominatorMin,
	blendHorizon:   uploadLiveBlendHorizon,
	smoothingAlpha: uploadLiveSmoothingAlpha,
}

// Throughput tests manage cancellation explicitly, so they must not inherit the
// generic client-wide deadline used by setup requests.
func throughputClient(base *http.Client) *http.Client {
	if base == nil {
		return base
	}

	clone := *base
	clone.Timeout = 0
	clone.Transport = throughputRoundTripper(base.Transport)
	return &clone
}

func throughputRoundTripper(rt http.RoundTripper) http.RoundTripper {
	switch t := rt.(type) {
	case *http.Transport:
		return throughputTransport(t)
	case *client.BrowserTransport:
		clone := *t
		clone.Transport = throughputTransport(t.Transport)
		return &clone
	default:
		return rt
	}
}

func throughputTransport(base *http.Transport) *http.Transport {
	if base == nil {
		return nil
	}

	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return clone
}

func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(speedTestRetryDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newThroughputRecorder(start time.Time, warmup, measure time.Duration) *throughputRecorder {
	recorder := &throughputRecorder{
		start:       start,
		warmup:      warmup,
		measure:     measure,
		firstByteCh: make(chan struct{}),
	}
	recorder.firstByteNs.Store(-1)
	return recorder
}

func (r *throughputRecorder) Add(n int) {
	if n <= 0 {
		return
	}

	r.totalBytes.Add(int64(n))

	elapsedNs := time.Since(r.start).Nanoseconds()
	firstByteNs := r.firstByteNs.Load()
	if firstByteNs < 0 {
		if r.firstByteNs.CompareAndSwap(-1, elapsedNs) {
			close(r.firstByteCh)
			firstByteNs = elapsedNs
		} else {
			firstByteNs = r.firstByteNs.Load()
		}
	}

	measurementStartNs := firstByteNs + r.warmup.Nanoseconds()
	if elapsedNs < measurementStartNs {
		return
	}

	if r.measure > 0 {
		measurementEndNs := measurementStartNs + r.measure.Nanoseconds()
		if elapsedNs > measurementEndNs {
			return
		}
	}

	r.measuredBytes.Add(int64(n))
}

func (r *throughputRecorder) summarize(now time.Time) throughputSummary {
	totalBytes := r.totalBytes.Load()
	dataMB := float64(totalBytes) / 1e6

	mbps := 0.0
	measuredBytes := r.measuredBytes.Load()
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
	firstByteNs := r.firstByteNs.Load()
	if firstByteNs < 0 {
		return 0
	}

	elapsedNs := now.Sub(r.start).Nanoseconds()
	measurementStartNs := firstByteNs + r.warmup.Nanoseconds()
	if elapsedNs <= measurementStartNs {
		return 0
	}

	if r.measure > 0 {
		measurementEndNs := measurementStartNs + r.measure.Nanoseconds()
		if elapsedNs > measurementEndNs {
			elapsedNs = measurementEndNs
		}
	}

	return time.Duration(elapsedNs - measurementStartNs)
}

func (r *throughputRecorder) activeDuration(now time.Time) time.Duration {
	firstByteNs := r.firstByteNs.Load()
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
	return newLiveSpeedEstimatorWithConfig(recorder, defaultLiveSpeedEstimatorConfig)
}

func newLiveSpeedEstimatorWithConfig(recorder *throughputRecorder, config liveSpeedEstimatorConfig) *liveSpeedEstimator {
	return &liveSpeedEstimator{
		recorder: recorder,
		config:   config,
	}
}

func (e *liveSpeedEstimator) current(now time.Time) float64 {
	totalBytes := e.recorder.totalBytes.Load()
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
	for len(e.samples) > 1 && now.Sub(e.samples[0].at) > e.config.window {
		e.samples = e.samples[1:]
	}

	denominator := activeDuration
	if denominator < e.config.denominatorMin {
		denominator = e.config.denominatorMin
	}
	estimate := (float64(totalBytes) * 8) / (denominator.Seconds() * 1e6)

	if rollingMbps, ok := e.rollingMbps(now, totalBytes); ok {
		estimate = rollingMbps
	}

	measuredDuration := e.recorder.measuredDuration(now)
	measuredBytes := e.recorder.measuredBytes.Load()
	if measuredDuration > 0 && measuredBytes > 0 {
		measuredMbps := (float64(measuredBytes) * 8) / (measuredDuration.Seconds() * 1e6)
		confidence := measuredDuration.Seconds() / e.config.blendHorizon.Seconds()
		if confidence > 1 {
			confidence = 1
		}
		estimate = ((1 - confidence) * estimate) + (confidence * measuredMbps)
	}

	if e.hasLast {
		estimate = (e.config.smoothingAlpha * estimate) + ((1 - e.config.smoothingAlpha) * e.lastMbps)
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
	if window < e.config.rollingMin {
		return 0, false
	}

	deltaBytes := totalBytes - oldest.totalBytes
	if deltaBytes <= 0 {
		return 0, false
	}

	mbps := (float64(deltaBytes) * 8) / (window.Seconds() * 1e6)
	return mbps, true
}

func (m TestMode) runsLatency() bool {
	return m == TestModeDefault || m == TestModeLatencyOnly || m == TestModeBidirectional
}

func (m TestMode) runsDownload() bool {
	return m == TestModeDefault || m == TestModeDownloadOnly || m == TestModeBidirectional
}

func (m TestMode) runsUpload() bool {
	return m == TestModeDefault || m == TestModeUploadOnly || m == TestModeBidirectional
}

func RunSpeedTest(client *http.Client, opts RunOptions) (*data.TestResult, error) {
	trace, err := location.GetServerTrace(client)
	if err != nil {
		return nil, fmt.Errorf("failed to get server info: %w", err)
	}

	locs := opts.Locations
	if len(locs) == 0 {
		var err error
		locs, err = location.FetchLocations(client)
		if err != nil {
			return nil, fmt.Errorf("failed to get locations: %w", err)
		}
	}
	server, err := location.FindServerInfo(trace["colo"], locs)
	if err != nil {
		return nil, err
	}
	server.IP = opts.OriginIP

	if !opts.SuppressIntro {
		output.PrintConnectionInfo(trace, server, opts.JSONOutput, opts.HideIP)
	}

	resultIP := trace["ip"]
	if opts.HideIP {
		resultIP = "---"
	}

	result := &data.TestResult{
		IP:     resultIP,
		Server: server,
	}

	if opts.Mode.runsLatency() {
		latency, err := measureLatency(client, opts.LatencyAttempts)
		if err != nil {
			return nil, fmt.Errorf("latency test failed: %w", err)
		}
		output.PrintLatencyInfo(latency, opts.JSONOutput)
		result.Latency = data.Stats{
			Value:  latency.Avg,
			Jitter: latency.Jitter,
			Min:    latency.Min,
			Max:    latency.Max,
		}
	}

	if opts.Mode == TestModeLatencyOnly {
		return result, nil
	}
	if opts.Mode == TestModeBidirectional {
		result.Download, result.Upload = runBidirectionalTest(
			downloadWorker,
			uploadWorker,
			client,
			!opts.SingleConnection,
			opts.Workers,
			opts.JSONOutput,
			opts.Continuous,
			opts.DownloadLimitMbps,
			opts.UploadLimitMbps,
		)
		return result, nil
	}

	if opts.Mode.runsDownload() {
		result.Download = runTest("Download:", downloadWorker, client, !opts.SingleConnection, opts.Workers, opts.JSONOutput, opts.Continuous, opts.DownloadLimitMbps)
	}
	if opts.Mode.runsUpload() {
		result.Upload = runTest("Upload:", uploadWorker, client, !opts.SingleConnection, opts.Workers, opts.JSONOutput, opts.Continuous, opts.UploadLimitMbps)
	}

	return result, nil
}

func measureLatency(c *http.Client, latencyAttempts int) (*data.LatencyResult, error) {
	var transport *http.Transport
	if t, ok := c.Transport.(*http.Transport); ok {
		transport = t
	} else if t, ok := c.Transport.(*client.BrowserTransport); ok {
		transport = t.Transport
	} else {
		return nil, fmt.Errorf("invalid transport type, expected *http.Transport")
	}

	usesProxy, err := transportUsesProxy(transport, latencyProbeURL)
	if err != nil {
		return nil, fmt.Errorf("proxy resolution failed: %w", err)
	}
	if usesProxy {
		return measureHTTPLatency(c, transport, latencyAttempts)
	}

	return measureDialLatency(transport, latencyAttempts)
}

func transportUsesProxy(transport *http.Transport, targetURL string) (bool, error) {
	if transport == nil || transport.Proxy == nil {
		return false, nil
	}

	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return false, err
	}

	proxyURL, err := transport.Proxy(req)
	if err != nil {
		return false, err
	}
	return proxyURL != nil, nil
}

func measureDialLatency(transport *http.Transport, attempts int) (*data.LatencyResult, error) {
	var latencies []float64
	dialContext := transport.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{}
		dialContext = dialer.DialContext
	}

	for i := 0; i < attempts; i++ {
		// Increased timeout significantly to 15s.
		// This handles slow DNS resolution and multiple IP failovers in the custom dialer.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		conn, err := dialContext(ctx, "tcp", "speed.cloudflare.com:443")
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

	return summarizeLatencies(latencies), nil
}

func measureHTTPLatency(c *http.Client, transport *http.Transport, attempts int) (*data.LatencyResult, error) {
	var latencies []float64

	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, latencyProbeURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		req.Close = true

		transport.CloseIdleConnections()
		start := time.Now()
		resp, err := c.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to probe server through proxy: %w", err)
		}
		latency := time.Since(start).Seconds() * 1000

		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		closeErr := resp.Body.Close()
		cancel()
		transport.CloseIdleConnections()

		if readErr != nil {
			return nil, fmt.Errorf("reading latency probe response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing latency probe response: %w", closeErr)
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return nil, fmt.Errorf("latency probe returned status %d", resp.StatusCode)
		}

		latencies = append(latencies, latency)

		if i < attempts-1 {
			time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
		}
	}

	return summarizeLatencies(latencies), nil
}

func summarizeLatencies(latencies []float64) *data.LatencyResult {
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
	}
}

type throughputWorker func(context.Context, *throughputRecorder, *http.Client, bool, *bandwidthLimiter)

func runTest(name string, worker throughputWorker, client *http.Client, multiple bool, workers int, jsonOutput bool, continuous bool, limitMbps float64) data.Speed {
	client = throughputClient(client)
	limiter := newBandwidthLimiter(limitMbps)

	start := time.Now()
	measureWindow := speedTestMeasure
	if continuous {
		measureWindow = 0
	}
	recorder := newThroughputRecorder(start, speedTestWarmup, measureWindow)
	liveEstimator := newLiveSpeedEstimatorWithConfig(recorder, liveSpeedConfigForTest(name))

	parentCtx := context.Background()
	var stopSignals context.CancelFunc
	if continuous {
		parentCtx, stopSignals = signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
		defer stopSignals()
		if !jsonOutput {
			cyan := color.New(color.FgCyan).SprintFunc()
			fmt.Printf("%s %s running continuously (Ctrl + C to stop)\n", cyan("✓"), name)
		}
	}

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if continuous {
		ctx, cancel = context.WithCancel(parentCtx)
	} else {
		ctx, cancel = context.WithTimeout(parentCtx, speedTestHardTimeout)
	}
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
			worker(ctx, recorder, client, continuous, limiter)
		}()
	}

	if !continuous {
		go cancelAfterMeasurement(ctx, recorder, cancel)
	}

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

	return data.Speed{
		Mbps:   summary.mbps,
		DataMB: summary.dataMB,
	}
}

func cancelAfterMeasurement(ctx context.Context, recorder *throughputRecorder, cancel context.CancelFunc) {
	select {
	case <-ctx.Done():
		return
	case <-recorder.firstByteCh:
	}

	timer := time.NewTimer(recorder.warmup + recorder.measure)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
		cancel()
	}
}

func runBidirectionalTest(download, upload throughputWorker, client *http.Client, multiple bool, workers int, jsonOutput, continuous bool, downloadLimitMbps, uploadLimitMbps float64) (data.Speed, data.Speed) {
	client = throughputClient(client)
	downloadLimiter := newBandwidthLimiter(downloadLimitMbps)
	uploadLimiter := newBandwidthLimiter(uploadLimitMbps)

	start := time.Now()
	measureWindow := speedTestMeasure
	if continuous {
		measureWindow = 0
	}
	downloadRecorder := newThroughputRecorder(start, speedTestWarmup, measureWindow)
	uploadRecorder := newThroughputRecorder(start, speedTestWarmup, measureWindow)
	downloadEstimator := newLiveSpeedEstimatorWithConfig(downloadRecorder, defaultLiveSpeedEstimatorConfig)
	uploadEstimator := newLiveSpeedEstimatorWithConfig(uploadRecorder, uploadLiveSpeedEstimatorConfig)

	parentCtx := context.Background()
	var parentCancel context.CancelFunc
	if continuous {
		parentCtx, parentCancel = signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
		if !jsonOutput {
			cyan := color.New(color.FgCyan).SprintFunc()
			fmt.Printf("%s Bidirectional: running continuously (Ctrl + C to stop)\n", cyan("✓"))
		}
	} else {
		parentCtx, parentCancel = context.WithTimeout(parentCtx, speedTestHardTimeout)
	}
	defer parentCancel()

	downloadCtx, downloadCancel := context.WithCancel(parentCtx)
	uploadCtx, uploadCancel := context.WithCancel(parentCtx)
	defer downloadCancel()
	defer uploadCancel()

	if !continuous {
		go cancelAfterMeasurement(downloadCtx, downloadRecorder, downloadCancel)
		go cancelAfterMeasurement(uploadCtx, uploadRecorder, uploadCancel)
	}

	numWorkers := 1
	if multiple {
		numWorkers = workers
	}

	startWorkers := make(chan struct{})
	downloadDone := runThroughputWorkers(startWorkers, downloadCtx, downloadCancel, downloadRecorder, client, download, numWorkers, continuous, downloadLimiter)
	uploadDone := runThroughputWorkers(startWorkers, uploadCtx, uploadCancel, uploadRecorder, client, upload, numWorkers, continuous, uploadLimiter)
	close(startWorkers)

	done := make(chan struct{})
	go func() {
		<-downloadDone
		<-uploadDone
		close(done)
	}()

	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		output.BidirectionalProgressReporter(done, jsonOutput, func() (string, string) {
			now := time.Now()
			return fmt.Sprintf("%8.2f Mbps", downloadEstimator.current(now)),
				fmt.Sprintf("%8.2f Mbps", uploadEstimator.current(now))
		})
	}()

	<-done
	<-progressDone
	now := time.Now()
	downloadSummary := downloadRecorder.summarize(now)
	uploadSummary := uploadRecorder.summarize(now)

	if !jsonOutput {
		green := color.New(color.FgGreen).SprintFunc()
		fmt.Printf("\r\033[K%s Download: %8.2f Mbps (Used: %.2f MB)\n",
			green("✓"),
			downloadSummary.mbps,
			downloadSummary.dataMB,
		)
		fmt.Printf("%s Upload:   %8.2f Mbps (Used: %.2f MB)\n",
			green("✓"),
			uploadSummary.mbps,
			uploadSummary.dataMB,
		)
	}

	return data.Speed{
		Mbps:   downloadSummary.mbps,
		DataMB: downloadSummary.dataMB,
	}, data.Speed{
		Mbps:   uploadSummary.mbps,
		DataMB: uploadSummary.dataMB,
	}
}

func runThroughputWorkers(start <-chan struct{}, ctx context.Context, cancel context.CancelFunc, recorder *throughputRecorder, client *http.Client, worker throughputWorker, workers int, continuous bool, limiter *bandwidthLimiter) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-start:
			}
			worker(ctx, recorder, client, continuous, limiter)
		}()
	}

	go func() {
		wg.Wait()
		cancel()
		close(done)
	}()

	return done
}

func liveSpeedConfigForTest(name string) liveSpeedEstimatorConfig {
	if name == "Upload:" {
		return uploadLiveSpeedEstimatorConfig
	}
	return defaultLiveSpeedEstimatorConfig
}

func downloadWorker(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
	// crypto/tls returns at most one record (16 KiB) per Read, so a larger
	// buffer only costs memory.
	buf := make([]byte, 32*1024)
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://speed.cloudflare.com/__down?bytes=2147483648", nil)
		resp, err := client.Do(req)
		if err != nil {
			if continuous && waitForRetry(ctx) {
				continue
			}
			return
		}

		if resp.StatusCode >= http.StatusBadRequest {
			resp.Body.Close()
			if continuous && waitForRetry(ctx) {
				continue
			}
			return
		}

		shouldRestart := false
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if waitErr := limiter.wait(ctx, n); waitErr != nil {
					resp.Body.Close()
					return
				}
				recorder.Add(n)
			}
			if err == nil {
				continue
			}

			resp.Body.Close()
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, io.EOF) {
				shouldRestart = true
			} else if continuous {
				shouldRestart = waitForRetry(ctx)
			}
			break
		}

		if !shouldRestart {
			return
		}
	}
}

func uploadWorker(ctx context.Context, recorder *throughputRecorder, client *http.Client, continuous bool, limiter *bandwidthLimiter) {
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://speed.cloudflare.com/__up", newUploadStream(ctx, recorder, uploadRequestBytes, limiter))
		req.ContentLength = uploadRequestBytes
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := client.Do(req)
		if err != nil {
			if continuous && waitForRetry(ctx) {
				continue
			}
			return
		}

		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if continuous && waitForRetry(ctx) {
				continue
			}
			return
		}

		if resp.StatusCode >= http.StatusBadRequest {
			if continuous && waitForRetry(ctx) {
				continue
			}
			return
		}
	}
}

type uploadStream struct {
	ctx       context.Context
	recorder  *throughputRecorder
	payload   []byte
	offset    int
	remaining int64
	limiter   *bandwidthLimiter
}

// uploadPayload is generated once and shared read-only by every upload stream;
// regenerating it per request burned CPU and churned the heap.
var uploadPayload = sync.OnceValue(func() []byte {
	payload := make([]byte, 1<<20)
	rand.Read(payload)
	return payload
})

func newUploadStream(ctx context.Context, recorder *throughputRecorder, size int64, limiter *bandwidthLimiter) io.Reader {
	return &uploadStream{
		ctx:       ctx,
		recorder:  recorder,
		payload:   uploadPayload(),
		remaining: size,
		limiter:   limiter,
	}
}

func (s *uploadStream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > s.remaining {
		p = p[:int(s.remaining)]
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

	if err := s.limiter.wait(s.ctx, written); err != nil {
		return 0, err
	}
	s.recorder.Add(written)
	s.remaining -= int64(written)
	return written, nil
}
