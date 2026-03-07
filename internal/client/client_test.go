package client

import (
	"net"
	"strings"
	"testing"
)

func TestBuildOriginDialTarget(t *testing.T) {
	tests := []struct {
		name      string
		originIP  string
		localAddr net.Addr
		ipv4Only  bool
		ipv6Only  bool
		wantNil   bool
		wantIP    string
		wantNet   string
		wantErr   string
	}{
		{
			name:    "empty origin ip",
			wantNil: true,
		},
		{
			name:     "invalid ip",
			originIP: "not-an-ip",
			wantErr:  "invalid origin IP",
		},
		{
			name:     "ipv4 required but ipv6 provided",
			originIP: "2606:4700:4700::1111",
			ipv4Only: true,
			wantErr:  "is not IPv4",
		},
		{
			name:     "ipv6 required but ipv4 provided",
			originIP: "1.1.1.1",
			ipv6Only: true,
			wantErr:  "is not IPv6",
		},
		{
			name:     "source family mismatch",
			originIP: "1.1.1.1",
			localAddr: &net.TCPAddr{
				IP: net.ParseIP("2001:db8::10"),
			},
			wantErr: "bound IPv6 source address",
		},
		{
			name:     "ipv4 success",
			originIP: "1.1.1.1",
			wantIP:   "1.1.1.1",
			wantNet:  "tcp4",
		},
		{
			name:     "ipv6 success",
			originIP: "2606:4700:4700::1111",
			wantIP:   "2606:4700:4700::1111",
			wantNet:  "tcp6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := buildOriginDialTarget(tt.originIP, tt.localAddr, tt.ipv4Only, tt.ipv6Only)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error mismatch: got %q want substring %q", err.Error(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if target != nil {
					t.Fatalf("expected nil target, got %+v", target)
				}
				return
			}
			if target == nil {
				t.Fatal("expected target, got nil")
			}
			if target.ip != tt.wantIP {
				t.Fatalf("ip mismatch: got %q want %q", target.ip, tt.wantIP)
			}
			if target.network != tt.wantNet {
				t.Fatalf("network mismatch: got %q want %q", target.network, tt.wantNet)
			}
		})
	}
}
