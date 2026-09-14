#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "Usage: bash build-linux.sh OUTPUT_DIRECTORY" >&2
  exit 1
fi
mkdir -p "$1"
output=$(cd "$1" && pwd)
cd "$(dirname "$0")"

go build -mod=readonly -buildmode=c-shared -o "$output/sentrylib.so" .
cp sandbox.h "$output/sandbox.h"
