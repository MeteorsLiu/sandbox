#!/usr/bin/env python3
"""Prepare pinned upstream inputs outside the timed region."""
import hashlib
import io
import json
import pathlib
import sys
import tarfile
import urllib.request

HUB = "1c2b4666ef598ed51afeb44221a73f53eea7e65c"
ZLIB = "51b7f2abdade71cd9bb0e7a373ef2610ec6f9daf"
out = pathlib.Path(sys.argv[1])
formula = out / "formula" / "v1.3.1"
formula.mkdir(parents=True)
manifest = {"llarhub_commit": HUB, "zlib_commit": ZLIB, "sha256": {}}
for name in ("zlib_llar.gox", "consumer.c"):
    url = f"https://raw.githubusercontent.com/xgo-dev/llarhub/{HUB}/madler/zlib/v1.3.1/{name}"
    data = urllib.request.urlopen(url, timeout=60).read()
    (formula / name).write_bytes(data)
    manifest["sha256"][name] = hashlib.sha256(data).hexdigest()
data = urllib.request.urlopen(f"https://api.github.com/repos/madler/zlib/tarball/{ZLIB}", timeout=60).read()
manifest["sha256"]["zlib.tar.gz"] = hashlib.sha256(data).hexdigest()
with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
    prefix = archive.getmembers()[0].name.split("/")[0]
    for member in archive.getmembers():
        parts = pathlib.PurePosixPath(member.name).parts
        if parts[0] != prefix or ".." in parts or member.issym() or member.islnk():
            raise ValueError(f"unexpected archive entry {member.name}")
        target = out / "source" / pathlib.Path(*parts[1:])
        if member.isdir():
            target.mkdir(parents=True, exist_ok=True)
        elif member.isfile():
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(archive.extractfile(member).read())
            target.chmod(member.mode & 0o777)
        else:
            raise ValueError(f"unsupported archive entry {member.name}")
(out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
