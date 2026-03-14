package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/gwatts/rootcerts"
	"github.com/miekg/dns"
)

// BrowserTransport wraps an http.RoundTripper and adds browser-mimicking headers.
type BrowserTransport struct {
	Transport *http.Transport
}

type dialTarget struct {
	ip      string
	network string
}

type localAddrs struct {
	interfaceName string
	ipv4          *net.TCPAddr
	ipv6          *net.TCPAddr
}

func (t *BrowserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())

	if clone.Header.Get("User-Agent") == "" {
		clone.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	}
	clone.Header.Set("Referer", "https://speed.cloudflare.com/")
	clone.Header.Set("Accept-Language", "en-US,en;q=0.9")
	clone.Header.Set("Accept", "*/*")
	clone.Header.Set("Cache-Control", "no-cache")

	return t.Transport.RoundTrip(clone)
}

func (l *localAddrs) hasIPv4() bool {
	return l != nil && l.ipv4 != nil && l.ipv4.IP != nil
}

func (l *localAddrs) hasIPv6() bool {
	return l != nil && l.ipv6 != nil && l.ipv6.IP != nil
}

func (l *localAddrs) hasAny() bool {
	return l.hasIPv4() || l.hasIPv6()
}

func (l *localAddrs) singleAddr() *net.TCPAddr {
	switch {
	case l == nil:
		return nil
	case l.hasIPv4() && !l.hasIPv6():
		return l.ipv4
	case l.hasIPv6() && !l.hasIPv4():
		return l.ipv6
	default:
		return nil
	}
}

func (l *localAddrs) addrForIP(ip net.IP) *net.TCPAddr {
	if l == nil || ip == nil {
		return nil
	}
	if ip.To4() != nil {
		return l.ipv4
	}
	return l.ipv6
}

func (l *localAddrs) resolveNetworkPreference(ipv4Only, ipv6Only bool) (bool, bool, string) {
	currentIpv4Only := ipv4Only
	currentIpv6Only := ipv6Only
	networkPreference := "tcp"

	switch {
	case currentIpv4Only:
		networkPreference = "tcp4"
	case currentIpv6Only:
		networkPreference = "tcp6"
	case l == nil:
		// No local binding requested; allow both families.
	case l.hasIPv4() && !l.hasIPv6():
		currentIpv4Only = true
		networkPreference = "tcp4"
	case l.hasIPv6() && !l.hasIPv4():
		currentIpv6Only = true
		networkPreference = "tcp6"
	}

	return currentIpv4Only, currentIpv6Only, networkPreference
}

func sourceFamilyMismatchError(targetIP net.IP, localAddr *net.TCPAddr) error {
	if targetIP == nil || localAddr == nil || localAddr.IP == nil {
		return nil
	}

	targetIsIPv4 := targetIP.To4() != nil
	localIsIPv4 := localAddr.IP.To4() != nil
	if targetIsIPv4 == localIsIPv4 {
		return nil
	}

	localFamily := "IPv6"
	targetFamily := "IPv6"
	if localIsIPv4 {
		localFamily = "IPv4"
	}
	if targetIsIPv4 {
		targetFamily = "IPv4"
	}

	return fmt.Errorf("cannot use %s origin IP %s with bound %s source address %s", targetFamily, targetIP.String(), localFamily, localAddr.IP.String())
}

func extractIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func selectInterfaceAddrs(addrs []net.Addr, ifaceName string) *localAddrs {
	selected := &localAddrs{interfaceName: ifaceName}

	for _, addr := range addrs {
		ip := extractIP(addr)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}

		if ipv4 := ip.To4(); ipv4 != nil {
			if !selected.hasIPv4() {
				selected.ipv4 = &net.TCPAddr{IP: ipv4, Port: 0}
			}
			continue
		}

		candidate := &net.TCPAddr{IP: ip, Port: 0}
		if ip.IsLinkLocalUnicast() {
			candidate.Zone = ifaceName
			if !selected.hasIPv6() {
				selected.ipv6 = candidate
			}
			continue
		}

		if !selected.hasIPv6() || selected.ipv6.IP.IsLinkLocalUnicast() {
			selected.ipv6 = candidate
		}
	}

	return selected
}

