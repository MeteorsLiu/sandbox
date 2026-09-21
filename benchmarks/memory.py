"""Attribute resident pages without subtracting a host-memory baseline."""
import math
import os
import pathlib
import re


HEADER = re.compile(r"^([0-9a-f]+)-([0-9a-f]+)\s+\S+\s+[0-9a-f]+\s+([0-9a-f]+):([0-9a-f]+)\s+(\d+)\s*(.*)$")
BACKING = {
    "/memfd:llar-runtime-memory (deleted)": "guest_backing_bytes",
    "/memfd:systrap-memory (deleted)": "platform_backing_bytes",
    "/memfd:memory-usage (deleted)": "platform_backing_bytes",
    "/memfd:llar-sandbox (deleted)": "transfer_backing_bytes",
}


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
    for process in processes.values():
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
            elif name == "/opt/benchmark/sentrylib.so":
                category = "sentry_image_pss_bytes"
            elif name.startswith("[anon: Go: "):
                category = "host_runtime_pss_bytes"
            elif name == "/opt/benchmark/formula-bench":
                category = "host_image_pss_bytes"
            else:
                category = "unattributed_pss_bytes"
            result[category] += size
    result["sentry_guest_bytes"] = sum(result[key] for key in (
        "sentry_runtime_pss_bytes", "sentry_image_pss_bytes", "guest_backing_bytes",
        "platform_backing_bytes", "transfer_backing_bytes", "helper_pss_bytes"))
    result["pids"] = sorted(processes)
    return result


def resident_memory(group):
    if int((group / "memory.swap.current").read_text()):
        raise RuntimeError("resident memfd accounting requires swap to be disabled")
    processes, backing = {}, {}
    for pid in (group / "cgroup.procs").read_text().split():
        proc = pathlib.Path("/proc") / pid
        try:
            parent = int((proc / "stat").read_text().rsplit(")", 1)[1].split()[1])
            mappings = smaps((proc / "smaps").read_text())
        except (FileNotFoundError, ProcessLookupError):
            continue
        processes[int(pid)] = {"parent": parent, "mappings": mappings}
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
    records = {record["container"]: record for record in data["records"]}
    windows = {cid: callback_windows(record) for cid, record in records.items()}
    selected = []
    for entry in samples:
        active = [cid for cid, record in records.items()
                  if record["start_ns"] <= entry["end_ns"] and
                  record["start_ns"] + record["wall_ns"] >= entry["begin_ns"]]
        if not active or any(cid not in entry["containers"] or not any(
                start <= entry["begin_ns"] and entry["end_ns"] <= end
                for start, end in windows[cid]) for cid in active):
            continue
        totals = {"payload_cgroup_bytes": 0, "docker_services_cgroup_bytes": entry["engine_bytes"]}
        for cid in active:
            container = entry["containers"][cid]
            totals["payload_cgroup_bytes"] += container["bytes"]
            for key, value in container["resident"].items():
                if key != "pids":
                    totals[key] = totals.get(key, 0) + value
        totals["with_docker_services_cgroup_bytes"] = totals["payload_cgroup_bytes"] + entry["engine_bytes"]
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
