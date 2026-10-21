package location

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mydearniko/speedflare/internal/client"
)

type geoIPResponse struct {
	IP        string  `json:"ip"`
	Success   bool    `json:"success"`
	Message   string  `json:"message"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

var (
	geoIPEndpoint = "https://ipwho.is/"
	geoIPAttempts = 3
	geoIPClient   = newGeoIPHTTPClient
)

// GetUserCoordinates performs parallel GeoIP requests and returns the first
// successful result. publicIP makes the lookup independent of the address
// family used to reach the API, which is important on dual-stack systems.
func GetUserCoordinates(interfaceOrIP, publicIP string, ipv4Only, ipv6Only bool, proxyAddress string) (float64, float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	endpoint, err := geoIPRequestURL(geoIPEndpoint, publicIP)
	if err != nil {
		return 0, 0, err
	}

	httpClient, err := geoIPClient(ipv4Only, ipv6Only, interfaceOrIP, proxyAddress)
	if err != nil {
		return 0, 0, err
	}
	defer httpClient.CloseIdleConnections()
	resultCh := make(chan geoIPResponse, 1)
	errCh := make(chan error, geoIPAttempts)
	doneCh := make(chan struct{})

	var wg sync.WaitGroup
	for range geoIPAttempts {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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

			var data geoIPResponse
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				errCh <- err
				return
			}

			if !data.Success {
				message := strings.TrimSpace(data.Message)
				if message == "" {
					message = "lookup was unsuccessful"
				}
				errCh <- errors.New(message)
				return
			}
			if data.Latitude == 0 && data.Longitude == 0 {
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
		return res.Latitude, res.Longitude, nil
	case <-doneCh:
		return 0, 0, collectGeoIPErrors(errCh, errors.New("geoip failed"))
	case <-ctx.Done():
		return 0, 0, collectGeoIPErrors(errCh, errors.New("geoip timed out"))
	}
}

func geoIPRequestURL(rawEndpoint, publicIP string) (string, error) {
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid geoip endpoint: %w", err)
	}

	publicIP = strings.TrimSpace(publicIP)
	if publicIP != "" {
		ip := net.ParseIP(publicIP)
		if ip == nil {
			return "", fmt.Errorf("invalid public IP for geoip lookup: %q", publicIP)
		}
		endpoint = endpoint.JoinPath(ip.String())
	}

	query := endpoint.Query()
	query.Set("fields", "success,message,ip,type,latitude,longitude")
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func newGeoIPHTTPClient(ipv4Only, ipv6Only bool, interfaceOrIP, proxyAddress string) (*http.Client, error) {
	httpClient, err := client.NewHTTPClient(ipv4Only, ipv6Only, interfaceOrIP, false, "", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("creating GeoIP client: %w", err)
	}

	clone := *httpClient
	clone.Timeout = 8 * time.Second
	return &clone, nil
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
