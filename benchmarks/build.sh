#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$1"
output=$(cd "$1" && pwd)
python3 "$repo/benchmarks/prepare.py" "$output/input"
bash "$repo/sentry/build-linux.sh" "$output"
# Instrument only the benchmark build. Production guest entry stays unchanged.
python3 - "$repo" "$output" <<'PY'
import json, pathlib, sys
repo, output = map(pathlib.Path, sys.argv[1:])
source = repo / "guest_linux.go"
text = source.read_text()
entry = "func guestEntry() {\n"
assert text.count(entry) == 1
replacement = output / "guest_linux.go"
replacement.write_text(text.replace(entry, entry + "\tunix.RawSyscall(unix.SYS_GETPID, 0x53424d31, 0, 0)\n"))
(output / "overlay.json").write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
PY
go -C "$repo/testdata/llar" build -mod=readonly -overlay="$output/overlay.json" \
  -ldflags='-checklinkname=0 -extldflags=-Wl,-z,separate-code' -o "$output/formula-bench" ./benchmark
gcc -O2 -o "$output/reboot" "$repo/benchmarks/reboot.c"
chmod 755 "$output" "$output/formula-bench"
go version > "$output/toolchain.txt"
gcc --version >> "$output/toolchain.txt"
make --version >> "$output/toolchain.txt"
pkg-config --version >> "$output/toolchain.txt"
