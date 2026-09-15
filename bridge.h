#ifndef LLAR_SANDBOX_BRIDGE_H
#define LLAR_SANDBOX_BRIDGE_H
// C ABI mirrored from sentry/sandbox.h. The library is loaded at runtime.
#include <stdint.h>
#include <stddef.h>

typedef char *(*read_memory_fn)(uintptr_t, uint64_t, void *, size_t, size_t *);
struct syscall_event {
    uint64_t number;
    uint64_t args[6];
    const char *name;
    uintptr_t context;
    read_memory_fn read;
};
typedef void (*inspect_fn)(uintptr_t, struct syscall_event *);
typedef int (*run_sentry_fn)(char *, int, uintptr_t, uintptr_t, uintptr_t,
                             inspect_fn, char *, size_t);

static inline void invoke_inspector(inspect_fn fn, uintptr_t owner,
                                    struct syscall_event *event) {
    fn(owner, event);
}
static inline char *inspect_read(struct syscall_event *event, uint64_t address,
                                void *dst, size_t size, size_t *copied) {
    return event->read(event->context, address, dst, size, copied);
}
int sandbox_load(char *, char *, int, uintptr_t, uintptr_t, uintptr_t,
                 char *, size_t);
#endif
