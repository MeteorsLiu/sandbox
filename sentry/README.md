# Sentry Shared Library

This module builds `sentrylib.so`, the gVisor Sentry backend for the `github.com/xgo-dev/sandbox` library. It owns Sentry startup, the Systrap platform, configured filesystems and the context-switch inspection hook. The caller owns closure/value transfer and the guest entry.

The modules communicate through the C ABI in [sandbox.h](sandbox.h). There is no Go dependency in either direction. The host keeps its own copy of the ABI declarations in `bridge.h`; downloading the host Go module does not require these Sentry sources.

## Build

Build on Linux with Go 1.26.6, cgo and a C compiler for the target architecture:

```sh
bash build-linux.sh /tmp/llar-sandbox
```

The output contains `sentrylib.so`, the generated `sentrylib.h`, and `sandbox.h`. The Go host loads only the `.so` at runtime. Its `Sandbox.Library` field selects a path; the default is `sentrylib.so` beside the host executable.

## Releases

Pushing a tag matching `sentry/v*`, such as `sentry/v0.1.0`, runs GoReleaser to build both Linux architectures and upload these shared libraries directly to the matching GitHub Release after both builds succeed:

- `sentrylib-linux-amd64.so`
- `sentrylib-linux-arm64.so`

Builds use Go 1.26.6 in a Debian Bookworm container, with an ARM64 cross compiler. GoReleaser also uploads `checksums.txt`; C headers are available from a local build. Set `Sandbox.Library` to the downloaded file's path, or rename it to `sentrylib.so` beside the host executable. The runtime conditions below still apply.

The workflow can also be dispatched with an existing Sentry tag to retry a failed release. It takes the release configuration from the workflow commit and builds the source at the requested tag. The tag prefix is checked and HEAD must match that tag with a clean checkout; GoReleaser's own validation is skipped because its open-source edition does not parse `sentry/v*` as a semantic version.

## C Entry

```c
int RunSandbox(char *config, int image_fd, uintptr_t main_pc, uintptr_t entry_pc,
                 uintptr_t owner, inspect_fn inspect, char *message, size_t capacity);
```