func getLocalAddrs(interfaceOrIP string, ipv4Only, ipv6Only bool) (*localAddrs, error) {
	if interfaceOrIP == "" {
		return nil, nil
	}

	if ip := net.ParseIP(interfaceOrIP); ip != nil {
		isIPv4 := ip.To4() != nil
		if ipv4Only && !isIPv4 {
			return nil, fmt.Errorf("provided IP %s is not IPv4, but --ipv4 flag was specified", interfaceOrIP)
		}
		if ipv6Only && isIPv4 {
			return nil, fmt.Errorf("provided IP %s is not IPv6, but --ipv6 flag was specified", interfaceOrIP)
		}

		selected := &localAddrs{}
		if isIPv4 {
			selected.ipv4 = &net.TCPAddr{IP: ip.To4(), Port: 0}
		} else {
			selected.ipv6 = &net.TCPAddr{IP: ip, Port: 0}
		}
		return selected, nil
	}

	iface, err := net.InterfaceByName(interfaceOrIP)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %q: %w", interfaceOrIP, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get addresses for interface %q: %w", interfaceOrIP, err)
	}

	selected := selectInterfaceAddrs(addrs, iface.Name)

	switch {
	case ipv4Only:
		if !selected.hasIPv4() {
			return nil, fmt.Errorf("no suitable IPv4 IP address found for interface %q", interfaceOrIP)
		}
		return &localAddrs{interfaceName: selected.interfaceName, ipv4: selected.ipv4}, nil
	case ipv6Only:
		if !selected.hasIPv6() {
			return nil, fmt.Errorf("no suitable IPv6 IP address found for interface %q", interfaceOrIP)
		}
		return &localAddrs{interfaceName: selected.interfaceName, ipv6: selected.ipv6}, nil
	case !selected.hasAny():
		return nil, fmt.Errorf("no suitable IP address found for interface %q", interfaceOrIP)
	default:
		return selected, nil
	}
}

func buildOriginDialTarget(originIP string, localAddrs *localAddrs, ipv4Only, ipv6Only bool) (*dialTarget, error) {
	if originIP == "" {
		return nil, nil
	}

	targetIP := net.ParseIP(originIP)
	if targetIP == nil {
		return nil, fmt.Errorf("invalid origin IP: %s", originIP)
	}

	isIPv4 := targetIP.To4() != nil
	if ipv4Only && !isIPv4 {
		return nil, fmt.Errorf("origin IP %s is not IPv4, but --ipv4 flag was specified", originIP)
	}
	if ipv6Only && isIPv4 {
		return nil, fmt.Errorf("origin IP %s is not IPv6, but --ipv6 flag was specified", originIP)
	}

	if tcpAddr := localAddrs.singleAddr(); tcpAddr != nil {
		if err := sourceFamilyMismatchError(targetIP, tcpAddr); err != nil {
			return nil, err
		}
	}

	network := "tcp6"
	if isIPv4 {
		network = "tcp4"
	}

	return &dialTarget{
		ip:      targetIP.String(),
		network: network,
	}, nil
}

