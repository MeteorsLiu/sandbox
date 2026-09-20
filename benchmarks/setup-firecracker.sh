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
import hashlib, json, pathlib, sys, urllib.parse, urllib.request, xml.etree.ElementTree as ET
out = pathlib.Path(sys.argv[1])
base = "https://s3.amazonaws.com/spec.ccfc.min"
def listing(prefix, delimiter=None):
    args = {"list-type": "2", "prefix": prefix}
    if delimiter: args["delimiter"] = delimiter
    return ET.fromstring(urllib.request.urlopen(base+"?"+urllib.parse.urlencode(args), timeout=60).read())
ns = {"s": "http://s3.amazonaws.com/doc/2006-03-01/"}
prefix = sorted(n.text for n in listing("firecracker-ci/", "/").findall("s:CommonPrefixes/s:Prefix", ns))[-1]
keys = [n.text for n in listing(prefix+"x86_64/vmlinux-").findall("s:Contents/s:Key", ns)]
key = sorted(k for k in keys if k.rsplit("/",1)[-1].removeprefix("vmlinux-").replace(".","").isdigit())[-1]
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
