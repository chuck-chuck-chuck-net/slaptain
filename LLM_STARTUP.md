# LLM_STARTUP.md

## Project: slaptain (Kubernetes OpenLDAP Operator)

### Objective
Create a Kubernetes operator for a multi-master replicating OpenLDAP (slapd) cluster.
Prioritize a rootless, distroless image built with Wolfi and apko.

### Requirements & Constraints
- **Image:** Rootless, distroless (Wolfi/apko).
- **Architecture:** 
    - **Multi-Site:** Setup spans multiple physical sites.
    - **Per-Site HA:** Each site must be autonomous and highly available internally (local K8s cluster).
    - **Cross-Cluster Replication:** Data must replicate between independent K8s clusters.
- **Platform:** Kubernetes (separate clusters per site).
- **Security:** Rootless execution (UID 1024), minimal attack surface.

### Current Implementation
- [x] Wolfi/apko base image configuration (`image/slapd.yaml`).
- [x] Init-image for bootstrapping (`image/Containerfile.init`).
- [x] Ported bootstrap logic with `slaptest` conversion (`images/bootstrap.sh`).
- [x] Helm Chart for standalone deployment (`charts/slapd`).
- [ ] Cross-cluster replication logic (to be added to operator/init).
- [ ] Kubernetes Operator (to be implemented).

### Image Details
- **Project Name:** `slaptain`
- **Base:** Wolfi (chainguard-style)
- **User:** `openldap` (UID/GID 1024)
- **Mount Points:** `/ldap-config` and `/ldap-data` (root-level to avoid base image permission issues).
- **Init Container:** Handles LDIF generation, `slapd.conf` -> `slapd.d` conversion, and initial object creation.
