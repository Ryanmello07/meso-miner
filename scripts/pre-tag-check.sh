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
echo "all pre-tag checks passed"
