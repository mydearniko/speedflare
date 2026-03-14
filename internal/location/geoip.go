package location

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/gwatts/rootcerts"

	"github.com/idanyas/speedflare/internal/client"
)

type ipapiResponse struct {
	Location struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
}

var (
	geoIPEndpoint    = "https://api.ipapi.is/"
	geoIPAttempts    = 3
	geoIPResolveHost = func(ctx context.Context, host string, rootCAs *x509.CertPool) ([]net.IP, error) {
		return client.ResolveHost(ctx, host, false, false, false, rootCAs)
	}
	geoIPTLSConfig = func(serverName string, rootCAs *x509.CertPool) *tls.Config {
		return &tls.Config{
			ServerName: serverName,
			RootCAs:    rootCAs,
		}
	}
)

// GetUserCoordinates performs parallel GeoIP requests and returns the first successful result.
func GetUserCoordinates() (float64, float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	endpoint, err := url.Parse(geoIPEndpoint)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid geoip endpoint: %w", err)
	}

	httpClient, err := newGeoIPHTTPClient(endpoint)
	if err != nil {
		return 0, 0, err
	}

	resultCh := make(chan ipapiResponse, 1)
	errCh := make(chan error, geoIPAttempts)
	doneCh := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < geoIPAttempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
			if err != nil {
				errCh <- err
				return
			}

			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Speedflare/1.0)")

			resp, err := httpClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("status %d", resp.StatusCode)
				return
			}

			var data ipapiResponse
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				errCh <- err
				return
			}

			if data.Location.Latitude == 0 && data.Location.Longitude == 0 {
				errCh <- errors.New("zero coordinates received")
				return
			}

			select {
			case resultCh <- data:
			case <-ctx.Done():
			}
		}()
	}

	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case res := <-resultCh:
		return res.Location.Latitude, res.Location.Longitude, nil
	case <-doneCh:
		return 0, 0, collectGeoIPErrors(errCh, errors.New("geoip failed"))
	case <-ctx.Done():
		return 0, 0, collectGeoIPErrors(errCh, errors.New("geoip timed out"))
	}
}

func newGeoIPHTTPClient(endpoint *url.URL) (*http.Client, error) {
	rootCAs := rootcerts.ServerCertPool()
	if rootCAs == nil {
		return nil, errors.New("unable to obtain a valid root CA pool for GeoIP lookup")
	}

	serverName := endpoint.Hostname()
	baseDialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
		Proxy:             http.ProxyFromEnvironment,
		TLSClientConfig:   geoIPTLSConfig(serverName, rootCAs),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address format: %w", err)
			}

			ips, err := geoIPResolveHost(ctx, host, rootCAs)
			if err != nil {
				return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("DNS resolution failed: no usable IPs returned for %s", host)
			}

			var firstDialErr error
			for _, ip := range ips {
				dialNetwork := "tcp6"
				if ip.To4() != nil {
					dialNetwork = "tcp4"
				}

				dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
				conn, dialErr := baseDialer.DialContext(dialCtx, dialNetwork, net.JoinHostPort(ip.String(), port))
				dialCancel()

				if dialErr == nil {
					return conn, nil
				}
				if firstDialErr == nil {
					firstDialErr = dialErr
				}
			}

			return nil, fmt.Errorf("connection failed to all resolved IPs for %s:%s (first error: %v)", host, port, firstDialErr)
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
	}, nil
}

func collectGeoIPErrors(errCh <-chan error, fallback error) error {
	var errs []string
	for {
		select {
		case err := <-errCh:
			if err != nil {
				errs = append(errs, err.Error())
			}
		default:
			if len(errs) == 0 {
				return fallback
			}
			slices.Sort(errs)
			errs = slices.Compact(errs)
			return fmt.Errorf("geoip failed: %v", errs)
		}
	}
}
