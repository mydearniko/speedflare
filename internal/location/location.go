package location

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mydearniko/speedflare/internal/client"
	"github.com/mydearniko/speedflare/internal/data"
)

var defaultProbeRanges = []string{
	"162.159.128.0/19",
	"104.16.176.0/20",
	"172.66.40.0/21",
	"172.67.176.0/20",
	"188.114.97.0/24",
	"198.41.192.0/21",
	"198.41.200.0/21",
}

var probe198Ranges = []string{
	"198.41.192.0/21",
	"198.41.200.0/21",
}

const (
	defaultProbeCountPerRange = 255
	probe198CountPerRange     = 765
	probeWorkerLimit          = 256
	proxyProbeWorkerLimit     = 128
	earthRadiusKm             = 6371.0
)

var probeRanges = append([]string(nil), defaultProbeRanges...)
var probeCountPerRange = defaultProbeCountPerRange

func SetProbeRanges198Only(only198 bool) {
	if only198 {
		probeRanges = append([]string(nil), probe198Ranges...)
		probeCountPerRange = probe198CountPerRange
		return
	}

	probeRanges = append([]string(nil), defaultProbeRanges...)
	probeCountPerRange = defaultProbeCountPerRange
}

func GetServerTrace(client *http.Client) (map[string]string, error) {
	resp, err := client.Get("https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 4096))
	info := make(map[string]string)
	for scanner.Scan() {
		if k, v, ok := strings.Cut(scanner.Text(), "="); ok {
			info[k] = v
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading trace response: %w", err)
	}
	return info, nil
}

func FetchLocations(client *http.Client) ([]data.Location, error) {
	resp, err := client.Get("https://speed.cloudflare.com/locations")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var locations []data.Location
	if err := json.NewDecoder(resp.Body).Decode(&locations); err != nil {
		return nil, err
	}

	slices.SortFunc(locations, func(a, b data.Location) int {
		return strings.Compare(a.IATA, b.IATA)
	})

	return locations, nil
}

func FindServerInfo(iata string, locs []data.Location) (data.Server, error) {
	for _, loc := range locs {
		if loc.IATA == iata {
			return data.Server{
				IATA:    loc.IATA,
				City:    loc.City,
				Country: loc.CCA2,
				Lat:     loc.Lat,
				Lon:     loc.Lon,
			}, nil
		}
	}
	return data.Server{}, fmt.Errorf("server location not found")
}

type serverCandidate struct {
	server       data.Server
	sortDistance float64
}

// ProbeColos determines the closest servers.
// userLat/userLon: Explicit coordinates from GeoIP.
// userColo: The current Cloudflare PoP, used only as a last-resort sorting hint.
// userCountryCode: Country code from Cloudflare trace.
type ProbeOptions struct {
	InterfaceOrIP string
	IPv6          bool
	ProxyURL      *url.URL
}

func ProbeColos(userLat, userLon float64, userColo string, userCountryCode string, allLocs []data.Location, opts ProbeOptions) ([]data.Server, error) {
	var ips []string
	if opts.IPv6 {
		var err error
		ips, err = ipv6ProbeTargets()
		if err != nil {
			return nil, err
		}
	} else {
		for _, cidr := range probeRanges {
			generated, err := generateRandomIPs(cidr, probeCountPerRange)
			if err == nil {
				ips = append(ips, generated...)
			}
		}
	}

	concurrency := probeWorkerLimit
	if opts.ProxyURL != nil {
		concurrency = proxyProbeWorkerLimit
	}
	uniqueColos := probeColoTargets(ips, opts, concurrency, probeSingleIP)
	return buildServerChoices(uniqueColos, userLat, userLon, userColo, userCountryCode, allLocs), nil
}

type coloProbeFunc func(string, ProbeOptions) (string, error)

func probeColoTargets(ips []string, opts ProbeOptions, concurrency int, probe coloProbeFunc) map[string]string {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(ips) {
		concurrency = len(ips)
	}

	// A fixed pool avoids parking one goroutine (and its stack) per target.
	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		next        atomic.Int64
		uniqueColos = make(map[string]string)
	)
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(ips) {
					return
				}
				targetIP := ips[i]
				colo, err := probe(targetIP, opts)
				if err != nil || colo == "" {
					continue
				}
				mu.Lock()
				if _, exists := uniqueColos[colo]; !exists {
					uniqueColos[colo] = targetIP
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return uniqueColos
}

func buildServerChoices(uniqueColos map[string]string, userLat, userLon float64, userColo string, userCountryCode string, allLocs []data.Location) []data.Server {
	displayLat, displayLon, hasDisplayReference := resolveDisplayReference(userLat, userLon, userCountryCode, allLocs)
	sortLat, sortLon, hasSortReference := displayLat, displayLon, hasDisplayReference

	if !hasSortReference {
		if primaryInfo, err := FindServerInfo(userColo, allLocs); err == nil {
			sortLat = primaryInfo.Lat
			sortLon = primaryInfo.Lon
			hasSortReference = true
		}
	}

	var candidates []serverCandidate
	for colo, ip := range uniqueColos {
		info, err := FindServerInfo(colo, allLocs)
		if err != nil {
			continue
		}

		info.IP = ip
		if hasDisplayReference {
			info.Distance = haversine(displayLat, displayLon, info.Lat, info.Lon)
			info.HasDistance = true
		}

		candidate := serverCandidate{server: info}
		if hasSortReference {
			candidate.sortDistance = haversine(sortLat, sortLon, info.Lat, info.Lon)
		}
		candidates = append(candidates, candidate)
	}

	slices.SortFunc(candidates, func(a, b serverCandidate) int {
		// Priority 1: Same country as user
		s1InCountry := a.server.Country == userCountryCode
		s2InCountry := b.server.Country == userCountryCode

		if s1InCountry && !s2InCountry {
			return -1
		}
		if !s1InCountry && s2InCountry {
			return 1
		}

		// Priority 2: Distance (if available)
		if hasSortReference {
			if math.Abs(a.sortDistance-b.sortDistance) > 1.0 { // tolerance
				return cmp.Compare(a.sortDistance, b.sortDistance)
			}
		}

		// Priority 3: Alphabetical IATA
		return strings.Compare(a.server.IATA, b.server.IATA)
	})

	servers := make([]data.Server, 0, len(candidates))
	for _, candidate := range candidates {
		servers = append(servers, candidate.server)
	}

	return servers
}

func resolveDisplayReference(userLat, userLon float64, userCountryCode string, allLocs []data.Location) (float64, float64, bool) {
	if userLat != 0 || userLon != 0 {
		return userLat, userLon, true
	}

	var latSum, lonSum float64
	var count int
	for _, loc := range allLocs {
		if loc.CCA2 == userCountryCode {
			latSum += loc.Lat
			lonSum += loc.Lon
			count++
		}
	}
	if count == 0 {
		return 0, 0, false
	}

	return latSum / float64(count), lonSum / float64(count), true
}

func probeSingleIP(ip string, opts ProbeOptions) (string, error) {
	targetIP := net.ParseIP(ip)
	if targetIP == nil {
		return "", fmt.Errorf("invalid probe IP %q", ip)
	}

	dialer, err := newProbeDialer(targetIP, opts.InterfaceOrIP, opts.ProxyURL == nil)
	if err != nil {
		return "", err
	}

	dialNetwork := "tcp4"
	if targetIP.To4() == nil {
		dialNetwork = "tcp6"
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if opts.ProxyURL != nil {
				return dialer.DialContext(ctx, network, addr)
			}
			return dialer.DialContext(ctx, dialNetwork, net.JoinHostPort(ip, "443"))
		},
		TLSHandshakeTimeout: 1500 * time.Millisecond,
		DisableKeepAlives:   true,
		// Each probe is a single tiny GET over a throwaway connection, so skip
		// HTTP/2 framing state and the post-quantum key share: plain X25519
		// keeps hundreds of concurrent handshakes cheap in CPU and memory.
		TLSClientConfig: &tls.Config{
			ServerName:         "speed.cloudflare.com",
			InsecureSkipVerify: true,
			CurvePreferences:   []tls.CurveID{tls.X25519},
			NextProtos:         []string{"http/1.1"},
		},
		ForceAttemptHTTP2:  false,
		DisableCompression: true,
		WriteBufferSize:    1024,
		ReadBufferSize:     2048,
	}
	if opts.ProxyURL != nil {
		tr.Proxy = http.ProxyURL(opts.ProxyURL)
		if strings.EqualFold(opts.ProxyURL.Scheme, "https") {
			baseDialContext := tr.DialContext
			tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := baseDialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					conn.Close()
					return nil, err
				}
				proxyTLSConfig := tr.TLSClientConfig.Clone()
				proxyTLSConfig.ServerName = host
				// The Cloudflare-specific handshake tuning does not apply to
				// arbitrary proxies; keep Go's defaults for that hop.
				proxyTLSConfig.CurvePreferences = nil
				proxyTLSConfig.NextProtos = nil
				tlsConn := tls.Client(conn, proxyTLSConfig)
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				return tlsConn, nil
			}
		}
	}

	httpClient := &http.Client{
		Transport: tr,
		Timeout:   2500 * time.Millisecond,
	}

	probeURL := (&url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(ip, "443"),
		Path:   "/cdn-cgi/trace",
	}).String()
	req, err := http.NewRequest(http.MethodGet, probeURL, nil)
	if err != nil {
		return "", err
	}
	req.Host = "speed.cloudflare.com"

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("probe returned status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1024))
	for scanner.Scan() {
		b := scanner.Bytes()
		if bytes.HasPrefix(b, []byte("colo=")) {
			return string(bytes.TrimPrefix(b, []byte("colo="))), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading probe response: %w", err)
	}
	return "", fmt.Errorf("no colo found")
}

func newProbeDialer(targetIP net.IP, interfaceOrIP string, enforceTargetFamily bool) (*net.Dialer, error) {
	// Increased timeout slightly to avoid skipping good servers on jittery lines.
	dialer := &net.Dialer{Timeout: 1500 * time.Millisecond}

	if sourceIP := net.ParseIP(interfaceOrIP); sourceIP != nil {
		if enforceTargetFamily && (sourceIP.To4() == nil) != (targetIP.To4() == nil) {
			return nil, fmt.Errorf("source IP %s and probe IP %s use different address families", sourceIP, targetIP)
		}
		dialer.LocalAddr = &net.TCPAddr{IP: sourceIP}
	} else if interfaceOrIP != "" {
		dialer.Control = func(network, address string, c syscall.RawConn) error {
			return client.BindSocketToDevice(network, address, interfaceOrIP, c)
		}
	}

	return dialer, nil
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const degToRad = math.Pi / 180.0
	dLat := (lat2 - lat1) * degToRad
	dLon := (lon2 - lon1) * degToRad

	lat1 = lat1 * degToRad
	lat2 = lat2 * degToRad

	sinDLat2 := math.Sin(dLat / 2)
	sinDLon2 := math.Sin(dLon / 2)

	a := sinDLat2*sinDLat2 + sinDLon2*sinDLon2*math.Cos(lat1)*math.Cos(lat2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadiusKm * c
}
func generateRandomIPs(cidr string, count int) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	mask, _ := ipnet.Mask.Size()
	size := 32 - mask
	totalHosts := 1 << size
	if totalHosts <= 0 {
		totalHosts = 1
	}

	ips := make([]string, 0, count)
	startIP := binary.BigEndian.Uint32(ipnet.IP)

	for range count {
		offset := rand.IntN(totalHosts)
		ipInt := startIP + uint32(offset)

		ips = append(ips, netip.AddrFrom4([4]byte{
			byte(ipInt >> 24),
			byte(ipInt >> 16),
			byte(ipInt >> 8),
			byte(ipInt),
		}).String())
	}
	return ips, nil
}
