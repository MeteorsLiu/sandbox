# Sandbox

`sandbox.Run(fn)` runs a native Go closure in Sentry, then writes changes to captured objects back into the original host objects. The host and guest run the same executable. Native types and code come from that ELF; captured values travel through a memfd. There is no CRIU checkpoint, global STW, or RPC callback proxy in this path.

```go
func main() {
    count := 41
    if err := sandbox.Run(func() { count++ }); err != nil {
        panic(err)
    }
    fmt.Println(count) // 42, in the original host process
}
```

Guest startup is internal to the library. A new guest executes Go runtime and package initialization, then enters the imported closure without executing the application's `main`. Initialization side effects must be appropriate there. Captured objects must be exclusively owned until `Run` returns. The function must finish its own goroutines before returning. Concurrent or nested host calls return an error.

## Modules

The repository contains two independently buildable Go modules:

```text
github.com/xgo-dev/sandbox           sandbox.Run(fn), guest entry, value transfer
github.com/xgo-dev/sandbox/sentry    c-shared Sentry backend -> sentrylib.so
```

Import `github.com/xgo-dev/sandbox` in the host. It loads `sentrylib.so` through `dlopen` and does not import the Sentry module or gVisor. Each module carries its own C ABI declarations so either module can be fetched independently. The ABI carries guest startup addresses, integer syscall fields and a synchronous callback.

Automatic guest startup requires the new `RunSandboxAt` library symbol. Rebuild the Sentry module together with this host revision; the published `sentry/v0.1.0` library exports the older `RunSandbox` entry and is rejected with an explicit loading error. This change has not been released.

LLAR's formula integration stays in its own repository at `experimental/sandbox-formula`, where it can use LLAR's internal formula loader. The sandbox library has no LLAR or ixgo dependency.

## Host Inspection

```go
s := sandbox.Sandbox{
    Library: "/opt/llar/sentrylib.so",
    Inspect: func(call *sandbox.Syscall) {
        // Inspect or change Number and Args before Sentry dispatches them.
        // Pointer arguments are guest addresses, not host pointers.
    },
}
err := s.Run(fn)
```

The default library is `sentrylib.so` beside the executable. One library remains loaded for the process lifetime. Inspector callbacks run synchronously in the original host Go runtime and are serialized; they must not access the captured objects while a call is active. An inspector panic is reported and prevents result writeback, but is not a mechanism for denying the syscall.

```text
Host runtime                           c-shared Sentry runtime
Run(fn)
  closure + reachable values
       |
       +-- memfd, startup addresses ---> CreateProcess(same ELF), guest fd 3
                                           |
                                      install main -> guestEntry branch
                                           |
                                      Start -> Go runtime and package init
                                           |
                                      guestEntry -> reconstruct captures -> fn()
                                           |
                                      syscall -> seccomp/SIGSYS
                                           |
                                      sysmsg -> Context.Switch returns
                                           |
  Inspect(Number, Args) <-------------------+
       |                                   |
       +-- modified Number, Args ---------> Sentry syscall dispatch
                                           |
                                      next Switch -> guest continues
                                           |
                                      fn returns -> export changed graph
                                           |
  wait for guest exit <--------------------+
  copy output out of shared memory
  validate types/code/references
  update original captured objects
Run returns
```

The wrapper remains at `Context.Switch`; the sysmsg/Sentry shared-memory and futex implementation is unchanged. DirectFS uses an in-process LISAFS service, without a separate gofer process. The current root filesystem is the host `/`, mounted read-only in the guest. This is not yet a file-visibility policy or a reproducible build environment.

## Reading Order

1. [host_linux.go](host_linux.go): public `Run`, image ownership, c-shared call, validation and return.
2. [guest_linux.go](guest_linux.go): private guest entry, value restoration, closure invocation and result export.
3. [transfer_linux.go](transfer_linux.go): retained object identities and host writeback.
4. [image_linux.go](image_linux.go): typed graph traversal and relative image offsets.
5. [native_linux.go](native_linux.go): ELF/DWARF type and closure metadata, GC allocation/layout checks.
6. [sentry/run_linux.go](sentry/run_linux.go), `sentry/entry_{amd64,arm64}.go` and [sentry/platform_linux.go](sentry/platform_linux.go): guest entry redirection, startup and syscall interception.

