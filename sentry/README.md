# Sentry Shared Library

This module builds `sentrylib.so`, the gVisor Sentry backend for the `github.com/xgo-dev/sandbox` library. It owns Sentry startup, the Systrap platform, the read-only DirectFS root and the context-switch inspection hook. The caller owns closure/value transfer and the guest entry.

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

Builds use Go 1.26.6 in a Debian Bookworm container, with an ARM64 cross compiler. GoReleaser also uploads the generated C headers and `checksums.txt`. Set `Sandbox.Library` to the downloaded file's path, or rename it to `sentrylib.so` beside the host executable. The runtime conditions below still apply.

The workflow can also be dispatched with an existing Sentry tag to retry a failed release. It takes the release configuration from the workflow commit and builds the source at the requested tag. The tag prefix is checked and HEAD must match that tag with a clean checkout; GoReleaser's own validation is skipped because its open-source edition does not parse `sentry/v*` as a semantic version.

## C Entry

```c
int RunSandboxAtV2(char *guest, int image_fd, uintptr_t main_pc, uintptr_t entry_pc,
                 uintptr_t owner, inspect_fn inspect, char *message, size_t capacity);
```

- `guest` is the guest executable's absolute path. Sentry starts it with that path as `argv[0]` and no extra arguments.
- `image_fd` is a caller-owned descriptor imported as guest fd 3. The library does not interpret its contents. Guest fd 0, 1 and 2 are imported from host stdin, stdout and stderr.
- `main_pc` is the guest virtual address to redirect; the caller supplies its `main.main` address. The caller must verify that the symbol contains at least 5 bytes on AMD64 or 4 bytes on ARM64. Before starting guest tasks, the library writes a relative branch into this private executable mapping using Sentry's existing memory manager.
- `entry_pc` is the guest virtual address of the caller's private, non-capturing Go `func()` startup entry. It runs after Go package initialization and returns after exporting closure results. The caller owns ELF symbol resolution, guest code and value reconstruction. The library checks branch range and alignment; it does not interpret the closure image. Unsupported branch layouts fail before creating the guest.
- `owner` is an opaque integer passed unchanged to `inspect`. A Go caller can use a `cgo.Handle` owned by its own runtime.
- `inspect` is an optional synchronous callback. It receives a borrowed `syscall_event` containing the syscall number, name, six arguments and memory/codec callbacks. It may change the registers directly or call `decode` and `rewrite` for structured input edits. A null callback skips inspection setup. Guest pointer arguments must not be dereferenced in the host.
- `message` is a writable error buffer of `capacity` bytes. A nonzero result indicates an error; zero means that the guest exited successfully.

All supplied strings, buffers and callback state must remain valid until `RunSandboxAtV2` returns. Each event, its name and its context handle are borrowed only for the duration of `inspect`. Calls are serialized inside the library. Load one library per host process and keep it loaded: its Go runtime and Systrap workers retain executable code for the process lifetime.

`RunSandboxAtV2` replaces the older `RunSandbox` and `RunSandboxAt` symbols. This revision is unreleased and requires a matching host build. Distinct symbols make an incompatible host/library combination fail during symbol lookup rather than calling an incompatible C signature.

The event's `read` callback copies guest bytes into a caller-owned buffer and reports both a byte count and an optional error. `decode` returns a JSON argument snapshot under a caller-supplied guest-read budget. `rewrite` accepts the edited JSON snapshot, encodes changed input values in guest memory and updates the event's registers. Returned JSON and error strings are allocated with the C allocator; the caller frees them with `free`. No callback retains a caller buffer. The host implementation serializes these operations and invalidates them before returning from `inspect`.

[codec_linux.go](codec_linux.go) owns the structured codecs. [inspect_linux.go](inspect_linux.go) performs memory operations through Sentry's memory manager and exports the synchronous C callbacks. Generated `strace_amd64.go` and `strace_arm64.go` contain the pinned gVisor strace argument classifications; regenerate them with `go generate .` from this module. The generator reads the dependency's Go AST without modifying it. Display classifications do not always specify a writable layout, so output, in/out and unsupported overloaded arguments remain raw. See the root README for the supported representations and lifetime limits.

```text
guest syscall
    -> seccomp / SIGSYS / sysmsg
    -> underlying Context.Switch returns
    -> host inspection callback
    -> updated registers
    -> Sentry syscall dispatch
    -> next Context.Switch resumes the guest
```

The hook is implemented in [platform_linux.go](platform_linux.go). It does not modify gVisor's sysmsg queues, futex handoff or kernel syscall implementations. [run_linux.go](run_linux.go) creates a new Sentry kernel/guest for each call; the Systrap platform is retained between calls.

## Runtime Conditions

Start the host with `GLIBC_TUNABLES=glibc.pthread.rseq=0`. Systrap's ptrace/seccomp initialization must be permitted by the surrounding environment. The current guest uses UID/GID 1000, working directory `/`, a read-only host root and an in-process LISAFS service for DirectFS. These are the existing backend settings, not a configurable filesystem or environment policy.

Native execution has been verified on Linux ARM64 with 4 KiB pages, including LLAR formula execution and syscall rewriting through the host callback. AMD64 builds are verified; native AMD64 Sentry execution and other page sizes still need validation. Both Go runtimes share the host OS address space and signal dispositions. Dependency separation through c-shared is not itself a memory protection boundary inside the host.

The startup code in `boot_linux.go` and `fs_linux.go` is adapted from gVisor Go-export commit `d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382`; original notices and the Apache 2.0 license are retained. The gVisor dependency is pinned in `go.mod`, and its sources are unmodified.
