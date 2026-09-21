# LLAR Formula benchmarks

The payload is the unmodified `madler/zlib/v1.3.1/zlib_llar.gox` from xgo-dev/llarhub commit `1c2b4666ef598ed51afeb44221a73f53eea7e65c`, building zlib commit `51b7f2abdade71cd9bb0e7a373ef2610ec6f9daf`. Preparation records input hashes. Every successful sample must install a nonempty static library, header, license and pkg-config file, and return `-lz` metadata. Downloads and image construction happen before timing.

All backends call the loaded Formula's `OnBuild` directly; the `llar make` build-cache lookup is not on this path. Every job copies pristine source into its own directory on a fresh `/work` tmpfs. The controller also requires one callback start, one configure, all 19 expected source/object compilation pairs and one archive with the 15 zlib members per build. Missing or repeated work fails the run even if artifacts already exist. Normalized compiler-command hashes are retained for comparison between jobs and backends. This verifies build work; it does not claim a cold host filesystem page cache.

```sh
docker build -f benchmarks/Dockerfile -t sandbox-formula-bench:local .
python3 benchmarks/run.py --image sandbox-formula-bench:local \
  --output /tmp/formula-results --backends sandbox,docker \
  --concurrency 1,2,4,8 --builds 8
```

For all three backends, use a native Linux x86_64 machine with Docker, Python 3, e2fsprogs and readable/writable `/dev/kvm`:

```sh
sudo bash benchmarks/setup-firecracker.sh sandbox-formula-bench:local /tmp/firecracker-assets
sudo python3 benchmarks/run.py --image sandbox-formula-bench:local \
  --output /tmp/formula-results --firecracker-assets /tmp/firecracker-assets
```

The `Formula benchmark` workflow provisions this environment on `ubuntu-latest`. Firecracker v1.17.0 and the pinned CI guest kernel 6.18.48 boot a read-only ext4 export of the exact Docker payload image, including its environment variables. It mounts private writable work/tmp directories and runs the same executable as UID/GID 1000. Kernel URL and SHA-256 are retained. No network, swap or build cache is available during execution. Every backend shares the same selected host CPU affinity, toolchain and `MAKEFLAGS=-j1`. The total memory budget defaults to 4096 MiB: sandbox shares it across its workers; Docker and Firecracker split it equally across concurrent containers. Firecracker guest RAM matches its container share; actual memory usage is measured separately.

## Measurements

- The payload comparison includes LLAR and its build tools for Docker; host LLAR, Sentry, guest and build tools for sandbox; and VMM, guest Linux, LLAR and build tools for Firecracker. The CI wrapper additionally samples `docker.service` and `containerd.service`, including their shims, and reports their sum with payload memory at each sample. These shared services are counted once per sample, not once per concurrent build. All three backends currently use an outer Docker container, so this total includes Docker services for all three. The benchmark controller and its Docker CLI clients remain excluded. Source revision, image identity, binary hashes and input hashes are retained.
- Three independent cold trials create a fresh execution environment and complete one build each. Backend order rotates across trials. These are process/VM cold starts; the host page cache is not globally flushed. `launch_to_callback_ns` uses the same callback-start boundary for all backends. The batch matrix separately measures eight builds at each concurrency with the lifecycle described below.
- `launch_to_entry_ns` is host-observed time from `docker start -a` to the entry event. Docker/Firecracker emit it at `main.main`; sandbox emits it at the redirected guest entry. All three include their outer Docker launch. For sandbox this also includes host Formula preparation and export. Marker delivery overhead is included; these are not bare-kernel boot times.
- Serial sandbox samples additionally report `entry_ns`: host-observed Run-start to guest-entry markers, after Go initialization but before `state.Load`, and `callback_ns`: Run-start to the first closure instruction after restoration. The benchmark-only Go overlay adds a stdout marker at guest entry. Library sources and production behavior are unchanged; no syscall inspector is enabled. All backends send markers through stdout, so delivery overhead is included. Parallel sandbox runs do not guess which Run an entry belongs to. Direct workers also emit a callback-start event so application readiness can be distinguished from reaching main.
- `duration_ns` times `OnBuild` plus validation; sandbox includes state export, guest execution and writeback. The batch throughput includes Formula preparation, container startup, execution, exit and collection, but ends before container deletion. Raw events distinguish these phases.
- The controller samples raw Docker API `memory_stats.usage` with a 100 ms wait between rounds and sums active containers. The optional `engine_memory.py` wrapper reads disjoint cgroup-v2 `memory.current` counters for payloads and Docker services on the native systemd runner, preserving baseline, per-sample breakdowns and runtime-process membership. Totals come from paired samples, not the sum of separate peaks. Both metrics include page cache and remain separate from cache-subtracted Docker CLI displays and Go heap counters. Short peaks can be missed and reads across cgroups are not atomic. State Save/Load and exit GC remain inside the measurement window.
- Every concurrency level completes the same requested number of builds. Each sandbox worker owns one interpreter, reused sequentially with fresh build contexts and directories; workers share one Kernel. Docker and Firecracker create one execution environment per build. Interpreter construction finishes before concurrent sandbox transfers, as required by the current type-cache contract. Failures remain failures and are excluded from successful latency percentiles.

Each run writes environment metadata, container logs, per-task JSON, raw memory samples and batch summaries. Do not compare local Docker Desktop ARM64 results with native amd64 runner results as if they came from the same machine.
