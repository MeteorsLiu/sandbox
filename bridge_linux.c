//go:build linux && (arm64 || amd64) && cgo

#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "bridge.h"

extern void sandboxInspect(uintptr_t, struct syscall_event *);

static run_sentry_fn entry;
static create_sentry_fn create_entry;
static close_sentry_fn close_entry;
static char *loaded_path;

// Go serializes library initialization. Published symbols never change, and
// Run/Close calls do not hold the initialization lock.
int sandbox_create(char *library, uintptr_t *kernel, char *error, size_t capacity) {
    if (entry != NULL) {
        if (strcmp(library, loaded_path) != 0) {
            snprintf(error, capacity, "Sentry library is already loaded from %s", loaded_path);
            return 1;
        }
        return create_entry(kernel, error, capacity);
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
    create_sentry_fn create = (create_sentry_fn)dlsym(handle, "CreateSandbox");
    message = dlerror();
    if (message != NULL) {
        snprintf(error, capacity, "dlsym CreateSandbox: %s", message);
        return 1;
    }
    close_sentry_fn close = (close_sentry_fn)dlsym(handle, "CloseSandbox");
    message = dlerror();
    if (message != NULL) {
        snprintf(error, capacity, "dlsym CloseSandbox: %s", message);
        return 1;
    }
    // Go runtimes leave background threads alive. Never dlclose their code.
    loaded_path = strdup(library);
    if (loaded_path == NULL) {
        snprintf(error, capacity, "cannot retain library path");
        return 1;
    }
    create_entry = create;
    close_entry = close;
    entry = run;
    return create_entry(kernel, error, capacity);
}

int sandbox_run(uintptr_t kernel, char *config, int image_fd, uintptr_t main_pc,
                uintptr_t entry_pc, uintptr_t owner, char *error, size_t capacity) {
    return entry(kernel, config, image_fd, main_pc, entry_pc, owner,
                 owner ? sandboxInspect : NULL, error, capacity);
}

int sandbox_close(uintptr_t kernel, char *error, size_t capacity) {
    return close_entry(kernel, error, capacity);
}