// NewHTTPClient creates a new HTTP client.
// originIP allows binding to a specific destination IP (ignoring DNS).
func NewHTTPClient(ipv4OnlyFlag, ipv6OnlyFlag bool, interfaceOrIP string, insecureSkipVerify bool, originIP string) (*http.Client, error) {
	localAddrs, err := getLocalAddrs(interfaceOrIP, ipv4OnlyFlag, ipv6OnlyFlag)
	if err != nil {
		return nil, err
	}

	originDialTarget, err := buildOriginDialTarget(originIP, localAddrs, ipv4OnlyFlag, ipv6OnlyFlag)
	if err != nil {
		return nil, err
	}

	baseDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	tlsClientConfig := &tls.Config{
		ServerName:         "speed.cloudflare.com",
		InsecureSkipVerify: insecureSkipVerify,
	}

	if !insecureSkipVerify {
		tlsClientConfig.RootCAs = rootcerts.ServerCertPool()
		if tlsClientConfig.RootCAs == nil {
			log.Println("Warning: rootcerts.ServerCertPool() returned nil. Forcing embedded certs again.")
			tlsClientConfig.RootCAs = rootcerts.ServerCertPool()
			if tlsClientConfig.RootCAs == nil {
				return nil, errors.New("critical failure: unable to obtain a valid root CA pool")
			}
		}
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialWithLocalAddr := func(ctx context.Context, network, addr string, localAddr net.Addr) (net.Conn, error) {
				dialer := *baseDialer
				dialer.LocalAddr = localAddr
				if localAddrs != nil && localAddrs.interfaceName != "" {
					dialer.Control = func(network, address string, c syscall.RawConn) error {
						return bindSocketToDevice(network, address, localAddrs.interfaceName, c)
					}
				}
				return dialer.DialContext(ctx, network, addr)
			}

			// Explicit origin IP override: bypass DNS for speed.cloudflare.com.
			if originDialTarget != nil {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("invalid address format: %w", err)
				}

				targetIP := net.ParseIP(originDialTarget.ip)
				localAddr := localAddrs.addrForIP(targetIP)
				if localAddrs != nil && localAddrs.hasAny() && localAddr == nil {
					return nil, fmt.Errorf("cannot use origin IP %s with the requested source binding", originDialTarget.ip)
				}

				return dialWithLocalAddr(ctx, originDialTarget.network, net.JoinHostPort(originDialTarget.ip, port), localAddr)
			}

			// Standard Logic
			currentIpv4Only, currentIpv6Only, networkPreference := localAddrs.resolveNetworkPreference(ipv4OnlyFlag, ipv6OnlyFlag)

			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address format: %w", err)
			}

			if ip := net.ParseIP(host); ip != nil {
				isIPv4 := ip.To4() != nil
				currentNetworkType := "tcp6"
				if isIPv4 {
					currentNetworkType = "tcp4"
				}

				if (networkPreference == "tcp4" && !isIPv4) || (networkPreference == "tcp6" && isIPv4) {
					return nil, fmt.Errorf("target IP address %s does not match required network type %s", host, networkPreference)
				}
				localAddr := localAddrs.addrForIP(ip)
				if localAddrs != nil && localAddrs.hasAny() && localAddr == nil {
					return nil, fmt.Errorf("target IP address %s does not match the requested source binding", host)
				}
				conn, dialErr := dialWithLocalAddr(ctx, currentNetworkType, net.JoinHostPort(ip.String(), port), localAddr)
				if dialErr != nil {
					// Error handling logic
					return nil, dialErr
				}
				return conn, nil
			}

			resolvedIPs, resolveErr := resolveHost(ctx, host, currentIpv4Only, currentIpv6Only, insecureSkipVerify, tlsClientConfig.RootCAs)
			if resolveErr != nil {
				return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, resolveErr)
			}
			if len(resolvedIPs) == 0 {
				return nil, fmt.Errorf("DNS resolution failed: no usable IPs returned for %s", host)
			}

			var firstDialErr error
			for _, ip := range resolvedIPs {
				ipStr := ip.String()
				isIPv4 := ip.To4() != nil

				currentNetworkType := "tcp"
				if isIPv4 {
					currentNetworkType = "tcp4"
				} else {
					currentNetworkType = "tcp6"
				}

				if (networkPreference == "tcp4" && !isIPv4) || (networkPreference == "tcp6" && isIPv4) {
					continue
				}

				localAddr := localAddrs.addrForIP(ip)
				if localAddrs != nil && localAddrs.hasAny() && localAddr == nil {
					continue
				}

				// Implement per-IP timeout to prevent one blackholed IP from stalling the entire process
				dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				conn, dialErr := dialWithLocalAddr(dialCtx, currentNetworkType, net.JoinHostPort(ipStr, port), localAddr)
				cancel()

				if dialErr == nil {
					return conn, nil // Success
				}
				if firstDialErr == nil {
					firstDialErr = dialErr
				}
			}

			return nil, fmt.Errorf("connection failed to all resolved IPs for %s:%s (first error: %v)", host, port, firstDialErr)

		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       tlsClientConfig,
	}

	return &http.Client{
		Transport: &BrowserTransport{Transport: transport},
		Timeout:   30 * time.Second,
	}, nil
}

