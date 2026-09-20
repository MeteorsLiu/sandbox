#!/usr/bin/env python3
"""Run real Formula builds and retain raw timings and cgroup samples."""
import argparse
import concurrent.futures
import http.client
import json
import math
import os
import pathlib
import socket
import subprocess
import threading
import time


class Engine(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--backends", default="sandbox,docker,firecracker")
    parser.add_argument("--concurrency", default="1,2,4,8")
    parser.add_argument("--builds", type=int, default=8, help="same total build count at every concurrency")
    parser.add_argument("--cpuset", default="0,1")
    parser.add_argument("--memory-mib", type=int, default=4096)
    parser.add_argument("--firecracker-assets")
    args = parser.parse_args()
    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    context = json.loads(subprocess.check_output(["docker", "context", "inspect"]))[0]
    endpoint = context["Endpoints"]["docker"]["Host"]
    if not endpoint.startswith("unix://"):
        raise ValueError("run the controller beside the Docker daemon using its Unix socket")
    engine_socket = endpoint[7:]

    def api(method, path, data=None):
        conn = Engine(engine_socket)
        try:
            conn.request(method, path, json.dumps(data) if data is not None else None,
                         {"Content-Type": "application/json"})
            response = conn.getresponse()
            body = response.read()
            if response.status >= 300:
                raise RuntimeError(f"Docker {method} {path}: {response.status} {body.decode()}")
            return json.loads(body) if body else None
        finally:
            conn.close()

    (output / "environment.json").write_text(json.dumps({
        "arguments": vars(args), "engine": api("GET", "/info"),
        "image": api("GET", f"/images/{args.image}/json"),
        "memory_metric": "Docker API memory_stats.usage, including cache; shared Docker daemon excluded for all backends",
        "sample_interval_seconds": 0.1,
    }, indent=2) + "\n")
    lock = threading.Lock()
    samples = []
    active = set()
    finished = threading.Event()

    def sample():
        while not finished.is_set():
            with lock:
                ids = list(active)
            current = []
            for cid in ids:
                stats = api("GET", f"/containers/{cid}/stats?stream=false&one-shot=true")
                memory = stats.get("memory_stats", {})
                current.append({"id": cid, "bytes": memory.get("usage", 0),
                                "cpu_ns": stats.get("cpu_stats", {}).get("cpu_usage", {}).get("total_usage", 0)})
            if current:
                samples.append({"time_ns": time.perf_counter_ns(), "containers": current,
                                "memory_bytes": sum(s["bytes"] for s in current)})
            finished.wait(0.1)

    def task(backend, concurrency, count, task_id):
        name = f"{backend}-c{concurrency}-{task_id}"
        host = {"ReadonlyRootfs": True, "NetworkMode": "none", "CapDrop": ["ALL"],
                "SecurityOpt": ["seccomp=unconfined"], "CpusetCpus": args.cpuset,
                "Memory": args.memory_mib * 1024 * 1024,
                "Tmpfs": {"/work": "rw,exec,nosuid,nodev,mode=1777,size=256m",
                          "/tmp": "rw,exec,nosuid,nodev,mode=1777,size=256m"}}
        config = {"Image": args.image, "HostConfig": host,
                  "Cmd": [f"-backend={'sandbox' if backend == 'sandbox' else 'direct'}",
                          f"-count={count}", f"-concurrency={concurrency if backend == 'sandbox' else 1}"],
                  "AttachStdout": True, "AttachStderr": True}
        if backend == "firecracker":
            assets = pathlib.Path(args.firecracker_assets).resolve()
            fc = {"boot-source": {"kernel_image_path": "/assets/vmlinux",
                  "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/benchmark-init"},
                  "drives": [{"drive_id": "rootfs", "path_on_host": "/assets/rootfs.ext4",
                              "is_root_device": True, "is_read_only": True}],
                  "machine-config": {"vcpu_count": len(args.cpuset.split(",")), "mem_size_mib": args.memory_mib}}
            directory = output / name
            directory.mkdir()
            (directory / "config.json").write_text(json.dumps(fc))
            host["Binds"] = [f"{assets}:/assets:ro", f"{directory}:/config:ro"]
            host["Devices"] = [{"PathOnHost": "/dev/kvm", "PathInContainer": "/dev/kvm", "CgroupPermissions": "rwm"}]
            host["GroupAdd"] = [str(os.stat("/dev/kvm").st_gid)]
            config["Entrypoint"] = ["/assets/firecracker"]
            config["Cmd"] = ["--no-api", "--config-file", "/config/config.json"]
        t0 = time.perf_counter_ns()
        cid = api("POST", "/containers/create", config)["Id"]
        record = {"backend": backend, "concurrency": concurrency, "count": count, "task": task_id,
                  "container": cid, "create_ns": time.perf_counter_ns()-t0, "events": []}
        with lock:
            active.add(cid)
        t0 = time.perf_counter_ns()
        record["start_ns"] = t0
        with (output / f"{name}.log").open("w") as log:
            proc = subprocess.Popen(["docker", "start", "-a", cid], stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, text=True)
            for line in proc.stdout:
                now = time.perf_counter_ns()
                log.write(line)
                log.flush()
                if "BENCH:" in line:
                    event = json.loads(line.split("BENCH:", 1)[1])
                    event["observed_ns"] = now-t0
                    if event["event"] in ("main", "ready", "guest_entry"):
                        stats = api("GET", f"/containers/{cid}/stats?stream=false&one-shot=true")
                        event["memory_bytes"] = stats.get("memory_stats", {}).get("usage", 0)
                        event["memory_sample_lag_ns"] = time.perf_counter_ns()-now
                    record["events"].append(event)
            proc.wait()
        record["wall_ns"] = time.perf_counter_ns()-t0
        record["state"] = api("GET", f"/containers/{cid}/json")["State"]
        record["exit_code"] = record["state"]["ExitCode"]
        expected = "guest_entry" if backend == "sandbox" else "main"
        entries = [e["observed_ns"] for e in record["events"] if e["event"] == expected]
        record["launch_to_entry_ns"] = entries[0] if entries else None
        outcomes = [e for e in record["events"] if e["event"] == "result"]
        record["success"] = record["exit_code"] == 0 and len(outcomes) == count and all(not e.get("error") for e in outcomes)
        with lock:
            active.remove(cid)
        # An exited container keeps its logs/state until its measurements are retained.
        (output / f"{name}.json").write_text(json.dumps(record, indent=2)+"\n")
        return record

    failures = 0
    for backend in args.backends.split(","):
        for concurrency in map(int, args.concurrency.split(",")):
            if backend == "firecracker" and not args.firecracker_assets:
                raise ValueError("Firecracker needs --firecracker-assets; run setup-firecracker.sh first")
            count = args.builds
            if concurrency > count:
                raise ValueError("--builds must be at least every requested concurrency")
            samples.clear()
            finished.clear()
            sampler_errors = []

            def observe():
                try:
                    sample()
                except Exception as error:
                    sampler_errors.append(str(error))

            observer = threading.Thread(target=observe)
            observer.start()
            t0 = time.perf_counter_ns()
            try:
                if backend == "sandbox":
                    records = [task(backend, concurrency, count, 0)]
                else:
                    with concurrent.futures.ThreadPoolExecutor(concurrency) as workers:
                        records = list(workers.map(lambda i: task(backend, concurrency, 1, i), range(count)))
            finally:
                finished.set()
                observer.join()
            wall = time.perf_counter_ns()-t0
            success = sum(1 for r in records for e in r["events"]
                          if e["event"] == "result" and not e.get("error"))
            durations = sorted(e["duration_ns"] for r in records for e in r["events"]
                               if e["event"] == "result" and not e.get("error"))
            percentiles = {f"p{p}_ns": durations[min(len(durations)-1, math.ceil(len(durations)*p/100)-1)]
                           for p in (50, 95, 99)} if durations else {}
            summary = {"backend": backend, "concurrency": concurrency, "count": count,
                       "successful": success, "wall_ns": wall, "builds_per_second": success/(wall/1e9),
                       "memory_peak_bytes": max((s["memory_bytes"] for s in samples), default=0),
                       "build_latency": percentiles,
                       "oom_killed": sum(r["state"].get("OOMKilled", False) for r in records),
                       "sampler_errors": sampler_errors, "records": records, "samples": list(samples)}
            (output / f"{backend}-c{concurrency}.json").write_text(json.dumps(summary, indent=2)+"\n")
            print(json.dumps({k: v for k, v in summary.items() if k not in ("records", "samples")}), flush=True)
            for record in records:
                api("DELETE", f"/containers/{record['container']}")
            failures += count-success+bool(sampler_errors)+sum(r["exit_code"] != 0 for r in records)
    if failures:
        raise SystemExit(f"{failures} failed builds/measurements; see raw results")


if __name__ == "__main__":
    main()