The Sentry build and C entry contract are described in [sentry/README.md](sentry/README.md).

## Build And Run

Use native Linux, Go 1.26.6, cgo and a C compiler. Both executable and library must be built for the same architecture. Do not strip ELF symbols or DWARF, and do not use PIE. Set the glibc tunable before starting the host, not from Go initialization:

```sh
bash sentry/build-linux.sh /tmp/llar-sandbox
bash build-linux.sh /tmp/llar-sandbox
/tmp/llar-sandbox/transfer.test -test.v
GLIBC_TUNABLES=glibc.pthread.rseq=0 /tmp/llar-sandbox/smoke
```

Systrap needs host-kernel support for its ptrace/seccomp setup. A surrounding container's seccomp policy must allow that setup. Verification used an unprivileged Linux ARM64 container with no capabilities, no network, a read-only root and Docker seccomp disabled for Systrap.

## Current Limits

This is an executable module with a general `func()` entry, not yet a production sandbox for arbitrary Go programs or general Linux installations.

- Native Sentry execution is verified on Linux ARM64 with 4 KiB pages and Go 1.26.6. AMD64 builds and value-transfer tests pass under emulation; native AMD64 Sentry execution and other page sizes require validation. The unsupported entry builds on macOS and Windows; these are not Sentry execution targets.
- Private Go runtime ABI checks currently require exactly Go 1.26.6. Dynamic reflect types, generic dictionaries and bound method wrappers are not supported.
- Guest entry redirection requires the `main.main` ELF symbol to contain at least 5 bytes on AMD64 or 4 bytes on ARM64. The private entry must be reachable by a relative jump (signed 32-bit displacement on AMD64, signed 28-bit byte displacement aligned to 4 bytes on ARM64). Unsupported layouts return an error before the guest starts. Only the guest's private executable mapping is patched; the host mapping and executable file are unchanged.
- Channels, non-nil `unsafe.Pointer`, synchronization primitives, timers, cancellation contexts and open file objects cannot be transferred as ordinary values. Known process-local structures are rejected. This is not an exhaustive resource classifier for arbitrary third-party types.
- Package global state, goroutines, stacks, file descriptors and singleton identity such as `io.EOF` are not migrated. `uintptr` stays an integer; pointers hidden inside it are not relocated.
- Shared pointer/map identities, exact slice aliases and cycles are retained. Overlapping slices and some interior-pointer traversal orders are rejected. Each input/output image currently has a 16 MiB limit and a traversal depth limit of 256.
- Native functions already reachable from the input are authorized for host result restoration. Returning a newly created function with a previously unseen code entry is rejected.
- Input objects stay alive through the call. Imported objects stay alive in the guest even after the closure drops its reference, so their mutations can still reach host aliases. Result decoding finishes before writeback starts; a guest panic, exit failure or malformed result prevents that writeback. External syscall side effects are not rolled back.
- The shared library retains a Systrap platform and its process-lifetime workers. Each call creates a fresh Sentry kernel/guest. The two Go runtimes still share OS signal dispositions and the host process address space; c-shared isolates dependencies and runtime heaps, not hostile native code within the host.
- Filesystem/environment policies, cancellation, comprehensive startup-failure cleanup, hostile-image fuzzing and a broader platform/toolchain matrix remain necessary before production use.

## Verification

`sentry/build-linux.sh` builds only the shared library and its C headers. The root `build-linux.sh` builds the generic call smoke test and value-transfer tests, and checks that the host dependency list contains no gVisor packages. Neither script builds the other module.

The smoke test covers automatic guest entry after package initialization without entering application `main`, integer capture, original host PID, GC, pointer/map aliases, cycles, slice growth, interfaces, nested native callbacks, mutation before dropping a reference, guest panic without writeback, static functions, syscall number rewriting, nested-call rejection and unchanged stdin. It also checks for descriptor growth after warming the shared platform.

LLAR's separate formula integration passes `OnBuild(ctx)` with both `Project.ReadFile` and `os.ReadFile`, a native output-directory callback, and result writeback to the original `Context` and `Project`. Both ARM64 and emulated AMD64 value-transfer tests reject malformed lengths, unauthorized code entries, unknown types, changed retention counts, merged identities and overlapping slices without changing host captures.
