# LLAR Formula benchmarks

The payload is the unmodified `madler/zlib/v1.3.1/zlib_llar.gox` from xgo-dev/llarhub commit `1c2b4666ef598ed51afeb44221a73f53eea7e65c`, building zlib commit `51b7f2abdade71cd9bb0e7a373ef2610ec6f9daf`. Preparation records input hashes. Every successful sample must install a nonempty static library, header, license and pkg-config file, and return `-lz` metadata. Downloads and image construction happen before timing.

```sh
docker build -f benchmarks/Dockerfile -t sandbox-formula-bench:local .
python3 benchmarks/run.py --image sandbox-formula-bench:local \
  --output /tmp/formula-results --backends sandbox,docker \
  --concurrency 1,2,4,8 --waves 3
```

For all three backends, use a native Linux x86_64 machine with Docker, Python 3, e2fsprogs and readable/writable `/dev/kvm`:

```sh
sudo bash benchmarks/setup-firecracker.sh sandbox-formula-bench:local /tmp/firecracker-assets
sudo python3 benchmarks/run.py --image sandbox-formula-bench:local \
  --output /tmp/formula-results --firecracker-assets /tmp/firecracker-assets
```

The `Formula benchmark` workflow provisions this environment on `ubuntu-latest`. Firecracker v1.17.0 boots a read-only ext4 export of the exact Docker payload image; it mounts private writable work/tmp directories and runs the same executable as UID/GID 1000. Kernel URL and SHA-256 are retained. No network or build cache is available during execution. Every backend uses the selected CPU affinity, container memory ceiling, toolchain and `MAKEFLAGS=-j1`. Firecracker guest RAM is configured to that same ceiling; its actual consumption is measured, not inferred from the configuration.

## Measurements

- `launch_to_entry_ns` is host-observed time from `docker start -a` to the entry event. Docker/Firecracker emit it at `main.main`; sandbox emits it at the redirected guest entry. All three include their outer Docker launch. For sandbox this also includes host Formula preparation and export. Marker delivery overhead is included; these are not bare-kernel boot times.
- Serial sandbox samples additionally report `entry_ns`: `Sandbox.Run` to guest entry, after Go initialization but before `state.Load`. The benchmark-only Go overlay adds a tagged `getpid` syscall there. Library sources and production behavior are unchanged. Parallel runs disable this inspector and do not guess which Run an entry belongs to.
- `duration_ns` times `OnBuild` plus validation; sandbox includes state export, guest execution and writeback. The batch throughput includes Formula preparation and container startup/cleanup. Raw events distinguish these phases.
- The controller samples raw Docker API `memory_stats.usage` every 100 ms and sums active containers. This includes page cache, the sandbox host/Sentry/workers, or the Firecracker VMM and resident guest memory. The common Docker daemon is excluded. Shared-page charge ownership follows cgroup accounting, not summed process RSS; peaks shorter than the sampling interval can be missed.
- Concurrency uses independent Formula instances and contexts. The sandbox case shares one Kernel; Docker and Firecracker create one execution environment per build. Interpreter construction finishes before concurrent sandbox transfers, as required by the current type-cache contract. Failures remain failures and are excluded from successful latency percentiles.

Each run writes environment metadata, container logs, per-task JSON, raw memory samples and batch summaries. Do not compare local Docker Desktop ARM64 results with native amd64 runner results as if they came from the same machine.
