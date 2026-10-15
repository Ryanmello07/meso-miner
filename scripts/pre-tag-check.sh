#!/usr/bin/env bash
# Run before pushing a release tag. A tag push starts build.yml, which runs
# gofmt and the full -short -race suite in shards; a failure there leaves a
# published pre-release without its Docker image. This runs the same checks
# locally, in the same shape, and stops at the first failure.
#
#   scripts/pre-tag-check.sh            # everything
#   scripts/pre-tag-check.sh --quick    # gofmt, vet and build only (no tests)
set -euo pipefail
cd "$(dirname "$0")/.."

# Some tests bind a unix socket under t.TempDir(), whose path embeds the test
# name; a long TMPDIR pushes it past the 108 byte sun_path limit ("bind: invalid
# argument"). A hosted runner uses /tmp, so use /tmp here too, whatever the
# session set.
export TMPDIR=/tmp

quick=0
[ "${1:-}" = "--quick" ] && quick=1

echo "== gofmt (build.yml lint job)"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "Files not gofmt-clean:"
  echo "$unformatted"
  exit 1
fi

echo "== go vet, every release target"
for target in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 freebsd/amd64 freebsd/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} go vet ./... >/dev/null
  echo "  ok $target"
done

echo "== shard partition covers every package"
./.github/scripts/test_shard.sh --verify

if [ "$quick" = 1 ]; then
  echo "quick checks passed (tests not run)"
  exit 0
fi

echo "== go test -short -race, one shard at a time (build.yml test job)"
for shard in 0 1 2; do
  pkgs=$(./.github/scripts/test_shard.sh "$shard")
  echo "-- shard $shard"
  # shellcheck disable=SC2086
  go test -short -race -timeout 600s $pkgs
done
# A GitHub runner differs from a workstation: unprivileged ICMP sockets are
# refused (net.ipv4.ping_group_range closed) and there is no global IPv6. A test
# that passes here can fail there. Run the root package once more in a network
# namespace that refuses ICMP sockets, with the race detector, like the runner.
echo "== root package in a network namespace that refuses ICMP sockets (-short -race)"
if command -v unshare >/dev/null 2>&1 && unshare -Urn true 2>/dev/null; then
  testbin=$(mktemp /tmp/connect-ci-like.XXXXXX)
  go test -c -race -o "$testbin" .
  # Only the ICMP and packet-parsing tests run here: the namespace has a
  # loopback and nothing else, which is what exposes ICMP state leaks but also
  # breaks tests that need a real network (the family and H3 tests).
  unshare -Urn sh -c "ip link set lo up 2>/dev/null; \"$testbin\" -test.short -test.timeout=600s -test.run \"Icmp|ParseIpPath|Ipv6|Smtp|Fragment|Packet|Flow|Dial|Parse\""
  rm -f "$testbin"
else
  echo "  unshare -Urn is not available here: skipping the ICMP-refused run (run it where it is)"
fi
echo "all pre-tag checks passed"
