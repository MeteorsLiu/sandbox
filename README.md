# Sandbox

`sandbox.Run(fn)` runs a Go closure in Sentry, then writes changes to captured objects back into the original host objects. Captures can include existing ixgo callbacks. The host and guest run the same executable. Native types and code come from that ELF; captured values and ixgo program descriptions travel through a memfd. There is no CRIU checkpoint, global STW, or RPC callback proxy in this path.

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

The sandbox module depends on ixgo for rebuilding interpreted closures. It does not depend on LLAR or gVisor. LLAR can continue using its own formula loader and pass an already loaded callback through the ordinary `Run` entry.

## Interpreted Closures

An existing interpreted callback can appear inside a native closure, a struct, an interface or another callback's environment. For example, with an already loaded LLAR formula and build context:

```go
err := sandbox.Run(func() {
    f.OnBuild(ctx)
})
```

The transfer layer unwraps `reflect.MakeFunc` and checks ixgo's callback entry before reading its interpreter, function and captured environment. Each host interpreter gets its own guest interpreter. The scanner follows ordinary captures through native wrappers, including LLAR's callback lifetime wrapper; it does not copy ixgo's scheduler, locks or frame caches.

| Stage | Data and behavior |
| --- | --- |
| Host export | Keep the loaded Go source for interpreted packages, interpreter mode, SSA function identities and per-context external bindings. Scan captures, class instances and interpreted-package globals with one object identity table. |
| Guest setup | Run normal executable package initialization, restore native external bindings, load the saved source and create ixgo interpreters. Do not replay interpreted `RunInit`, class initialization or closure factories. |
| Type resolution | Reuse native ELF types. Resolve interpreted types against the rebuilt program, including local declaration and instance identity, and reconstruct unnamed composite types. Dynamic type addresses never cross the process boundary. |
| Value restoration | Restore the object graph and bind captured environments to the corresponding guest functions. Preserve shared cells, cycles and interface dynamic values. Explicit `reflect.Type` values become type references; addressable `reflect.Value` values refer to restored storage. |
| Execution | Call the original outer closure. Interpreted code and native callbacks execute in the guest; syscalls use the existing Sentry context-switch inspector. |
| Host return | Check the returned program, type and function identities before writeback. Update original objects and interpreted globals. Bind returned ixgo closures to the original host interpreter, preserving captured-cell aliases. |

Standard `os.Stdin`, `os.Stdout` and `os.Stderr` references are rebound to the corresponding runtime's standard streams, matching Sentry's existing descriptor imports. Their `os.File` internals are not copied. Other open files remain unsupported.

The interpreter, its captures and its global variables must remain exclusively owned until `Run` returns. Native package registrations must also be present after guest package initialization. Per-context `RegisterExternal` native functions and variable bindings are transferred; bindings that themselves require an interpreted type or ixgo callback before that interpreter exists are rejected. Custom execution/debug hooks, REPL contexts, inaccessible source and ambiguous type/function identities are rejected. A custom importer is not reconstructed; interpreted source dependencies are included in the image.

This path uses ixgo v1.1.6 private layouts and entry points together with the pinned Go toolchain. Methods, local generic types and new closures from an existing interpreted program are covered by transfer tests; this is not a claim that every generic or reflection operation is supported. Restricted `reflect.Value` access, native generic dictionaries, and incomplete native closure DWARF remain unsupported. A guest cannot introduce a new interpreter/program into the host on return.

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

`MMap(address, size)` creates independent temporary pages in the guest's Sentry memory manager. Address `0` allocates `size` zeroed bytes without reading guest memory; a nonzero address initializes the pages from the requested guest bytes. `Memory.Data` directly maps those temporary pages into the host, while `Memory.Addr` is their guest address. Editing `Data` changes the temporary pages immediately and leaves the original guest memory unchanged. Initialization from a source copies bytes inside Sentry; this is not copy-on-write. There is no host staging buffer or copy-back step. Anonymous allocation requires `sentry/v0.3.0` or later; `sentry/v0.2.0` does not support it.

