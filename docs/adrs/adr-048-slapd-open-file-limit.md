# ADR-048: The slapd image caps slapd's open-file limit

**Status:** Proposed
**Date:** 2026-11-09

slapd hangs or runs out of memory at startup on kind and other clusters
that hand containers a high open-file limit.

## Context

slapd allocates one connection slot per allowed file descriptor at startup
(`daemon.c`, `connection.c`), and the node decides that limit.
Measured: 47 MiB at 65,535, about 720 MiB at 2^20,
and a hang before the listener at 2^31 (kind on GitHub-hosted runners).

## Decisions

1. **Where?** A small C launcher is the slapd image's `ENTRYPOINT`.
   It lowers the soft limit, then `execv`s slapd, which keeps PID 1 and its signals.
   - Discarded:
     - containerd's `LimitNOFILE`: the cluster administrator's setting, not ours.
     - A slapd patch: the default pure flavour carries none (ADR-040).
     - An `LD_PRELOAD` shim: more machinery for the same effect.
2. **Which value?** 65,536, hard-coded, applied only when the inherited limit is higher.
   `spec.tuning.maxOpenFiles` (ADR-024) can make it configurable later.
3. **Which language?** C, built in a one-step stage beside the package build.
   - Discarded:
     - Go: needs a Go builder stage in the OpenLDAP Containerfile now.

## Consequences

- slapd's memory no longer depends on the node.
- A pod `command:` bypasses the cap.
- The launcher folds into the planned Go init binary.
- The kind smoke run is the regression test.
