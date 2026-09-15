#ifndef XGO_SANDBOX_H
#define XGO_SANDBOX_H
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

int RunSandbox(char *guest, int image_fd, uintptr_t main_pc, uintptr_t entry_pc,
                 uintptr_t owner, inspect_fn inspect, char *message, size_t capacity);

char *ReadSyscallMemory(uintptr_t, uint64_t, void *, size_t, size_t *);

static inline void prepare_inspection(struct syscall_event *event, uintptr_t context) {
    event->context = context;
    event->read = ReadSyscallMemory;
}

static inline void invoke_inspector(inspect_fn fn, uintptr_t owner,
                                    struct syscall_event *event) {
    fn(owner, event);
}
#endif
