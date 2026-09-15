//go:build linux && (arm64 || amd64) && cgo

#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "bridge.h"

extern void sandboxInspect(uintptr_t, struct syscall_event *);

static run_sentry_fn entry;
static char *loaded_path;

int sandbox_load(char *library, char *guest, int image_fd, uintptr_t main_pc,
                 uintptr_t entry_pc, uintptr_t owner, char *error, size_t capacity) {
    if (entry != NULL) {
        if (strcmp(library, loaded_path) != 0) {
            snprintf(error, capacity, "Sentry library is already loaded from %s", loaded_path);
            return 1;
        }
        return entry(guest, image_fd, main_pc, entry_pc, owner, owner ? sandboxInspect : NULL, error, capacity);
    }
    void *handle = dlopen(library, RTLD_NOW | RTLD_LOCAL);
    if (handle == NULL) {
        snprintf(error, capacity, "dlopen: %s", dlerror());
        return 1;
    }
    dlerror();
    run_sentry_fn run = (run_sentry_fn)dlsym(handle, "RunSandbox");
    const char *message = dlerror();
    if (message != NULL) {
        snprintf(error, capacity, "dlsym RunSandbox: %s", message);
        return 1;
    }
    // Go runtimes leave background threads alive. Never dlclose their code.
    loaded_path = strdup(library);
    if (loaded_path == NULL) {
        snprintf(error, capacity, "cannot retain library path");
        return 1;
    }
    entry = run;
    return entry(guest, image_fd, main_pc, entry_pc, owner, owner ? sandboxInspect : NULL, error, capacity);
}
