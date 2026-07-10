package location

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func configureGeoIPTest(t *testing.T, server *httptest.Server) {
	t.Helper()

	originalEndpoint := geoIPEndpoint
	originalAttempts := geoIPAttempts
	originalClient := geoIPClient

	geoIPEndpoint = server.URL + "/"
	geoIPAttempts = 1
	geoIPClient = func(ipv4Only, ipv6Only bool, interfaceOrIP, proxyAddress string) (*http.Client, error) {
		return server.Client(), nil
	}

	t.Cleanup(func() {
		geoIPEndpoint = originalEndpoint
		geoIPAttempts = originalAttempts
		geoIPClient = originalClient
	})
}

func TestGetUserCoordinatesLooksUpIPv6Address(t *testing.T) {
	requestErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/2001:db8::1234"; got != want {
			requestErr <- fmt.Errorf("request path mismatch: got %q want %q", got, want)
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if fields := r.URL.Query().Get("fields"); !strings.Contains(fields, "latitude") || !strings.Contains(fields, "longitude") {
			requestErr <- fmt.Errorf("coordinate fields missing from request: %q", fields)
			http.Error(w, "missing fields", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ip":"2001:db8::1234","success":true,"type":"IPv6","latitude":50.0755,"longitude":14.4378}`)
	}))
	t.Cleanup(server.Close)
	configureGeoIPTest(t, server)

	var gotIPv4Only, gotIPv6Only bool
	var gotInterface string
	var gotProxy string
	geoIPClient = func(ipv4Only, ipv6Only bool, interfaceOrIP, proxyAddress string) (*http.Client, error) {
		gotIPv4Only = ipv4Only
		gotIPv6Only = ipv6Only
		gotInterface = interfaceOrIP
		gotProxy = proxyAddress
		return server.Client(), nil
	}

	lat, lon, err := GetUserCoordinates("2001:db8::10", "2001:db8::1234", false, true, "socks5h://proxy.test:1080")
	if err != nil {
		t.Fatalf("GetUserCoordinates returned error: %v", err)
	}
	select {
	case err := <-requestErr:
		t.Fatal(err)
	default:
	}

	if lat != 50.0755 || lon != 14.4378 {
		t.Fatalf("unexpected coordinates: got %.4f, %.4f", lat, lon)
	}
	if gotIPv4Only || !gotIPv6Only {
		t.Fatalf("unexpected address-family options: ipv4Only=%v ipv6Only=%v", gotIPv4Only, gotIPv6Only)
	}
	if gotInterface != "2001:db8::10" {
		t.Fatalf("source address mismatch: got %q", gotInterface)
	}
	if gotProxy != "socks5h://proxy.test:1080" {
		t.Fatalf("proxy address mismatch: got %q", gotProxy)
	}
}

func TestGetUserCoordinatesRejectsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":false,"message":"Invalid IP address"}`)
	}))
	t.Cleanup(server.Close)
	configureGeoIPTest(t, server)

	_, _, err := GetUserCoordinates("", "2001:db8::1234", false, true, "")
	if err == nil {
		t.Fatal("expected provider error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid IP address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGeoIPRequestURLRejectsInvalidPublicIP(t *testing.T) {
	_, err := geoIPRequestURL("https://ipwho.is/", "not-an-ip")
	if err == nil {
		t.Fatal("expected invalid public IP error, got nil")
	}
}

func TestGeoIPRequestURLSupportsBothAddressFamilies(t *testing.T) {
	tests := []struct {
		name     string
		publicIP string
		wantPath string
	}{
		{name: "IPv4", publicIP: "192.0.2.10", wantPath: "/192.0.2.10"},
		{name: "IPv6", publicIP: "2001:db8::10", wantPath: "/2001:db8::10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawURL, err := geoIPRequestURL("https://ipwho.is/", tt.publicIP)
			if err != nil {
				t.Fatalf("geoIPRequestURL returned error: %v", err)
			}

			req, err := http.NewRequest(http.MethodGet, rawURL, nil)
			if err != nil {
				t.Fatalf("parse generated URL: %v", err)
			}
			if req.URL.Path != tt.wantPath {
				t.Fatalf("path mismatch: got %q want %q", req.URL.Path, tt.wantPath)
			}
		})
	}
}

func TestNewGeoIPHTTPClientUsesShortRequestTimeout(t *testing.T) {
	httpClient, err := newGeoIPHTTPClient(false, true, "2001:db8::10", "")
	if err != nil {
		t.Fatalf("newGeoIPHTTPClient returned error: %v", err)
	}
	if got, want := httpClient.Timeout, 8*time.Second; got != want {
		t.Fatalf("timeout mismatch: got %v want %v", got, want)
	}
}
