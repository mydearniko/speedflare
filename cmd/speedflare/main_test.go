package main

import (
	"testing"

	"github.com/idanyas/speedflare/internal/app"
)

func TestResolveTestMode(t *testing.T) {
	tests := []struct {
		name      string
		upload    bool
		download  bool
		latency   bool
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTestMode(tt.upload, tt.download, tt.latency)
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
