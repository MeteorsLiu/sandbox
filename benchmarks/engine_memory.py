#!/usr/bin/env python3
"""Include the shared Docker services without changing the measured payload."""
import argparse
import json
import pathlib
import subprocess
import threading
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("a benchmark command is required")

    info = json.loads(subprocess.check_output(["docker", "info", "--format", "{{json .}}"], text=True))
    if info["CgroupDriver"] != "systemd" or info["CgroupVersion"] != "2" or info["Containers"] != 0:
        raise RuntimeError("engine accounting requires an idle native Linux systemd/cgroup-v2 Docker daemon")
    root = pathlib.Path("/sys/fs/cgroup")
    services = {}
    for service in ("docker.service", "containerd.service"):
        group = subprocess.check_output(["systemctl", "show", "--property=ControlGroup", "--value", service], text=True).strip()
        if not group or group == "/":
            raise RuntimeError(f"missing service cgroup: {service}")
        services[service] = root / group.lstrip("/")
    paths = list(services.values())
    if paths[0] == paths[1] or paths[0] in paths[1].parents or paths[1] in paths[0].parents:
        raise RuntimeError("Docker service cgroups overlap")

    output = pathlib.Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    samples, errors = [], []
    finished = threading.Event()

    def memory(path):
        return {"bytes": int((path / "memory.current").read_text()),
                "stats": dict((key, int(value)) for key, value in
                              (line.split() for line in (path / "memory.stat").read_text().splitlines()))}

    def sample():
        begin = time.perf_counter_ns()
        engine = {name: memory(path) for name, path in services.items()}
        containers = {}
        for path in (root / "system.slice").glob("docker-*.scope"):
            cid = path.name.removeprefix("docker-").removesuffix(".scope")
            if len(cid) != 64:
                raise RuntimeError(f"unexpected container cgroup: {path}")
            if any(service == path or service in path.parents or path in service.parents for service in paths):
                raise RuntimeError(f"container and service cgroups overlap: {path}")
            try:
                containers[cid] = memory(path)
            except FileNotFoundError:
                # A completed container can disappear between the directory
                # listing and the read. Service cgroups must remain available.
                if path.exists():
                    raise
        runtime_processes = []
        for proc in pathlib.Path("/proc").iterdir():
            if not proc.name.isdecimal():
                continue
            try:
                name = (proc / "comm").read_text().strip()
                if name not in ("dockerd", "containerd") and not name.startswith("containerd-shim"):
                    continue
                group = (proc / "cgroup").read_text().strip().removeprefix("0::")
            except FileNotFoundError:
                continue
            path = root / group.lstrip("/")
            if not any(service == path or service in path.parents for service in paths):
                raise RuntimeError(f"unaccounted Docker runtime process: {proc.name} {name} {group}")
            runtime_processes.append({"pid": int(proc.name), "name": name, "cgroup": group})
        entry = {"begin_ns": begin, "end_ns": time.perf_counter_ns(),
                 "services": engine, "containers": containers, "runtime_processes": runtime_processes,
                 "engine_bytes": sum(value["bytes"] for value in engine.values()),
                 "payload_bytes": sum(value["bytes"] for value in containers.values())}
        entry["total_bytes"] = entry["engine_bytes"] + entry["payload_bytes"]
        samples.append(entry)
        log.write(json.dumps(entry) + "\n")
        log.flush()

    def observe():
        try:
            while not finished.wait(0.1):
                sample()
        except Exception as error:
            errors.append(str(error))

    with output.open("w") as log:
        sample()
        observer = threading.Thread(target=observe)
        observer.start()
        try:
            result = subprocess.run(command)
        finally:
            finished.set()
            observer.join()
        sample()

    for path in sorted(output.parent.glob("*-c*.json")):
        data = json.loads(path.read_text())
        if "records" not in data:
            continue
        start = min(record["start_ns"] - record["create_ns"] for record in data["records"])
        end = max(record["start_ns"] + record["wall_ns"] for record in data["records"])
        ids = {record["container"] for record in data["records"]}
        window = [entry for entry in samples if entry["begin_ns"] <= end and entry["end_ns"] >= start]
        if not window or not any(ids.intersection(entry["containers"]) for entry in window):
            errors.append(f"no payload cgroup samples for {path.name}")
        data["engine_accounting"] = {
            "services": {name: str(path) for name, path in services.items()},
            "baseline_bytes": samples[0]["engine_bytes"],
            "engine_peak_bytes": max((entry["engine_bytes"] for entry in window), default=0),
            "payload_peak_bytes": max((sum(value["bytes"] for cid, value in entry["containers"].items() if cid in ids) for entry in window), default=0),
            "total_peak_bytes": max((entry["engine_bytes"] + sum(value["bytes"] for cid, value in entry["containers"].items() if cid in ids) for entry in window), default=0),
            "samples": len(window), "errors": list(errors),
            "metric": "Sum of disjoint cgroup-v2 memory.current reads in each sample; includes cache and shared Docker services once",
            "excluded": "benchmark controller and its Docker CLI clients; host kernel outside these cgroups",
        }
        path.write_text(json.dumps(data, indent=2) + "\n")
    if errors:
        raise SystemExit("engine memory sampling failed: " + "; ".join(errors))
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
