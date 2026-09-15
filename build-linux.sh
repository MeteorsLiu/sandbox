#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "Usage: bash build-linux.sh OUTPUT_DIRECTORY" >&2
  exit 1
fi
mkdir -p "$1"
output=$(cd "$1" && pwd)
cd "$(dirname "$0")"

flags='-checklinkname=0 -extldflags=-Wl,-z,separate-code'
go -C testdata build -mod=readonly -ldflags="$flags" -o "$output/smoke" .
go test -c -mod=readonly -ldflags="$flags" -o "$output/transfer.test" .
go list -mod=readonly -deps . > "$output/host-deps.txt"
if grep -q '^gvisor.dev/gvisor' "$output/host-deps.txt"; then
  echo "Host dependency graph unexpectedly includes gVisor" >&2
  exit 1
fi
chmod 755 "$output" "$output/smoke" "$output/transfer.test"
