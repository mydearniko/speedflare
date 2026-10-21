package location

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"time"
)

const ipv6ProbeCountPerRange = 16

// Randomly sampling Cloudflare's published /29 and /32 allocations almost
// never finds an active IPv6 address. These narrower /48s are CDN ingress
// ranges observed in active AAAA records and IPv6 Cloudflare hitlists.
var ipv6ProbeRanges = []string{
	"2400:cb00:2049::/48",
	"2400:cb00:f00e::/48",
	"2606:4700:7::/48",
	"2606:4700:10::/48",
	"2606:4700:20::/48",
	"2606:4700:30::/48",
	"2606:4700:130::/48",
	"2606:4700:3000::/48",
	"2606:4700:3001::/48",
	"2606:4700:3002::/48",
	"2606:4700:3003::/48",
	"2606:4700:3004::/48",
	"2606:4700:3005::/48",
	"2606:4700:3006::/48",
	"2606:4700:3007::/48",
	"2606:4700:3008::/48",
	"2606:4700:3009::/48",
	"2606:4700:300a::/48",
	"2606:4700:3010::/48",
	"2606:4700:3011::/48",
	"2606:4700:3012::/48",
	"2606:4700:3013::/48",
	"2606:4700:3014::/48",
	"2606:4700:3015::/48",
	"2606:4700:3016::/48",
	"2606:4700:3017::/48",
	"2606:4700:3018::/48",
	"2606:4700:3019::/48",
	"2606:4700:3020::/48",
	"2606:4700:3021::/48",
	"2606:4700:3022::/48",
	"2606:4700:3023::/48",
	"2606:4700:3024::/48",
	"2606:4700:3025::/48",
	"2606:4700:3026::/48",
	"2606:4700:3027::/48",
	"2606:4700:3028::/48",
	"2606:4700:3029::/48",
	"2606:4700:3030::/48",
	"2606:4700:3031::/48",
	"2606:4700:3032::/48",
	"2606:4700:3033::/48",
	"2606:4700:3034::/48",
	"2606:4700:3035::/48",
	"2606:4700:3036::/48",
	"2606:4700:3037::/48",
	"2606:4700:3038::/48",
	"2606:4700:3039::/48",
	"2606:4700:a::/48",
	"2606:4700:a0::/48",
	"2606:4700:a1::/48",
	"2606:4700:a8::/48",
	"2606:4700:a9::/48",
	"2606:4700:b::/48",
	"2606:4700:c::/48",
	"2606:4700:d::/48",
	"2606:4700:d0::/48",
	"2606:4700:d1::/48",
	"2606:4700:e::/48",
	"2606:4700:e0::/48",
	"2606:4700:e1::/48",
	"2606:4700:e2::/48",
	"2606:4700:e3::/48",
	"2606:4700:e4::/48",
	"2606:4700:e5::/48",
	"2606:4700:e6::/48",
	"2606:4700:e7::/48",
	"2606:4700:f::/48",
	"2606:4700:f1::/48",
	"2606:4700:f2::/48",
	"2606:4700:f3::/48",
	"2606:4700:f4::/48",
	"2606:4700:f5::/48",
	"2803:f800:50::/48",
	"2803:f800:51::/48",
	"2a06:98c1:49::/48",
	"2a06:98c1:4a::/48",
	"2a06:98c1:4b::/48",
	"2a06:98c1:4c::/48",
	"2a06:98c1:4d::/48",
	"2a06:98c1:4e::/48",
	"2a06:98c1:50::/48",
	"2a06:98c1:51::/48",
	"2a06:98c1:52::/48",
	"2a06:98c1:53::/48",
	"2a06:98c1:54::/48",
	"2a06:98c1:55::/48",
	"2a06:98c1:56::/48",
	"2a06:98c1:57::/48",
	"2a06:98c1:58::/48",
	"2a06:98c1:59::/48",
	"2a06:98c1:5b::/48",
	"2a06:98c1:5c::/48",
	"2a06:98c1:5d::/48",
	"2a06:98c1:5e::/48",
	"2a06:98c1:3100::/48",
	"2a06:98c1:3101::/48",
	"2a06:98c1:3102::/48",
	"2a06:98c1:3103::/48",
	"2a06:98c1:3104::/48",
	"2a06:98c1:3105::/48",
	"2a06:98c1:3106::/48",
	"2a06:98c1:3107::/48",
	"2a06:98c1:3108::/48",
	"2a06:98c1:3109::/48",
	"2a06:98c1:310a::/48",
	"2a06:98c1:310b::/48",
	"2a06:98c1:310c::/48",
	"2a06:98c1:310d::/48",
	"2a06:98c1:310e::/48",
	"2a06:98c1:310f::/48",
	"2a06:98c1:3120::/48",
	"2a06:98c1:3121::/48",
	"2a06:98c1:3122::/48",
	"2a06:98c1:3123::/48",
	"2a06:98c1:3200::/48",
}

