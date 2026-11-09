/*
 * slapd-launcher: cap the open-file limit, then become slapd.
 *
 * slapd sizes its connection table by the soft RLIMIT_NOFILE
 * (daemon.c: dtblsize = sysconf(_SC_OPEN_MAX)) and initialises every slot at
 * startup (connection.c: connections_init). A node that hands containers a
 * limit of 2^31 makes slapd hang there; 2^20 costs about 720 MiB per pod.
 * Kubernetes has no per-container rlimit and this image has no shell, so the
 * cap is set here. A limit already at or below the cap is left alone.
 *
 * execv replaces this process, so slapd keeps its PID and receives every
 * signal directly. Meant to fold into a Go init binary once one exists.
 */
#include <stdio.h>
#include <sys/resource.h>
#include <unistd.h>

#ifndef SLAPD_PATH
#define SLAPD_PATH "/usr/sbin/slapd"
#endif
#ifndef NOFILE_MAX
#define NOFILE_MAX 65536
#endif

int main(int argc, char **argv) {
    struct rlimit rl;

    if (getrlimit(RLIMIT_NOFILE, &rl) != 0) {
        perror("slapd-launcher: getrlimit");
    } else if (rl.rlim_cur > NOFILE_MAX) {
        rl.rlim_cur = NOFILE_MAX;
        if (setrlimit(RLIMIT_NOFILE, &rl) != 0)
            perror("slapd-launcher: setrlimit");
    }

    /* A caller may pass an empty argv (CVE-2021-4034); give slapd a valid one. */
    char *fallback[] = { SLAPD_PATH, NULL };
    char **args = (argc > 0) ? argv : fallback;
    args[0] = SLAPD_PATH;

    execv(SLAPD_PATH, args);
    perror("slapd-launcher: execv " SLAPD_PATH);
    return 127;
}
