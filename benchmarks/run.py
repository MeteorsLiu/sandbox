#!/usr/bin/env python3
"""Run Formula builds directly on Linux, using containers only for Docker."""
import argparse
import concurrent.futures
import http.client
import json
import math
import os
import pathlib
import socket
import subprocess
import tempfile
import time
import uuid

from workload import audit_workload


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
    parser.add_argument("--vm-memory-mib", type=int, default=4096, help="guest RAM per Firecracker VM; no host memory limit")
    parser.add_argument("--measure", choices=("timing", "memory"), default="timing")
    parser.add_argument("--firecracker-assets", required=True, help="common exported rootfs and native VMM assets")
    args = parser.parse_args()
    cpus = sorted(os.sched_getaffinity(0))
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

    image = api("GET", f"/images/{args.image}/json")
    assets = pathlib.Path(args.firecracker_assets).resolve()
    if json.loads((assets / "image.json").read_text())[0]["Id"] != image["Id"]:
        raise ValueError("native rootfs and Docker image differ")
    native_env = os.environ.copy()
    native_env.update(entry.split("=", 1) for entry in image["Config"]["Env"])
    native_env["GOMAXPROCS"] = str(len(cpus))
    host_process = {"uid": os.geteuid(), "cpus": cpus,
                    "cgroup": pathlib.Path("/proc/self/cgroup").read_text(),
                    "namespaces": {name: os.readlink(f"/proc/self/ns/{name}")
                                   for name in ("mnt", "net", "pid", "user", "cgroup", "uts", "ipc")}}
    active = output / "active"
    if args.measure == "memory":
        active.mkdir()
    (output / "environment.json").write_text(json.dumps({
        "arguments": vars(args), "host_process": host_process, "engine": api("GET", "/info"),
        "image": image,
        "source_revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=pathlib.Path(__file__).resolve().parents[1], text=True).strip(),
        "launchers": {"sandbox": "direct formula-bench exec; rootfs mounted only inside Sentry guest", "firecracker": "direct VMM exec; no jailer", "docker": "Docker Engine"},
        "memory_metric": "native PID-tree PSS; Docker cgroups and services for Docker only; separate attributed Sentry+guest residency",
        "sample_interval_seconds": 0.1 if args.measure == "memory" else None,
        "timing_metric": "verified completed builds divided by batch start to final process exit; excludes workload audit, JSON writing, sampler shutdown and container deletion",
    }, indent=2) + "\n")
    def task(backend, concurrency, count, task_id):
        name = f"{backend}-c{concurrency}-{task_id}"
        host = {"ReadonlyRootfs": True, "NetworkMode": "none", "CapDrop": ["ALL"],
                "SecurityOpt": ["seccomp=unconfined"],
                "Tmpfs": {"/work": "rw,exec,nosuid,nodev,mode=1777,size=256m",
                          "/tmp": "rw,exec,nosuid,nodev,mode=1777,size=256m"}}
        config = {"Image": image["Id"], "HostConfig": host,
                  "Env": [f"GOMAXPROCS={len(cpus)}"],
                  "Cmd": [f"-backend={'sandbox' if backend == 'sandbox' else 'direct'}",
                          f"-count={count}", f"-concurrency={concurrency if backend == 'sandbox' else 1}"],
                  "AttachStdout": True, "AttachStderr": True}
        t0 = time.perf_counter_ns()
        if backend == "docker":
            cid = api("POST", "/containers/create", config)["Id"]
            group = pathlib.Path("/sys/fs/cgroup/system.slice") / f"docker-{cid}.scope"
            command = ["docker", "start", "-a", cid]
        else:
            cid = f"{backend}-{uuid.uuid4().hex}"
        if backend == "sandbox":
            # Use the existing host tmpfs, without creating a mount or namespace.
            work = tempfile.TemporaryDirectory(prefix="formula-bench-", dir="/dev/shm")
            command = ["/opt/benchmark/formula-bench", *config["Cmd"],
                       f"-work={work.name}", f"-rootfs={assets / 'rootfs'}"]
        elif backend == "firecracker":
            fc = {"boot-source": {"kernel_image_path": str(assets / "vmlinux"),
                  "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/benchmark-init"},
                  "drives": [{"drive_id": "rootfs", "path_on_host": str(assets / "rootfs.ext4"),
                              "is_root_device": True, "is_read_only": True}],
                  "machine-config": {"vcpu_count": len(cpus), "mem_size_mib": args.vm_memory_mib}}
            directory = output / name
            directory.mkdir()
            data = json.dumps(fc)
            (directory / "config.json").write_text(data)
            command = [str(assets / "firecracker"), "--no-api", "--config-file", str(directory / "config.json")]
        record = {"backend": backend, "concurrency": concurrency, "count": count, "task": task_id,
                  "workload": cid, "command": command,
                  "create_ns": time.perf_counter_ns()-t0, "events": [], "event_errors": []}
        if backend == "docker":
            record["cgroup"] = str(group)
        t0 = time.perf_counter_ns()
        record["start_ns"] = t0
        print(json.dumps({"event": "task_start", "backend": backend, "concurrency": concurrency, "task": task_id}), flush=True)
        with (output / f"{name}.log").open("w") as log:
            proc = subprocess.Popen(command, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, text=True,
                                    env=native_env if backend == "sandbox" else None)
            record["pid"] = proc.pid
            if backend != "docker":
                record["host_process"] = {
                    "uid": int(pathlib.Path(f"/proc/{proc.pid}/status").read_text().split("Uid:", 1)[1].split()[1]),
                    "cpus": sorted(os.sched_getaffinity(proc.pid)),
                    "cgroup": pathlib.Path(f"/proc/{proc.pid}/cgroup").read_text(),
                    "namespaces": {name: os.readlink(f"/proc/{proc.pid}/ns/{name}")
                                   for name in host_process["namespaces"]},
                }
                if args.measure == "memory":
                    identity = {"pid": proc.pid, "starttime": pathlib.Path(f"/proc/{proc.pid}/stat").read_text().rsplit(")", 1)[1].split()[19]}
                    pending = active / f"{cid}.tmp"
                    pending.write_text(json.dumps(identity))
                    pending.rename(active / f"{cid}.json")
            for line in proc.stdout:
                now = time.perf_counter_ns()
                log.write(line)
                log.flush()
                if "BENCH:" in line:
                    try:
                        event = json.loads(line.split("BENCH:", 1)[1])
                    except json.JSONDecodeError as error:
                        # A guest panic can interleave kernel output with serial
                        # JSON. Retain the failed measurement and finish cleanup.
                        record["event_errors"].append(str(error))
                        continue
                    event["observed_ns"] = now-t0
                    record["events"].append(event)
                    if event["event"] in ("main", "ready", "result", "guest_exit"):
                        print(json.dumps({"backend": backend, "task": task_id, **event}), flush=True)
            proc.wait()
        record["end_ns"] = time.perf_counter_ns()
        record["wall_ns"] = record["end_ns"]-t0
        if backend != "docker" and args.measure == "memory":
            (active / f"{cid}.json").unlink()
        if backend == "docker":
            record["state"] = api("GET", f"/containers/{cid}/json")["State"]
        else:
            record["state"] = {"ExitCode": proc.returncode}
        if backend == "sandbox":
            work.cleanup()
        record["exit_code"] = record["state"]["ExitCode"]
        print(json.dumps({"event": "task_exit", "backend": backend, "task": task_id, "exit_code": record["exit_code"]}), flush=True)
        expected = "guest_entry" if backend == "sandbox" else "main"
        entries = [e["observed_ns"] for e in record["events"] if e["event"] == expected]
        record["launch_to_entry_ns"] = entries[0] if entries else None
        mains = [e["observed_ns"] for e in record["events"] if e["event"] == "main"]
        record["launch_to_main_ns"] = mains[0] if mains else None
        callbacks = [e["observed_ns"] for e in record["events"] if e["event"] == "callback_start"]
        record["launch_to_callback_ns"] = callbacks[0] if callbacks else None
        outcomes = [e for e in record["events"] if e["event"] == "result"]
        if backend == "sandbox" and concurrency == 1:
            start = None
            phase_times = {}
            for event in record["events"]:
                if event["event"] == "run_start":
                    start = event["observed_ns"]
                    phase_times = {}
                elif start is not None and event["event"] in ("guest_entry", "callback_start"):
                    phase_times[event["event"]] = event["observed_ns"] - start
                elif event["event"] == "result":
                    if "guest_entry" in phase_times:
                        event["entry_ns"] = phase_times["guest_entry"]
                    if "callback_start" in phase_times:
                        event["callback_ns"] = phase_times["callback_start"]
        for outcome in outcomes:
            job_events = {e["event"]: e for e in record["events"] if e.get("id") == outcome["id"]}
            if "run_start" in job_events and "callback_start" in job_events and "callback_end" in job_events:
                outcome["before_callback_ns"] = job_events["callback_start"]["observed_ns"] - job_events["run_start"]["observed_ns"]
                outcome["after_callback_ns"] = outcome["observed_ns"] - job_events["callback_end"]["observed_ns"]
        return record

    failures = 0
    for backend in args.backends.split(","):
        for concurrency in map(int, args.concurrency.split(",")):
            if backend == "firecracker" and not args.firecracker_assets:
                raise ValueError("Firecracker needs --firecracker-assets; run setup-firecracker.sh first")
            count = args.builds
            if concurrency > count:
                raise ValueError("--builds must be at least every requested concurrency")
            t0 = time.perf_counter_ns()
            if backend == "sandbox":
                records = [task(backend, concurrency, count, 0)]
            else:
                with concurrent.futures.ThreadPoolExecutor(concurrency) as workers:
                    records = list(workers.map(lambda i: task(backend, concurrency, 1, i), range(count)))
            wall = max(r["end_ns"] for r in records)-t0
            for record in records:
                name = f"{backend}-c{concurrency}-{record['task']}"
                record["audit"] = audit_workload((output / f"{name}.log").read_text(), record["count"], record["events"])
                outcomes = [e for e in record["events"] if e["event"] == "result"]
                record["success"] = record["exit_code"] == 0 and not record["event_errors"] and not record["audit"]["errors"] and len(outcomes) == record["count"] and all(not e.get("error") for e in outcomes)
                (output / f"{name}.json").write_text(json.dumps(record, indent=2)+"\n")
            success = sum(r["count"] for r in records if r["success"])
            durations = sorted(e["duration_ns"] for r in records if r["success"] for e in r["events"]
                               if e["event"] == "result" and not e.get("error"))
            percentiles = {f"p{p}_ns": durations[min(len(durations)-1, math.ceil(len(durations)*p/100)-1)]
                           for p in (50, 95, 99)} if durations else {}
            summary = {"backend": backend, "concurrency": concurrency, "count": count, "measure": args.measure,
                       "successful": success, "wall_ns": wall, "builds_per_second": success/(wall/1e9),
                       "build_latency": percentiles,
                       "event_errors": sum(len(r["event_errors"]) for r in records),
                       "workload_errors": sum(len(r["audit"]["errors"]) for r in records),
                       "records": records}
            (output / f"{backend}-c{concurrency}.json").write_text(json.dumps(summary, indent=2)+"\n")
            print(json.dumps({k: v for k, v in summary.items() if k not in ("records", "samples")}), flush=True)
            for record in records:
                if backend == "docker":
                    api("DELETE", f"/containers/{record['workload']}")
            failures += count-success+sum(r["exit_code"] != 0 for r in records)+summary["event_errors"]+summary["workload_errors"]
    if failures:
        raise SystemExit(f"{failures} failed builds/measurements; see raw results")


if __name__ == "__main__":
    main()