// ResolveHost resolves a hostname using the same DoH/system/direct-DNS fallback chain
// used by the main HTTP client.
func ResolveHost(ctx context.Context, host string, ipv4Only, ipv6Only bool, insecureSkipVerify bool, rootCAs *x509.CertPool) ([]net.IP, error) {
	return resolveHost(ctx, host, ipv4Only, ipv6Only, insecureSkipVerify, rootCAs)
}

func resolveHost(ctx context.Context, host string, ipv4Only, ipv6Only bool, insecureSkipVerify bool, rootCAs *x509.CertPool) ([]net.IP, error) {
	var lastErr error
	var ips []net.IP

	// Attempt 1: DoH
	ips, err := resolveWithDoH(ctx, host, ipv4Only, ipv6Only, insecureSkipVerify, rootCAs)
	if err == nil && len(ips) > 0 {
		return ips, nil
	}
	if err != nil {
		lastErr = fmt.Errorf("doH failed: %w", err)
	}

	// Attempt 2: System Resolver
	resolver := net.Resolver{}
	ipAddrs, err := resolver.LookupIPAddr(ctx, host)
	if err == nil && len(ipAddrs) > 0 {
		filteredSystemIPs := ipAddrs[:0]
		for _, ipAddr := range ipAddrs {
			if ipAddr.IP.IsUnspecified() || ipAddr.IP.IsLoopback() {
				continue
			}
			isIPv4 := ipAddr.IP.To4() != nil
			if ipv4Only && !isIPv4 {
				continue
			}
			if ipv6Only && isIPv4 {
				continue
			}
			filteredSystemIPs = append(filteredSystemIPs, ipAddr)
		}
		if len(filteredSystemIPs) > 0 {
			resultIPs := make([]net.IP, len(filteredSystemIPs))
			for i, ipa := range filteredSystemIPs {
				resultIPs[i] = ipa.IP
			}
			return resultIPs, nil
		}
	}
	if err != nil {
		sysDNSErr := fmt.Errorf("system DNS failed: %w", err)
		if lastErr != nil {
			lastErr = fmt.Errorf("%w; %w", lastErr, sysDNSErr)
		} else {
			lastErr = sysDNSErr
		}
	}

	// Attempt 3: Direct DNS
	ips, err = resolveWithDirectDNS(ctx, host, ipv4Only, ipv6Only)
	if err == nil && len(ips) > 0 {
		return ips, nil
	}
	if err != nil {
		directDNSErr := fmt.Errorf("direct DNS failed: %w", err)
		if lastErr != nil {
			lastErr = fmt.Errorf("%w; %w", lastErr, directDNSErr)
		} else {
			lastErr = directDNSErr
		}
	}

	if lastErr == nil {
		lastErr = errors.New("all resolution methods failed")
	}
	return nil, fmt.Errorf("all resolution methods failed for %s: %w", host, lastErr)
}

