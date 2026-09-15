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

The repository contains two library modules and a separate integration-test module:

```text
github.com/xgo-dev/sandbox             sandbox.Run(fn), guest entry, value transfer
github.com/xgo-dev/sandbox/sentry      c-shared Sentry backend -> sentrylib.so
github.com/xgo-dev/sandbox/testdata    integration test executable -> smoke
```

Import `github.com/xgo-dev/sandbox` in the host. It loads `sentrylib.so` through `dlopen` and does not import the Sentry module or gVisor. Each module carries its own C ABI declarations so either module can be fetched independently. The ABI carries guest startup addresses, syscall registers and synchronous inspection and memory mapping callbacks.

The C entry is `RunSandbox`. This host API requires the Sentry ABI introduced in `sentry/v0.2.0`; the `sentry/v0.1.0` library is incompatible. Build the host and Sentry from the same source revision. Go module version selection does not check the ABI of a library loaded through `dlopen`.

LLAR's formula integration stays in its own repository at `experimental/sandbox-formula`, where it can use LLAR's internal formula loader. The sandbox library has no LLAR or ixgo dependency.

## Host Inspection

```go
s := sandbox.Sandbox{
    Library: "/opt/llar/sentrylib.so",
    Inspect: func(call *sandbox.Syscall) {
        if call.Name != "write" {
            return
        }
        size := min(call.Args[2], uint64(256))
        view, err := call.MMap(call.Args[1], int(size))
        log.Printf("write(fd=%d, prefix=%q)", call.Args[0], view.Data)
        if err != nil {
            log.Printf("read guest memory: %v", err)
        }
    },
}
err := s.Run(fn)
```

The default library is `sentrylib.so` beside the executable. One library remains loaded for the process lifetime. Inspector callbacks run synchronously in the original host Go runtime and are serialized; they must not access the captured objects while a call is active. An inspector panic is reported and prevents result writeback, but is not a mechanism for denying the syscall.

`Name`, `Number` and the six raw `Args` are available without reading guest memory. The interceptor owns syscall argument parsing. It can change `Number` and `Args` directly before returning. There is no structured argument codec.

`MMap(address, size)` creates independent temporary pages in the guest's Sentry memory manager and initializes them from the requested guest bytes. `Memory.Data` directly maps those temporary pages into the host, while `Memory.Addr` is their guest address. Editing `Data` changes the temporary pages immediately and leaves the original guest memory unchanged. Initialization copies bytes inside Sentry; this is not copy-on-write. There is no host staging buffer or copy-back step.

For an `openat` whose original pathname is `/tmp/input`:

```go
view, err := call.MMap(call.Args[1], len("/tmp/input\x00"))
if err != nil {
    panic(err)
}
copy(view.Data, "/tmp/other\x00")
call.Args[1] = view.Addr
```

To read the pathname as a Go string, use cgo's `C.GoString`. In a file with `import "C"` and imports for `bytes`, `fmt` and `unsafe`, the view above can be read inside `Inspect`:

```go
if bytes.IndexByte(view.Data, 0) < 0 {
    fmt.Println("pathname has no NUL within mapped bytes")
    return
}
path := C.GoString((*C.char)(unsafe.Pointer(unsafe.SliceData(view.Data))))
fmt.Println(path) // /tmp/other
```

Check for a NUL within `Data` before calling `C.GoString`, which otherwise reads until it finds one. The resulting Go string is a copy and may outlive the callback. `C.CString` does the reverse conversion and allocates separate host C memory; its pointer is not a guest address.

The interceptor explicitly assigns guest addresses to arguments and nested pointers. The library never scans integers or guesses which values are pointers. For a Linux AMD64/ARM64 `writev` with one iovec containing the 5-byte payload `hello`:

```go
vector, err := call.MMap(call.Args[1], 16)
if err != nil {
    panic(err)
}
base := binary.NativeEndian.Uint64(vector.Data[:8])
payload, err := call.MMap(base, 5)
if err != nil {
    panic(err)
}
copy(payload.Data, "world")
binary.NativeEndian.PutUint64(vector.Data[:8], payload.Addr)
call.Args[1] = vector.Addr
```

Returning from `Inspect` submits the edited registers. There is no public allocator, `Commit`, or `WriteMemory`. The host view is borrowed only until the callback returns and must not contain host Go pointers. Do not convert its host pointer to a guest address. These views replace synchronous syscall inputs; syscall outputs are not copied back to the original buffers. The returned size is limited to readable source bytes; growing a replacement beyond that size is not supported.

Reads respect guest permissions and may return a shorter `Data` slice together with an error. Zero length returns an empty view. Access after the callback returns fails. Use the view synchronously and do not concurrently modify event fields. Other guest threads can change source memory, so this is not an atomic snapshot. Read errors do not automatically deny the syscall.

