package location

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetUserCoordinatesUsesFallbackResolver(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"location":{"latitude":50.0755,"longitude":14.4378}}`)
	}))
	t.Cleanup(server.Close)

	originalEndpoint := geoIPEndpoint
	originalAttempts := geoIPAttempts
	originalResolveHost := geoIPResolveHost
	originalTLSConfig := geoIPTLSConfig

	t.Cleanup(func() {
		geoIPEndpoint = originalEndpoint
		geoIPAttempts = originalAttempts
		geoIPResolveHost = originalResolveHost
		geoIPTLSConfig = originalTLSConfig
	})

	listenerHost, listenerPort, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	geoIPEndpoint = "https://geoip.test:" + listenerPort + "/"
	geoIPAttempts = 1
	geoIPResolveHost = func(ctx context.Context, host string, rootCAs *x509.CertPool) ([]net.IP, error) {
		if host != "geoip.test" {
			t.Fatalf("unexpected host to resolve: %s", host)
		}

		ip := net.ParseIP(listenerHost)
		if ip == nil {
			t.Fatalf("listener host is not an IP: %s", listenerHost)
		}

		return []net.IP{ip}, nil
	}
	geoIPTLSConfig = func(serverName string, rootCAs *x509.CertPool) *tls.Config {
		return &tls.Config{InsecureSkipVerify: true}
	}

	lat, lon, err := GetUserCoordinates()
	if err != nil {
		t.Fatalf("GetUserCoordinates returned error: %v", err)
	}

	if lat != 50.0755 || lon != 14.4378 {
		t.Fatalf("unexpected coordinates: got %.4f, %.4f", lat, lon)
	}
}
