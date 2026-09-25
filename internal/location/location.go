package location

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading trace response: %w", err)
	}
	info := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			info[parts[0]] = parts[1]
		}
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

	sort.Slice(locations, func(i, j int) bool {
		return locations[i].IATA < locations[j].IATA
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
	type probeResult struct {
		Colo string
		IP   string
	}
	results := make(chan probeResult, len(ips))
	var wg sync.WaitGroup

	if concurrency < 1 {
		concurrency = 1
	}
	workerLimit := make(chan struct{}, concurrency)

	for _, ip := range ips {
		wg.Add(1)
		go func(targetIP string) {
			defer wg.Done()
			workerLimit <- struct{}{}
			defer func() { <-workerLimit }()

			colo, err := probe(targetIP, opts)
			if err == nil && colo != "" {
				results <- probeResult{Colo: colo, IP: targetIP}
			}
		}(ip)
	}

	wg.Wait()
	close(results)

	uniqueColos := make(map[string]string)
	for res := range results {
		if _, exists := uniqueColos[res.Colo]; !exists {
			uniqueColos[res.Colo] = res.IP
		}
	}

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

	sort.Slice(candidates, func(i, j int) bool {
		// Priority 1: Same country as user
		s1InCountry := candidates[i].server.Country == userCountryCode
		s2InCountry := candidates[j].server.Country == userCountryCode

		if s1InCountry && !s2InCountry {
			return true
		}
		if !s1InCountry && s2InCountry {
			return false
		}

		// Priority 2: Distance (if available)
		if hasSortReference {
			if math.Abs(candidates[i].sortDistance-candidates[j].sortDistance) > 1.0 { // tolerance
				return candidates[i].sortDistance < candidates[j].sortDistance
			}
		}

		// Priority 3: Alphabetical IATA
		return candidates[i].server.IATA < candidates[j].server.IATA
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
		TLSClientConfig: &tls.Config{
			ServerName:         "speed.cloudflare.com",
			InsecureSkipVerify: true,
		},
		ForceAttemptHTTP2: true,
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("reading probe response: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "colo=") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				return parts[1], nil
			}
		}
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
	dLat := (lat2 - lat1) * (math.Pi / 180.0)
	dLon := (lon2 - lon1) * (math.Pi / 180.0)

	lat1 = lat1 * (math.Pi / 180.0)
	lat2 = lat2 * (math.Pi / 180.0)

	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Sin(dLon/2)*math.Sin(dLon/2)*math.Cos(lat1)*math.Cos(lat2)
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

	var ips []string
	startIP := binary.BigEndian.Uint32(ipnet.IP)

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < count; i++ {
		offset := r.Intn(totalHosts)
		ipInt := startIP + uint32(offset)

		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, ipInt)
		ips = append(ips, ip.String())
	}
	return ips, nil
}
