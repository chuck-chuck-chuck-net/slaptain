REGISTRY ?= ghcr.io/chuck-chuck-chuck-net
PROJECT ?= slaptain
NAMESPACE ?= slaptain-system
NAMESPACE_TESTING ?= slaptain-testing
# ── How images get built ────────────────────────────────────────────────────
# The graph is docker-bake.hcl; scripts/bake.sh is the one entry point. make
# does NOT appear in any pipeline: its defining value -- rebuilding only what
# changed -- never applied in CI, where a fresh checkout has no stamp files
# and everything rebuilds regardless. What is incremental there is BuildKit's
# content-addressed cache, one layer below and indifferent to make.
#
# So the stamp machinery is gone, and the image targets below are aliases.
# make keeps the job it is good at: being the frontend a human types at.
BAKE := scripts/bake.sh

# podman is no longer a build tool here. It is still a container RUNTIME for
# the ITS#9398 reproduction (docs/its9398/) and the exporter's testdata
# recorder, which run containers rather than build images.

# Image tag: exact git tag if on one, else short commit hash, else — on a
# dirty tree — <hash>-dirty-<contenthash>. Derived by scripts/image-tag.sh,
# the single source shared with tests/e2e.sh; see the script for the rule and
# why the dirty suffix is content-hashed.
GIT_TAG := $(shell scripts/image-tag.sh)

# IS_VERSION_TAG answers "is HEAD on a version tag at all" — deliberately NOT
# "is this a final release", which is a different question (that one gates
# things like :latest, and must exclude an rc; this one must not, because an rc
# is addressed like any other tagged build). Prefix match, so v0.2.1-rc1
# counts. See docs/VERSIONING.md trap 2 — reusing one predicate for both is how
# an rc ends up published under a nonsense tag.
IS_VERSION_TAG := $(shell echo '$(GIT_TAG)' | grep -qE '^v?[0-9]+\.[0-9]+\.[0-9]+' && echo true)

# IMAGE_TAG: how that build is ADDRESSED, which is a different question from
# what it IS (docs/VERSIONING.md). The rule lives in scripts/image-tag.sh
# --image, not here: the pipelines drive buildx directly and need the same
# answer, and a second copy of the rule in a workflow is exactly the fork that
# script's header forbids.
IMAGE_TAG := $(shell scripts/image-tag.sh --image)

# The slapd exporter is its own version line (ADR-033 decision 4,
# docs/VERSIONING.md): git tags exporter/vX.Y.Z, image tag the bare X.Y.Z, and
# off-tag the same sha-<hash>[-dirty-<state8>] addressing as above. Same script,
# its own stream — each stream matches only its own tag pattern, so a commit
# carrying both kinds of tag resolves unambiguously in either.
EXPORTER_GIT_TAG := $(shell scripts/image-tag.sh exporter)
EXPORTER_IMAGE_TAG := $(shell scripts/image-tag.sh --image exporter)

# Image delivery: "push" = registry, "import" = direct to k8s node CRI via SSH
DELIVERY ?= push

# Kubecontext: pass CONTEXT=<name> to target a specific cluster.
# Threads --context / --kube-context through all kubectl and helm calls.
ifdef CONTEXT
  KUBECTL := kubectl --context $(CONTEXT)
  HELM   := helm --kube-context $(CONTEXT)
else
  KUBECTL := kubectl
  HELM   := helm
endif

# Container runtime on k8s nodes: env/CLI > node annotation > default.
# Set once per cluster:
#   kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/cri=crio
ifndef CRI
  CRI := $(or $(shell $(KUBECTL) get nodes -o jsonpath='{.items[0].metadata.annotations.slaptain\.chuck-chuck-chuck\.net/cri}' 2>/dev/null),containerd)
endif

# SSH user for node imports: env/CLI > node annotation > default.
#   kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/node-user=dominik
ifndef NODE_USER
  NODE_USER := $(or $(shell $(KUBECTL) get nodes -o jsonpath='{.items[0].metadata.annotations.slaptain\.chuck-chuck-chuck\.net/node-user}' 2>/dev/null),debian)
endif

# Node IPs: auto-discover from kubectl, override with NODE_IPS="1.2.3.4 5.6.7.8"
NODE_IPS ?= $(shell $(KUBECTL) get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}')

INIT_IMAGE       = $(REGISTRY)/$(PROJECT)/slapd-init:$(IMAGE_TAG)
SLAPD_IMAGE      = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)
TOOLKIT_IMAGE    = $(REGISTRY)/$(PROJECT)/slapd-toolkit:$(IMAGE_TAG)
OPERATOR_IMAGE   = $(REGISTRY)/$(PROJECT)/operator:$(IMAGE_TAG)
EXPORTER_IMAGE   = $(REGISTRY)/$(PROJECT)/slapd-exporter:$(EXPORTER_IMAGE_TAG)

# Legacy OpenLDAP 2.6 variants of the slapd pair (ADR-021). The plain tags above
# are OpenLDAP 2.7.1; these keep the previous Debian-package build reachable for
# hot-migration / legacy-interop clusters (ADR-011). Pin them per SlapdCluster
# with spec.images.{slapd,init}.tag=<tag>-ol26 — both, never one.
# The Debian slapd version both 2.6 lines are pinned to (ADR-040 decision 1).
# One variable so the slapd / slapd-init pair cannot drift apart — their tools
# must match the server — and so the source build patches a fixed tree. It will
# stop resolving when a stable point release retires it from the mirror; that
# is the intended failure, and the fix is to bump it here and re-run the 2.6
# e2e cycle.
OPENLDAP_OL26_VERSION ?= 2.6.10+dfsg-1

# The 2.7 OpenLDAP version, DERIVED from the packaging fork's changelog rather
# than written here (ADR-040 decision 1), so the two cannot disagree.
OPENLDAP_27_VERSION := $(shell head -1 images/openldap-deb/debian/changelog | tr -d '()' | awk '{print $$2}' | cut -d- -f1)

