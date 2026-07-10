package client

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalAddrsResolveNetworkPreferenceDualStackDoesNotForceIPv6(t *testing.T) {
	addrs := selectInterfaceAddrs([]net.Addr{
		&net.IPNet{IP: net.ParseIP("2001:db8::10"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(24, 32)},
	}, "warp")

	if !addrs.hasIPv4() || !addrs.hasIPv6() {
		t.Fatalf("expected dual-stack interface selection, got %+v", addrs)
	}

	ipv4Only, ipv6Only, networkPreference := addrs.resolveNetworkPreference(false, false)
	if ipv4Only || ipv6Only {
		t.Fatalf("expected no forced family for dual-stack interface, got ipv4Only=%v ipv6Only=%v", ipv4Only, ipv6Only)
	}
	if networkPreference != "tcp" {
		t.Fatalf("unexpected network preference: got %q want %q", networkPreference, "tcp")
	}

	v4 := addrs.addrForIP(net.ParseIP("198.51.100.20"))
	if v4 == nil || v4.IP.String() != "192.0.2.10" {
		t.Fatalf("unexpected IPv4 source address: got %+v", v4)
	}

	v6 := addrs.addrForIP(net.ParseIP("2001:db8::20"))
	if v6 == nil || v6.IP.String() != "2001:db8::10" {
		t.Fatalf("unexpected IPv6 source address: got %+v", v6)
	}
}

func TestBuildOriginDialTarget(t *testing.T) {
	tests := []struct {
		name      string
		originIP  string
		localAddr *localAddrs
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
			localAddr: &localAddrs{
				ipv6: &net.TCPAddr{
					IP: net.ParseIP("2001:db8::10"),
				},
			},
			wantErr: "bound IPv6 source address",
		},
		{
			name:     "dual stack binding supports ipv4 origin",
			originIP: "1.1.1.1",
			localAddr: &localAddrs{
				ipv4: &net.TCPAddr{IP: net.ParseIP("192.0.2.10")},
				ipv6: &net.TCPAddr{IP: net.ParseIP("2001:db8::10")},
			},
			wantIP:  "1.1.1.1",
			wantNet: "tcp4",
		},
		{
			name:     "dual stack binding supports ipv6 origin",
			originIP: "2606:4700:4700::1111",
			localAddr: &localAddrs{
				ipv4: &net.TCPAddr{IP: net.ParseIP("192.0.2.10")},
				ipv6: &net.TCPAddr{IP: net.ParseIP("2001:db8::10")},
			},
			wantIP:  "2606:4700:4700::1111",
			wantNet: "tcp6",
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

func TestParseProxyURL(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantNil    bool
		wantScheme string
		wantHost   string
		wantErr    string
	}{
		{
			name:    "empty",
			wantNil: true,
		},
		{
			name:       "http explicit",
			input:      "http://proxy.example:8080",
			wantScheme: "http",
			wantHost:   "proxy.example:8080",
		},
		{
			name:       "scheme defaults to http",
			input:      "proxy.example:8080",
			wantScheme: "http",
			wantHost:   "proxy.example:8080",
		},
		{
			name:       "socks5h",
			input:      "socks5h://proxy.example:1080",
			wantScheme: "socks5h",
			wantHost:   "proxy.example:1080",
		},
		{
			name:    "unsupported scheme",
			input:   "ftp://proxy.example:21",
			wantErr: "unsupported proxy scheme",
		},
		{
			name:    "missing host",
			input:   "http://",
			wantErr: "must include a host",
		},
		{
			name:    "missing hostname",
			input:   "http://:8080",
			wantErr: "must include a host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxyURL(tt.input)
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
				if got != nil {
					t.Fatalf("expected nil proxy URL, got %s", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected proxy URL, got nil")
			}
			if got.Scheme != tt.wantScheme {
				t.Fatalf("scheme mismatch: got %q want %q", got.Scheme, tt.wantScheme)
			}
			if got.Host != tt.wantHost {
				t.Fatalf("host mismatch: got %q want %q", got.Host, tt.wantHost)
			}
		})
	}
}

func TestEffectiveProxyURLUsesExplicitProxy(t *testing.T) {
	got, err := EffectiveProxyURL("socks5h://proxy.example:1080", "https://speed.cloudflare.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected proxy URL, got nil")
	}
	if got.String() != "socks5h://proxy.example:1080" {
		t.Fatalf("unexpected proxy URL: got %q", got.String())
	}
}

func TestNewHTTPClientCanUseLocalhostProxy(t *testing.T) {
	var requests int32
	errCh := make(chan error, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if got, want := r.URL.String(), "http://speed.cloudflare.test/probe"; got != want {
			select {
			case errCh <- fmt.Errorf("unexpected proxied URL: got %q want %q", got, want):
			default:
			}
			http.Error(w, "unexpected URL", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(proxy.Close)

	_, port, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}

	httpClient, err := NewHTTPClient(false, false, "", false, "", "http://localhost:"+port)
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}

	resp, err := httpClient.Get("http://speed.cloudflare.test/probe")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		select {
		case err := <-errCh:
			t.Fatal(err)
		default:
		}
		t.Fatalf("unexpected response status: got %d want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected response body: got %q want %q", string(body), "ok")
	}
	if got, want := atomic.LoadInt32(&requests), int32(1); got != want {
		t.Fatalf("proxy request count mismatch: got %d want %d", got, want)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestRewriteRequestForOriginPreservesSpeedflareIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://speed.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	if err := rewriteRequestForOrigin(req, "2001:db8::20"); err != nil {
		t.Fatalf("rewriteRequestForOrigin returned error: %v", err)
	}
	if got, want := req.URL.Host, "[2001:db8::20]:443"; got != want {
		t.Fatalf("target authority mismatch: got %q want %q", got, want)
	}
	if got, want := req.Host, "speed.cloudflare.com"; got != want {
		t.Fatalf("HTTP host mismatch: got %q want %q", got, want)
	}
}

func TestRewriteRequestForOriginLeavesOtherHostsUnchanged(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/probe", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	if err := rewriteRequestForOrigin(req, "192.0.2.20"); err != nil {
		t.Fatalf("rewriteRequestForOrigin returned error: %v", err)
	}
	if got, want := req.URL.Host, "example.com"; got != want {
		t.Fatalf("unexpected target rewrite: got %q want %q", got, want)
	}
}

