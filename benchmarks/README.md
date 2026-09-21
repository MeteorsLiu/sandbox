# LLAR Formula benchmarks

The workload is the unmodified `madler/zlib/v1.3.1/zlib_llar.gox` from xgo-dev/llarhub commit `1c2b4666ef598ed51afeb44221a73f53eea7e65c`, building zlib commit `51b7f2abdade71cd9bb0e7a373ef2610ec6f9daf`. Each job calls the loaded Formula's `OnBuild` directly on a pristine source copy. The `llar make` cache is never consulted. A successful build must run configure, compile the same 19 source/object pairs, archive the same 15 library members, install nonempty artifacts and return `-lz` metadata. `audit.py` rejects differing normalized compiler commands across backends.

## Native Execution

Run on native Linux x86_64 with Docker Engine, Python 3, util-linux, e2fsprogs and KVM. Docker builds the common binaries and toolchain image before measurement. Its exported filesystem supplies the same executable, compiler, libraries, Formula and source to all three backends.

- Sentry's host executable runs directly on the runner. It inherits the controller's UID, namespaces, cgroup and CPU affinity. The common filesystem is configured through `Sandbox.Mounts` **inside the Sentry guest only**. The host uses an ordinary temporary directory on the existing `/dev/shm`; no host mounts are created.
- Firecracker's VMM runs directly on the runner using `/dev/kvm`, booting a read-only ext4 export of the common filesystem. It inherits the same host settings. There is no jailer or outer container.
- Only Docker uses Docker containers. The host controller starts them through the native Docker Engine.

There is no native launcher wrapper: no `unshare`, `chroot`, host mount, cgroup creation, affinity setter or UID/GID change. `audit.py` compares each native process's actual namespaces, cgroup, UID and allowed CPUs against the controller. These are read-only observations.

All groups use `MAKEFLAGS=-j1` and every CPU available to the runner. `GOMAXPROCS` and each Firecracker VM's vCPU count equal that CPU count. There are no benchmark-imposed host CPU or memory limits, including for Docker. `--vm-memory-mib` configures Firecracker's guest RAM (4096 MiB per VM by default); it is not a host memory limit. The Formula executes as UID/GID 1000 inside each backend; the native Sentry host and VMM retain the controller's UID. Fresh source/output directories are writable by the Sentry guest without changing the host process's credentials. Shared host page caches are not flushed.

```sh
docker build -f benchmarks/Dockerfile -t sandbox-formula-bench:local .
sudo bash benchmarks/setup-firecracker.sh sandbox-formula-bench:local /tmp/firecracker-assets
sudo python3 benchmarks/run.py --image sandbox-formula-bench:local \
  --output /tmp/formula-results --firecracker-assets /tmp/firecracker-assets
python3 benchmarks/audit.py /tmp/formula-results
```

The measured Sentry command is an ordinary exec, with the image's environment and all available CPUs:

```sh
GLIBC_TUNABLES=glibc.pthread.rseq=0 LANG=C LC_ALL=C HOME=/work TMPDIR=/tmp \
  MAKEFLAGS=-j1 GOMAXPROCS="$(nproc)" /opt/benchmark/formula-bench \
  -backend=sandbox -count=8 -concurrency=4 \
  -work=/dev/shm/formula-bench-EXAMPLE -rootfs=/tmp/firecracker-assets/rootfs
```

`run.py` creates the unique work directory and invokes the executable with `subprocess.Popen`. `-rootfs` is a Sentry guest mount source; it does not change the host filesystem view. The common payload is copied to the host's `/opt/benchmark` during setup, so host and guest execute the identical binary at the same path.

The measured Firecracker command is:

```sh
/tmp/firecracker-assets/firecracker --no-api --config-file /tmp/formula-results/firecracker-c4-0/config.json
```

The generated JSON selects `vmlinux`, read-only `rootfs.ext4`, all available CPUs and the configured guest RAM. VM init starts `/opt/benchmark/formula-bench -backend=direct` as UID 1000. There is no outer Docker or namespace wrapper around the VMM.

Docker creation uses `POST /containers/create` on the Docker Unix socket, with the image's entrypoint, `-backend=direct -count=1 -concurrency=1`, all available Go CPUs, a read-only root, no network, and writable tmpfs `/work` and `/tmp`. The measured launch command is `docker start -a <container-id>`. Container creation is included in batch throughput. No Docker container starts for Sentry or Firecracker measurements; `docker create`/`export` in setup only prepare their common filesystem.

