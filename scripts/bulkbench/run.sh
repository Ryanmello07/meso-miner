#!/bin/bash
# usage: run.sh <engine-dir> <out.jsonl>
set -u
cd "$1"
export GOTOOLCHAIN=go1.27.3 BULK_OUT="$2"
BULK_N=1,100,500 BULK_SETTLE=10s BULK_IDLE=20s go test -tags bulkbench -run 'TestBulkIdle' -count=1 -timeout 60m . >/dev/null 2>&1
for enc in default off; do
  BULK_ENC=$enc BULK_PAIRS=1,16,64 BULK_MSGS=20000 BULK_SIZE=1024 go test -tags bulkbench -run 'TestBulkTransfer' -count=1 -timeout 60m . >/dev/null 2>&1
done
