#!/bin/bash
# test_auto_update.sh — the container auto-update policy: AUTO_UPDATE picks
# the interval (daily by default), and stable/nightly both run the updater.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
COMMON="$HERE/provider_common.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0
check() { if [ "$2" = "$3" ]; then pass=$((pass+1)); echo "PASS: $1"; else fail=$((fail+1)); echo "FAIL: $1 (want '$2', got '$3')"; fi; }

awk '$0 ~ /^func_auto_update_interval\(\) *\{/ {g=1} g {print} g && /^}/ {exit}' "$COMMON" > "$tmp/fn.sh"
. "$tmp/fn.sh"

check "unset AUTO_UPDATE is daily" "86400" "$(unset AUTO_UPDATE; func_auto_update_interval)"
check "daily" "86400" "$(AUTO_UPDATE=daily func_auto_update_interval)"
check "hourly" "3600" "$(AUTO_UPDATE=hourly func_auto_update_interval)"
check "HOURLY is case-insensitive" "3600" "$(AUTO_UPDATE=HOURLY func_auto_update_interval)"
check "an unknown value falls back to daily" "86400" "$(AUTO_UPDATE=weekly func_auto_update_interval)"
for off in off OFF false 0 no; do
    check "AUTO_UPDATE=$off disables it" "" "$(AUTO_UPDATE=$off func_auto_update_interval)"
done

# stable must load the updater (it used to be nightly-only).
check "stable sources update_verify.sh" "1" "$(grep -c 'if \[ "\$PROVIDER_MODE" = "nightly" \] || \[ "\$PROVIDER_MODE" = "stable" \]; then' "$COMMON")"
# The stable bootstrap checks before the first start and starts the watcher.
stable_block="$(awk '/^func_bootstrap\(\)/ {g=1} g && /^      stable\)/ {s=1} s {print} s && /;;/ {exit}' "$COMMON")"
check "stable bootstrap runs func_check_update" "1" "$(printf '%s\n' "$stable_block" | grep -c '^ *func_check_update$')"
check "stable bootstrap starts the watcher" "1" "$(printf '%s\n' "$stable_block" | grep -c '^ *func_start_update_watcher$')"

echo
echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
