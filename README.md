# speedflare - Cloudflare Speed Test CLI

[![Go Version](https://img.shields.io/github/go-mod/go-version/mydearniko/speedflare)](https://go.dev/)
[![Release](https://img.shields.io/github/v/release/mydearniko/speedflare)](https://github.com/mydearniko/speedflare/releases)

**speedflare is a terminal-based Cloudflare speed test CLI for measuring latency, jitter, download speed, and upload speed through `speed.cloudflare.com`.**

Use it as a lightweight alternative to a browser speed test when you want scriptable JSON output, IPv4/IPv6 control, or a quick diagnosis of Cloudflare PoP routing and datacenter selection from the command line.

<img src=".github/assets/speedflare.gif" alt="speedflare Cloudflare speed test CLI demonstration" />

## Installation

### Prebuilt Binaries

Download the latest binary for your operating system and CPU architecture from the [GitHub Releases page](https://github.com/mydearniko/speedflare/releases/latest).

Linux and macOS users can install the matching release asset with:

```bash
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/arm.*/arm/;s/i.86/i386/')
curl -Lo speedflare "https://github.com/mydearniko/speedflare/releases/latest/download/speedflare_${OS}_${ARCH}"
chmod +x speedflare
sudo mv speedflare /usr/local/bin/
```

### Go Install

```bash
go install github.com/mydearniko/speedflare@latest
```

### Build from Source

```bash
git clone https://github.com/mydearniko/speedflare.git
cd speedflare
go build -o speedflare ./cmd/speedflare/main.go
```

Go 1.24 or newer is recommended for source builds.

## Quick Start

```bash
# Run a Cloudflare speed test from the terminal
speedflare

# Output speed test results as JSON
speedflare --json

# Force IPv4 or IPv6
speedflare --ipv4
speedflare --ipv6
```

## Why speedflare?

Most speed test tools answer one question: "How fast is this connection?" speedflare also helps answer "Which Cloudflare datacenter am I reaching, and is my ISP routing me to a strange PoP?"

That makes it useful for:

- Cloudflare speed tests from Linux, macOS, Windows, BSD, servers, VPS instances, and CI jobs.
- Terminal diagnostics for `speed.cloudflare.com` without opening a browser.
- Checking latency, jitter, download, and upload performance against Cloudflare's edge network.
- Finding fragmented or unexpected Cloudflare routing, such as being sent to a distant PoP.
- Comparing IPv4 and IPv6 performance with explicit `--ipv4` and `--ipv6` modes.
- Producing machine-readable JSON for monitoring, dashboards, and shell scripts.

## Features

- **Cloudflare speed test CLI**: Measures network performance using Cloudflare's global edge network.
- **PoP and routing diagnostics**: Detects your current Cloudflare Point of Presence and can probe alternate IPv4 or IPv6 datacenters.
- **Interactive datacenter selection**: Offers nearby or country-preferred Cloudflare colos when multiple options are reachable.
- **Latency and jitter metrics**: Reports average, jitter, minimum, and maximum latency.
- **Download and upload throughput**: Supports multi-worker and single-connection tests.
- **IPv4 and IPv6 control**: Force IPv4-only or IPv6-only tests from the terminal.
- **Source interface control**: Test from a specific network interface or source IP with `--interface`.
- **Proxy support**: Route speed tests and colo detection through an explicit HTTP, HTTPS, SOCKS5, or SOCKS5H proxy with `--proxy`.
- **JSON output**: Use `--json` for automation and monitoring integrations.
- **Targeted test modes**: Run download-only, upload-only, latency-only, simultaneous bidirectional, or continuous throughput tests.
- **Privacy option**: Hide your IP address in terminal output with `--hide-ip`.

## How It Works

1. **Trace Cloudflare routing**: speedflare connects to Cloudflare to identify your current PoP/colo.
2. **Estimate location**: It can query the dual-stack `ipwho.is` API to estimate your IPv4 or IPv6 location for practical distance calculations.
3. **Probe alternate colos**: It checks bounded IPv4 targets or active IPv6 CDN ingress ranges to discover reachable datacenters for the connection's address family.
4. **Select a datacenter**: If multiple Cloudflare colos are found, speedflare can show an interactive selection sorted by distance and country preference.
5. **Measure performance**: It runs latency tests followed by parallel download and upload streams.

### IPv6 Colo Discovery

Cloudflare's published IPv6 allocations are too large for useful random sampling. In IPv6 mode, speedflare instead combines the current `speed.cloudflare.com` AAAA records with more than 100 narrow `/48` CDN ingress ranges derived from active Cloudflare address layouts and the [CloudflareSpeedTest IPv6 hitlist](https://github.com/XIU2/CloudflareSpeedTest/blob/master/ipv6.txt). Each candidate is verified over HTTPS using `speed.cloudflare.com` SNI and is accepted only when `/cdn-cgi/trace` returns a valid `colo` value.

## Examples

```bash
# Compare single-connection performance
speedflare --single

# Use a specific network interface or source IP
speedflare --interface eth0
speedflare --interface 192.0.2.10

# Use a specific Cloudflare origin IP
speedflare --origin-ip 104.16.177.1

# Route the test through a proxy
speedflare --proxy http://127.0.0.1:8080
speedflare --proxy socks5h://127.0.0.1:1080
speedflare --ipv6 --proxy socks5h://127.0.0.1:1080

# Run only one part of the test
speedflare --latency-only
speedflare --download-only
speedflare --upload-only

# Measure upload and download at the same time
speedflare --bidirectional

# Continuous throughput test until interrupted
speedflare --download-only --continuous
speedflare --upload-only --continuous

# List Cloudflare server locations
speedflare --list
```

## JSON Output

Use `--json` when you want to collect Cloudflare speed test results from a script, cron job, monitoring check, or CI workflow.

```bash
speedflare --json
```

Example output:

```json
{
  "ip": "203.0.113.10",
  "server": {
    "iata": "AMS",
    "city": "Amsterdam",
    "country": "NL",
    "lat": 52.3105,
    "lon": 4.7683,
    "ip": "104.16.177.1",
    "distance": 650.4
  },
  "latency": {
    "value_ms": 18.42,
    "jitter_ms": 1.31,
    "min_ms": 16.98,
    "max_ms": 21.77
  },
  "download": {
    "mbps": 412.8,
    "data_mb": 523.6
  },
  "upload": {
    "mbps": 95.4,
    "data_mb": 121.2
  }
}
```

## Command-Line Options

```text
  -j, --json              Output results in JSON format.
      --list              List all Cloudflare server locations.
  -4, --ipv4              Use IPv4 only connection.
  -6, --ipv6              Use IPv6 only connection.
  -I, --interface         Network interface or source IP address to use.
  -O, --origin-ip         Override speed.cloudflare.com with a specific origin IP address.
      --proxy             Proxy URL for HTTP(S) requests (http, https, socks5, or socks5h).
  -l, --latency-attempts  Number of latency attempts (default: 10).
  -s, --single            Use a single connection instead of multiple.
  -U, --upload-only       Run only the upload test.
  -D, --download-only     Run only the download test.
  -L, --latency-only      Run only the latency test.
  -B, --bidirectional     Run upload and download tests at the same time.
  -C, --continuous        Run upload-only or download-only continuously until interrupted.
  -w, --workers           Number of workers for multithreaded speed tests (default: 6).
      --198               Use only 198.41.192.0/21 and 198.41.200.0/21 for IPv4 datacenter probing.
      --insecure          Skip TLS certificate verification (UNSAFE).
      --hide-ip           Hide the IP address in output.
```

## Privacy and External Services

- **Speed test traffic**: By default, generated directly between your machine and `speed.cloudflare.com`.
- **Proxy traffic**: When `--proxy` or standard proxy environment variables apply to `speed.cloudflare.com`, speed tests and colo probes are routed through that proxy. Colo detection asks the proxy to connect to candidate Cloudflare IP addresses while preserving `speed.cloudflare.com` as TLS SNI and HTTP Host.
- **Cloudflare trace and location metadata**: Used to identify the Cloudflare PoP/colo serving your connection.
- **Geolocation lookup**: speedflare may make a GET request to `https://ipwho.is` to estimate latitude and longitude for datacenter distance calculations. The tested public IPv4 or IPv6 address is included in the request path.
- **Local storage**: speedflare does not store personal data.

## Related Search Terms

People looking for this tool may search for Cloudflare speed test CLI, speed.cloudflare.com CLI, Cloudflare speed test terminal, Cloudflare PoP routing diagnostic, Cloudflare colo latency test, or command-line internet speed test with JSON output.