func resolveWithDoH(ctx context.Context, host string, ipv4Only, ipv6Only bool, insecureSkipVerify bool, rootCAs *x509.CertPool) ([]net.IP, error) {
	dohServers := []struct {
		address string
		sni     string
		isV4    bool
	}{
		{"1.1.1.1:443", "cloudflare-dns.com", true},
		{"1.0.0.1:443", "cloudflare-dns.com", true},
		{"8.8.8.8:443", "dns.google", true},
		{"8.8.4.4:443", "dns.google", true},
		{"9.9.9.9:443", "dns.quad9.net", true},
		{"[2606:4700:4700::1111]:443", "cloudflare-dns.com", false},
		{"[2606:4700:4700::1001]:443", "cloudflare-dns.com", false},
		{"[2001:4860:4860::8888]:443", "dns.google", false},
		{"[2001:4860:4860::8844]:443", "dns.google", false},
		{"[2620:fe::fe]:443", "dns.quad9.net", false},
	}

	rand.Shuffle(len(dohServers), func(i, j int) {
		dohServers[i], dohServers[j] = dohServers[j], dohServers[i]
	})

	var ips []net.IP
	var queryTypes []uint16

	switch {
	case ipv4Only:
		queryTypes = []uint16{dns.TypeA}
	case ipv6Only:
		queryTypes = []uint16{dns.TypeAAAA}
	default:
		queryTypes = []uint16{dns.TypeA, dns.TypeAAAA}
	}

	var lastErr error
	var wg sync.WaitGroup
	var mu sync.Mutex
	processedQueryType := make(map[uint16]bool)

	for _, qtype := range queryTypes {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), qtype)
		m.RecursionDesired = true

		msg, err := m.Pack()
		if err != nil {
			continue
		}
		b64 := base64.RawURLEncoding.EncodeToString(msg)

		queryCtx, queryCancel := context.WithCancel(ctx)
		typeAttempted := false

		for _, server := range dohServers {
			if (ipv4Only && !server.isV4) || (ipv6Only && server.isV4) {
				continue
			}

			typeAttempted = true
			wg.Add(1)
			go func(server struct {
				address, sni string
				isV4         bool
			}, currentQType uint16) {
				defer wg.Done()
				select {
				case <-queryCtx.Done():
					return
				default:
				}

				dialer := &net.Dialer{Timeout: 5 * time.Second}
				dohTransport := &http.Transport{
					TLSClientConfig: &tls.Config{
						ServerName:         server.sni,
						RootCAs:            rootCAs,
						InsecureSkipVerify: insecureSkipVerify,
					},
					Proxy: http.ProxyFromEnvironment,
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						dohNet := "tcp4"
						if !server.isV4 {
							dohNet = "tcp6"
						}
						dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
						defer dialCancel()
						return dialer.DialContext(dialCtx, dohNet, server.address)
					},
					DisableKeepAlives:     true,
					ForceAttemptHTTP2:     true,
					TLSHandshakeTimeout:   5 * time.Second,
					ExpectContinueTimeout: 1 * time.Second,
				}
				dohClient := &http.Client{
					Transport: dohTransport,
					Timeout:   10 * time.Second,
				}
				reqCtx, reqCancel := context.WithTimeout(queryCtx, 10*time.Second)
				defer reqCancel()
				req, err := http.NewRequestWithContext(reqCtx, "GET", fmt.Sprintf("https://%s/dns-query?dns=%s", server.sni, b64), nil)
				if err != nil {
					mu.Lock()
					if !errors.Is(reqCtx.Err(), context.Canceled) {
						if lastErr == nil {
							lastErr = err
						}
					}
					mu.Unlock()
					return
				}
				req.Header.Set("Accept", "application/dns-message")
				resp, err := dohClient.Do(req)
				if err != nil {
					mu.Lock()
					if !errors.Is(reqCtx.Err(), context.Canceled) {
						if lastErr == nil {
							lastErr = err
						}
					}
					mu.Unlock()
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					return
				}
				body, _ := io.ReadAll(resp.Body)
				response := new(dns.Msg)
				if err := response.Unpack(body); err != nil {
					return
				}

				mu.Lock()
				if errors.Is(queryCtx.Err(), context.Canceled) {
					mu.Unlock()
					return
				}
				addedRelevantIP := false
				for _, ans := range response.Answer {
					switch a := ans.(type) {
					case *dns.A:
						if !ipv6Only && currentQType == dns.TypeA && !a.A.IsUnspecified() && !a.A.IsLoopback() {
							ips = append(ips, a.A)
							addedRelevantIP = true
						}
					case *dns.AAAA:
						if !ipv4Only && currentQType == dns.TypeAAAA && !a.AAAA.IsUnspecified() && !a.AAAA.IsLoopback() {
							ips = append(ips, a.AAAA)
							addedRelevantIP = true
						}
					}
				}

				if addedRelevantIP && !processedQueryType[currentQType] {
					processedQueryType[currentQType] = true
					queryCancel()
				}
				mu.Unlock()
			}(server, qtype)
		}

		if typeAttempted {
			wg.Wait()
		}
		queryCancel()

		// If IPs found, break outer loop
		mu.Lock()
		if len(ips) > 0 {
			mu.Unlock()
			break
		}
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ips) > 0 {
		return ips, nil
	}

	return nil, fmt.Errorf("no usable IPs resolved via DoH")
}

