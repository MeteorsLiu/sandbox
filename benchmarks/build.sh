#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$1"
output=$(cd "$1" && pwd)
python3 "$repo/benchmarks/prepare.py" "$output/input"
# Give only the shared library's Go runtime a distinct VMA label. The host
# executable and the installed Go toolchain keep their original sources.
python3 - "$output" <<'PY'
import json, pathlib, subprocess, sys
output = pathlib.Path(sys.argv[1])
goroot = pathlib.Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/set_vma_name_linux.go"
text = source.read_text()
old = 'copy(sysName[:], " Go: ")'
assert text.count(old) == 1, "review the Go runtime VMA naming implementation"
replacement = output / "sentry_vma_linux.go"
replacement.write_text(text.replace(old, 'copy(sysName[:], " Sentry Go: ")'))
(output / "sentry-overlay.json").write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
PY
go -C "$repo/sentry" build -mod=readonly -overlay="$output/sentry-overlay.json" \
  -buildmode=c-shared -o "$output/sentrylib.so" .
cp "$repo/sentry/sandbox.h" "$output/sandbox.h"
# Instrument only the benchmark build. Production guest entry stays unchanged.
python3 - "$repo" "$output" <<'PY'
import json, pathlib, sys
repo, output = map(pathlib.Path, sys.argv[1:])
source = repo / "guest_linux.go"
text = source.read_text()
entry = "func guestEntry() {\n"
assert text.count(entry) == 1
replacement = output / "guest_linux.go"
replacement.write_text(text.replace(entry, entry + '\tif _, err := os.Stdout.WriteString("BENCH:{\\"event\\":\\"guest_entry\\"}\\n"); err != nil { panic(err) }\n'))
(output / "overlay.json").write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
PY
go -C "$repo/testdata/llar" build -mod=readonly -overlay="$output/overlay.json" \
  -ldflags='-checklinkname=0 -s=false -w=false -extldflags=-Wl,-z,separate-code' -o "$output/formula-bench" ./benchmark
gcc -O2 -o "$output/reboot" "$repo/benchmarks/reboot.c"
chmod 755 "$output" "$output/formula-bench"
go version > "$output/toolchain.txt"
gcc --version >> "$output/toolchain.txt"
make --version >> "$output/toolchain.txt"
pkg-config --version >> "$output/toolchain.txt"
sha256sum "$output/formula-bench" "$output/sentrylib.so" > "$output/binaries.sha256"