func TestNewHTTPClientRoutesIPv6OriginThroughIPv4Proxy(t *testing.T) {
	httpClient, err := NewHTTPClient(false, true, "", true, "2001:db8::20", "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}

	browserTransport, ok := httpClient.Transport.(*BrowserTransport)
	if !ok {
		t.Fatalf("unexpected transport type: %T", httpClient.Transport)
	}
	if !browserTransport.RewriteOrigin {
		t.Fatal("expected proxied origin requests to be rewritten")
	}
	if got, want := browserTransport.Transport.TLSClientConfig.ServerName, "speed.cloudflare.com"; got != want {
		t.Fatalf("TLS server name mismatch: got %q want %q", got, want)
	}

	req, err := http.NewRequest(http.MethodGet, "https://speed.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := rewriteRequestForOrigin(req, browserTransport.OriginIP); err != nil {
		t.Fatalf("rewrite request: %v", err)
	}
	proxyURL, err := browserTransport.Transport.Proxy(req)
	if err != nil {
		t.Fatalf("resolve proxy: %v", err)
	}
	if got, want := proxyURL.String(), "http://127.0.0.1:8080"; got != want {
		t.Fatalf("proxy mismatch: got %q want %q", got, want)
	}
}

func TestNewHTTPClientConfiguresSeparateHTTPSProxyTLS(t *testing.T) {
	httpClient, err := NewHTTPClient(false, true, "", true, "2001:db8::20", "https://proxy.example:8443")
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}
	browserTransport, ok := httpClient.Transport.(*BrowserTransport)
	if !ok {
		t.Fatalf("unexpected transport type: %T", httpClient.Transport)
	}
	if browserTransport.Transport.DialTLSContext == nil {
		t.Fatal("expected a separate TLS dialer for the HTTPS proxy hop")
	}
	if got, want := browserTransport.Transport.TLSClientConfig.ServerName, "speed.cloudflare.com"; got != want {
		t.Fatalf("origin TLS server name mismatch: got %q want %q", got, want)
	}
}

func TestNewHTTPClientConnectsToIPv6OriginThroughIPv4Proxy(t *testing.T) {
	requestErr := make(chan error, 2)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Host, "speed.cloudflare.com"; got != want {
			requestErr <- fmt.Errorf("HTTP host mismatch: got %q want %q", got, want)
		}
		if r.TLS == nil || r.TLS.ServerName != "speed.cloudflare.com" {
			requestErr <- fmt.Errorf("TLS server name mismatch: got %#v", r.TLS)
		}
		fmt.Fprintln(w, "colo=FRA")
	}))
	t.Cleanup(origin.Close)

	connectTarget := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connectTarget <- r.Host

		upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		downstream, buffered, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()

		go func() {
			_, _ = io.Copy(upstream, downstream)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()
	}))
	t.Cleanup(proxy.Close)

	httpClient, err := NewHTTPClient(false, true, "", true, "2001:db8::20", proxy.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}
	resp, err := httpClient.Get("https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		t.Fatalf("proxied origin request failed: %v", err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("closing response: read=%v close=%v", readErr, closeErr)
	}

	select {
	case got := <-connectTarget:
		if want := "[2001:db8::20]:443"; got != want {
			t.Fatalf("CONNECT target mismatch: got %q want %q", got, want)
		}
	default:
		t.Fatal("proxy did not receive CONNECT request")
	}
	select {
	case err := <-requestErr:
		t.Fatal(err)
	default:
	}
}
