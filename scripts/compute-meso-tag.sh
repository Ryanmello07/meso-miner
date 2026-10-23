#!/bin/bash
# compute-meso-tag.sh — prints the next meso-miner release tag.
#
# Tag shape: <mainnet tag>-<YYYY.MM.DD>-meso-miner-v<NNN>
#   e.g.     v2026.10.22-1078873780-2026.10.23-meso-miner-v001
#
#   <mainnet tag>  the newest mainnet release tag in urnetwork/build
#                  (vYYYY.M.D-<code>), so a meso node reports the mainnet
#                  generation it tracks. Override with MAINNET_TAG.
#   <YYYY.MM.DD>   the UTC date the meso release is cut.
#   v<NNN>         the meso-miner release counter: one more than the highest
#                  existing *-meso-miner-v<NNN> tag in this repo. It never
#                  resets. Override with MESO_COUNTER.
#
# The whole tag is the provider's app version (main.Version), which the
# platform caps at 64 characters; this shape is 49.
#
# Usage:
#   scripts/compute-meso-tag.sh            # print the next tag
#   scripts/compute-meso-tag.sh --verify <tag>
#                                           # check a tag's shape and decode
#                                           # its mainnet code to its UTC mint
#                                           # time before pushing
set -euo pipefail

BUILD_REPO="${BUILD_REPO:-https://github.com/urnetwork/build}"
FOUNDING_EPOCH=1684800000 # 2023-05-23T00:00:00Z; mainnet codes are
                          # (seconds since founding) * 10

TAG_RE='^(v[0-9]{4}\.[0-9]+\.[0-9]+-[0-9]+)-([0-9]{4}\.[0-9]{2}\.[0-9]{2})-meso-miner-v([0-9]{3,})$'

latest_mainnet_tag() {
	local tags plain
	tags=$(git ls-remote --tags --refs "$BUILD_REPO" | awk '{print $2}' | sed 's#^refs/tags/##')
	plain=$(echo "$tags" | grep -E '^v[0-9]{4}\.[0-9]+\.[0-9]+-[0-9]+$' | sort -V)
	# A mainnet release publishes several plain codes (one per build target)
	# plus -fdroid/-ungoogle variants of its primary code. Prefer the newest
	# code that has a variant sibling: that is the release's primary version.
	local tag
	for tag in $(echo "$plain" | sort -rV); do
		if echo "$tags" | grep -qxE "${tag//./\\.}-(fdroid|ungoogle)"; then
			echo "$tag"
			return
		fi
	done
	echo "$plain" | tail -1
}

next_counter() {
	local max
	max=$(git tag -l '*-meso-miner-v*' | sed -nE 's/.*-meso-miner-v([0-9]+)$/\1/p' | sort -n | tail -1)
	printf '%03d' $((10#${max:-0} + 1))
}

compute_tag() {
	local mainnet counter
	mainnet="${MAINNET_TAG:-$(latest_mainnet_tag)}"
	if [ -z "$mainnet" ]; then
		echo "error: could not read a mainnet tag from $BUILD_REPO" >&2
		exit 1
	fi
	counter="${MESO_COUNTER:-$(next_counter)}"
	echo "${mainnet}-$(date -u +%Y.%m.%d)-meso-miner-v${counter}"
}

verify_tag() {
	local tag="$1" mainnet code
	if ! [[ "$tag" =~ $TAG_RE ]]; then
		echo "error: '$tag' does not match <mainnet tag>-<YYYY.MM.DD>-meso-miner-v<NNN>" >&2
		exit 1
	fi
	if [ "${#tag}" -gt 64 ]; then
		echo "error: '$tag' is ${#tag} characters; the app version limit is 64" >&2
		exit 1
	fi
	mainnet="${BASH_REMATCH[1]}"
	code="${mainnet##*-}"
	echo "tag:          $tag"
	echo "mainnet:      $mainnet (minted $(date -u -d "@$((FOUNDING_EPOCH + code / 10))" +"%Y-%m-%dT%H:%M:%SZ"))"
	echo "meso date:    ${BASH_REMATCH[2]}"
	echo "meso counter: v${BASH_REMATCH[3]}"
}

if [ "${1:-}" = "--verify" ]; then
	if [ -z "${2:-}" ]; then
		echo "usage: $0 --verify <tag>" >&2
		exit 1
	fi
	verify_tag "$2"
else
	compute_tag
fi
