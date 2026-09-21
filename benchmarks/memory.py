"""Attribute resident pages without subtracting a host-memory baseline."""
import ctypes
import math
import os
import pathlib
import platform
import re
import sys


HEADER = re.compile(r"^([0-9a-f]+)-([0-9a-f]+)\s+\S+\s+[0-9a-f]+\s+([0-9a-f]+):([0-9a-f]+)\s+(\d+)\s*(.*)$")
BACKING = {
    "/memfd:llar-runtime-memory (deleted)": "guest_backing_bytes",
    "/memfd:systrap-memory (deleted)": "platform_backing_bytes",
    "/memfd:memory-usage (deleted)": "platform_backing_bytes",
    "/memfd:llar-sandbox (deleted)": "transfer_backing_bytes",
}
SYS_KCMP = {"x86_64": 312, "aarch64": 272}.get(platform.machine())
if sys.platform == "linux":
    syscall = ctypes.CDLL(None, use_errno=True).syscall
    syscall.restype = ctypes.c_long
else:
    syscall = None


def same_vm(first, second):
    if syscall is None or SYS_KCMP is None:
        raise RuntimeError("kcmp memory accounting requires Linux amd64 or arm64")
    # Linux uapi/linux/kcmp.h: KCMP_VM = 1. Compare mm, not PID or file mappings:
    # systrap sysmsg threads use CLONE_VM without CLONE_THREAD.
    result = syscall(ctypes.c_long(SYS_KCMP), ctypes.c_int(first), ctypes.c_int(second),
                     ctypes.c_int(1), ctypes.c_ulong(0), ctypes.c_ulong(0))
    if result == -1:
        error = ctypes.get_errno()
        raise OSError(error, f"kcmp(KCMP_VM, {first}, {second}): {os.strerror(error)}")
    return result == 0


def smaps(text):
    mappings = []
    for line in text.splitlines():
        match = HEADER.match(line)
        if match:
            start, end, major, minor, inode, name = match.groups()
            mappings.append({"start": int(start, 16), "end": int(end, 16), "name": name,
                             "file": (int(major, 16), int(minor, 16), int(inode)), "pss": 0})
        elif line.startswith("Pss:"):
            mappings[-1]["pss"] = int(line.split()[1]) * 1024
    return mappings


def attribute(processes, backing):
    result = dict.fromkeys(("pss_bytes", "sentry_runtime_pss_bytes", "sentry_image_pss_bytes",
                           "guest_backing_bytes", "platform_backing_bytes", "transfer_backing_bytes",
                           "helper_pss_bytes", "host_runtime_pss_bytes", "host_image_pss_bytes",
                           "unattributed_pss_bytes"), 0)
    for file in backing.values():
        result[file["category"]] += file["bytes"]
    groups = {}
    for pid, process in processes.items():
        groups.setdefault(process["vm"], []).append(pid)
    for representative in groups:
        process = processes[representative]
        for mapping in process["mappings"]:
            size, name = mapping["pss"], mapping["name"]
            result["pss_bytes"] += size
            if mapping["file"] in backing:
                # Count tmpfs blocks once, including pages not currently mapped.
                continue
            if name in BACKING:
                category = BACKING[name]
            elif process["parent"] in processes:
                category = "helper_pss_bytes"
            elif name.startswith("[anon: Sentry Go: "):
                category = "sentry_runtime_pss_bytes"
            elif name.endswith("/opt/benchmark/sentrylib.so"):
                category = "sentry_image_pss_bytes"
            elif name.startswith("[anon: Go: "):
                category = "host_runtime_pss_bytes"
            elif name.endswith("/opt/benchmark/formula-bench"):
                category = "host_image_pss_bytes"
            else:
                category = "unattributed_pss_bytes"
            result[category] += size
    result["sentry_guest_bytes"] = sum(result[key] for key in (
        "sentry_runtime_pss_bytes", "sentry_image_pss_bytes", "guest_backing_bytes",
        "platform_backing_bytes", "transfer_backing_bytes", "helper_pss_bytes"))
    result["pids"] = sorted(processes)
    result["mm_groups"] = [sorted(pids) for pids in groups.values()]
    return result


def resident_memory(group):
    if int((group / "memory.swap.current").read_text()):
        raise RuntimeError("resident memfd accounting requires swap to be disabled")
    return resident_processes((group / "cgroup.procs").read_text().split())


def process_tree(root, processes):
    pid = root["pid"]
    if pid not in processes or processes[pid]["starttime"] != root["starttime"]:
        return []
    children = {}
    for child, process in processes.items():
        children.setdefault(process["parent"], []).append(child)
    pids = [pid]
    for parent in pids:
        pids.extend(children.get(parent, ()))
    return pids


