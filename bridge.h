#ifndef LLAR_SANDBOX_BRIDGE_H
#define LLAR_SANDBOX_BRIDGE_H
// C ABI mirrored from sentry/sandbox.h. The library is loaded at runtime.
#include <stdint.h>
#include <stddef.h>

struct syscall_event;
typedef char *(*read_memory_fn)(uintptr_t, uint64_t, void *, size_t, size_t *);
typedef char *(*decode_syscall_fn)(uintptr_t, struct syscall_event *, size_t, char **);
typedef char *(*rewrite_syscall_fn)(uintptr_t, struct syscall_event *, void *, size_t);
struct syscall_event {
    uint64_t number;
    uint64_t args[6];
    const char *name;
    uintptr_t context;
    read_memory_fn read;
    decode_syscall_fn decode;
    rewrite_syscall_fn rewrite;
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
static inline char *inspect_decode(struct syscall_event *event, size_t budget, char **error) {
    return event->decode(event->context, event, budget, error);
}
static inline char *inspect_rewrite(struct syscall_event *event, void *data, size_t size) {
    return event->rewrite(event->context, event, data, size);
}
int sandbox_load(char *, char *, int, uintptr_t, uintptr_t, uintptr_t,
                 char *, size_t);
#endif