## Timing

The workflow first verifies one build per backend, then runs three timing rounds with rotating backend order. Each case completes eight builds at concurrency 1, 2, 4 and 8. No memory sampler runs during these timing rounds.

`python3 benchmarks/report.py results` produces one combined report: throughput is the median and range across timing repeats, and memory comes exclusively from profile runs. It does not print memory tables for timing-only runs. Missing samples in a memory run are errors, not silent N/A values.

- Batch throughput is verified builds divided by elapsed time from batch dispatch to final process exit. It includes environment startup, Formula preparation, execution, state transfers and exit. Workload audits, JSON reporting and environment deletion follow the measured batch.
- `callback_duration_ns` measures only `OnBuild` inside the executing process using Go's monotonic clock. Sentry returns that measurement through state writeback. It excludes Save/Load and artifact verification.
- `launch_to_main_ns` measures native-host-observed launch to the common executable's `main.main`. For Sentry this is the host main, before preparing the interpreter. For Docker/Firecracker it is main inside the container/VM. Sentry launch is direct exec; Docker includes `docker start -a`; Firecracker includes direct VMM exec and Linux boot. Environment creation before process launch is recorded separately in `create_ns` and included in batch throughput.
- `launch_to_entry_ns` uses the redirected guest entry for Sentry, and main for Docker/Firecracker. It is intentionally a different boundary from Sentry host main. Serial Sentry results also expose Run-to-guest-entry and pre/post-callback times. A benchmark-only source overlay emits the guest entry marker before `state.Load`.
- Marker-based times include stdout delivery; callback duration does not. Process/VM startup is fresh, but these are not cold-disk benchmarks.

Lifecycle is explicit: Sentry uses one host, one shared Kernel, one interpreter per worker and one guest process per build. Docker and Firecracker start one environment per build. All interpreter preparation completes before concurrent Sentry transfers. These are end-to-end deployment throughput numbers, not identical-lifecycle interpreter microbenchmarks.

## Memory

Memory runs repeat the workload separately. `engine_memory.py` reads each native workload's PID tree and `/proc` residency, with a 100 ms wait between reads. Docker additionally uses its existing service/container cgroup counters. Only complete samples within callback windows enter the main P50/P95 tables; samples overlapping Save/Load or validation are excluded. Full-lifetime raw samples are retained. Native PID identity includes process start time, preventing unrelated reused PIDs from being counted. No native workload is moved into a new cgroup for accounting.

Before summing PSS, `kcmp(KCMP_VM)` groups PIDs that share one address space, as systrap's `CLONE_VM` helpers do. Each address space contributes once; raw samples retain the PID groups. Memfd backing pages are independently deduplicated by device/inode. A process exiting during comparison discards that incomplete sample, with the discard count recorded. Permission errors or unavailable `kcmp` fail measurement rather than falling back to PID sums. Linux amd64 and arm64 syscall numbers are supported; the CI runner exercises a real shared-VM/fork regression before benchmarking.

- Docker: payload processes plus `docker.service` and `containerd.service`, including shims, counted once per sample. Both cache-inclusive cgroup counters and mapped resident PSS are retained.
- Firecracker: native VMM and guest residency. Docker services are not charged to it.
- Sentry: attributed Sentry and guest residency, separately from full payload and host LLAR. A benchmark-only overlay labels the shared library Go runtime's VMAs `Sentry Go:`. The sampler adds its runtime/library PSS, resident guest/platform/transfer memfd blocks deduplicated by device/inode, and helper PSS excluding those memfds. It excludes the host Go runtime/executable and reports unattributed mappings separately. This attributed total excludes unmapped filesystem cache and kernel memory, so it must not be equated with `memory.current`.

The controller and CLI clients are excluded. Sampling is not atomic and can miss short peaks. Unattributed Sentry mappings remain an explicit limitation. Memfd block accounting is rejected if the host uses swap; the harness does not disable swap or apply a memory policy. Native PSS is never labeled as `memory.current`. Memory-run timings are not used for primary throughput results. Raw logs, events, compile audits, native commands/PIDs/inherited settings, input and binary hashes, image ID, source revision and environment metadata are uploaded with every workflow run.
