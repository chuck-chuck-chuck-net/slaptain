# LLM_STARTUP.md

## Project: slaptain (Kubernetes OpenLDAP Operator)

### Objective
Create a Kubernetes operator for a multi-master replicating OpenLDAP (slapd) cluster.
Decision drivers: functionality, performance, scalability first. Security hardening (rootless,
distroless, read-only root FS) as secondary goal — pursued where it doesn't compromise the primary drivers.

### Requirements & Constraints
- **Image:** Rootless, distroless. Debian/glibc base (not Alpine/musl — glibc required for
  production performance and compatibility at scale).
- **Architecture:**
    - **Multi-Site:** Setup spans multiple physical sites.
    - **Per-Site HA:** Each site must be autonomous and highly available internally (local K8s cluster).
    - **Cross-Cluster Replication:** Data must replicate between independent K8s clusters.
- **Platform:** Kubernetes (separate clusters per site).
- **Security:** Rootless execution (UID/GID 1024), no privilege escalation, read-only root FS.

### Current Implementation
- [x] slapd runtime image: Debian trixie-slim build → `gcr.io/distroless/base-debian13` (`images/slapd/Containerfile`).
- [x] slapd-init image: Debian trixie-slim, full shell environment for bootstrap (`images/slapd-init/Containerfile`).
- [x] Bootstrap logic with `slaptest` conversion (`images/slapd-init/bootstrap.sh`).
- [x] Helm Chart for standalone deployment (`charts/slapd`).
- [ ] Cross-cluster replication logic (to be added to operator/init).
- [ ] Kubernetes Operator (to be implemented).

### Image Details
- **Project Name:** `slaptain`
- **Build tooling:** Plain Containerfile + podman (apko dropped — only beneficial in the Wolfi ecosystem).
- **slapd runtime:** `gcr.io/distroless/base-debian13` — glibc, libssl, ca-certs, no shell, no package manager.
- **slapd-init:** `debian:trixie-slim` — ephemeral init container, needs shell + python3 + OpenLDAP tools.
- **User:** `openldap` (UID/GID 1024). Debian's slapd package creates this user; we `groupmod`/`usermod` it to 1024.
- **Mount Points:** `/ldap-config` (slapd.d config, PVC) and `/ldap-data` (LMDB data, PVC).
- **Ports:** 1024 (ldap), 1025 (ldaps) — non-privileged; service maps 389→1024 and 636→1025.
- **Module path:** `/usr/lib/ldap` (Debian path). Modules currently loaded dynamically; plan to compile in statically later.
- **Schema path:** `/etc/ldap/schema/` (Debian path).
- **Init Container:** Handles `slapd.conf` generation, `slaptest` conversion to `slapd.d` format, and initial LDIF population.
