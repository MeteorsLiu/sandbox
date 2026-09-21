#!/usr/bin/env python3
"""Keep timing-only repeats separate from memory-profiled runs."""
import collections
import csv
import json
import math
import pathlib
import statistics
import sys


def percentile(values, p):
    return round(sorted(values)[math.ceil(len(values) * p / 100) - 1], 3) if values else "N/A"


def table(headers, rows):
    print("| " + " | ".join(headers) + " |")
    print("| " + " | ".join("---" for _ in headers) + " |")
    for row in rows:
        print("| " + " | ".join(map(str, row)) + " |")
    print()


def report(root):
    if (root / "environment.json").exists():
        paths = sorted(root.glob("*-c*.json"))
    else:
        paths = sorted(root.glob("timing/*/*/*-c*.json")) + sorted(root.glob("memory/*/*-c*.json"))
    timing, memory, rows, errors = collections.defaultdict(list), [], [], []
    cpu_sets = set()
    for path in paths:
        data = json.loads(path.read_text())
        if "records" not in data:
            continue
        env = json.loads((path.parent / "environment.json").read_text())
        cpu_sets.add(",".join(map(str, env["host_process"]["cpus"])))
        rows.append({"source": str(path.relative_to(root)), "measure": data["measure"],
                     "backend": data["backend"], "concurrency": data["concurrency"],
                     "successful": data["successful"], "count": data["count"],
                     "builds_per_second": data["builds_per_second"]})
        if data["successful"] != data["count"]:
            errors.append(f"{path}: {data['successful']}/{data['count']} verified builds")
        if data["measure"] == "timing":
            timing[data["backend"], data["concurrency"]].append(data)
        else:
            sampled = data.get("callback_memory", {})
            problems = data.get("engine_accounting", {}).get("errors", []) + sampled.get("errors", [])
            if problems or not sampled.get("samples"):
                errors.append(f"{path}: invalid memory measurement: {problems or 'no samples'}")
            else:
                memory.append(data)
    if not rows:
        raise SystemExit("no batch results")
    with (root / "summary.csv").open("w") as out:
        writer = csv.DictWriter(out, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    print("# Formula Benchmark\n")
    print("Selected host CPUs: " + "; ".join(sorted(cpu_sets)) + ". Sentry and Firecracker run natively; only Docker uses containers.\n")

    if timing:
        print("## Timing Without Memory Sampling\n")
        print("Throughput includes startup, Formula preparation, state transfer and exit. Median/range are across batch repeats; OnBuild P50 is the in-process callback time, excluding transfers. Preflight builds are excluded.\n")
        results, starts = [], []
        for (backend, concurrency), repeats in sorted(timing.items()):
            rates = [run["builds_per_second"] for run in repeats]
            records = [record for run in repeats for record in run["records"] if record["success"]]
            outcomes = [event for record in records for event in record["events"] if event["event"] == "result"]
            results.append([backend, concurrency, len(repeats),
                            f"{sum(run['successful'] for run in repeats)}/{sum(run['count'] for run in repeats)}",
                            round(statistics.median(rates), 4), f"{min(rates):.4f} .. {max(rates):.4f}",
                            percentile([e["callback_duration_ns"] / 1e6 for e in outcomes], 50)])
            if concurrency == 1:
                starts.append([backend, len(records),
                               percentile([r["launch_to_main_ns"] / 1e6 for r in records if r.get("launch_to_main_ns") is not None], 50),
                               percentile([r["launch_to_callback_ns"] / 1e6 for r in records if r.get("launch_to_callback_ns") is not None], 50)])
        table(["Backend", "Concurrency", "Repeats", "Verified builds", "Builds/s median", "Builds/s range", "OnBuild P50 ms"], results)
        print("### Fresh Process Startup, Concurrency 1\n")
        print("Host-observed launch times include launcher and marker delivery. Sentry main is the host main; Docker/Firecracker main is inside the container/VM. Callback entry follows Formula preparation and, for Sentry, state restoration.\n")
        table(["Backend", "Fresh processes", "Launch to main P50 ms", "Launch to callback P50 ms"], starts)
        sentry = timing.get(("sandbox", 1), [])
        if sentry:
            outcomes = [e for run in sentry for r in run["records"] if r["success"] for e in r["events"] if e["event"] == "result"]
            print("### Sentry Run Phases, Concurrency 1\n")
            table(["Phase", "P50 ms"], [[label, percentile([e[key] / 1e6 for e in outcomes if key in e], 50)] for key, label in (
                ("entry_ns", "Run to guest entry"), ("before_callback_ns", "Run to callback"),
                ("callback_duration_ns", "OnBuild"), ("after_callback_ns", "Callback end to returned result"))])

    if memory:
        print("## Callback Memory, Separate Profile Runs\n")
        print("These runs are excluded from throughput. Only complete samples within callbacks are included; Save/Load and validation are excluded. Docker services are counted once for Docker only.\n")
        resident, full, sentry, docker_cgroups = [], [], [], []
        for data in sorted(memory, key=lambda d: (d["backend"], d["concurrency"])):
            backend, concurrency = data["backend"], data["concurrency"]
            sampled = data["callback_memory"]
            def mib(key, p=50):
                return round(sampled["bytes"][key][f"p{p}"] / 1048576, 2)
            if backend == "sandbox":
                metric, key = "Attributed Sentry + guest; host LLAR excluded", "sentry_guest_bytes"
                sentry.append([concurrency, mib("sentry_runtime_pss_bytes"), mib("guest_backing_bytes"),
                               mib("host_runtime_pss_bytes"), mib("unattributed_pss_bytes")])
            else:
                metric = "Payload + Docker services PSS" if backend == "docker" else "Native VMM + guest PSS"
                key = "with_runtime_pss_bytes"
            resident.append([backend, concurrency, sampled["samples"], metric, mib(key), mib(key, 95)])
            full.append([backend, concurrency, mib("pss_bytes"), mib("docker_services_pss_bytes")])
            if backend == "docker":
                docker_cgroups.append([concurrency, mib("with_docker_services_cgroup_bytes")])
        table(["Backend", "Concurrency", "Samples", "Resident metric", "P50 MiB", "P95 MiB"], resident)
        print("Sentry attribution adds tagged runtime/library PSS, unique resident memfd blocks and helper PSS; unattributed host mappings and kernel/cache memory are excluded. It is not the full-process PSS or cgroup total.\n")
        if sentry:
            table(["Sentry concurrency", "Runtime P50 MiB", "Guest backing P50 MiB", "Excluded host runtime P50 MiB", "Unattributed P50 MiB"], sentry)
        print("### Full Accounting\n")
        print("Payload PSS includes host LLAR for Sentry. Native workloads inherit the runner's cgroup and namespaces; no workload cgroup is created. Component percentiles do not necessarily sum to a total percentile.\n")
        table(["Backend", "Concurrency", "Payload PSS P50 MiB", "Docker services PSS P50 MiB"], full)
        if docker_cgroups:
            print("Docker's cache-inclusive cgroup counters are separate from PSS and have no native-workload equivalent in this experiment.\n")
            table(["Docker concurrency", "Payload + Docker services cgroup P50 MiB"], docker_cgroups)
    if errors:
        print("## Invalid Measurements\n")
        for error in errors:
            print("- " + error)
        raise SystemExit(1)


if __name__ == "__main__":
    report(pathlib.Path(sys.argv[1]))
