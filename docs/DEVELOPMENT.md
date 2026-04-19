# Development Guide

> **Status:** Stub. Fleshing this out with the full dev workflow (image builds,
> operator local-run, Helm deploy cycle, e2e test cycle, etc.) is TODO.
> For now see `CLAUDE.md` "Local Dev Workflow" and the root `Makefile` targets.

---

## Makefile: CONTEXT, Node Annotations, Incremental Imports

### Targeting a specific kubecontext

Pass `CONTEXT=<name>` to any Make target. It threads `--context` / `--kube-context`
through every `kubectl` and `helm` call:

```bash
make deploy-operator CONTEXT=bento
make cluster-helm-install CONTEXT=t3e
make gencert CONTEXT=bento
make import CONTEXT=bento
```

When `CONTEXT` is unset, commands use the current kubeconfig context (unchanged
from previous behaviour).

### Node annotations for CRI and NODE_USER

Instead of passing `CRI=` and `NODE_USER=` on every `make import` invocation,
you can set them once as node annotations:

```bash
kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/cri=crio
kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/node-user=dominik
```

The Makefile reads these at invocation time. Precedence (highest wins):

1. CLI / environment variable (`make import CRI=crio NODE_USER=dominik`)
2. Node annotation (`slaptain.chuck-chuck-chuck.net/cri`, `.../node-user`)
3. Built-in default (`containerd`, `debian`)

### Incremental image imports

`make import` tracks per-image stamp files (`.stamps/import-*`). Only images
that were rebuilt since the last import are re-imported to the cluster nodes.

When `CONTEXT` is set, stamps are per-context (e.g. `.stamps/import-bento-operator`),
so importing the same image to two clusters is tracked independently.

```bash
make import                   # imports only changed images
make import CONTEXT=bento     # same, tracked separately for "bento"
make import-operator          # single image, also incremental
make clean-import             # remove all import stamps (forces full re-import)
```