Sentry releases temporary mappings after synchronous syscall execution. Restarted calls and calls skipped before dispatch retain their mappings until the sandbox exits. Temporary addresses must not escape the syscall or be unmapped/remapped by guest threads; the current implementation does not protect against those operations. Pinning keeps the backing pages alive, but does not reserve a guest virtual address against later replacement.

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
  Inspect(Name, Number, Args) <-------------+
       |                                   |
       +-- MMap(addr, size) --------------> MMap + Pin + CopyIn to temporary pages
       | <------ Data alias, guest Addr ----+
       +-- edit Data --------------------> same temporary pages
       +-- optional Number / Args edits --> syscall registers
       |                                   |
       +-- callback returns --------------> Sentry syscall dispatch
                                           +-- release temporary guest memory
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

### Third-Party strace Formatting

[u-root's strace package](https://github.com/u-root/u-root/blob/v0.16.0/pkg/strace/syscall_linux.go) exposes `SysCallEnter` and a `Task` interface with `Name` and `Read` methods. The interceptor can adapt `MMap` to that interface and format `openat` from its number and raw arguments. Add `github.com/u-root/u-root/pkg/strace@v0.16.0` to the calling application's module:

```go
import (
    "encoding/binary"
    "fmt"

    "github.com/u-root/u-root/pkg/strace"
    "github.com/xgo-dev/sandbox"
)

type traceTask struct {
    call *sandbox.Syscall
}

func (t traceTask) Name() string { return "guest" }

func (t traceTask) Read(addr strace.Addr, dst any) (int, error) {
    size := binary.Size(dst)
    if size < 0 {
        return 0, fmt.Errorf("unsupported strace destination %T", dst)
    }
    view, err := t.call.MMap(uint64(addr), size)
    if err != nil {
        return len(view.Data), err
    }
    return binary.Decode(view.Data, binary.NativeEndian, dst)
}

func printSyscall(call *sandbox.Syscall) {
    if call.Name != "openat" {
        return
    }
    event := strace.SyscallEvent{Sysno: int(call.Number)}
    for i, arg := range call.Args {
        event.Args[i].Value = uintptr(arg)
    }
    fmt.Println(strace.SysCallEnter(traceTask{call}, &event))
}
```

Use it as the host interceptor:

```go
s := sandbox.Sandbox{Inspect: printSyscall}
err := s.Run(fn) // for example, fn calls os.ReadFile("/etc/hostname")
```

An observed Linux ARM64 entry (addresses vary):

```text
guest E openat(0xffffffffffffff9c, 0x56793be86040 /etc/hostname, O_RDONLY|O_CLOEXEC, ----------)
```

Formatting runs in the host callback; it needs no ptrace attachment or Sentry decoder. u-root chooses its syscall table for the build architecture. This example formats syscall entry arguments; `Inspect` runs before execution, so return values and `read` output are unavailable there.

The adapter handles the byte slices used for pathnames and other fixed-size values accepted by `encoding/binary`. Layouts containing `uintptr` require additional caller-side decoding. u-root v0.16.0 reads strings one byte at a time, so this simple adapter creates a temporary mapping for each byte; account for that cost before using it for high-volume tracing.

## Reading Order

1. [host_linux.go](host_linux.go): public `Run`, image ownership, c-shared call, validation and return.
   [inspect.go](inspect.go) defines temporary memory views. [sentry/inspect_linux.go](sentry/inspect_linux.go) owns the C memory callback; [sentry/memory_linux.go](sentry/memory_linux.go) allocates, maps and releases temporary Sentry pages.
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

`sentry/build-linux.sh` builds only the shared library and its C headers. The root `build-linux.sh` builds the integration program in [testdata/main.go](testdata/main.go), including the syscall-memory checks in [testdata/memory.go](testdata/memory.go), into `smoke`. It also builds the root module's value-transfer tests and checks that the host dependency list contains no gVisor packages. The [testdata/go.mod](testdata/go.mod) module uses a local `replace` directive to test the current checkout; it is not included by the root module's `go test ./...`. The root build script does not build the Sentry library.

The smoke test covers automatic guest entry after package initialization without entering application `main`, integer capture, original host PID, GC, pointer/map aliases, cycles, slice growth, interfaces, nested native callbacks, mutation before dropping a reference, guest panic without writeback, static functions, syscall number rewriting, nested-call rejection and unchanged stdin. It also checks for descriptor growth after warming the shared platform, guest path/buffer reads, raw syscall argument editing, invalid guest addresses and expired inspection access. Temporary-memory checks cover immediate visibility of host edits, unchanged original guest bytes, explicit path and nested `writev` pointer replacement, cross-page data, concurrent calls, guest mapping cleanup and a panic after mapping memory.

LLAR's separate formula integration passes `OnBuild(ctx)` with both `Project.ReadFile` and `os.ReadFile`, a native output-directory callback, and result writeback to the original `Context` and `Project`. Both ARM64 and emulated AMD64 value-transfer tests reject malformed lengths, unauthorized code entries, unknown types, changed retention counts, merged identities and overlapping slices without changing host captures.
