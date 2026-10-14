package main

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect"
)

// h1PlusProcessStats is the one H1+ collector every identity's platform transport
// reports to. H1+ is the authenticated custom upgrade each H1 dial tries before
// falling back to a plain websocket.
var h1PlusProcessStats = &connect.H1PlusStats{}

// writeH1PlusMetrics exports whether H1+ is switched on and what the dials did.
func writeH1PlusMetrics(b *strings.Builder) {
	stats := h1PlusProcessStats.Snapshot()
	enabled := 0
	if connect.H1PlusAvailable() {
		enabled = 1
	}
	counter := func(name string, help string, value uint64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
	}
	fmt.Fprintf(b, "# HELP urnet_h1plus_enabled 1 while H1+ upgrades are allowed (control key h1_plus, urnet-tools set h1-plus).\n# TYPE urnet_h1plus_enabled gauge\nurnet_h1plus_enabled %d\n", enabled)
	counter("urnet_h1plus_attempts_total", "H1+ upgrade attempts.", stats.Attempts)
	counter("urnet_h1plus_accepted_total", "H1+ upgrades the server accepted: connections that run on the custom carrier.", stats.Accepted)
	counter("urnet_h1plus_websocket_selected_total", "H1 dials that ended on a plain websocket (a fallback, or H1+ off).", stats.WebSocketSelected)
	fmt.Fprintf(b, "# HELP urnet_h1plus_fallbacks_total H1+ upgrades that fell back to a websocket, by reason.\n# TYPE urnet_h1plus_fallbacks_total counter\n")
	for reason, value := range map[string]uint64{"rejected": stats.FallbackRejected, "invalid": stats.FallbackInvalid, "io": stats.FallbackIO, "proxy": stats.FallbackProxy} {
		fmt.Fprintf(b, "urnet_h1plus_fallbacks_total{reason=%s} %d\n", connect.PrometheusLabelValue(reason), value)
	}
	counter("urnet_h1plus_handshake_failures_total", "H1+ upgrades that failed without falling back (TLS, authentication).", stats.HandshakeFailures)
	counter("urnet_h1plus_auth_failures_total", "H1+ upgrades answered 401 or 403.", stats.AuthFailures)
	counter("urnet_h1plus_handshake_seconds_total", "Time spent in H1+ handshakes, seconds.", stats.HandshakeNanos/1_000_000_000)
	counter("urnet_h1plus_messages_total", "Messages carried on H1+ connections.", stats.Messages)
	counter("urnet_h1plus_bytes_total", "Message bytes carried on H1+ connections.", stats.Bytes)
	counter("urnet_h1plus_writes_total", "Physical writes on H1+ connections.", stats.Writes)
	counter("urnet_h1plus_flushes_total", "Batched flushes on H1+ connections.", stats.Flushes)
	counter("urnet_h1plus_read_errors_total", "Read errors on H1+ connections.", stats.ReadErrors)
	counter("urnet_h1plus_write_errors_total", "Write errors on H1+ connections.", stats.WriteErrors)
}
