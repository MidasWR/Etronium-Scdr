/* etronium-nsenter.c — wrapper that creates fresh PID+mount namespace.
 *
 * Approach:  fork; child does unshare(NEWPID|NEWNS) then forks again. The
 * grandchild (which becomes init of new namespace) does mount -t proc proc
 * /proc and exec the real binary. This avoids issues where mounting /proc
 * from a process that's still reference-holding the old /proc fails with
 * EINVAL/ENOMEM.
 *
 * argv[0] = wrapper, argv[1..] = real binary + args.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef CLONE_NEWNS
#define CLONE_NEWNS 0x00020000
#endif
#ifndef CLONE_NEWPID
#define CLONE_NEWPID 0x20000000
#endif

int main(int argc, char *argv[]) {
    if (argc < 2) {
        fprintf(stderr, "usage: etronium-nsenter REAL_BIN [ARGS...]\n");
        return 2;
    }

    pid_t outer = fork();
    if (outer < 0) {
        perror("fork");
        return 3;
    }
    if (outer == 0) {
        /* Outer child: enter namespaces. */
        if (unshare(CLONE_NEWPID | CLONE_NEWNS) < 0) {
            fprintf(stderr, "nsenter: unshare failed: %s\n", strerror(errno));
            _exit(4);
        }
        pid_t inner = fork();
        if (inner < 0) {
            perror("fork (inner)");
            _exit(5);
        }
        if (inner == 0) {
            /* Grandchild (becomes init of new namespace). */
            if (mount("proc", "/proc", "proc", 0, NULL) < 0) {
                fprintf(stderr, "nsenter: mount proc failed: %s\n", strerror(errno));
                _exit(6);
            }
            char **sub_argv = &argv[1];
            execvp(sub_argv[0], sub_argv);
            fprintf(stderr, "nsenter: execvp(%s) failed: %s\n", sub_argv[0], strerror(errno));
            _exit(7);
        }
        /* Outer-child now waitpid for grandchild to avoid orphaning. */
        int status;
        waitpid(inner, &status, 0);
        if (WIFEXITED(status)) _exit(WEXITSTATUS(status));
        if (WIFSIGNALED(status)) _exit(128 + WTERMSIG(status));
        _exit(1);
    }

    /* Top-level: wait for outer child. */
    int status;
    waitpid(outer, &status, 0);
    if (WIFEXITED(status)) return WEXITSTATUS(status);
    if (WIFSIGNALED(status)) return 128 + WTERMSIG(status);
    return 1;
}
