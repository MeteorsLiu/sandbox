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

Pushing a tag matching `sentry/v*`, such as `sentry/v0.1.0`, builds both Linux architectures and uploads these shared libraries to the matching GitHub Release after both builds succeed:

- `sentrylib-linux-amd64.so`
- `sentrylib-linux-arm64.so`

Builds use Go 1.26.6 in Debian Bookworm containers on native runners. Set `Sandbox.Library` to the downloaded file's path, or rename it to `sentrylib.so` beside the host executable. The runtime conditions below still apply.

## C Entry

```c
int RunSandbox(char *guest, int image_fd, uintptr_t owner,
               inspect_fn inspect, char *message, size_t capacity);
```

- `guest` is the guest executable's absolute path. Sentry starts it with the argument `--llar-sandbox-guest`.
- `image_fd` is a caller-owned descriptor imported as guest fd 3. The library does not interpret its contents. Guest fd 0, 1 and 2 are imported from host stdin, stdout and stderr.
- `owner` is an opaque integer passed unchanged to `inspect`. A Go caller can use a `cgo.Handle` owned by its own runtime.
- `inspect` is a required synchronous callback. It receives a borrowed `syscall_event` containing the syscall number and six arguments, and may change those integers before returning. Guest pointer arguments must not be dereferenced in the host.
- `message` is a writable error buffer of `capacity` bytes. A nonzero result indicates an error; zero means that the guest exited successfully.

All supplied strings, buffers and callback state must remain valid until `RunSandbox` returns. Calls are serialized inside the library. Load one library per host process and keep it loaded: its Go runtime and Systrap workers retain executable code for the process lifetime.

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

Start the host with `GLIBC_TUNABLES=glibc.pthread.rseq=0`. Systrap's ptrace/seccomp initialization must be permitted by the surrounding environment. The current guest uses UID/GID 1000, working directory `/`, `GOMAXPROCS=2`, a read-only host root and an in-process LISAFS service for DirectFS. These are the existing backend settings, not a configurable filesystem or environment policy.

Native execution has been verified on Linux ARM64 with 4 KiB pages, including LLAR formula execution and syscall rewriting through the host callback. AMD64 builds are verified; native AMD64 Sentry execution and other page sizes still need validation. Both Go runtimes share the host OS address space and signal dispositions. Dependency separation through c-shared is not itself a memory protection boundary inside the host.

The startup code in `boot_linux.go` and `fs_linux.go` is adapted from gVisor Go-export commit `d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382`; original notices and the Apache 2.0 license are retained. The gVisor dependency is pinned in `go.mod`, and its sources are unmodified.
