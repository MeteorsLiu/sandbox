#!/usr/bin/env python3
"""Observe native PID trees and Docker service/container cgroups without limits."""
import argparse
import json
import pathlib
import subprocess
import threading
import time

from memory import callback_memory, process_tree, resident_memory, resident_processes


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
    discarded = 0
    finished = threading.Event()

    def memory(path):
        return {"bytes": int((path / "memory.current").read_text()),
                "stats": dict((key, int(value)) for key, value in
                              (line.split() for line in (path / "memory.stat").read_text().splitlines()))}

    def sample():
        begin = time.perf_counter_ns()
        engine = {name: {**memory(path), "resident": resident_memory(path)} for name, path in services.items()}
        workloads = {}
        groups = list((root / "system.slice").glob("docker-*.scope"))
        for path in groups:
            cid = path.name.removeprefix("docker-").removesuffix(".scope") if path.name.startswith("docker-") else path.name
            if any(service == path or service in path.parents or path in service.parents for service in paths):
                raise RuntimeError(f"container and service cgroups overlap: {path}")
            try:
                value = memory(path)
                value["resident"] = resident_memory(path)
                workloads[cid] = value
            except FileNotFoundError:
                # A completed container can disappear between the directory
                # listing and the read. Service cgroups must remain available.
                if path.exists():
                    raise
        runtime_processes, processes = [], {}
        for proc in pathlib.Path("/proc").iterdir():
            if not proc.name.isdecimal():
                continue
            try:
                fields = (proc / "stat").read_text().rsplit(")", 1)[1].split()
                processes[int(proc.name)] = {"parent": int(fields[1]), "starttime": fields[19]}
                name = (proc / "comm").read_text().strip()
                if name not in ("dockerd", "containerd") and not name.startswith("containerd-shim"):
                    continue
                group = (proc / "cgroup").read_text().strip().removeprefix("0::")
            except (FileNotFoundError, ProcessLookupError):
                continue
            path = root / group.lstrip("/")
            if not any(service == path or service in path.parents for service in paths):
                raise RuntimeError(f"unaccounted Docker runtime process: {proc.name} {name} {group}")
            runtime_processes.append({"pid": int(proc.name), "name": name, "cgroup": group})
        for path in (output.parent / "active").glob("*.json"):
            try:
                identity = json.loads(path.read_text())
            except FileNotFoundError:
                continue
            workloads[path.stem] = {"resident": resident_processes(process_tree(identity, processes))}
        if any(cid.startswith("sandbox-") for cid in workloads):
            # st_blocks includes swapped shmem pages. Reject those samples
            # instead of changing the host's swap or cgroup configuration.
            meminfo = dict(line.split(":", 1) for line in pathlib.Path("/proc/meminfo").read_text().splitlines())
            if int(meminfo["SwapTotal"].split()[0]) != int(meminfo["SwapFree"].split()[0]) or int(meminfo["SwapCached"].split()[0]):
                raise RuntimeError("host swap is in use; memfd blocks cannot be reported as resident pages")
        entry = {"begin_ns": begin, "end_ns": time.perf_counter_ns(),
                 "services": engine, "workloads": workloads, "runtime_processes": runtime_processes,
                 "engine_bytes": sum(value["bytes"] for value in engine.values()),
                 "engine_pss_bytes": sum(value["resident"]["pss_bytes"] for value in engine.values()),
                 "docker_payload_bytes": sum(value["bytes"] for value in workloads.values() if "bytes" in value)}
        samples.append(entry)
        log.write(json.dumps(entry) + "\n")
        log.flush()

    def observe():
        nonlocal discarded
        try:
            while not finished.wait(0.1):
                try:
                    sample()
                except ProcessLookupError:
                    # kcmp can lose either process while build children exit.
                    # Keep incomplete reads out of the memory percentiles.
                    discarded += 1
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
        ids = {record["workload"] for record in data["records"]}
        window = [entry for entry in samples if entry["begin_ns"] <= end and entry["end_ns"] >= start]
        if not window or not any(ids.intersection(entry["workloads"]) for entry in window):
            errors.append(f"no payload samples for {path.name}")
        data["engine_accounting"] = {
            "services": {name: str(path) for name, path in services.items()},
            "baseline_bytes": samples[0]["engine_bytes"],
            "samples": len(window), "errors": list(errors),
            "discarded_process_exit_samples": discarded,
            "metric": "Native process-tree residency; Docker also has cache-inclusive cgroup counters and services counted once",
            "excluded": "benchmark controller and its Docker CLI clients; host kernel outside Docker cgroups",
        }
        data["callback_memory"] = callback_memory(data, window)
        errors.extend(f"{path.name}: {error}" for error in data["callback_memory"]["errors"])
        path.write_text(json.dumps(data, indent=2) + "\n")
    if errors:
        raise SystemExit("engine memory sampling failed: " + "; ".join(errors))
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
