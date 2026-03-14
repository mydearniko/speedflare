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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/idanyas/speedflare/internal/data"
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

	body, _ := io.ReadAll(resp.Body)
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
func ProbeColos(userLat, userLon float64, userColo string, userCountryCode string, allLocs []data.Location) ([]data.Server, error) {
	var ips []string
	for _, cidr := range probeRanges {
		generated, err := generateRandomIPs(cidr, probeCountPerRange)
		if err == nil {
			ips = append(ips, generated...)
		}
	}

	type probeResult struct {
		Colo string
		IP   string
	}
	results := make(chan probeResult, len(ips))
	var wg sync.WaitGroup

	// Use high concurrency for speed
	workerLimit := make(chan struct{}, 256)

	for _, ip := range ips {
		wg.Add(1)
		go func(targetIP string) {
			defer wg.Done()
			workerLimit <- struct{}{}
			defer func() { <-workerLimit }()

			colo, err := probeSingleIP(targetIP)
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

	return buildServerChoices(uniqueColos, userLat, userLon, userColo, userCountryCode, allLocs), nil
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

func probeSingleIP(ip string) (string, error) {
	// Increased timeouts slightly to avoid skipping good servers on jittery lines
	dialer := &net.Dialer{
		Timeout: 1500 * time.Millisecond,
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip, "443"))
		},
		TLSHandshakeTimeout: 1500 * time.Millisecond,
		DisableKeepAlives:   true,
		TLSClientConfig: &tls.Config{
			ServerName:         "speed.cloudflare.com",
			InsecureSkipVerify: true,
		},
		ForceAttemptHTTP2: true,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   2500 * time.Millisecond,
	}

	resp, err := client.Get("https://speed.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
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
