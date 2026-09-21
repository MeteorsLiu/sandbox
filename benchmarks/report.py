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
    memory = data.get("callback_memory", {})
    def memory_mib(key, percentile=50):
        if memory.get("errors") or key not in memory.get("bytes", {}):
            return "N/A"
        return round(memory["bytes"][key][f"p{percentile}"]/1048576, 2)
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
                 "event_errors": data.get("event_errors", 0),
                 "callback_memory_samples": memory.get("samples", 0),
                 "callback_payload_cgroup_p50_mib": memory_mib("payload_cgroup_bytes"),
                 "callback_payload_pss_p50_mib": memory_mib("pss_bytes"),
                 "callback_docker_services_cgroup_p50_mib": memory_mib("docker_services_cgroup_bytes"),
                 "callback_with_docker_services_cgroup_p50_mib": memory_mib("with_docker_services_cgroup_bytes"),
                 "callback_sentry_guest_p50_mib": memory_mib("sentry_guest_bytes") if data["backend"] == "sandbox" else "N/A",
                 "callback_sentry_guest_p95_mib": memory_mib("sentry_guest_bytes", 95) if data["backend"] == "sandbox" else "N/A",
                 "callback_sentry_runtime_p50_mib": memory_mib("sentry_runtime_pss_bytes"),
                 "callback_guest_backing_p50_mib": memory_mib("guest_backing_bytes"),
                 "callback_host_runtime_p50_mib": memory_mib("host_runtime_pss_bytes"),
                 "callback_unattributed_p50_mib": memory_mib("unattributed_pss_bytes")})
if not rows:
    raise SystemExit("no batch result files")
with (root/"summary.csv").open("w") as f:
    writer = csv.DictWriter(f, fieldnames=list(rows[0]))
    writer.writeheader()
    writer.writerows(rows)
print("### Callback Memory\n")
print("Complete callback-only samples; state transfers and result validation are excluded. Cgroup counters include cache; PSS measures mapped resident pages.\n")
print("| Backend | Concurrency | Samples | Payload cgroup P50 MiB | Payload PSS P50 MiB | Docker services cgroup P50 MiB | Payload + Docker services cgroup P50 MiB |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
for row in rows:
    print("| " + " | ".join(str(row[k]) for k in ("backend", "concurrency", "callback_memory_samples",
          "callback_payload_cgroup_p50_mib", "callback_payload_pss_p50_mib",
          "callback_docker_services_cgroup_p50_mib", "callback_with_docker_services_cgroup_p50_mib")) + " |")
if any(row["backend"] == "sandbox" for row in rows):
    print("\n### Sentry And Guest Without Host LLAR\n")
    print("Attributed resident memory: Sentry runtime/library PSS, guest/platform/transfer memfd blocks counted once, and helper PSS excluding those memfds. Host mappings and unattributed mappings are excluded; this is not the full cgroup total. Component percentiles do not sum to the total percentile.\n")
    print("| Concurrency | Attributed P50 MiB | Attributed P95 MiB | Sentry runtime P50 MiB | Guest backing P50 MiB | Excluded host runtime P50 MiB | Unattributed P50 MiB |")
    print("| ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    for row in rows:
        if row["backend"] == "sandbox":
            print("| " + " | ".join(str(row[k]) for k in ("concurrency", "callback_sentry_guest_p50_mib",
                  "callback_sentry_guest_p95_mib", "callback_sentry_runtime_p50_mib", "callback_guest_backing_p50_mib",
                  "callback_host_runtime_p50_mib", "callback_unattributed_p50_mib")) + " |")
print("\n### Full Lifetime And Timing\n")
print("| Backend | Concurrency | Success | Builds/s | Build p50 ms | Build p95 ms | Payload cgroup peak MiB | With Docker services peak MiB |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
for row in rows:
    print("| " + " | ".join(str(row[k]) for k in list(row)[:8]) + " |")
