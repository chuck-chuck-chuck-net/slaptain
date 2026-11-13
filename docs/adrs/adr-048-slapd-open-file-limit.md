# ADR-048: The slapd image sets slapd's open-file limit

**Status:** Proposed
**Date:** 2026-11-09

slapd's memory and its connection ceiling both follow the open-file limit
the node hands the container.
A high limit costs hundreds of MiB or stops slapd before it listens,
and a low one leaves 1024 descriptors for everything.

## Context

slapd reads the soft `RLIMIT_NOFILE` once (`daemon.c`: `dtblsize = sysconf(_SC_OPEN_MAX)`)
and uses it as both its descriptor allowance and the length of its connection table.
`connections_init` (`connection.c`) allocates `dtblsize` slots, indexes them by descriptor number,
and initialises two mutexes and a condition variable in each.
slapd never raises the soft limit towards the hard limit, so the node decides both numbers.

`FD_SETSIZE` does not bound the table.
The clamp `if (dtblsize > FD_SETSIZE)` sits in the `select(2)` variant of `SLAP_SOCK_INIT` only,
and the epoll variant Linux builds use sizes everything by `dtblsize`.

Measured with the same image: 47 MiB at 65,535 and about 720 MiB at 2^20,
which is 718 bytes per slot.
At 2^31 (kind on GitHub-hosted runners) slapd opened no listener before the startup probe gave up.
We did not measure why.
The init loop over 2^31 slots would run for hours,
and `dtblsize` is an `int`, so the value may overflow before the allocation.
A longer timeout does not fix either.

At systemd's default `1024:524288` slapd gets 1024 descriptors
for both listeners, every client and syncrepl connection, and the LMDB files.
No suite has hit that ceiling.
If one did, slapd would stop accepting and no metric we export would name the cause.

## Decisions

1. **Where?** A small C launcher is the slapd image's `ENTRYPOINT`.
   It sets the soft limit, then `execv`s slapd, which keeps PID 1 and its signals.
   - Discarded:
     - containerd's `LimitNOFILE`: the cluster administrator's setting, not ours.
     - A slapd patch: the default pure flavour carries none (ADR-040).
     - An `LD_PRELOAD` shim: more machinery for the same effect.
2. **Which value?** `min(hard limit, 65,536)`, hard-coded, lowered or raised to match.
   Raising the soft limit up to the hard limit needs no privilege,
   so it works as UID 1024 with every capability dropped.
   A hard limit below 65,536 stays the ceiling.
   `spec.tuning.maxOpenFiles` (ADR-024) can make it configurable later.
   The launcher logs the result on every start, as the first line of the pod log:
   `slapd-launcher: open-file limit soft 65536 (was 1024), hard 524288`.
3. **Which language?** C, built in a one-step stage beside the package build.
   - Discarded:
     - Go: needs a Go builder stage in the OpenLDAP Containerfile now.
4. **Who may set the command?** Nobody.
   The operator sets `args` on the slapd container and never `command`
   (`buildStatefulSetSpec`), because a `command` replaces the image `ENTRYPOINT`
   and slapd would start with the node's limit again, without a symptom.
   `TestSlapdContainerKeepsImageEntrypoint` fails when one appears.

## Consequences

- slapd has 65,536 connection slots and about 47 MiB of table on every node
  whose hard limit allows it.
- A pod that inherited 1024 now allocates the full table, about 45 MiB more than before.
- The launcher folds into the planned Go init binary.
- The kind smoke run is the regression test.
