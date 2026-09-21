#!/usr/bin/env python3
"""Summarize retained samples without hiding unsuccessful builds."""
import csv
import json
import math
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
rows = []
for path in sorted(root.glob("*-c*.json")):
    data = json.loads(path.read_text())
    if "records" not in data:
        continue
    starts = [r["launch_to_entry_ns"]/1e6 for r in data["records"] if r.get("launch_to_entry_ns") is not None]
    ready = [r["launch_to_callback_ns"]/1e6 for r in data["records"] if r.get("launch_to_callback_ns") is not None]
    entries = [e["entry_ns"]/1e6 for r in data["records"] for e in r["events"] if e.get("entry_ns")]
    callbacks = [e["callback_ns"]/1e6 for r in data["records"] for e in r["events"] if e.get("callback_ns")]
    def percentile(values, p):
        return round(sorted(values)[math.ceil(len(values)*p/100)-1], 3) if values else ""
    rows.append({"backend": data["backend"], "concurrency": data["concurrency"],
                 "success": f"{data['successful']}/{data['count']}",
                 "builds_per_second": round(data["builds_per_second"], 4),
                 "build_p50_ms": round(data["build_latency"]["p50_ns"]/1e6, 3) if data["build_latency"] else "N/A",
                 "build_p95_ms": round(data["build_latency"]["p95_ns"]/1e6, 3) if data["build_latency"] else "N/A",
                 "sampled_full_environment_peak_mib": round(data["memory_peak_bytes"]/1048576, 2),
                 "sampled_with_docker_services_peak_mib": round(data["engine_accounting"]["total_peak_bytes"]/1048576, 2) if data.get("engine_accounting") and not data["engine_accounting"]["errors"] else "N/A",
                 "launch_to_entry_p50_ms": percentile(starts, 50),
                 "launch_to_entry_p95_ms": percentile(starts, 95),
                 "launch_to_entry_p99_ms": percentile(starts, 99),
                 "launch_to_callback_p50_ms": percentile(ready, 50),
                 "run_to_entry_p50_ms": percentile(entries, 50),
                 "run_to_callback_p50_ms": percentile(callbacks, 50),
                 "run_to_entry_first_ms": entries[0] if entries else "",
                 "run_to_entry_warm_p50_ms": percentile(entries[1:], 50),
                 "oom_killed": data.get("oom_killed", "unrecorded"),
                 "event_errors": data.get("event_errors", 0)})
if not rows:
    raise SystemExit("no batch result files")
with (root/"summary.csv").open("w") as f:
    writer = csv.DictWriter(f, fieldnames=list(rows[0]))
    writer.writeheader()
    writer.writerows(rows)
print("| Backend | Concurrency | Success | Builds/s | Build p50 ms | Build p95 ms | Payload cgroup peak MiB | With Docker services peak MiB |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
for row in rows:
    print("| " + " | ".join(str(row[k]) for k in list(row)[:8]) + " |")