var ipv6ProbeSeedIPs = []string{
	"2606:4700:7::da",
	"2a06:98c1:58::da",
}

// Cloudflare commonly encodes an IPv4 CDN address in the low 32 bits of an
// IPv6 address. Trying representative suffixes from the working IPv4 probe
// pools gives each /48 a better hit rate than pure random sampling alone.
var ipv6ProbeHostSuffixes = []string{
	"::da",
	"::6810:b001", // 104.16.176.1
	"::a29f:8001", // 162.159.128.1
	"::ac42:2801", // 172.66.40.1
	"::ac43:b001", // 172.67.176.1
	"::bc72:6101", // 188.114.97.1
	"::c629:c001", // 198.41.192.1
	"::c629:c801", // 198.41.200.1
	"::1",
}

func ipv6ProbeTargets() ([]string, error) {
	seeds := append([]string(nil), ipv6ProbeSeedIPs...)
	seeds = append(seeds, resolveSpeedIPv6Targets()...)
	return generateIPv6ProbeTargets(ipv6ProbeRanges, seeds, ipv6ProbeCountPerRange)
}

func resolveSpeedIPv6Targets() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, "speed.cloudflare.com")
	if err != nil {
		return nil
	}

	var targets []string
	for _, addr := range addrs {
		if addr.IP != nil && addr.IP.To4() == nil {
			targets = append(targets, addr.IP.String())
		}
	}
	return targets
}

func generateIPv6ProbeTargets(ranges, seeds []string, countPerRange int) ([]string, error) {
	seen := make(map[string]struct{}, len(seeds)+(len(ranges)*countPerRange))
	targets := make([]string, 0, len(seeds)+(len(ranges)*countPerRange))
	add := func(ip net.IP) {
		if ip == nil || ip.To4() != nil {
			return
		}
		value := ip.String()
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		targets = append(targets, value)
	}

	for _, seed := range seeds {
		add(net.ParseIP(seed))
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, cidr := range ranges {
		generated, err := generateIPv6RangeTargets(cidr, countPerRange, rng)
		if err != nil {
			return nil, err
		}
		for _, ip := range generated {
			add(ip)
		}
	}

	return targets, nil
}

func generateIPv6RangeTargets(cidr string, count int, rng *rand.Rand) ([]net.IP, error) {
	base, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid IPv6 probe range %q: %w", cidr, err)
	}
	base = base.To16()
	if base == nil || base.To4() != nil {
		return nil, fmt.Errorf("probe range %q is not IPv6", cidr)
	}
	if count <= 0 {
		return nil, nil
	}

	generated := make([]net.IP, 0, count)
	for _, rawSuffix := range ipv6ProbeHostSuffixes {
		if len(generated) == count {
			return generated, nil
		}
		suffix := net.ParseIP(rawSuffix).To16()
		candidate := append(net.IP(nil), base...)
		for i := range candidate {
			candidate[i] = (candidate[i] & network.Mask[i]) | (suffix[i] &^ network.Mask[i])
		}
		generated = append(generated, candidate)
	}

	random, err := generateRandomIPv6s(cidr, count-len(generated), rng)
	if err != nil {
		return nil, err
	}
	return append(generated, random...), nil
}

func generateRandomIPv6s(cidr string, count int, rng *rand.Rand) ([]net.IP, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid IPv6 probe range %q: %w", cidr, err)
	}
	base := ip.To16()
	if base == nil || ip.To4() != nil {
		return nil, fmt.Errorf("probe range %q is not IPv6", cidr)
	}
	if count <= 0 {
		return nil, nil
	}

	generated := make([]net.IP, 0, count)
	var randomBytes [net.IPv6len]byte
	for range count {
		_, _ = rng.Read(randomBytes[:])

		candidate := make(net.IP, net.IPv6len)
		for j := range candidate {
			candidate[j] = (base[j] & network.Mask[j]) | (randomBytes[j] &^ network.Mask[j])
		}
		generated = append(generated, candidate)
	}
	return generated, nil
}