`Malloc(size)` is shorthand for `MMap(0, size)`: use it for a new zeroed buffer, and use `MMap(address, size)` to initialize a buffer from existing guest memory. Both return the same `Memory` view and follow the same lifetime rules.

**Pointer safety:** When `Inspect` injects a new syscall buffer, it must allocate that buffer through `Malloc` or `MMap` and use the returned `Memory.Addr` for pointer arguments and nested pointers such as `iovec.base` or entries in `argv`. Never inject addresses of host Go objects, Go slice/string backing memory, `C.CString`/`C.malloc` allocations, or `Memory.Data` itself. `Data` is only the host view used to access the temporary pages; converting its pointer to `uintptr` does not produce a guest address.

Sentry interprets syscall pointers in the guest address space. A host address can cause `EFAULT` or refer to unrelated guest memory, causing unintended reads or writes; exposing it also leaks a host address. Treat injecting host pointers as a security bug. Copying intended payload bytes from host memory into `Data` is allowed, but do not copy Go slice/string headers or structs containing host pointers as syscall data. Encode their pointer fields explicitly with guest addresses. The interceptor owns this rule; raw register edits are not checked for host-pointer provenance.

For an `openat` whose original pathname is `/tmp/input`, allocate enough memory for a longer replacement and use `C.CString` to supply its contents. Add the cgo declaration and `unsafe` import to the file:

```go
/*
#include <stdlib.h>
*/
import "C"

import "unsafe"
```

Inside `Inspect`:

```go
const replacement = "/tmp/longer-replacement"
view, err := call.Malloc(len(replacement)+1)
if err != nil {
    panic(err)
}
cpath := C.CString(replacement)
defer C.free(unsafe.Pointer(cpath))
copy(view.Data, unsafe.Slice((*byte)(unsafe.Pointer(cpath)), len(replacement)+1))
call.Args[1] = view.Addr
```

`C.CString` supplies the terminating NUL; the copy includes that extra byte. `C.free` releases the host C allocation after the callback returns. The temporary guest pages keep their own copy. Only `view.Addr` goes into the syscall:

```go
call.Args[1] = view.Addr // Correct: guest address.

// Never inject either host address:
// call.Args[1] = uint64(uintptr(unsafe.Pointer(cpath)))
// call.Args[1] = uint64(uintptr(unsafe.Pointer(unsafe.SliceData(view.Data))))
```

To read the pathname as a Go string, use cgo's `C.GoString`. With additional imports for `bytes` and `fmt`, the view above can be read inside `Inspect`:

