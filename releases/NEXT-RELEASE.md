# NEXT RELEASE

> [!NOTE]
> This is a patch on `v2026.10.20-1076956260-meso`. The provider's cumulative metrics counters now survive a restart, the HotSwap parent flushes them before it exits, the build moves to Go 1.27.3 for the compromised-certificate block, the VirusTotal scan covers every release artifact, the docs are rewritten for meso-miner, and the commands and settings for the retired fleet hub are removed. There is nothing to configure and no operator action beyond the normal update and restart. Provider traffic behavior is unchanged.

## The metrics counters survive a restart

The cumulative counters behind `/metrics` (`urnet_*_total`) used to start at zero every time the provider started, so a 24-hour attribution window silently undercounted after every restart, reboot or update. They are now stored in `~/.urnetwork/metrics_counters.json` and added back before the metrics listener serves, so a restart shows up as a step in `urnet_process_start_time_seconds`, not as a drop to zero. The file is a separate store from `lifetime_metrics.json`, which keeps its delta-based all-time accounting. Writes are atomic (temp, fsync, rename, directory fsync), happen every 45 seconds and on shutdown, and are skipped entirely when nothing changed, so an idle node does not rewrite the file. A HotSwap candidate restores the stored totals but does not write while the parent serves; the parent flushes its final counters at the end of its drain, and the candidate catches up on takeover and again after the drain window. The bundled Grafana dashboard gates the rate panels for one window after a node's start time changes, so a restored total does not read as a spike.

## Go 1.27.3 and CVE-2026-111943

Go 1.27.3 and 1.26.10 block eight compromised eMudhra intermediate CA keys at certificate verification (`CVE-2026-111943`). `go.mod`, the Dockerfile builder and every `setup-go` now pin Go 1.27.3, so binaries built from this release carry the denylist. A binary built before it may continue trusting the intermediates, although an updated operating-system trust store may already reject them. The installed fleet only receives the block when it runs a binary rebuilt from this release, so the update is the delivery mechanism, not the version bump by itself.

## The release scan covers every artifact

The VirusTotal scan now covers every artifact a release publishes, not a subset, gives a duplicate artifact the verdict of the source it duplicates instead of failing the run, honors an HTTP-date `Retry-After` header with a capped wait, and its own tests run in the lint job so a broken scanner fails before publication.

## Installers

The Linux installer now installs missing download and JSON dependencies, uses `yum` when `dnf` is absent, keeps the root prompt off a non-interactive run, and honors the autoinstall opt-out. A CI job runs the real installer on four distributions, and the release gate refuses to publish when the installer tier fails.

## Report commands removed

The fleet hub that received bandwidth reports and heartbeats is gone, so `urnet-tools report`, `urnet-docker report`, the `report-url` and `report-interval` settings, and `URNETWORK_REPORT_URL`, `URNETWORK_REPORT_INTERVAL` and `URNETWORK_HEARTBEAT_INTERVAL` are removed, along with the reporters that sent the data. A script or cron job that still calls them now fails or has the variable ignored; remove the call. A `report_url` already saved in `provider_state.json` or `pending_overrides.json` is ignored, so nodes that had one still start. Use `urnet-tools metrics on` and the monitoring bundle for fleet visibility.

## Docs

The README, project structure and AI guide are rewritten for meso-miner, with a plain start-here, a Bittensor quick start, the head tier and a glossary. The Hub and report-URL pages are gone. The changelog backfills the missing `2026.9.9` and `2026.8.28` entries.
