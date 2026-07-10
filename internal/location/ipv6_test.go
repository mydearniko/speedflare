package location

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIPv6ProbeRangesAreNarrowAndNumerous(t *testing.T) {
	if len(ipv6ProbeRanges) < 100 {
		t.Fatalf("expected at least 100 active IPv6 probe ranges, got %d", len(ipv6ProbeRanges))
	}

	cloudflareAllocations := parseNetworks(t, []string{
		"2400:cb00::/32",
		"2405:b500::/32",
		"2405:8100::/32",
		"2606:4700::/32",
		"2803:f800::/32",
		"2a06:98c0::/29",
		"2c0f:f248::/32",
	})
	seen := make(map[string]struct{}, len(ipv6ProbeRanges))
	for _, cidr := range ipv6ProbeRanges {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("invalid probe range %q: %v", cidr, err)
		}
		if ip.To4() != nil {
			t.Fatalf("IPv4 range present in IPv6 probes: %q", cidr)
		}
		ones, bits := network.Mask.Size()
		if ones != 48 || bits != 128 {
			t.Fatalf("probe range must be a bounded IPv6 /48, got %q", cidr)
		}
		if !containedByAny(ip, cloudflareAllocations) {
			t.Fatalf("probe range is outside Cloudflare's published allocations: %q", cidr)
		}
		if _, exists := seen[network.String()]; exists {
			t.Fatalf("duplicate IPv6 probe range %q", cidr)
		}
		seen[network.String()] = struct{}{}
	}
}

func parseNetworks(t *testing.T, cidrs []string) []*net.IPNet {
	t.Helper()
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("invalid test network %q: %v", cidr, err)
		}
		networks = append(networks, network)
	}
	return networks
}

func containedByAny(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func TestGenerateIPv6ProbeTargetsIncludesSeedsAndSamplesEveryRange(t *testing.T) {
	ranges := []string{
		"2606:4700:7::/48",
		"2a06:98c1:58::/48",
	}
	seeds := []string{
		"2606:4700:7::da",
		"2606:4700:7::da",
		"192.0.2.1",
	}

	targets, err := generateIPv6ProbeTargets(ranges, seeds, 4)
	if err != nil {
		t.Fatalf("generateIPv6ProbeTargets returned error: %v", err)
	}
	if len(targets) != 8 {
		t.Fatalf("target count mismatch: got %d want 8", len(targets))
	}
	if targets[0] != "2606:4700:7::da" {
		t.Fatalf("expected validated seed first, got %q", targets[0])
	}
	if _, ok := sliceToSet(targets)["2a06:98c1:58::6810:b001"]; !ok {
		t.Fatal("expected an IPv4-embedded CDN target in the second range")
	}

	counts := make([]int, len(ranges))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		ip := net.ParseIP(target)
		if ip == nil || ip.To4() != nil {
			t.Fatalf("generated non-IPv6 target %q", target)
		}
		if _, exists := seen[target]; exists {
			t.Fatalf("duplicate target %q", target)
		}
		seen[target] = struct{}{}

		for i, cidr := range ranges {
			_, network, _ := net.ParseCIDR(cidr)
			if network.Contains(ip) {
				counts[i]++
				break
			}
		}
	}
	if counts[0] != 4 || counts[1] != 4 {
		t.Fatalf("unexpected per-range target counts: got %v want [4 4]", counts)
	}
}

func sliceToSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func TestGenerateRandomIPv6sHonorsPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	ips, err := generateRandomIPv6s("2606:4700:3039::/64", 20, rng)
	if err != nil {
		t.Fatalf("generateRandomIPv6s returned error: %v", err)
	}
	_, network, _ := net.ParseCIDR("2606:4700:3039::/64")
	for _, ip := range ips {
		if !network.Contains(ip) {
			t.Fatalf("generated address %s is outside %s", ip, network)
		}
	}
}

func TestGenerateRandomIPv6sRejectsIPv4Range(t *testing.T) {
	_, err := generateRandomIPv6s("192.0.2.0/24", 1, rand.New(rand.NewSource(1)))
	if err == nil {
		t.Fatal("expected IPv4 range error, got nil")
	}
}

func TestProbeColoTargetsKeepsOneWorkingIPPerColo(t *testing.T) {
	targets := []string{"2001:db8::1", "2001:db8::2", "2001:db8::3", "2001:db8::4"}
	probe := func(ip string, opts ProbeOptions) (string, error) {
		if opts.InterfaceOrIP != "eth-test" {
			t.Errorf("interface mismatch: got %q", opts.InterfaceOrIP)
		}
		switch ip {
		case "2001:db8::1", "2001:db8::2":
			return "FRA", nil
		case "2001:db8::3":
			return "AMS", nil
		default:
			return "", errors.New("unreachable")
		}
	}

	got := probeColoTargets(targets, ProbeOptions{InterfaceOrIP: "eth-test"}, 2, probe)
	if len(got) != 2 {
		t.Fatalf("colo count mismatch: got %d want 2 (%v)", len(got), got)
	}
	if got["FRA"] != "2001:db8::1" && got["FRA"] != "2001:db8::2" {
		t.Fatalf("unexpected FRA target: %q", got["FRA"])
	}
	if got["AMS"] != "2001:db8::3" {
		t.Fatalf("unexpected AMS target: %q", got["AMS"])
	}
}

func TestNewProbeDialerSupportsIPv6SourceAddress(t *testing.T) {
	dialer, err := newProbeDialer(net.ParseIP("2606:4700:7::da"), "2001:db8::10", true)
	if err != nil {
		t.Fatalf("newProbeDialer returned error: %v", err)
	}
	local, ok := dialer.LocalAddr.(*net.TCPAddr)
	if !ok || local.IP.String() != "2001:db8::10" {
		t.Fatalf("unexpected local address: %#v", dialer.LocalAddr)
	}
}

func TestNewProbeDialerRejectsSourceFamilyMismatch(t *testing.T) {
	_, err := newProbeDialer(net.ParseIP("2606:4700:7::da"), "192.0.2.10", true)
	if err == nil {
		t.Fatal("expected source-family mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "different address families") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProbeSingleIPv6ThroughConnectProxy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		tlsProxy bool
	}{
		{name: "HTTP"},
		{name: "HTTPS", tlsProxy: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testProbeSingleIPv6ThroughConnectProxy(t, tt.tlsProxy)
		})
	}
}

func testProbeSingleIPv6ThroughConnectProxy(t *testing.T, tlsProxy bool) {
	t.Helper()
	requestErr := make(chan error, 3)
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
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tlsProxy && (r.TLS == nil || r.TLS.ServerName != "localhost") {
			requestErr <- fmt.Errorf("proxy TLS server name mismatch: got %#v", r.TLS)
		}
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
	})
	var proxy *httptest.Server
	if tlsProxy {
		proxy = httptest.NewTLSServer(proxyHandler)
	} else {
		proxy = httptest.NewServer(proxyHandler)
	}
	t.Cleanup(proxy.Close)

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	if tlsProxy {
		_, port, err := net.SplitHostPort(proxyURL.Host)
		if err != nil {
			t.Fatalf("split proxy host: %v", err)
		}
		proxyURL.Host = net.JoinHostPort("localhost", port)
	}
	colo, err := probeSingleIP("2001:db8::1234", ProbeOptions{ProxyURL: proxyURL})
	if err != nil {
		t.Fatalf("probeSingleIP returned error: %v", err)
	}
	if colo != "FRA" {
		t.Fatalf("colo mismatch: got %q want FRA", colo)
	}

	select {
	case got := <-connectTarget:
		if want := "[2001:db8::1234]:443"; got != want {
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
