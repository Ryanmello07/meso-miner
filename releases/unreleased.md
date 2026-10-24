# Next release: draft (urfoundation meso-miner)

**Status:** draft. Nothing is tagged or pushed. Base tag `v2026.10.22-1078667370-meso`, the latest release here. Head: `main` at `132d0bdcd`, plus the H3 outage-egress commit `23a7741c4`, which landed directly on `main` with no PR. No PRs are open. The next tag is not chosen.
**Note:** this repository has no release workflow. The release is cut by hand, so rename this file to `releases/<tag>.md` at the cut and attach it to the release.
**Sourcing:** condensed from the merge commits and PR bodies in this window. Not re-verified line by line.

> [!NOTE]
> The Observability and Documentation sections below describe PRs 13 to 15, which landed before the `v2026.10.22-1078667370-meso` tag and have shipped. They stay only until the cut rewrites this draft. The sections added for this window are Transport and Merged since the last release. The per-PR prose for the 27 PRs below is not written yet: the list gives each title as merged, and the cut needs it grouped and described, with the extender stack (#39 to #43), the lifetime and trim counters (#33, #35, #36, #38) and the installer and CI hardening (#44, #46, #48, #50) the largest groups.

Legend: **[redeploy]** needs the built artifact redeployed. **[installer]** needs the installer or unit refresh. **[no redeploy]** docs, CI or tests only.

## Transport

- **A silent platform edge no longer drains egress through proxied H3** **[redeploy]** (commit `23a7741c4`). With `h3` set to `all`, a count or `auto`, every proxy identity runs the H3, H3-over-dns and dns-pump carriers beside its H1. When the edge answers nothing, each carrier redials on its own 30 to 60 second timer and every dial runs the whole QUIC retransmit ladder, about 33 KB. One node on `h3 all` spent a flat 15.5 to 16.4 Mbit/s for about 15 hours of the 2026-10-23 outage, roughly 30 times more tx than rx, while the governor read `verdict=ok rate=200.0/s`. Three things hid it, and each is fixed: QUIC bytes were never charged to the connect attempt, so the failed-byte breaker saw zero; a canceled dial (two of every three, because the per-transport H3 gate cancels a lower-priority carrier when a better one waits) was dropped with its bytes; and H3 dials never waited on the attempt budget. The handshake's wire bytes are now charged once, a canceled attempt's bytes count toward the breaker without counting as an attempt, and a proxied identity's H3 waits on the shared budget like its H1. The direct identity, proxy index 0 and family-pinned transports never wait.

## Observability

- **PQE handshake stages and negotiated curves** **[redeploy]** (<https://github.com/urfoundation/meso-miner/pull/13>). The `[pqe]` line reports attempts, tls_ok, verified and identity_failed, plus a per-curve histogram of the group each handshake negotiated. `/metrics` carries the same data as `urnet_sessions_handshake_total{stage=...}` and `urnet_sessions_curve_total{curve=...}`, and two dashboard panels show both rates. A zero classical count is now explainable instead of ambiguous: the histogram names the curve a peer actually chose.
- **Lifetime counters survive a proxy exit** **[redeploy]** (<https://github.com/urfoundation/meso-miner/pull/13>, <https://github.com/urfoundation/meso-miner/pull/14>). A manager was dropped when its proxy exited, so a `_total` could decrease and `rate()` read it as a reset. The counts fold into a provider-wide accumulator first, and live counts are summed under the registry lock so an exit cannot be counted twice.

## Documentation

- **Four Bittensor claims are corrected** **[no redeploy]** (<https://github.com/urfoundation/meso-miner/pull/15>). v1 and v2 claim triggers are described separately. The signed challenge quotes the message, because an unquoted copy breaks verification. The stopped-daemon answer says unsigned v1 claims still expire. The consent is scoped to one URnetwork account.

## Merged since the last release

Titles as merged, oldest first. Docs, release-note and test-only PRs are included so the list is complete.

- [#21](https://github.com/urfoundation/meso-miner/pull/21) docs(bittensor): link Current blockers and say dollar amounts are estimates
- [#29](https://github.com/urfoundation/meso-miner/pull/29) docs(release): finalize the release notes
- [#30](https://github.com/urfoundation/meso-miner/pull/30) fix(docs): act on the review of the live-numbers refresh
- [#31](https://github.com/urfoundation/meso-miner/pull/31) docs(release): name the previous release and the stop test
- [#32](https://github.com/urfoundation/meso-miner/pull/32) docs(release): cite the shakedown stop check
- [#33](https://github.com/urfoundation/meso-miner/pull/33) feat(urnet-tools): report the proxy trim cap and show it in status
- [#34](https://github.com/urfoundation/meso-miner/pull/34) docs: label the docker wrapper as optional but recommended
- [#35](https://github.com/urfoundation/meso-miner/pull/35) fix(provider): report trim state only from a complete pool
- [#36](https://github.com/urfoundation/meso-miner/pull/36) fix(logs): date the lifetime counters so all-time means something
- [#37](https://github.com/urfoundation/meso-miner/pull/37) fix(provider): bound the url-sourced warmup hold to five minutes
- [#38](https://github.com/urfoundation/meso-miner/pull/38) fix(logs): stop the lifetime start rolling forward on idle restarts
- [#39](https://github.com/urfoundation/meso-miner/pull/39) Port the v2026 extender stack, blocker wiring, fingerprint, leaves and small subsystems
- [#40](https://github.com/urfoundation/meso-miner/pull/40) fix: act on the remaining extender review findings
- [#41](https://github.com/urfoundation/meso-miner/pull/41) fix: act on the remaining extender review findings (round 2)
- [#42](https://github.com/urfoundation/meso-miner/pull/42) fix(extender): wait for a started dial cancel callback
- [#43](https://github.com/urfoundation/meso-miner/pull/43) test(extender): bound the relay read-count assertion
- [#44](https://github.com/urfoundation/meso-miner/pull/44) PSI reader, symlinked install path, unusable binary, tag parsing
- [#45](https://github.com/urfoundation/meso-miner/pull/45) fix(hotkeywallet): finish a wallet write without a directory flush
- [#46](https://github.com/urfoundation/meso-miner/pull/46) Refuse a symlinked shell rc when adding PATH (root write)
- [#47](https://github.com/urfoundation/meso-miner/pull/47) Poll for the reload trailing edge instead of a fixed sleep
- [#48](https://github.com/urfoundation/meso-miner/pull/48) Never write the PATH hint through a symlinked shell rc (supersedes #46)
- [#49](https://github.com/urfoundation/meso-miner/pull/49) Do not read a thrash restart as a rejected sign-in
- [#50](https://github.com/urfoundation/meso-miner/pull/50) ci: run the installer shellrc symlink test, and fail if its pin is missing
- [#51](https://github.com/urfoundation/meso-miner/pull/51) perf(provider): size the disk sample by time instead of 16 MiB
- [#52](https://github.com/urfoundation/meso-miner/pull/52) fix(provider): report an oom cap state write that fails
- [#53](https://github.com/urfoundation/meso-miner/pull/53) fix(provider): tie the disk bench cache to its sampling shape
- [#54](https://github.com/urfoundation/meso-miner/pull/54) fix(provider): name what a failed cap write actually lost
- `23a7741c4` fix(governor): price QUIC dials in bytes and admit proxied H3 under the attempt budget (direct to `main`)

## Deploy notes

- **The provider needs a redeploy for the counters.** An old process sends none of the new series.
- **The dashboard panels stay empty until a node runs this build.**
- **Only nodes with `h3` set to `all`, a count or `auto` change for the H3 fix.** In an outage they log `[govern] budget cut ... reason=bytes`, which was silent before. `urnet-tools set h3` shows the setting.
- **No installer change is required.** Unit files and the install flow are unchanged in this window.

## Caveats

- The H3 fix has not been run against a live outage. The attempt budget floor is 2 per second, so a throttled node still spends about 0.5 Mbit/s, not zero.
- The curve histogram covers sessions that terminate on this node. Forwarded traffic never opens one here.
- The counters are lifetime totals. A provider restart starts them at zero again, so compare rates, not absolute values across a restart.

**Full Changelog**: filled at the cut, from the base tag to the new tag.