func resolveWithDirectDNS(ctx context.Context, host string, ipv4Only, ipv6Only bool) ([]net.IP, error) {
	servers := []struct {
		addr string
		isV4 bool
	}{
		{"1.1.1.1:53", true},
		{"8.8.8.8:53", true},
		{"9.9.9.9:53", true},
		{"[2606:4700:4700::1111]:53", false},
	}

	rand.Shuffle(len(servers), func(i, j int) {
		servers[i], servers[j] = servers[j], servers[i]
	})

	var queryTypes []uint16
	switch {
	case ipv4Only:
		queryTypes = []uint16{dns.TypeA}
	case ipv6Only:
		queryTypes = []uint16{dns.TypeAAAA}
	default:
		queryTypes = []uint16{dns.TypeA, dns.TypeAAAA}
	}

	var ips []net.IP
	var wg sync.WaitGroup
	var mu sync.Mutex
	processedQueryType := make(map[uint16]bool)

	dnsClientUDP := &dns.Client{Net: "udp", Timeout: 3 * time.Second}

	for _, qtype := range queryTypes {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(host), qtype)
		m.RecursionDesired = true

		queryCtx, queryCancel := context.WithCancel(ctx)
		typeAttempted := false

		for _, server := range servers {
			if (ipv4Only && !server.isV4) || (ipv6Only && server.isV4) {
				continue
			}

			typeAttempted = true
			wg.Add(1)
			go func(serverAddr string, currentQType uint16) {
				defer wg.Done()
				select {
				case <-queryCtx.Done():
					return
				default:
				}

				response, _, err := dnsClientUDP.ExchangeContext(queryCtx, m, serverAddr)

				mu.Lock()
				if errors.Is(queryCtx.Err(), context.Canceled) {
					mu.Unlock()
					return
				}

				if err != nil || (response != nil && response.Rcode != dns.RcodeSuccess) {
					mu.Unlock()
					return
				}

				addedRelevantIP := false
				for _, ans := range response.Answer {
					switch a := ans.(type) {
					case *dns.A:
						if !ipv6Only && currentQType == dns.TypeA && !a.A.IsUnspecified() && !a.A.IsLoopback() {
							ips = append(ips, a.A)
							addedRelevantIP = true
						}
					case *dns.AAAA:
						if !ipv4Only && currentQType == dns.TypeAAAA && !a.AAAA.IsUnspecified() && !a.AAAA.IsLoopback() {
							ips = append(ips, a.AAAA)
							addedRelevantIP = true
						}
					}
				}

				if addedRelevantIP && !processedQueryType[currentQType] {
					processedQueryType[currentQType] = true
					queryCancel()
				}
				mu.Unlock()

			}(server.addr, qtype)
		}

		if typeAttempted {
			wg.Wait()
		}
		queryCancel()

		mu.Lock()
		if len(ips) > 0 {
			mu.Unlock()
			break
		}
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()

	if len(ips) > 0 {
		return ips, nil
	}
	return nil, errors.New("no usable IPs resolved via direct DNS")
}
