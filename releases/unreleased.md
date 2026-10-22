# Next release: draft (urfoundation meso-miner)

**Status:** draft. Nothing is tagged or pushed. Base tag `v2026.10.21-1077734740-meso`. Head: `main` after PR 15.
**Note:** this repository has no release workflow. The release is cut by hand, so rename this file to `releases/<tag>.md` at the cut and attach it to the release.
**Sourcing:** condensed from the merge commits and PR bodies in this window. Not re-verified line by line.

Legend: **[redeploy]** needs the built artifact redeployed. **[installer]** needs the installer or unit refresh. **[no redeploy]** docs, CI or tests only.

## Observability

- **PQE handshake stages and negotiated curves** **[redeploy]** (<https://github.com/urfoundation/meso-miner/pull/13>). The `[pqe]` line reports attempts, tls_ok, verified and identity_failed, plus a per-curve histogram of the group each handshake negotiated. `/metrics` carries the same data as `urnet_sessions_handshake_total{stage=...}` and `urnet_sessions_curve_total{curve=...}`, and two dashboard panels show both rates. A zero classical count is now explainable instead of ambiguous: the histogram names the curve a peer actually chose.
- **Lifetime counters survive a proxy exit** **[redeploy]** (<https://github.com/urfoundation/meso-miner/pull/13>, <https://github.com/urfoundation/meso-miner/pull/14>). A manager was dropped when its proxy exited, so a `_total` could decrease and `rate()` read it as a reset. The counts fold into a provider-wide accumulator first, and live counts are summed under the registry lock so an exit cannot be counted twice.

## Documentation

- **Four Bittensor claims are corrected** **[no redeploy]** (<https://github.com/urfoundation/meso-miner/pull/15>). v1 and v2 claim triggers are described separately. The signed challenge quotes the message, because an unquoted copy breaks verification. The stopped-daemon answer says unsigned v1 claims still expire. The consent is scoped to one URnetwork account.

## Deploy notes

- **The provider needs a redeploy for the counters.** An old process sends none of the new series.
- **The dashboard panels stay empty until a node runs this build.**
- **No installer change is required.** Unit files and the install flow are unchanged in this window.

## Caveats

- The curve histogram covers sessions that terminate on this node. Forwarded traffic never opens one here.
- The counters are lifetime totals. A provider restart starts them at zero again, so compare rates, not absolute values across a restart.

**Full Changelog**: filled at the cut, from the base tag to the new tag.
