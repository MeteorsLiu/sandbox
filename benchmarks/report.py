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
    entries = [e["entry_ns"]/1e6 for r in data["records"] for e in r["events"] if e.get("entry_ns")]
    def median(values):
        return round(sorted(values)[len(values)//2], 3) if values else ""
    rows.append({"backend": data["backend"], "concurrency": data["concurrency"],
                 "success": f"{data['successful']}/{data['count']}",
                 "builds_per_second": round(data["builds_per_second"], 4),
                 "build_p50_ms": round(data["build_latency"].get("p50_ns", 0)/1e6, 3),
                 "build_p95_ms": round(data["build_latency"].get("p95_ns", 0)/1e6, 3),
                 "peak_memory_mib": round(data["memory_peak_bytes"]/1048576, 2),
                 "launch_to_entry_p50_ms": median(starts), "run_to_entry_p50_ms": median(entries),
                 "oom_killed": data.get("oom_killed", "unrecorded")})
if not rows:
    raise SystemExit("no batch result files")
with (root/"summary.csv").open("w") as f:
    writer = csv.DictWriter(f, fieldnames=list(rows[0]))
    writer.writeheader()
    writer.writerows(rows)
print("| Backend | Concurrency | Success | Builds/s | Build p50 ms | Build p95 ms | Peak MiB |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
for row in rows:
    print("| " + " | ".join(str(row[k]) for k in list(row)[:7]) + " |")