# Which flavour is the DEFAULT (unsuffixed) 2.7 build is no longer a variable
# here: docker-bake.hcl has one target per flavour, `openldap-deb` (pure) and
# `openldap-deb-slaptain` (patched), and the images inherit whichever they
# name. ADR-040 decision 2, executed in v0.5.0 -- the unsuffixed build is the
# PURE one, which is what gives the rotation a real two-act opt-in: turn the
# feature on, AND ask for a build that can express the phase.

# The PATCHED 2.7 pair, in ADR-040's target scheme with the OpenLDAP component
# derived: <slaptain>-ol<openldap>-slaptain. The operator reaches it through
# `spec.images.select.flavour: slaptain`, never by anyone typing this tag.
INIT_IMAGE_SLAPTAIN  = $(REGISTRY)/$(PROJECT)/slapd-init:$(IMAGE_TAG)-ol$(OPENLDAP_27_VERSION)-slaptain
SLAPD_IMAGE_SLAPTAIN = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-ol$(OPENLDAP_27_VERSION)-slaptain

INIT_IMAGE_OL26  = $(REGISTRY)/$(PROJECT)/slapd-init:$(IMAGE_TAG)-ol26
SLAPD_IMAGE_OL26 = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-ol26

# The 2.6 pair built from DEBIAN'S SOURCE package plus the local patches in
# images/openldap-deb/local-patches/ — a third pair, not a replacement for
# either of the two above. The -ol26 pair installs Debian's prebuilt binary and
# so has nowhere to carry a patch; this one builds the same 2.6 from source and
# does. Pin both images or neither, exactly as with -ol26.
INIT_IMAGE_OL26SRC  = $(REGISTRY)/$(PROJECT)/slapd-init:$(IMAGE_TAG)-ol26src
SLAPD_IMAGE_OL26SRC = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-ol26src

# Tag suffix applied to the slapd/slapd-init image tags when DEPLOYING (only —
# the operator and toolkit tags are untouched). It PINS a pair, which is why it
# bypasses spec.images.select: an explicit tag wins, by design.
#
# Empty = the default build: OpenLDAP 2.7.1, PURE (ADR-040 decision 2). The
# other three cells of the matrix:
#
#   make cluster-helm-install SLAPD_TAG_SUFFIX=-ol2.7.2-slaptain   # 2.7, our patches
#   make cluster-helm-install SLAPD_TAG_SUFFIX=-ol26               # 2.6, Debian's binary
#   make cluster-helm-install SLAPD_TAG_SUFFIX=-ol26src            # 2.6, our patches
#
# This is also how the second tier-1 e2e lane runs (ADR-040 decision 7): the
# rotation's phase-staggering modes need the patch, so a suite run against the
# default pure pair exercises WriteOnly and skips the rest. Run the patched
# lane with SLAPD_TAG_SUFFIX=-ol2.7.2-slaptain.
SLAPD_TAG_SUFFIX ?=

# Debug variants of the slapd runtime image (see docs/DEBUG-IMAGES.md). Built
# and pushed only on demand — never part of `all` or `push`, never the default
# in a chart. Pin per SlapdCluster with spec.images.slapd.tag=<tag>-debug;
# spec.images.init.tag stays on the plain tag (unlike -ol26, where both move
# together — these are the same OpenLDAP version, so the on-disk format is
# identical and only slapd's own flags differ).
SLAPD_IMAGE_DEBUG = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-debug
SLAPD_IMAGE_ASAN  = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-asan

# The openldap-deb carriers, the -ol26src/-ol2616 label sets and the 2.6
# forward-port's build arguments all moved into docker-bake.hcl. A carrier is
# no longer an image at all: bake hands it to both consumers of a pair as a
# `target:` context, in one BuildKit graph, so it is never named, pushed or
# pulled. See the header of that file.

# Set RUN_UPSTREAM_TESTS=1 to run OpenLDAP's own test suite during the package
# build (slow; off by default — see images/openldap-deb/README.md).
RUN_UPSTREAM_TESTS ?= 0

# Helm chart OCI registry. Charts land under <registry>/<project>/charts/<name>.
# Pull example: helm pull oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain --version X.Y.Z
CHART_REGISTRY ?= oci://$(REGISTRY)/charts
CHART_OUT      := .charts

# Charts that get published. Directory names under charts/; the packaged file
# is named from each Chart.yaml (charts/operator -> slaptain-X.Y.Z.tgz).
# All four are user-facing entry points and each answers a different question:
#   operator       the control plane; the other three need it
#   slapd-mesh     a whole multi-site mesh, same values file at every site
#   slapd          one site's SlapdCluster
#   slapd-toolkit  the debug pod
PUBLISH_CHARTS ?= operator slapd-mesh slapd slapd-toolkit

# Chart version derived from GIT_TAG; must be SemVer-2 for Helm.
# - On a release tag (vX.Y.Z): strip the leading 'v' → X.Y.Z.
# - Off-tag dev builds: 0.0.0-<commit>[-dirty] pseudo-version (valid SemVer pre-release).
# On a version tag IMAGE_TAG is already the bare semver, so chart version,
# appVersion and image tag are all one string. Off-tag, a hash is not SemVer-2
# and Helm rejects it, so it becomes a valid pre-release — built from GIT_TAG,
# NOT from IMAGE_TAG: `0.0.0-09ecf10`, not `0.0.0-sha-09ecf10`. The sha- prefix
# is an addressing convention for registries and has no business inside a
# version number. Same split as the sibling project.
CHART_VERSION := $(if $(IS_VERSION_TAG),$(IMAGE_TAG),0.0.0-$(GIT_TAG))

# Import an image into every node's CRI over SSH, for clusters with no
# reachable registry. $(1) is the bake target (which names the tarball), $(2)
# the image reference (which is what the nodes will have). The node check
# comes first: with content-derived tags, a tag match IS a version match.
ifeq ($(CRI),crio)
define bake-import
	@if $(KUBECTL) get nodes -o json 2>/dev/null | grep -Fq '"$(2)"'; then \
		echo "$(2) already on nodes, skipping import"; \
	else \
		$(BAKE) --import $(1) && \
		for node in $(NODE_IPS); do \
			echo "Importing $(2) to $$node (cri-o)..."; \
			ssh $(NODE_USER)@$$node \
				'cat > /tmp/_cri_import.tar && sudo skopeo copy docker-archive:/tmp/_cri_import.tar containers-storage:$(2) && rm -f /tmp/_cri_import.tar' \
				< .build/$(1).tar; \
		done; \
	fi
