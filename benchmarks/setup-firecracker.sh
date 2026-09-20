#!/usr/bin/env bash
set -euo pipefail
image=$1
mkdir -p "$2"
output=$(cd "$2" && pwd)
test -c /dev/kvm
test "$(uname -m)" = x86_64
curl -fL --retry 3 https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-x86_64.tgz -o "$output/firecracker.tgz"
tar -xzf "$output/firecracker.tgz" -C "$output"
cp "$output/release-v1.17.0-x86_64/firecracker-v1.17.0-x86_64" "$output/firecracker"
python3 - "$output" <<'PY'
import hashlib, json, pathlib, sys, urllib.request
out = pathlib.Path(sys.argv[1])
base = "https://s3.amazonaws.com/spec.ccfc.min"
key = "firecracker-ci/20260916-dcfc69b625d0-0/x86_64/vmlinux-6.18.48"
data = urllib.request.urlopen(base+"/"+key, timeout=120).read()
(out/"vmlinux").write_bytes(data)
(out/"kernel.json").write_text(json.dumps({"url": base+"/"+key, "sha256": hashlib.sha256(data).hexdigest()}, indent=2)+"\n")
PY
mkdir "$output/rootfs"
container=$(docker create "$image")
docker export "$container" | tar --numeric-owner -xf - -C "$output/rootfs"
docker rm "$container"
docker image inspect "$image" > "$output/image.json"
python3 - "$output" <<'PY'
import json, pathlib, re, shlex, sys
out = pathlib.Path(sys.argv[1])
image = json.loads((out/"image.json").read_text())[0]
lines = []
for entry in image["Config"]["Env"]:
    key, value = entry.split("=", 1)
    assert re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key)
    lines.append(f"export {key}={shlex.quote(value)}")
(out/"rootfs/opt/benchmark/environment.sh").write_text("\n".join(lines)+"\n")
PY
truncate -s 3G "$output/rootfs.ext4"
mkfs.ext4 -q -F -d "$output/rootfs" "$output/rootfs.ext4"
chmod -R a+rX "$output"
sha256sum "$output/firecracker" "$output/vmlinux" "$output/rootfs.ext4" > "$output/checksums.txt"