```go
if bytes.IndexByte(view.Data, 0) < 0 {
    fmt.Println("pathname has no NUL within mapped bytes")
    return
}
path := C.GoString((*C.char)(unsafe.Pointer(unsafe.SliceData(view.Data))))
fmt.Println(path) // /tmp/longer-replacement
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

Returning from `Inspect` submits the edited registers. There is no `Commit` or `WriteMemory`. The host view is borrowed only until the callback returns and must not contain host Go or C pointers. These views replace synchronous syscall inputs; syscall outputs are not copied back to the original buffers. For nonzero addresses, the returned size is limited to readable source bytes. Use `Malloc(size)` (equivalently, `MMap(0, size)`) when a replacement needs more space.

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
       +-- MMap(addr, size) --------------> MMap + Pin; CopyIn only if addr != 0
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
    if addr == 0 {
        return 0, fmt.Errorf("null guest address")
    }
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

The adapter rejects address zero because `MMap(0, size)` allocates memory instead of reading a null pointer. It handles the byte slices used for pathnames and other fixed-size values accepted by `encoding/binary`. Layouts containing `uintptr` require additional caller-side decoding. u-root v0.16.0 reads strings one byte at a time, so this simple adapter creates a temporary mapping for each byte; account for that cost before using it for high-volume tracing.

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
- Private Go runtime ABI checks currently require exactly Go 1.26.6; ixgo integration targets v1.1.6. Interpreted dynamic types and unnamed composites are resolved during transfer. Native generic dictionaries and bound method wrappers remain unsupported.
- Guest entry redirection requires the `main.main` ELF symbol to contain at least 5 bytes on AMD64 or 4 bytes on ARM64. The private entry must be reachable by a relative jump (signed 32-bit displacement on AMD64, signed 28-bit byte displacement aligned to 4 bytes on ARM64). Unsupported layouts return an error before the guest starts. Only the guest's private executable mapping is patched; the host mapping and executable file are unchanged.
- Channels, non-nil `unsafe.Pointer`, synchronization primitives, timers, cancellation contexts and open file objects cannot be transferred as ordinary values. Known process-local structures are rejected. This is not an exhaustive resource classifier for arbitrary third-party types.
- Interpreted-package globals are migrated. Native package global state, goroutines, stacks, arbitrary file descriptors and singleton identity such as `io.EOF` are not migrated. Standard stream references are rebound as described above. `uintptr` stays an integer; pointers hidden inside it are not relocated.
- Shared pointer/map identities, exact slice aliases and cycles are retained. Overlapping slices and some interior-pointer traversal orders are rejected. Each input/output image currently has a 16 MiB limit and a traversal depth limit of 256.
- Native functions already reachable from the input are authorized for host result restoration. Returning a native function with a previously unseen code entry is rejected. A returned ixgo closure must belong to an original transferred program and its known function set.
- Input objects stay alive through the call. Imported objects stay alive in the guest even after the closure drops its reference, so their mutations can still reach host aliases. Result decoding finishes before writeback starts; a guest panic, exit failure or malformed result prevents that writeback. External syscall side effects are not rolled back.
- The shared library retains a Systrap platform and its process-lifetime workers. Each call creates a fresh Sentry kernel/guest. The two Go runtimes still share OS signal dispositions and the host process address space; c-shared isolates dependencies and runtime heaps, not hostile native code within the host.
- Filesystem/environment policies, cancellation, comprehensive startup-failure cleanup, hostile-image fuzzing and a broader platform/toolchain matrix remain necessary before production use.

## Verification

`sentry/build-linux.sh` builds only the shared library and its C headers. The root `build-linux.sh` builds the integration program in [testdata/main.go](testdata/main.go), including the syscall-memory checks in [testdata/memory.go](testdata/memory.go), into `smoke`. It also builds the root module's value-transfer tests and checks that the host dependency list contains no gVisor packages. The [testdata/go.mod](testdata/go.mod) module uses a local `replace` directive to test the current checkout; it is not included by the root module's `go test ./...`. The root build script does not build the Sentry library.

The smoke test covers automatic guest entry after package initialization without entering application `main`, integer capture, original host PID, GC, pointer/map aliases, cycles, slice growth, interfaces, nested native callbacks, mutation before dropping a reference, guest panic without writeback, static functions, syscall number rewriting, nested-call rejection and unchanged stdin. It also checks for descriptor growth after warming the shared platform, guest path/buffer reads, raw syscall argument editing, invalid guest addresses and expired inspection access. Temporary-memory checks cover immediate visibility of host edits, zeroed anonymous allocations for longer path and buffer replacements, unchanged original guest bytes, explicit path and nested `writev` pointer replacement, cross-page data, concurrent calls, guest mapping cleanup and a panic after mapping memory.

The interpreted integration in [testdata/ixgo.go](testdata/ixgo.go) creates the method callback on the host, then verifies class state, reflection, package globals, a real file-read syscall, writeback and continued host execution. Transfer tests also cover multiple interpreters, interpreted source dependencies, local generic types, external native closure aliases, returned ixgo closures and malformed output without partial host mutation.

The separate LLAR check loads a classfile on the host before `Run`, calls its existing `OnBuild(ctx)` in Sentry with both `Project.ReadFile` and `os.ReadFile`, writes back a custom class counter and the native output-directory callback's captures, then calls the same formula again on the host. The real Sentry path is verified on ARM64. AMD64 value-transfer tests run under emulation; native AMD64 Sentry execution remains unverified.
