/*
 * slapd-launcher: set the open-file limit, then become slapd.
 *
 * slapd sizes its connection table by the soft RLIMIT_NOFILE
 * (daemon.c: dtblsize = sysconf(_SC_OPEN_MAX)) and initialises every slot at
 * startup (connection.c: connections_init). It never raises the soft limit
 * itself, so the node's value is both its memory cost and its connection
 * ceiling: 2^20 costs about 720 MiB per pod, a limit near 2^31 never reaches
 * the listener, and systemd's 1024:524288 leaves 1024 descriptors in total.
 * Kubernetes has no per-container rlimit and this image has no shell, so the
 * limit is set here, to NOFILE_MAX or to the hard limit where that is lower.
 * Raising the soft limit up to the hard limit needs no privilege.
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

/* Prints " <label> <n>", or "unlimited" for RLIM_INFINITY. */
static void print_limit(const char *label, rlim_t v) {
    if (v == RLIM_INFINITY)
        fprintf(stderr, " %s unlimited", label);
    else
        fprintf(stderr, " %s %llu", label, (unsigned long long)v);
}

int main(int argc, char **argv) {
    struct rlimit rl;

    if (getrlimit(RLIMIT_NOFILE, &rl) != 0) {
        perror("slapd-launcher: getrlimit");
    } else {
        rlim_t cap = NOFILE_MAX;
        rlim_t want = rl.rlim_max < cap ? rl.rlim_max : cap;
        rlim_t was = rl.rlim_cur;
        if (rl.rlim_cur != want) {
            rl.rlim_cur = want;
            if (setrlimit(RLIMIT_NOFILE, &rl) != 0) {
                perror("slapd-launcher: setrlimit");
                rl.rlim_cur = was;
            }
        }
        /* The limits slapd starts with: nothing slapd logs or exports names them. */
        fprintf(stderr, "slapd-launcher: open-file limit");
        print_limit("soft", rl.rlim_cur);
        if (rl.rlim_cur == was) {
            fprintf(stderr, " (unchanged),");
        } else {
            print_limit("(was", was);
            fprintf(stderr, "),");
        }
        print_limit("hard", rl.rlim_max);
        fputc('\n', stderr);
    }

    /* A caller may pass an empty argv (CVE-2021-4034); give slapd a valid one. */
    char *fallback[] = { SLAPD_PATH, NULL };
    char **args = (argc > 0) ? argv : fallback;
    args[0] = SLAPD_PATH;

    execv(SLAPD_PATH, args);
    perror("slapd-launcher: execv " SLAPD_PATH);
    return 127;
}
