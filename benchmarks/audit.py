#!/usr/bin/env python3
"""Reject comparisons with different compile commands or failed builds."""
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
digests, images, errors = set(), set(), []
builds = {}
cpus = {}
hosts = {}
for path in root.rglob("environment.json"):
    env = json.loads(path.read_text())
    images.add(env["image"]["Id"])
    hosts[path.parent] = env["host_process"]
    cpus[path.parent] = env["host_process"]["cpus"]
for path in sorted(root.rglob("*-c*.json")):
    data = json.loads(path.read_text())
    if "records" not in data:
        continue
    if data["successful"] != data["count"]:
        errors.append(f"{path}: only {data['successful']}/{data['count']} verified builds")
    for record in data["records"]:
        digests.update(record["audit"]["compiler_commands_sha256"].values())
        builds[data["backend"]] = builds.get(data["backend"], 0) + record["count"]
        if data["backend"] != "docker" and record["host_process"] != hosts[path.parent]:
            errors.append(f"{path}: native process differs from controller UID, CPUs, cgroup or namespaces")
        for event in record["events"]:
            uid = hosts[path.parent]["uid"] if data["backend"] == "sandbox" else 1000
            if event["event"] == "ready" and (event["gomaxprocs"] != len(cpus[path.parent]) or event["uid"] != uid or event["makeflags"] != "-j1"):
                errors.append(f"{path}: differing runtime settings: {event}")
if len({tuple(value) for value in cpus.values()}) != 1:
    errors.append("CPU affinity differs between cases")
if len(images) != 1 or len(digests) != 1 or set(builds) != {"sandbox", "docker", "firecracker"}:
    errors.append("missing backend or mismatched image/compiler command hashes")
print(json.dumps({"image_ids": sorted(images), "cpus": sorted({tuple(value) for value in cpus.values()}), "compiler_commands_sha256": sorted(digests), "builds": builds, "errors": errors}, indent=2))
if errors:
    raise SystemExit(1)
