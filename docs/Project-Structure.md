# Project Structure

```
meso-miner/
├── *.go                          # Core network stack (root package)
├── go.mod / go.sum               # Module: github.com/urnetwork/connect (Go 1.27)
├── Dockerfile                    # Alpine multi-arch provider image
├── CHANGELOG.md                  # Version history with per-release notes
├── FORK_CHANGES.md               # Delta summary vs upstream urnetwork/connect
├── CODESTYLE.md                  # Go conventions for this codebase
│
├── provider/                     # Provider CLI binary
│   ├── main.go                   # Entrypoint: auth / provide / auth-provide commands
│   ├── proxy_health_log.go       # Per-proxy health state machine and dead-proxy detection
│   ├── proxy_slow_retry.go       # Slow-retry state: 24h daily gate, 14-day drop ceiling, persistence
│   ├── proxy_reload.go           # SIGHUP-triggered hot-reload of proxy list
│   ├── proxy_state.go            # In-memory proxy registry with startup stagger
│   ├── proxy_benchmark.go        # Optional per-proxy SOCKS5 latency probes
│   ├── bandwidth_reporter.go     # Posts bandwidth metrics to configured fleet target
│   ├── proxy_id.go               # Stable proxy identity across reloads
│   ├── claim_credential.go       # `provider claim` credential choice (store client, client jwt file, legacy coldkey)
│   ├── thrash_watchdog.go        # Swap-thrash watchdog, supervised exit-75 restart, container opt-in
│   ├── shmlog_linux.go           # Linux shared-memory log ring buffer
│   ├── shmlog_fallback.go        # Fallback for non-Linux builds
│   ├── direct.go                 # Dynamic direct native connection controller
│   ├── proxy_paste.go            # Bulk proxy ingestion and format normalization
│   ├── ssrf_guard.go             # SSRF guard for proxy source URL fetching
│   ├── doh_cache.go              # Persistent DNS-over-HTTPS resolver cache
│   ├── client_jwt_hotrestart.go  # Identity snapshotting across hot restarts
│   ├── read_fd_frac_unix.go      # Linux/Unix file descriptor pressure reader
│   ├── read_fd_frac_windows.go   # Windows file descriptor pressure stub
│   ├── dup_linux_arm64.go        # ARM64-specific fd dup shim
│   ├── dup_linux_generic.go      # Generic Linux fd dup shim
│   ├── hotswap_windows.go        # Windows HotSwap via named pipes
│   ├── hotswap_unix.go           # Linux HotSwap via Unix sockets
│   ├── control_socket.go         # Control socket server (hotswap, shutdown, status commands)
│   ├── control_state.go          # Persistent provider state management
│   ├── restrict_socket_windows.go # Windows DACL socket permission hardening
│   ├── metrics_listen.go         # Prometheus /metrics HTTP listener
│   ├── metrics_provider.go       # Provider-specific metrics registration
│   ├── contract_metrics.go       # Per-contract Prometheus counters
│   ├── lifetime_metrics.go       # Provider lifetime Prometheus gauges
│   ├── family_transport.go       # Dual-stack group for the direct identity (`ip_version`: dual/ipv4/ipv6/legacy)
│   ├── network_refresh.go        # Network JWT renewal at POST /auth/network-refresh, legacy flow as fallback
│   ├── proxy_udp_check.go        # Per-proxy SOCKS5 UDP relay check and TCP/UDP exit comparison
│   ├── socks5_udp.go             # SOCKS5 UDP relay packet conn so a proxied H3 exits through its proxy
│   └── Makefile                  # Cross-compile targets (amd64, arm64, darwin)
│
├── hub/                          # [REMOVED v31.3+] Fleet bandwidth dashboard server (deprecated)
│
├── protocol/                     # Protobuf definitions and generated Go code
│   ├── *.proto                   # Source definitions (ip, transfer, frame, extender, audit)
│   ├── *.pb.go                   # Generated — do not edit directly
│   └── Makefile                  # Runs protoc to regenerate pb.go files
│
├── extender/                     # Extender transport implementation
│   ├── extender.go               # Client-side extender connection handler
│   └── extender_test.go
│
├── connectctl/                   # connectctl CLI (upstream tool, unmodified)
│
├── api/                          # API client bindings
│   ├── bringyour.yml             # API endpoint definitions
│   └── test.sh
│
├── docker/                       # Docker startup scripts
│   ├── scripts/                  # entrypoint.sh + provider_common.sh/provider_env.sh; start_* wrappers, urnet-tools
│   └── ...                       # Selected by BUILD env var at container start
│
├── pelican/                      # Pelican game-server panel egg
│   ├── egg-urnetwork-323fix.json # PLCN_v3 egg definition (BUILD, USER_AUTH, PASSWORD, AUTHCODE)
│   └── README.md                 # Panel deployment guide, env vars, PELICAN-gated updates
│
├── cmd/                         # Go binaries (v3.23.0-fix.27.0+)
│   ├── urnet-tools/             # Provider-aware fleet ops tool (process/systemd)
│   └── urnet-docker/            # Container variant (docker exec delegation)
│
├── internal/urnettools/         # Shared Go core for the urnet-tools binaries
│   ├── cli.go                   # Dispatch, flag parsing, confirm gates
│   ├── target.go                # Targeting (--unit/--user/--network/--network-id/--state-dir)
│   ├── discover.go              # Provider discovery (/proc + systemd units)
│   ├── update.go                # Interactive-first update, digest verify, atomic swap
│   ├── legacy_cmds.go           # Reporting config, lifecycle, tuning, and proxy commands
│   ├── log_stream.go            # Native log follower: last lines, then follow; rotation and truncation aware
│   ├── log_follow_open_*.go     # Per-OS open for the follower (Windows: FILE_SHARE_DELETE, paths over MAX_PATH)
│   ├── lifecycle_start_windows.go   # Windows provider start (schtasks/detached)
│   ├── lifecycle_stop_windows.go    # Windows provider stop (socket shutdown + TerminateProcess)
│   ├── lifecycle_restart_windows.go # Windows provider restart (HotSwap fallback)
│   ├── lifecycle_windows.go         # Windows lifecycle helpers (schtasks, task naming)
│   ├── lifecycle_stubs_notwindows.go # Stubs for non-Windows builds
│   ├── hotswap_windows.go           # Windows HotSwap candidate launch
│   └── ...                      # + tests (~73)
│
├── scripts/                      # Installer and test scripts (installer stays shell)
│   ├── Provider_Install_Linux.sh # One-command provider install for Linux (installs the Go urnet-tools)
│   ├── Provider_Install_Win32.ps1
│   ├── Provider_Uninstall_Linux.sh
│   ├── Provider_Uninstall_Win32.ps1
│   ├── test_provider_install.sh  # CI: validates installer script logic
│   └── test_fallback_logic.sh    # CI: validates fallback behavior
│
├── docs/                         # Operational documentation
│   ├── Configuration.md
│   ├── Docker-Deployment.md
│   ├── Installation.md
│   ├── Hub-Setup.md              # [deprecated v31.3+]
│   ├── Hub-Dashboard.md          # [deprecated v31.3+]
│   ├── High-Volume-Performance-Tuning.md
│   ├── Multi-Container-Scaling.md
│   ├── Proxy-Management.md
│   ├── Proxy-URL-Sources.md
│   ├── Traffic-Amplification.md
│   ├── Adding-Proxies.md
│   ├── Bittensor-Operations.md
│   ├── Troubleshooting.md
│   └── design/                   # Internal design docs (proxy health, hot-reload, bandwidth)
│
├── monitoring/                   # Prometheus + Grafana observability stack
│   ├── docker-compose.yml        # Prometheus + Grafana stack
│   ├── grafana/                  # Grafana dashboards and provisioning
│   ├── prometheus/               # Prometheus scrape configuration
│   └── setup.sh                  # Monitoring stack setup script
│
├── releases/                     # Per-version release notes (v3.23.0-fix.*)
│
├── .github/workflows/
│   ├── build.yml                 # CI: parallel test-and-lint + build-and-push Docker (multi-arch)
│   ├── dash-compat.yml           # Dash/POSIX compatibility check
│   ├── release.yml               # Tags a new release, scans (VirusTotal + ClamAV), uploads provider binaries
│   ├── shakedown.yml             # Pre-release shakedown: fresh-droplet install + proxy + URL + docker test on v3.23.0-fix.* tags
│   ├── shakedown-sweeper.yml     # Every 15 min: destroy orphaned shakedown-ci droplets >3h, reap stale SSH keys
│   ├── codeql.yml                # Weekly scheduled CodeQL security scan
│   └── upstream_monitor.yml      # Twice-daily: watch urnetwork/connect PRs and commits, Discord alerts
│
└── Root package files (core network stack):
    ├── ip.go                     # IP packet relay core (81 KB)
    ├── ip_remote_multi_client.go # Multi-client relay coordinator
    ├── ip_security.go            # Pre-compiled IP blocklist (2.3 MB)
    ├── ip_udp_socket_poller*.go  # Shared UDP socket lifecycle: epoll/kqueue/unsupported backends
    ├── transfer.go               # Contract-based bandwidth allocation
    ├── transfer_contract_manager.go  # Per-proxy contract lifecycle
    ├── transfer_route_manager.go     # Adaptive routing with RTT tracking
    ├── transfer_rtt.go           # RTT window measurement
    ├── transfer_encrypt.go       # Per-contract encryption
    ├── transfer_queue.go         # Backpressure queue
    ├── transfer_memory_budget.go # Composable byte budget shared across queue sequences
    ├── routing_class.go          # Traffic-class vocabulary (latency, streaming, bulk, ...)
    ├── routing_classifier_light.go  # Light flow classifier (inert: no consumer installed)
    ├── transport.go              # Transport abstraction layer
    ├── transport_family.go       # Family-pinned v4/v6 transports and the dual-stack group
    ├── transport_family_stats.go # Per-family counts, drops by cause, session lifetimes
    ├── transport_h1plus.go       # H1+ ready-batch writer over the framed carrier
    ├── http_upgrade.go           # H1 -> framed carrier upgrade (h1_plus), plain WS fallback
    ├── framed_message_conn.go    # Binary-message conn the upgraded carrier is spoken on
    ├── message_framer_xl.go      # urnetwork-framerxl/1 wire format (uint32 header, batched writes)
    ├── transport_sessions.go     # Session lifetime buckets and close cause
    ├── transport_authorization.go  # Authorization close codes -> close reason
    ├── transport_credential_refusal.go  # Dials held after a definitively refused credential
    ├── stream_metrics.go         # Stream/transport counters: kind, lifetime, end reason
    ├── platform_transport_budget.go  # Aggregate transport budget, make-before-break pairing
    ├── transport_p2p_webrtc.go   # WebRTC P2P transport
    ├── ice_resolve_net.go        # Peer-generation-scoped ICE address resolution
    ├── transport_pt.go           # Pluggable tunnel transport
    ├── net_http.go               # HTTP proxy with chunked encoding
    ├── net_http_doh.go           # DNS-over-HTTPS resolver
    ├── net_http_internal_doh.go  # In-process DoH name path for protected domains
    ├── net_dial_udp_candidates.go  # Progressive UDP dial candidates (first usable family wins)
    ├── egress_dial.go            # Egress-bound name resolution for control dials
    ├── egress_darwin.go          # macOS socket interface pinning
    ├── control_family.go         # Control-plane family policy and demotion ledger
    ├── control_family_dial.go    # Family-aware control dial, one cross-family retry
    ├── net_resilient.go          # Platform-aware resilient connections
    ├── message_pool.go           # GC-reducing byte buffer pool
    ├── proxy_health.go           # Proxy health scoring
    ├── tuning.go                 # Auto-tuning (turbo/lowmem modes)
    ├── log.go                    # Structured logging helpers
    ├── metrics_prometheus.go     # Prometheus metrics registry and collector
    └── connect.go                # Top-level client/server entrypoint
```