- `config` is a NUL-terminated JSON object containing `guest` (the executable's absolute guest path) and `mounts`. Sentry starts the executable with that path as `argv[0]` and no extra arguments. Mount entries contain `type`, optional `source`, `target` and optional `options` (an array of strings).
- `image_fd` is a caller-owned descriptor imported as guest fd 3. The library does not interpret its contents. Guest fd 0, 1 and 2 are imported from host stdin, stdout and stderr.
- `main_pc` is the guest virtual address to redirect; the caller supplies its `main.main` address. The caller must verify that the symbol contains at least 5 bytes on AMD64 or 4 bytes on ARM64. Before starting guest tasks, the library writes a relative branch into this private executable mapping using Sentry's existing memory manager.
- `entry_pc` is the guest virtual address of the caller's private, non-capturing Go `func()` startup entry. It runs after Go package initialization and returns after exporting closure results. The caller owns ELF symbol resolution, guest code and value reconstruction. The library checks branch range and alignment; it does not interpret the closure image. Unsupported branch layouts fail before creating the guest.
- `owner` is an opaque integer passed unchanged to `inspect`. A Go caller can use a `cgo.Handle` owned by its own runtime.
- `inspect` is an optional synchronous callback. It receives a borrowed `syscall_event` containing the syscall number, name, six arguments and a memory mapping callback. The caller owns argument parsing and may change the registers directly. A null callback skips inspection setup. Guest pointer arguments must not be dereferenced in the host.
- `message` is a writable error buffer of `capacity` bytes. A nonzero result indicates an error; zero means that the guest exited successfully.

All supplied strings, buffers and callback state must remain valid until `RunSandbox` returns. Each event, its name and its context handle are borrowed only for the duration of `inspect`. Calls are serialized inside the library. Load one library per host process and keep it loaded: its Go runtime and Systrap workers retain executable code for the process lifetime.

The C entry name stays `RunSandbox`; releases use Go module version tags. In `sentry/v0.4.0`, the first string changes from an executable path to startup JSON; the C parameter types and syscall callback layout remain unchanged. The host and shared library must both use this contract. An older path is rejected as invalid JSON; an older backend cannot interpret the new configuration as an executable path. The earlier `sentry/v0.1.0` library also has an incompatible C signature and must not be loaded. Go module version selection does not validate a library loaded with `dlopen`.

Example startup configuration:

```json
{
  "guest": "/opt/llar/llar",
  "mounts": [
    {"type": "bind", "source": "/", "target": "/", "options": ["ro"]},
    {"type": "bind", "source": "/var/tmp/build", "target": "/work", "options": ["rw"]},
    {"type": "tmpfs", "target": "/tmp", "options": ["mode=1777", "size=256m"]},
    {"type": "proc", "target": "/proc"}
  ]
}
```

The backend requires an explicit list beginning with a `bind` or `tmpfs` root at `/`. The Go host supplies the default read-only `/` and guest `/proc` when its `Mounts` is empty. Bind sources must be absolute host directory paths. Targets are absolute guest paths; mounts are applied in order, and duplicate targets are rejected. Parent mounts and overlay layers must precede their users. Missing directory mountpoints use Sentry's synthetic-mountpoint support and require a writable parent mount; targets under a read-only parent must already exist.

Registered types are `bind` (translated to gofer with DirectFS), `tmpfs`, `proc` and `overlay`. Common options are `ro`/`rw`, `noexec`/`exec`, `nosuid`/`suid` and `noatime`/`atime`, with the last option in a pair taking precedence. Tmpfs and overlay filesystem options are passed to Sentry as mount data; unsupported options fail. For overlay, `lowerdir` and `upperdir` refer to paths in the guest namespace, not host paths. An upper tmpfs makes changes temporary; use writable binds for persistent outputs. Mount namespaces, tmpfs contents and bind connections are released after the call, including partial setup failures.

The event's `mmap(context, address, size, memory)` callback allocates anonymous temporary guest pages using `MMap` and pins them. Address `0` requests `size` zeroed bytes without a source; a nonzero address initializes the pages using `CopyIn`. Sentry chooses a vacant guest address. The returned `syscall_memory` contains a host `data` pointer directly mapping those pages, their guest `address`, and the initialized `length`. The host and guest addresses need not match. Editing `data` immediately changes the temporary pages without changing the original guest bytes. There is no caller-owned staging buffer or write/commit callback; initialization from a source copies bytes and is not COW. Address-zero allocation requires `sentry/v0.3.0` or later; it is not supported by `sentry/v0.2.0`.

**Pointer safety:** New syscall buffers must be allocated through the memory callback, and injected pointer arguments and nested pointers must use the returned guest `address`. Never inject host Go/C addresses, including a `malloc`/`CString` pointer or the returned host `data` pointer. Copy intended payload bytes into `data`, but encode embedded pointers as guest addresses. Sentry resolves syscall pointers in the guest address space: a host pointer may cause `EFAULT`, access unrelated guest memory, or leak a host address. This is a security requirement for the interceptor; Sentry does not identify host-pointer provenance or automatically relocate values.

Memory access expires when `inspect` returns; the data pointer must not escape that callback, and the mapped contents must not contain host Go or C pointers. A partial read returns the initialized prefix and an optional C-allocated error string, which the caller frees with `free`. An inspection `failure` is a C-allocated error string consumed and freed by Sentry; it aborts execution and releases temporary mappings.

[inspect_linux.go](inspect_linux.go) implements the memory callback. [memory_linux.go](memory_linux.go) uses `MapInternal` to expose pinned pages; when those pages are fragmented, it maps their backing file ranges into a contiguous host reservation. It wraps registered syscall functions once to release temporary mappings after execution. Restarted calls and calls skipped before dispatch are retained until kernel teardown. The wrapper does not decode arguments or copy outputs to original guest buffers. Temporary addresses must not be retained by the syscall or unmapped/remapped by guest threads; these operations are not currently prevented.

```text
guest syscall
    -> seccomp / SIGSYS / sysmsg
    -> underlying Context.Switch returns
    -> host inspection callback
       -> MMap: MMap + Pin; CopyIn only for nonzero source addresses
       <- host alias + guest address
       -> edit temporary pages and explicitly replace arguments/pointers
    -> updated registers
    -> Sentry syscall dispatch
    -> release temporary mappings
    -> next Context.Switch resumes the guest
```

The hook is implemented in [platform_linux.go](platform_linux.go). It does not modify gVisor's sysmsg queues, futex handoff or kernel syscall implementations. [run_linux.go](run_linux.go) creates a new Sentry kernel/guest for each call; the Systrap platform is retained between calls.

## Runtime Conditions

Start the host with `GLIBC_TUNABLES=glibc.pthread.rseq=0`. Systrap's ptrace/seccomp initialization must be permitted by the surrounding environment. The guest uses UID/GID 1000 and working directory `/`; environment configuration is not exposed. Bind mounts use in-process LISAFS services for DirectFS. The executable, loader and shared libraries must be visible in the configured namespace. Filesystem permissions still apply in addition to mount flags.

Native execution has been verified on Linux ARM64 with 4 KiB pages, including LLAR formula execution and syscall rewriting through the host callback. AMD64 builds are verified; native AMD64 Sentry execution and other page sizes still need validation. Both Go runtimes share the host OS address space and signal dispositions. Dependency separation through c-shared is not itself a memory protection boundary inside the host.

The startup code in `boot_linux.go` and `fs_linux.go` is adapted from gVisor Go-export commit `d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382`; original notices and the Apache 2.0 license are retained. The gVisor dependency is pinned in `go.mod`, and its sources are unmodified.
