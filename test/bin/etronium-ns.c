/* etronium-ns.c — CRIU --exec-cmd helper.
 *
 * Called after CRIU restore inside an ephemeral PID namespace created by
 * `unshare -p -f --mount-proc`. When CRIU exits, the namespace closes and
 * any process still inside gets SIGKILLed. To survive, we open a file that
 * holds a reference to lord's PID namespace (created via bind-mount of
 * /proc/self/ns/pid BEFORE unshare) and setns() into it. Once we're in
 * lord's namespace, we exec the target binary.
 *
 * arg[1] = path to a file (lives on tmpfs/bind-mount) that points to lord's
 *          PID namespace. Opening this file returns a valid ns_fd even
 *          when executed from inside a different PID namespace.
 * arg[2] = target binary path
 * arg[3..] = target argv
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

int main(int argc, char *argv[]) {
    if (argc < 3) {
        fprintf(stderr, "etronium-ns: usage: NS_PATH_FILE TARGET BIN ARGS...\n");
        return 2;
    }

    const char *ns_path = argv[1];
    int nfd = open(ns_path, O_RDONLY);
    if (nfd < 0) {
        fprintf(stderr, "etronium-ns: cannot open namespace %s: %s\n", ns_path, strerror(errno));
        return 5;
    }

    if (setns(nfd, CLONE_NEWPID) < 0) {
        fprintf(stderr, "etronium-ns: setns(%s, CLONE_NEWPID) failed: %s\n", ns_path, strerror(errno));
        close(nfd);
        return 6;
    }
    close(nfd);

    char **target_argv = &argv[2];
    execvp(target_argv[0], target_argv);
    fprintf(stderr, "etronium-ns: execvp(%s) failed: %s\n", target_argv[0], strerror(errno));
    return 7;
}