endef
else
define bake-import
	@if $(KUBECTL) get nodes -o json 2>/dev/null | grep -Fq '"$(2)"'; then \
		echo "$(2) already on nodes, skipping import"; \
	else \
		$(BAKE) --import $(1) && \
		for node in $(NODE_IPS); do \
			echo "Importing $(2) to $$node (containerd)..."; \
			ssh $(NODE_USER)@$$node sudo ctr -n k8s.io images import - < .build/$(1).tar; \
		done; \
	fi
endef
endif

# `images` MUST be here, and the reason is not hygiene: there is a directory
# called images/ in this repository, so without it make takes the target as an
# up-to-date file and does nothing -- reporting success while building not one
# image. Found exactly that way.
.PHONY: images push-ol2616
.PHONY: all build-slaptain push-slaptain build-openldap-deb build-init build-slapd build-init-ol26 build-slapd-ol26 build-ol26 build-openldap-deb-ol26src build-init-ol26src build-slapd-ol26src build-ol26src build-openldap-deb-ol2616 build-init-ol2616 build-slapd-ol2616 build-ol2616 import-init-ol2616 import-slapd-ol2616 import-ol2616 push-init-ol2616 push-slapd-ol2616 push-ol2616 import-init-ol26src import-slapd-ol26src import-ol26src build-openldap-deb-debug build-slapd-debug build-openldap-deb-asan build-slapd-asan import-slapd-debug import-slapd-asan push-slapd-debug push-slapd-asan build-toolkit build-operator build-exporter build-slctl install-slctl push gencert helm-deploy cluster-helm-install cluster-helm-uninstall mesh-helm-install mesh-helm-uninstall operator-crd-apply operator-helm-install operator-helm-uninstall operator-chart-package operator-chart-push charts-package charts-push test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run e2e-resilience e2e-external-replication e2e-multisite e2e-multisite-setup e2e-multisite-test e2e-multisite-teardown e2e-migration e2e-migration-setup e2e-migration-test e2e-migration-teardown import import-init import-slapd import-toolkit import-operator import-exporter import-init-ol26 import-slapd-ol26 import-ol26 push-operator deliver deliver-operator deploy-operator clean show-tag helm-lint verify-generated tidy test-init


## ── Image targets ──────────────────────────────────────────────────────────
## Aliases onto docker-bake.hcl. A `build-*` target BUILDS and publishes
## nothing; `push*` publishes; `import*` writes a tarball and streams it to the
## nodes. That split is back to what it was before BuildKit: it was briefly
## lost when every build had to push, and bake's `target:` contexts gave it
## back by removing the registry from the carrier handoff.

## all: every published image plus slctl. Publishes nothing -- use `push`.
all: images build-slctl

## images: the eleven published images (both 2.7 pairs, both 2.6 pairs,
## toolkit, operator, exporter). The openldap-deb carriers are built as
## dependencies and are not images: they carry no tag, so no output setting
## can make one escape to a registry.
images:
	$(BAKE) images

push:
	$(BAKE) --push images

## Individual images. The bake target names are the image names, so there is
## nothing to translate; `scripts/bake.sh --print <target>` shows any of them.
build-init:         ; $(BAKE) slapd-init
build-slapd:        ; $(BAKE) slapd
build-slaptain:     ; $(BAKE) slapd-init-slaptain slapd-slaptain
build-init-ol26:    ; $(BAKE) slapd-init-ol26
build-slapd-ol26:   ; $(BAKE) slapd-ol26
build-ol26:         ; $(BAKE) ol26
build-ol26src:      ; $(BAKE) ol26src
build-toolkit:      ; $(BAKE) toolkit
build-operator:     ; $(BAKE) operator
build-exporter:     ; $(BAKE) exporter

## A carrier on its own. It publishes nothing by construction (carriers have
## no tag), so this is a build-and-discard: the point is the gates inside the
## Containerfile -- the version/flavour agreement gate and the logpurge arity
## gate -- which is exactly what you want to run after touching a patch.
build-openldap-deb:         ; $(BAKE) openldap-deb
build-openldap-deb-slaptain:; $(BAKE) openldap-deb-slaptain
build-openldap-deb-ol26src: ; $(BAKE) openldap-deb-ol26src
build-openldap-deb-ol2616:  ; $(BAKE) openldap-deb-ol2616
build-openldap-deb-debug:   ; $(BAKE) openldap-deb-debug
build-openldap-deb-asan:    ; $(BAKE) openldap-deb-asan

## Halves of a pair, where a doc or a runbook names one.
build-init-ol26src:  ; $(BAKE) slapd-init-ol26src
build-slapd-ol26src: ; $(BAKE) slapd-ol26src
build-init-ol2616:   ; $(BAKE) slapd-init-ol2616
build-slapd-ol2616:  ; $(BAKE) slapd-ol2616
push-init-ol2616:    ; $(BAKE) --push slapd-init-ol2616
push-slapd-ol2616:   ; $(BAKE) --push slapd-ol2616

push-slaptain:      ; $(BAKE) --push slapd-init-slaptain slapd-slaptain
push-operator:      ; $(BAKE) --push operator

## build-ol2616: EXPERIMENTAL upstream 2.6.16 pair (forward-port). Internal
## use only -- never in `images`, never in a chart, and deliberately not a
## select.flavour cell: the lmdb0.9 cells mean "Debian's", which is the
## contract -ol26/-ol26src carry and a forward-port breaks. Reach it by
## pinning both tags, or with SLAPD_TAG_SUFFIX=-ol2.6.16-slaptain.
build-ol2616:       ; $(BAKE) ol2616
push-ol2616:        ; $(BAKE) --push ol2616

