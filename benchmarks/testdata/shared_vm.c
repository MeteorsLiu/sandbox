#define _GNU_SOURCE
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

static int wait_for_parent(void *unused) {
    (void)unused;
    for (;;) {
        pause();
    }
    return 0;
}

int main(void) {
    size_t size = 8 * 1024 * 1024;
    volatile char *pages = mmap(NULL, size, PROT_READ | PROT_WRITE,
                               MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    void *stack = malloc(65536);
    if (pages == MAP_FAILED || stack == NULL) {
        return 1;
    }
    for (size_t i = 0; i < size; i += 4096) {
        pages[i] = 1;
    }
    pid_t shared = clone(wait_for_parent, (char *)stack + 65536, CLONE_VM | SIGCHLD, NULL);
    if (shared < 0) {
        return 1;
    }
    pid_t forked = fork();
    if (forked == 0) {
        return wait_for_parent(NULL);
    }
    if (forked > 0) {
        printf("%d %d %d\n", getpid(), shared, forked);
        fflush(stdout);
        getchar();
        kill(forked, SIGTERM);
        waitpid(forked, NULL, 0);
    }
    kill(shared, SIGTERM);
    waitpid(shared, NULL, 0);
    free(stack);
    munmap((void *)pages, size);
    return forked < 0;
}
