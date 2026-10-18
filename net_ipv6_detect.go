package connect

import (
	"net"
	"sync"
	"time"
)

// hostHasGlobalIpv6 reports whether this host can really reach the internet over
// IPv6. It is a variable so tests can pin it. A route and a global source address
// are not enough (a VPS can have both and a filtered upstream), so the check
// sends one tiny DNS query to a public resolver and needs an answer. The result
// is cached, so DefaultDohSettings and dnsResolverAddrs can call it freely.
var hostHasGlobalIpv6 = cachedGlobalIpv6

const ipv6DetectTtl = 10 * time.Minute

var ipv6Detect struct {
	mu sync.Mutex
	// at is when has was measured and checked whether a measurement exists
	at      time.Time
	has     bool
	checked bool
	// probe is non-nil while a probe is in flight, so exactly one runs at a time
	probe chan struct{}
}

// cachedGlobalIpv6 returns whether this host can really reach the internet over
// IPv6. The first caller probes and the rest wait for its answer, so the value is
// never guessed from a zero. Past ipv6DetectTtl callers get the cached answer and
// one background probe replaces it: the probe can take two seconds and this is
// called from the name resolution path, so it must never hold the lock across it
// or make a resolution wait for it.
func cachedGlobalIpv6() bool {
	for {
		ipv6Detect.mu.Lock()
		switch {
		case !ipv6Detect.checked:
			// the first answer must be real: one prober, the rest wait for it
			if probe := ipv6Detect.probe; probe != nil {
				ipv6Detect.mu.Unlock()
				<-probe
				continue
			}
			probe := make(chan struct{})
			ipv6Detect.probe = probe
			ipv6Detect.mu.Unlock()
			has := detectGlobalIpv6()
			ipv6Detect.mu.Lock()
			storeGlobalIpv6Locked(has)
			ipv6Detect.probe = nil
			ipv6Detect.mu.Unlock()
			close(probe)
			return has
		case time.Since(ipv6Detect.at) < ipv6DetectTtl:
			has := ipv6Detect.has
			ipv6Detect.mu.Unlock()
			return has
		case ipv6Detect.probe == nil:
			// stale: answer from the cache now, refresh behind the caller
			ipv6Detect.probe = make(chan struct{})
			has := ipv6Detect.has
			ipv6Detect.mu.Unlock()
			go refreshGlobalIpv6()
			return has
		default:
			// a refresh is already in flight: keep answering from the cache
			has := ipv6Detect.has
			ipv6Detect.mu.Unlock()
			return has
		}
	}
}

// storeGlobalIpv6Locked records a probe result. Caller holds ipv6Detect.mu.
func storeGlobalIpv6Locked(has bool) {
	ipv6Detect.has = has
	ipv6Detect.at = time.Now()
	ipv6Detect.checked = true
}

func refreshGlobalIpv6() {
	has := detectGlobalIpv6()
	ipv6Detect.mu.Lock()
	storeGlobalIpv6Locked(has)
	probe := ipv6Detect.probe
	ipv6Detect.probe = nil
	ipv6Detect.mu.Unlock()
	if probe != nil {
		close(probe)
	}
}

// dnsProbeQuery is a minimal query for the root NS records.
var dnsProbeQuery = []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01}

func detectGlobalIpv6() bool {
	c, err := net.DialTimeout("udp6", "[2606:4700:4700::1111]:53", 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || la.IP == nil || la.IP.To4() != nil || !la.IP.IsGlobalUnicast() || la.IP.IsPrivate() {
		return false
	}
	// a Tailscale or other ULA/private address does not count, nor does a
	// link-local one; and a global one only counts if an answer comes back
	c.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := c.Write(dnsProbeQuery); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	return err == nil && 2 <= n && buf[0] == 0x12 && buf[1] == 0x34
}

// defaultDohIpVersion returns 0 (both families) when the host has working global
// IPv6 and 4 otherwise, so an IPv4-only host behaves exactly as it always did
// while a dual-stack host can still resolve when its IPv4 path to the resolvers
// fails.
func defaultDohIpVersion() int {
	if hostHasGlobalIpv6() {
		return 0
	}
	return 4
}