## Diagnostic variants (docs/DEBUG-IMAGES.md). Each needs its own package
## build -- the compiler flags live in the .debs -- so each is a full cycle.
## Never in `images`, never in a chart. Deploy by pinning
## spec.images.slapd.tag ALONE; slapd-init stays on the plain tag.
build-slapd-debug:  ; $(BAKE) debug
build-slapd-asan:   ; $(BAKE) asan
push-slapd-debug:   ; $(BAKE) --push debug
push-slapd-asan:    ; $(BAKE) --push asan

## Import into the nodes' CRI. Each rebuilds (BuildKit serves it from cache)
## and writes .build/<target>.tar, which is then streamed over SSH.
import-init:        ; $(call bake-import,slapd-init,$(INIT_IMAGE))
import-slapd:       ; $(call bake-import,slapd,$(SLAPD_IMAGE))
import-toolkit:     ; $(call bake-import,toolkit,$(TOOLKIT_IMAGE))
import-operator:    ; $(call bake-import,operator,$(OPERATOR_IMAGE))
import-exporter:    ; $(call bake-import,exporter,$(EXPORTER_IMAGE))
import: import-init import-slapd import-toolkit import-operator import-exporter

import-init-ol26:   ; $(call bake-import,slapd-init-ol26,$(INIT_IMAGE_OL26))
import-slapd-ol26:  ; $(call bake-import,slapd-ol26,$(SLAPD_IMAGE_OL26))
import-ol26: import-init-ol26 import-slapd-ol26

import-init-ol26src:  ; $(call bake-import,slapd-init-ol26src,$(INIT_IMAGE_OL26SRC))
import-slapd-ol26src: ; $(call bake-import,slapd-ol26src,$(SLAPD_IMAGE_OL26SRC))
import-ol26src: import-init-ol26src import-slapd-ol26src

import-init-ol2616:   ; $(call bake-import,slapd-init-ol2616,$(INIT_IMAGE_OL2616))
import-slapd-ol2616:  ; $(call bake-import,slapd-ol2616,$(SLAPD_IMAGE_OL2616))
import-ol2616: import-init-ol2616 import-slapd-ol2616

import-slapd-debug: ; $(call bake-import,slapd-debug,$(SLAPD_IMAGE_DEBUG))
import-slapd-asan:  ; $(call bake-import,slapd-asan,$(SLAPD_IMAGE_ASAN))

## Delivery: dispatch to push or import based on DELIVERY
deliver: $(DELIVERY)
deliver-operator: $(DELIVERY)-operator

build-slctl:
	cd operator && go build -o ../bin/slctl ./cmd/slctl/

install-slctl: build-slctl
	sudo install -m 0755 bin/slctl /usr/local/bin/slctl

## Operator dev fast-path: build + deliver + helm upgrade + restart
deploy-operator: deliver-operator operator-helm-install
	$(KUBECTL) rollout restart deployment/slaptain -n $(NAMESPACE)

operator-generate:
	$(MAKE) -C operator generate

operator-manifests:
	$(MAKE) -C operator manifests
	$(MAKE) operator-sync-crd

