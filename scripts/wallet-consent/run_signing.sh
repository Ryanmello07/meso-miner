#!/usr/bin/env bash
# One-shot network wallet consent signing with the Talisman extension.
#
# 1. asks the server for a fresh, verified consent statement (signs nothing)
# 2. serves one local page where Talisman signs it (seed phrase never leaves Talisman)
# 3. submits the signature, verifies the server's receipt
#
# usage: run_signing.sh <ss58 address>
set -euo pipefail
TOOL="${TOOL:-provider}"
ADDRESS="$1"
DIR="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

"$TOOL" wallet challenge "$ADDRESS" > "$WORK/challenge.txt" 2>"$WORK/challenge.err" || { cat "$WORK/challenge.err" "$WORK/challenge.txt"; exit 1; }
FROM=$(sed -n 's/.*epochs \([0-9]*\) through \([0-9]*\)\.$/\1/p' "$WORK/challenge.txt" | head -1)
THROUGH=$(sed -n 's/.*epochs \([0-9]*\) through \([0-9]*\)\.$/\2/p' "$WORK/challenge.txt" | head -1)
# the message is the text between the markers, without the trailing newline
awk '/^----- message -----$/{f=1;next}/^----- end -----$/{f=0}f' "$WORK/challenge.txt" | head -c -1 > "$WORK/message.txt"
head -c 80 "$WORK/message.txt"; echo " ..."
echo "epochs $FROM through $THROUGH"

python3 "$DIR/sign_server.py" "$ADDRESS" "$WORK/message.txt" "$WORK/signature.txt" 8765 &
SERVER=$!
for _ in $(seq 1 420); do
  [ -s "$WORK/signature.txt" ] && break
  kill -0 "$SERVER" 2>/dev/null || break
  sleep 1
done
[ -s "$WORK/signature.txt" ] || { echo "no signature received"; exit 1; }
SIGNATURE=$(cat "$WORK/signature.txt")
MESSAGE=$(cat "$WORK/message.txt")
"$TOOL" wallet set "$ADDRESS" --wallet-from-epoch="$FROM" --wallet-through-epoch="$THROUGH" --message="$MESSAGE" --signature="$SIGNATURE"
