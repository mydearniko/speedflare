package main

import (
	"math"
	"testing"

	"github.com/idanyas/speedflare/internal/app"
)

func TestResolveTestMode(t *testing.T) {
	tests := []struct {
		name      string
		upload    bool
		download  bool
		latency   bool
		bidir     bool
		wantMode  app.TestMode
		wantError bool
	}{
		{
			name:     "default",
			wantMode: app.TestModeDefault,
		},
		{
			name:     "upload only",
			upload:   true,
			wantMode: app.TestModeUploadOnly,
		},
		{
			name:     "download only",
			download: true,
			wantMode: app.TestModeDownloadOnly,
		},
		{
			name:     "latency only",
			latency:  true,
			wantMode: app.TestModeLatencyOnly,
		},
		{
			name:     "bidirectional",
			bidir:    true,
			wantMode: app.TestModeBidirectional,
		},
		{
			name:      "upload and download conflict",
			upload:    true,
			download:  true,
			wantError: true,
		},
		{
			name:      "upload and latency conflict",
			upload:    true,
			latency:   true,
			wantError: true,
		},
		{
			name:      "download and latency conflict",
			download:  true,
			latency:   true,
			wantError: true,
		},
		{
			name:      "all three conflict",
			upload:    true,
			download:  true,
			latency:   true,
			wantError: true,
		},
		{
			name:      "bidirectional and upload conflict",
			upload:    true,
			bidir:     true,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTestMode(tt.upload, tt.download, tt.latency, tt.bidir)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantMode {
				t.Fatalf("mode mismatch: got %v want %v", got, tt.wantMode)
			}
		})
	}
}

func TestValidateSpeedLimit(t *testing.T) {
	tests := []struct {
		name      string
		value     float64
		wantError bool
	}{
		{name: "unlimited"},
		{name: "fractional", value: 0.25},
		{name: "high", value: 100_000},
		{name: "negative", value: -1, wantError: true},
		{name: "nan", value: math.NaN(), wantError: true},
		{name: "positive infinity", value: math.Inf(1), wantError: true},
		{name: "negative infinity", value: math.Inf(-1), wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSpeedLimit("download-limit (-d)", tt.value)
			if tt.wantError && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateContinuousFlag(t *testing.T) {
	tests := []struct {
		name      string
		mode      app.TestMode
		enabled   bool
		wantError bool
	}{
		{
			name:    "disabled",
			mode:    app.TestModeDefault,
			enabled: false,
		},
		{
			name:    "upload only",
			mode:    app.TestModeUploadOnly,
			enabled: true,
		},
		{
			name:    "download only",
			mode:    app.TestModeDownloadOnly,
			enabled: true,
		},
		{
			name:      "default mode rejected",
			mode:      app.TestModeDefault,
			enabled:   true,
			wantError: true,
		},
		{
			name:      "latency mode rejected",
			mode:      app.TestModeLatencyOnly,
			enabled:   true,
			wantError: true,
		},
		{
			name:    "bidirectional",
			mode:    app.TestModeBidirectional,
			enabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContinuousFlag(tt.mode, tt.enabled)
			if tt.wantError && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestUseIPv6ForProbing(t *testing.T) {
	tests := []struct {
		name     string
		traceIP  string
		ipv4Only bool
		ipv6Only bool
		wantIPv6 bool
	}{
		{name: "forced IPv4", traceIP: "2001:db8::10", ipv4Only: true},
		{name: "forced IPv6", traceIP: "192.0.2.10", ipv6Only: true, wantIPv6: true},
		{name: "automatic IPv4", traceIP: "192.0.2.10"},
		{name: "automatic IPv6", traceIP: "2001:db8::10", wantIPv6: true},
		{name: "invalid trace defaults to IPv4", traceIP: "not-an-ip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useIPv6ForProbing(tt.traceIP, tt.ipv4Only, tt.ipv6Only); got != tt.wantIPv6 {
				t.Fatalf("probe family mismatch: got IPv6=%v want %v", got, tt.wantIPv6)
			}
		})
	}
}