def resident_processes(pids):
    processes, backing = {}, {}
    for pid in pids:
        proc = pathlib.Path("/proc") / str(pid)
        try:
            parent = int((proc / "stat").read_text().rsplit(")", 1)[1].split()[1])
            mappings = smaps((proc / "smaps").read_text())
        except (FileNotFoundError, ProcessLookupError):
            continue
        processes[int(pid)] = {"parent": parent, "mappings": mappings}
    representatives = []
    for pid, process in processes.items():
        # Ignore an address space that was already gone when smaps was read.
        # If a process exits during kcmp, propagate ESRCH to discard this sample.
        if not process["mappings"]:
            continue
        process["vm"] = next((other for other in representatives if same_vm(pid, other)), pid)
        if process["vm"] == pid:
            representatives.append(pid)
    processes = {pid: process for pid, process in processes.items() if "vm" in process}
    for pid, process in processes.items():
        if process["parent"] in processes:
            continue
        try:
            for fd in (pathlib.Path("/proc") / str(pid) / "fd").iterdir():
                try:
                    category = BACKING.get(os.readlink(fd))
                    if category:
                        stat = fd.stat()
                        key = (os.major(stat.st_dev), os.minor(stat.st_dev), stat.st_ino)
                        backing[key] = {"category": category, "bytes": stat.st_blocks * 512}
                except (FileNotFoundError, ProcessLookupError):
                    continue
        except (FileNotFoundError, ProcessLookupError):
            continue
    return attribute(processes, backing)


def callback_windows(record):
    phases, windows = {}, []
    begin = None
    for event in record["events"]:
        kind = event["event"]
        if kind not in ("run_start", "callback_start", "callback_end", "result"):
            continue
        now = record["start_ns"] + event["observed_ns"]
        if begin is not None:
            windows.append((begin, now))
        if kind == "result":
            phases.pop(event["id"])
        else:
            phases[event["id"]] = kind
        begin = now if phases and all(value == "callback_start" for value in phases.values()) else None
    return windows


def callback_memory(data, samples):
    records = {record["workload"]: record for record in data["records"]}
    windows = {cid: callback_windows(record) for cid, record in records.items()}
    selected = []
    for entry in samples:
        active = [cid for cid, record in records.items()
                  if record["start_ns"] <= entry["end_ns"] and
                  record["start_ns"] + record["wall_ns"] >= entry["begin_ns"]]
        if not active or any(cid not in entry["workloads"] or not any(
                start <= entry["begin_ns"] and entry["end_ns"] <= end
                for start, end in windows[cid]) for cid in active):
            continue
        engine_bytes = entry["engine_bytes"] if data["backend"] == "docker" else 0
        engine_pss = entry["engine_pss_bytes"] if data["backend"] == "docker" else 0
        totals = {"docker_services_pss_bytes": engine_pss}
        if data["backend"] == "docker":
            totals.update(payload_cgroup_bytes=0, docker_services_cgroup_bytes=engine_bytes)
        for cid in active:
            container = entry["workloads"][cid]
            if data["backend"] == "docker":
                totals["payload_cgroup_bytes"] += container["bytes"]
            for key, value in container["resident"].items():
                if key not in ("pids", "mm_groups"):
                    totals[key] = totals.get(key, 0) + value
        if data["backend"] == "docker":
            totals["with_docker_services_cgroup_bytes"] = totals["payload_cgroup_bytes"] + engine_bytes
        totals["with_runtime_pss_bytes"] = totals["pss_bytes"] + engine_pss
        selected.append(totals)
    errors = []
    if not selected:
        errors.append("no complete callback-only memory samples")
    if data["backend"] == "sandbox" and selected and not all(s["sentry_runtime_pss_bytes"] > 0 for s in selected):
        errors.append("missing Sentry runtime VMA labels")
    percentiles = {}
    if selected:
        for key in selected[0]:
            values = sorted(sample[key] for sample in selected)
            percentiles[key] = {f"p{p}": values[math.ceil(len(values)*p/100)-1] for p in (50, 95, 99)}
    return {"samples": len(selected), "errors": errors, "bytes": percentiles,
            "window": "Only complete samples inside callbacks; exclude any overlapping preparation, Save, Load or result validation",
            "sentry_metric": "Tagged Sentry runtime and library PSS + unique resident memfd blocks + helper PSS excluding those memfds",
            "excluded": "Host Go runtime and executable; unattributed host-process mappings; unmapped filesystem cache and kernel memory"}