operator-sync-crd:
	cp operator/config/crd/bases/*.yaml charts/operator/crds/

## operator-crd-apply: install/update the operator CRDs directly via kubectl.
## Helm installs the chart's crds/ ONLY on the first `helm install` and NEVER on
## `helm upgrade` (by design — it refuses to own CRD lifecycle). So a new or
## changed CRD must be applied out-of-band, or the upgraded operator can't see
## it. Server-side apply sidesteps the client-side last-applied-configuration
## annotation size limit that large CRDs (e.g. slapdclusters) would otherwise hit.
operator-crd-apply:
	$(KUBECTL) apply --server-side --force-conflicts -f operator/config/crd/bases/

gencert:
	$(KUBECTL) get namespace $(NAMESPACE_TESTING) >/dev/null 2>&1 || $(KUBECTL) create namespace $(NAMESPACE_TESTING)
	cd tests && ./gencert.sh $(if $(CONTEXT),-c $(CONTEXT)) -n $(NAMESPACE_TESTING) -t slapd -s slapd -H slapd-headless slapd-tls

## helm-deploy: full pipeline — build images, deliver them, generate certs, and
## install the SlapdCluster chart. (The separate helm-install/helm-uninstall
## targets are gone with the standalone chart; cluster-helm-* is the one path.)
helm-deploy: deliver gencert cluster-helm-install ## Full pipeline: build images, deliver, generate certs, deploy

## Test resources: SlapdSchema + SlapdDatabase + readpw Secret.
## Set TEST_RESOURCES to "example" or "lab" (default: example; "lab" is the
## internal fixture set and requires SOPS-decryptable secrets — see tests/resources/lab/).
TEST_RESOURCES ?= example

testing-apply:
	$(KUBECTL) apply -n $(NAMESPACE_TESTING) -f tests/resources/$(TEST_RESOURCES)/

testing-delete:
	$(KUBECTL) delete -n $(NAMESPACE_TESTING) -f tests/resources/$(TEST_RESOURCES)/ --ignore-not-found

## Legacy aliases (deprecated — use testing-apply / testing-delete).
testing-helm-install: testing-apply
testing-helm-uninstall: testing-delete

cluster-helm-install:
	$(HELM) upgrade --install slapd ./charts/slapd \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		-f tests/values.slapd-persistent.yaml \
		--set images.slapd.tag=$(IMAGE_TAG)$(SLAPD_TAG_SUFFIX) \
		--set images.init.tag=$(IMAGE_TAG)$(SLAPD_TAG_SUFFIX)

cluster-helm-uninstall:
	$(HELM) uninstall slapd --namespace $(NAMESPACE_TESTING)

## mesh-helm-install: the slapd-mesh chart at ONE site, from the working tree
## instead of the published OCI chart — the local-build counterpart of step 7 in
## docs/MULTI-SITE.md. Loop it over the sites the same way:
##   yq -r '.sites[].context // .sites[].name' lab.yaml |
##     while read -r ctx; do make mesh-helm-install CONTEXT="$$ctx"; done
##
## NOTE there is no image pinning here, and that is deliberate rather than an
## omission: the mesh chart leaves cluster.images empty so the OPERATOR defaults
## both data-plane images to its own registry and tag. Installing the operator
## from a local build is therefore the whole of "run local images" — pinning
## them here as well would break that coupling and is how a site ends up running
## a slapd that does not match the operator managing it.
##
## The one exception is the exporter sidecar (cluster.monitoring.enabled): it is
## its own version line, and the operator defaults it to a compiled-in exporter
## RELEASE, not to its own tag. To run a locally built, unreleased exporter, pin
## it: HELM_ARGS="--set cluster.images.exporter.tag=<exporter image tag>" (the
## tag `make show-tag` prints).
##
## Measured 2026-09-28 walking docs/MULTI-SITE.md by hand at three sites: with
## nothing pinned anywhere, operator sha-f3782be produced slapd sha-f3782be at
## every site, and slctl inspect read 18/18.
##
## The values files are the two written in step 1b, and they are per-lab rather
## than per-site: every site gets the same bytes (ADR-028 §3).
MESH_RELEASE   ?= ldap
MESH_NAMESPACE ?= slaptain
MESH_VALUES    ?= values.topology.yaml values.directory.yaml

mesh-helm-install:
	$(HELM) upgrade --install $(MESH_RELEASE) ./charts/slapd-mesh \
		--namespace $(MESH_NAMESPACE) --create-namespace \
		$(foreach f,$(MESH_VALUES),-f $(f)) $(HELM_ARGS)

mesh-helm-uninstall:
	$(HELM) uninstall $(MESH_RELEASE) --namespace $(MESH_NAMESPACE)

## operator-helm-install: deploy/upgrade the operator. Depends on
## operator-crd-apply so CRDs are always current — `helm upgrade` won't do it
## (see operator-crd-apply), which otherwise leaves a new/changed CRD missing or
## stale after an upgrade. This is the t3e iteration-loop command:
##   make operator-helm-install CONTEXT=t3e GIT_TAG=<pushed-tag>
##
## SITE_NAME sets the operator's own site identity (ADR-028 §4) — the one
## per-site fact in a whole mesh, and the reason this target needs a knob at
## all: `make <target> --set foo=bar` cannot work, because make parses --set as
## its own option. Walking docs/MULTI-SITE.md against locally built images:
##   yq -r '.sites[] | (.context // .name) + " " + .name' lab.yaml |
##     while read -r ctx site; do
##       make operator-helm-install CONTEXT="$$ctx" SITE_NAME="$$site"
##     done
##
## HELM_ARGS is the escape hatch for everything else (-f values.yaml, --set
## resources..., --wait). Both are empty by default, so nothing changes for
## callers that pass neither.
operator-helm-install: operator-crd-apply
	$(HELM) upgrade --install slaptain ./charts/operator \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(REGISTRY)/$(PROJECT)/operator \
		--set image.tag=$(IMAGE_TAG) \
		$(if $(SITE_NAME),--set siteName=$(SITE_NAME)) $(HELM_ARGS)

operator-helm-uninstall:
	$(HELM) uninstall slaptain --namespace $(NAMESPACE)

## charts-package: package every chart in $(PUBLISH_CHARTS) into $(CHART_OUT)/.
## Version and appVersion are overridden from the git tag (see CHART_VERSION/
## GIT_TAG), so a chart's appVersion always names an image tag that exists — the
## charts default their image tags to .Chart.AppVersion for exactly this reason.
## CRDs are synced first so the packaged operator chart carries the current CRD.
charts-package: operator-sync-crd
	@mkdir -p $(CHART_OUT)
	@for d in $(PUBLISH_CHARTS); do \
		echo ">>> packaging charts/$$d"; \
		$(HELM) package ./charts/$$d -d $(CHART_OUT) \
			--version $(CHART_VERSION) --app-version $(IMAGE_TAG) || exit 1; \
	done

## charts-push: push every packaged chart to $(CHART_REGISTRY).
## Requires `helm registry login` against $(REGISTRY) first.
charts-push: charts-package
	@for d in $(PUBLISH_CHARTS); do \
		n=$$($(HELM) show chart ./charts/$$d | awk '/^name:/{print $$2}'); \
		echo ">>> pushing $$n-$(CHART_VERSION).tgz"; \
		$(HELM) push $(CHART_OUT)/$$n-$(CHART_VERSION).tgz $(CHART_REGISTRY) || exit 1; \
	done

## operator-chart-package / operator-chart-push: the operator chart alone.
## Kept because the release flow and docs name them, and because pushing just
## the control plane is a real thing to want.
operator-chart-package: operator-sync-crd
	@mkdir -p $(CHART_OUT)
	$(HELM) package ./charts/operator -d $(CHART_OUT) \
		--version $(CHART_VERSION) --app-version $(IMAGE_TAG)

operator-chart-push: operator-chart-package
	$(HELM) push $(CHART_OUT)/slaptain-$(CHART_VERSION).tgz $(CHART_REGISTRY)

## toolkit-install: deploy the slapd-toolkit debug pod (ldap-utils, python3, ldap3).
toolkit-install:
	$(HELM) upgrade --install toolkit ./charts/slapd-toolkit \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		--set image.tag=$(IMAGE_TAG)

toolkit-uninstall:
	$(HELM) uninstall toolkit --namespace $(NAMESPACE_TESTING)

## e2e: full setup + test run + teardown
e2e: e2e-run

## e2e-run: run tests against an already-installed cluster (skips helm setup/teardown)
e2e-run:
	cd tests/e2e && go test -v ./... -timeout 30m --ginkgo.v --ginkgo.timeout=25m

## e2e-resilience: run all tests including slow pod-restart and warm-start tests
e2e-resilience:
	cd tests/e2e && E2E_RESILIENCE=1 go test -v ./... -timeout 35m --ginkgo.v --ginkgo.timeout=30m

## e2e-external-replication: run cross-cluster external replication tests
e2e-external-replication:
	cd tests/e2e && E2E_EXTERNAL_REPL=1 go test -v ./... -timeout 20m --ginkgo.v --ginkgo.timeout=15m --ginkgo.label-filter=external-replication

## e2e-multisite: full multi-site setup + test + teardown (pass CONTEXTS="s1 s2 s3")
e2e-multisite:
	./tests/e2e.sh all $(CONTEXTS)

## e2e-multisite-setup: deploy multi-site infrastructure
e2e-multisite-setup:
	./tests/e2e.sh setup $(CONTEXTS)

## e2e-multisite-test: run tests against existing multi-site deployment
e2e-multisite-test:
	./tests/e2e.sh test $(CONTEXTS)

## e2e-multisite-teardown: remove multi-site infrastructure
e2e-multisite-teardown:
	./tests/e2e.sh teardown $(CONTEXTS)

## e2e-migration: full migration-scenario setup + test + teardown (ADR-010 3f).
## Stands up a "fake-prod" SlapdCluster (peer, plain syncrepl) and a slaptain
## SlapdCluster (consumer-only) in two namespaces, then exercises in-place
## promotion (consumer-only → peer) with operational-attribute preservation.
e2e-migration:
	./tests/e2e-migration.sh all $(CONTEXT)

e2e-migration-setup:
	./tests/e2e-migration.sh setup $(CONTEXT)

e2e-migration-test:
	./tests/e2e-migration.sh test $(CONTEXT)

e2e-migration-teardown:
	./tests/e2e-migration.sh teardown $(CONTEXT)

## helm-lint: lint every published chart and render each in the shapes its
## users actually install. A template error is invisible to every Go gate and
## otherwise surfaces at `helm install` time on the user's cluster. The mesh
## chart has no renderable default on purpose (mesh.sites is required — a
## quietly rendered one-site mesh would hand every site the same serverID
## slot), so it renders from its shipped three-site example. CI's chart gate
## (.github/workflows/lint.yml) and the release's pre-push check both run this.
helm-lint:
	$(HELM) lint ./charts/operator
	$(HELM) lint ./charts/slapd
	$(HELM) lint ./charts/slapd-mesh -f charts/slapd-mesh/examples/three-site.yaml
	$(HELM) lint ./charts/slapd-toolkit
	$(HELM) template slaptain ./charts/operator > /dev/null
	$(HELM) template slaptain ./charts/operator \
		--set siteName=site-a \
		--set multus.network=infra/replication-net \
		--set metrics.enabled=true \
		--set metrics.serviceMonitor.enabled=true \
		--set networkPolicy.enabled=true > /dev/null
	$(HELM) template slapd ./charts/slapd > /dev/null
	$(HELM) template slapd ./charts/slapd -f tests/values.slapd-persistent.yaml > /dev/null
	$(HELM) template ldap ./charts/slapd-mesh -f charts/slapd-mesh/examples/three-site.yaml > /dev/null
	$(HELM) template toolkit ./charts/slapd-toolkit > /dev/null
# Rotation is opt-in, so the shapes above all render it OFF. These two render it
# ON: without them nothing in CI exercises the rotation templating, and
# examples/rotation.yaml is a file nothing reads until it has already rotted.
	$(HELM) template slapd ./charts/slapd \
		--set rotation.enabled=true --set rotation.slotLength=6h > /dev/null
	$(HELM) template ldap ./charts/slapd-mesh \
		-f charts/slapd-mesh/examples/three-site.yaml \
		-f charts/slapd-mesh/examples/rotation.yaml > /dev/null

## verify-generated: regenerate the CRDs, the RBAC role and the deepcopy, and
## fail if any checked-in copy (operator/config, charts/operator/crds,
## zz_generated.deepcopy.go) changes. Same class of check as `go mod tidy`:
## nothing used to notice a stale tree. See hack/verify-generated.sh.
verify-generated:
	@./hack/verify-generated.sh

## tidy: `go mod tidy` in every Go module. The three are not independent:
## tests/e2e replaces the operator module by path, so its go.mod mirrors the
## operator's dependency graph and a bump in operator/go.mod leaves tests/e2e
## untidy until this runs. CI's tidy check and the Dependabot tidy workflow
## both call this, so the list of modules lives here and nowhere else.
tidy:
	@for m in operator exporter tests/e2e; do (cd "$$m" && go mod tidy) || exit 1; done

## ── Monitoring (ADR-033) ────────────────────────────────────────────────────
## Site-local (ADR-033 amendment 2026-10-09): EVERY site runs the same stack
## and scrapes only itself, over ClusterIP — kube-state-metrics (Surface 1),
## the operator (Surface 2) and the slapd exporter sidecars (Surface 3). No
## values file here carries a per-site fact. Per site, in this order:
##   1. monitoring-stack-install   Prometheus Operator CRDs + Prometheus +
##                                 Grafana, then the rules and the dashboard
##   2. monitoring-ksm-install     its ServiceMonitor needs the CRDs from 1
##   3. the operator, with its ServiceMonitor and slapd PodMonitor on. For an
##      operator that is ALREADY INSTALLED, keep its values and add the three:
##        helm upgrade slaptain ./charts/operator -n slaptain-system --reuse-values \
##          --set metrics.enabled=true --set metrics.serviceMonitor.enabled=true \
##          --set slapdPodMonitor.enabled=true
##      NOT operator-helm-install: that target sets image.repository/tag from
##      THIS checkout, so on an existing install it swaps the operator image —
##      which re-images every slapd and init left unpinned and rolls those
##      pods, or sticks in ImagePullBackOff if that tag was never pushed. (The
##      exporter sidecar is not among them: it has its own version line.) It is for
##      installs running this checkout's own images:
##        make operator-helm-install CONTEXT=<ctx> SITE_NAME=<site> \
##          HELM_ARGS="--set metrics.enabled=true --set metrics.serviceMonitor.enabled=true --set slapdPodMonitor.enabled=true"
##   4. the fixture / the SlapdCluster
## monitoring-install does 1 and 2. See docs/MONITORING.md.
MONITORING_NS ?= monitoring
MONITORING_KSM_RELEASE ?= ksm
MONITORING_STACK_RELEASE ?= monitoring
## Chart versions, pinned: the values files are written against these. Helm
## never upgrades a chart's crds/ — on a kube-prometheus-stack MAJOR bump,
## apply the new Prometheus Operator CRDs yourself first (the chart's upgrade
## notes name the exact command).
MONITORING_STACK_VERSION ?= 92.2.0
MONITORING_KSM_VERSION ?= 8.6.0
MONITORING_STACK_VALUES ?= monitoring/kube-prometheus-stack-values.yaml
## Optional: pin the Gateway addresses so a URL survives a recreate. Unset
## means LB-IPAM assigns whatever is free.
comma := ,
MONITORING_GRAFANA_IP ?=
MONITORING_PROMETHEUS_IP ?=
## Listener ports. 80 suits LB-IPAM, where each Gateway gets an address of its
## own. Under Cilium's host-network Gateway mode both Gateways listen on every
## node's own address, so they need DIFFERENT ports — e.g. 3000 and 9090.
MONITORING_GRAFANA_PORT ?= 80
MONITORING_PROMETHEUS_PORT ?= 80

monitoring-repo: ## Add/update the prometheus-community Helm repo
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
	helm repo update prometheus-community

## monitoring-install: steps 1 and 2 above at ONE site. Loop it over the sites:
##   for ctx in site1 site2 site3; do make monitoring-install CONTEXT=$$ctx; done
monitoring-install:
	$(MAKE) monitoring-stack-install CONTEXT=$(CONTEXT)
	$(MAKE) monitoring-ksm-install CONTEXT=$(CONTEXT)

## monitoring-stack-install: Prometheus + Grafana at ONE site, then the alert
## rules and the dashboard. Install it first: it brings the Prometheus
## Operator CRDs every later ServiceMonitor/PodMonitor needs. The namespace is
## labelled privileged because node-exporter needs hostNetwork/hostPID/host
## paths; under an enforced `baseline` (Talos' default) it would get no pod.
monitoring-stack-install:
	$(KUBECTL) create namespace $(MONITORING_NS) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) label namespace $(MONITORING_NS) --overwrite pod-security.kubernetes.io/enforce=privileged
	$(HELM) upgrade --install $(MONITORING_STACK_RELEASE) prometheus-community/kube-prometheus-stack \
		--version $(MONITORING_STACK_VERSION) \
		--namespace $(MONITORING_NS) --create-namespace \
		-f $(MONITORING_STACK_VALUES) --wait --timeout 10m
	$(MAKE) monitoring-rules-apply monitoring-dashboard-apply CONTEXT=$(CONTEXT)

## monitoring-ksm-install: kube-state-metrics at ONE site, with its
## ServiceMonitor — so after monitoring-stack-install.
monitoring-ksm-install:
	$(HELM) upgrade --install $(MONITORING_KSM_RELEASE) prometheus-community/kube-state-metrics \
		--version $(MONITORING_KSM_VERSION) \
		--namespace $(MONITORING_NS) --create-namespace \
		-f monitoring/kube-state-metrics-values.yaml --wait
	@# The chart puts no checksum annotation on the custom-resource-state
	@# ConfigMap, so a changed config leaves the running pod on the OLD one —
	@# and the only symptom is a label quietly missing from the output.
	$(KUBECTL) rollout restart deployment/$(MONITORING_KSM_RELEASE)-kube-state-metrics -n $(MONITORING_NS)
	$(KUBECTL) rollout status deployment/$(MONITORING_KSM_RELEASE)-kube-state-metrics -n $(MONITORING_NS) --timeout=120s

monitoring-ksm-uninstall:
	$(HELM) uninstall $(MONITORING_KSM_RELEASE) --namespace $(MONITORING_NS)

## monitoring-gateway-apply: expose Grafana and Prometheus over Gateway API,
## one Gateway each (see the header of monitoring/gateway.yaml for why not one
## Gateway with two listeners). Everything is rendered into the manifest before
## a single apply, so KUBECTL_APPLY_FLAGS=--dry-run=server previews all of it.
##   LB-IPAM (default): optionally pin MONITORING_GRAFANA_IP /
##     MONITORING_PROMETHEUS_IP; unset, LB-IPAM chooses.
##   Cilium host-network Gateway mode: no addresses, distinct ports —
##     make monitoring-gateway-apply CONTEXT=<site> \
##       MONITORING_GRAFANA_PORT=3000 MONITORING_PROMETHEUS_PORT=9090
MONITORING_GATEWAY_EXPR = \
  (select(.kind == "Gateway" and .metadata.name == "slaptain-grafana") | .spec.listeners[0].port) = $(MONITORING_GRAFANA_PORT) | \
  (select(.kind == "Gateway" and .metadata.name == "slaptain-prometheus") | .spec.listeners[0].port) = $(MONITORING_PROMETHEUS_PORT) \
  $(if $(MONITORING_GRAFANA_IP),| (select(.kind == "Gateway" and .metadata.name == "slaptain-grafana") | .spec.addresses) = [{"type": "IPAddress"$(comma) "value": "$(MONITORING_GRAFANA_IP)"}]) \
  $(if $(MONITORING_PROMETHEUS_IP),| (select(.kind == "Gateway" and .metadata.name == "slaptain-prometheus") | .spec.addresses) = [{"type": "IPAddress"$(comma) "value": "$(MONITORING_PROMETHEUS_IP)"}])

monitoring-gateway-render: ## Print the Gateway manifest monitoring-gateway-apply would apply
	@yq '$(MONITORING_GATEWAY_EXPR)' monitoring/gateway.yaml

monitoring-gateway-apply:
	yq '$(MONITORING_GATEWAY_EXPR)' monitoring/gateway.yaml | $(KUBECTL) apply $(KUBECTL_APPLY_FLAGS) -f -
	@$(KUBECTL) get gateway -n $(MONITORING_NS) \
		-o custom-columns='NAME:.metadata.name,ADDRESS:.status.addresses[*].value,PORT:.spec.listeners[0].port,PROGRAMMED:.status.conditions[?(@.type=="Programmed")].status'

monitoring-gateway-delete:
	$(KUBECTL) delete -f monitoring/gateway.yaml --ignore-not-found

## monitoring-demo-cut: cut inbound LDAP to ONE site's slapd pods, to watch the
## cross-site signals move. CILIUM_CLUSTER is that site's cilium cluster-name
## and is required — the manifest ships with a placeholder on purpose. Read it
## with: kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.cluster-name}'
monitoring-demo-cut:
	@test -n "$(CILIUM_CLUSTER)" || { echo "CILIUM_CLUSTER is required (that site's cilium cluster-name)"; exit 1; }
	@sed 's|__CILIUM_CLUSTER__|$(CILIUM_CLUSTER)|' monitoring/demo/cut-inbound.yaml | $(KUBECTL) apply -f -
	@echo "cut applied. Expect the OTHER sites to report this one Unreachable within ~60-120s."
	@echo "CHECK THE CUT SITE TOO: it should still report its own peers Synced. If it reports"
	@echo "them down with an empty replicationState, the policy is blocking the local operator"
	@echo "as well and the cut is broader than intended."

## monitoring-demo-blackout: take ONE site dark to inbound LDAP, its own
## operator included. The verified fault injection — see the file header for
## exactly what it produces, including the operator bug it exposes.
monitoring-demo-blackout:
	@test -n "$(SITE_POD_CIDR)" || { echo "SITE_POD_CIDR is required (that site's pod CIDR)"; exit 1; }
	@sed 's|__SITE_POD_CIDR__|$(SITE_POD_CIDR)|' monitoring/demo/blackout.yaml | $(KUBECTL) apply -f -
	@echo "blackout applied. Within ~60-120s the OTHER sites report this one Unreachable;"
	@echo "this site reports PodsNotAnswering + CSNQueriesIncomplete, and its own"
	@echo "slapd-exporter targets go down (the scrape is inbound to the slapd pods too)."

## monitoring-demo-restore: remove either fault injection. Name-based, so it
## does not need the parameter the cut was applied with.
monitoring-demo-restore:
	@sed 's|__CILIUM_CLUSTER__|placeholder|' monitoring/demo/cut-inbound.yaml | $(KUBECTL) delete -f - --ignore-not-found
	@sed 's|__SITE_POD_CIDR__|0.0.0.0/0|' monitoring/demo/blackout.yaml | $(KUBECTL) delete -f - --ignore-not-found

monitoring-stack-uninstall:
	$(HELM) uninstall $(MONITORING_STACK_RELEASE) --namespace $(MONITORING_NS)

## monitoring-rules-test: run the promtool unit tests over the alert rules.
## They catch the one failure a live induction cannot tell apart from a healthy
## system — an expression that silently matches nothing. promtool rejects a
## PrometheusRule CR, so spec.groups is extracted first.
## Needs promtool: `pacman -S prometheus` (Arch), or any distro's prometheus package.
monitoring-rules-test:
	@command -v promtool >/dev/null || { echo "promtool not found — install it (Arch: pacman -S prometheus)"; exit 1; }
	@tmp=$$(mktemp -d) && yq '.spec' monitoring/prometheus-rules.yaml > $$tmp/rules.yaml && \
		cp monitoring/prometheus-rules-test.yaml $$tmp/ && \
		(cd $$tmp && promtool test rules prometheus-rules-test.yaml); \
		rc=$$?; rm -rf $$tmp; exit $$rc

monitoring-rules-apply: ## Apply the alert rules (needs the Prometheus Operator CRDs)
	$(KUBECTL) apply -f monitoring/prometheus-rules.yaml

## monitoring-dashboard-apply: (re)load the Grafana dashboards from the repo —
## every monitoring/dashboards/*.json, one ConfigMap key per file.
## The JSON is the source of truth; a dashboard edited in the UI must be
## exported back over its file or the next run of this target discards it.
monitoring-dashboard-apply:
	$(KUBECTL) create configmap slaptain-dashboards --namespace $(MONITORING_NS) \
		--from-file=monitoring/dashboards/ \
		--dry-run=client -o yaml | \
		$(KUBECTL) label --local -f - grafana_dashboard=1 -o yaml --dry-run=client | \
		$(KUBECTL) apply -f -

## Port-forwards, as a fallback where Gateway API or a LoadBalancer is not
## available. With monitoring-gateway-apply these are not needed.
monitoring-grafana: ## Port-forward Grafana to http://localhost:3000 (admin, generated password)
	@echo "Grafana → http://localhost:3000  (user admin; password: kubectl -n $(MONITORING_NS) get secret $(MONITORING_STACK_RELEASE)-grafana -o jsonpath='{.data.admin-password}' | base64 -d)"
	$(KUBECTL) --namespace $(MONITORING_NS) port-forward svc/$(MONITORING_STACK_RELEASE)-grafana 3000:80

monitoring-prometheus: ## Port-forward Prometheus to http://localhost:9090
	@echo "Prometheus → http://localhost:9090"
	$(KUBECTL) --namespace $(MONITORING_NS) port-forward svc/$(MONITORING_STACK_RELEASE)-kube-prometheus-prometheus 9090:9090

## test-init: the init container's own unit tests — the offline startup-only
## cn=config converger (ADR-034). Standard library only and no container, so it
## runs anywhere python3 does; the fixtures are real `slapcat -n 0` output.
test-init:
	cd images/slapd-init && python3 -m unittest discover -s . -p 'test_*.py' -v

show-tag: ## Print the git tags and the image/chart tags derived from them
	@echo "git:    $(GIT_TAG)"
	@echo "image:  $(IMAGE_TAG)"
	@echo "chart:  $(CHART_VERSION)"
	@echo "exporter git:    $(EXPORTER_GIT_TAG)"
	@echo "exporter image:  $(EXPORTER_IMAGE_TAG)"

clean:
	rm -rf .charts .build bin/ *.tar
