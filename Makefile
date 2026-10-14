REGISTRY ?= ghcr.io/chuck-chuck-chuck-net
PROJECT ?= slaptain
NAMESPACE ?= slaptain-system
NAMESPACE_TESTING ?= slaptain-testing
CONTAINER_ENGINE ?= podman

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
# what it IS, and one variable cannot answer both (docs/VERSIONING.md).
#   tagged    the bare semver — v0.2.1 is addressed :0.2.1, matching the chart
#             version and appVersion exactly, which is what makes the charts'
#             `image.tag | default .Chart.AppVersion` fallback correct by
#             construction instead of by maintenance.
#   untagged  sha-<hash>, sha-<hash>-dirty-<state8>. The prefix matches the
#             sibling project (littlered), where it is forced: its CI's
#             docker/metadata-action `type=sha` publishes exactly that, so a
#             Makefile default asking for :<hash> could never address a
#             CI-built image. slaptain's own workflows (.github/workflows/
#             publish*.yml) publish through `make push`, i.e. through THIS
#             rule, so the spelling costs nothing here and makes the two
#             projects read the same. It also makes a registry listing self-describing:
#             `0.2.0` is a release, `sha-09ecf10` is a build of a commit.
IMAGE_TAG := $(if $(IS_VERSION_TAG),$(GIT_TAG:v%=%),sha-$(GIT_TAG))

# The slapd exporter is its own version line (ADR-033 decision 4,
# docs/VERSIONING.md): git tags exporter/vX.Y.Z, image tag the bare X.Y.Z, and
# off-tag the same sha-<hash>[-dirty-<state8>] addressing as above. Same script,
# its own stream — each stream matches only its own tag pattern, so a commit
# carrying both kinds of tag resolves unambiguously in either.
EXPORTER_GIT_TAG := $(shell scripts/image-tag.sh exporter)
IS_EXPORTER_VERSION_TAG := $(shell echo '$(EXPORTER_GIT_TAG)' | grep -qE '^exporter/v[0-9]+\.[0-9]+\.[0-9]+' && echo true)
EXPORTER_IMAGE_TAG := $(if $(IS_EXPORTER_VERSION_TAG),$(EXPORTER_GIT_TAG:exporter/v%=%),sha-$(EXPORTER_GIT_TAG))

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
INIT_IMAGE_OL26  = $(REGISTRY)/$(PROJECT)/slapd-init:$(IMAGE_TAG)-ol26
SLAPD_IMAGE_OL26 = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-ol26

# Tag suffix applied to the slapd/slapd-init image tags when DEPLOYING (only —
# the operator and toolkit tags are untouched). Empty = OpenLDAP 2.7.1;
# SLAPD_TAG_SUFFIX=-ol26 deploys the legacy 2.6 pair:
#   make cluster-helm-install SLAPD_TAG_SUFFIX=-ol26
SLAPD_TAG_SUFFIX ?=

# Debug variants of the slapd runtime image (see docs/DEBUG-IMAGES.md). Built
# and pushed only on demand — never part of `all` or `push`, never the default
# in a chart. Pin per SlapdCluster with spec.images.slapd.tag=<tag>-debug;
# spec.images.init.tag stays on the plain tag (unlike -ol26, where both move
# together — these are the same OpenLDAP version, so the on-disk format is
# identical and only slapd's own flags differ).
SLAPD_IMAGE_DEBUG = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-debug
SLAPD_IMAGE_ASAN  = $(REGISTRY)/$(PROJECT)/slapd:$(IMAGE_TAG)-asan

# OpenLDAP 2.7.1 .deb build. Local-only image (localhost/ prefix — never pushed):
# it carries nothing but /debs and is consumed by both the slapd and slapd-init
# 2.7 Containerfiles, so the package build runs once per tag for both.
OPENLDAP_DEB_IMAGE = localhost/$(PROJECT)/openldap-deb:$(IMAGE_TAG)
# Separate package builds for the debug variants: the compiler flags live in
# the .debs, so the carrier image cannot be shared with the release build.
OPENLDAP_DEB_IMAGE_DEBUG = localhost/$(PROJECT)/openldap-deb:$(IMAGE_TAG)-debug
OPENLDAP_DEB_IMAGE_ASAN  = localhost/$(PROJECT)/openldap-deb:$(IMAGE_TAG)-asan
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

# Stamp-file directory for incremental builds.
STAMPS := .stamps

# Source file dependencies per image. The 2.7 and 2.6 (-ol26) variants share a
# directory, so list their Containerfiles explicitly instead of globbing — a
# change to one variant must not rebuild the other.
OPENLDAP_DEB_SRCS := $(shell find images/openldap-deb -type f)
INIT_SRCS        := images/slapd-init/Containerfile images/slapd-init/bootstrap.sh \
                    images/slapd-init/startup_config.py
INIT_OL26_SRCS   := images/slapd-init/Containerfile.ol26 images/slapd-init/bootstrap.sh \
                    images/slapd-init/startup_config.py
SLAPD_SRCS       := images/slapd/Containerfile
SLAPD_OL26_SRCS  := images/slapd/Containerfile.ol26
TOOLKIT_SRCS := $(shell find images/slapd-toolkit -type f)
OPERATOR_SRCS := $(shell find images/operator -type f) $(shell find operator -type f -name '*.go') operator/go.mod operator/go.sum
EXPORTER_SRCS := $(shell find images/slapd-exporter -type f) $(shell find exporter -type f -name '*.go') exporter/go.mod exporter/go.sum

# Import a container image to all k8s nodes via SSH (unconditional).
# No leading @ — called from within import-if-needed which handles suppression.
ifeq ($(CRI),crio)
define do-import
for node in $(NODE_IPS); do \
	echo "Importing $(1) to $$node (cri-o)..."; \
	$(CONTAINER_ENGINE) save $(1) | ssh $(NODE_USER)@$$node \
		'cat > /tmp/_cri_import.tar && sudo skopeo copy docker-archive:/tmp/_cri_import.tar containers-storage:$(1) && rm -f /tmp/_cri_import.tar'; \
done
endef
else
define do-import
for node in $(NODE_IPS); do \
	echo "Importing $(1) to $$node (containerd)..."; \
	$(CONTAINER_ENGINE) save $(1) | ssh $(NODE_USER)@$$node sudo ctr -n k8s.io images import -; \
done
endef
endif

# Import a container image only if the tag is not already present on all nodes.
# Checks kubectl get nodes (CRI-agnostic, no SSH needed for the check).
# With content-based tags (:GIT_TAG), matching the tag guarantees the version.
define import-if-needed
	@if $(KUBECTL) get nodes -o json 2>/dev/null | grep -Fq '"$(1)"'; then \
		echo "$(1) already on nodes, skipping import"; \
	else \
		$(call do-import,$(1)); \
	fi
endef

.PHONY: all build-openldap-deb build-init build-slapd build-init-ol26 build-slapd-ol26 build-ol26 build-openldap-deb-debug build-slapd-debug build-openldap-deb-asan build-slapd-asan import-slapd-debug import-slapd-asan push-slapd-debug push-slapd-asan build-toolkit build-operator build-exporter build-slctl install-slctl push gencert helm-deploy cluster-helm-install cluster-helm-uninstall mesh-helm-install mesh-helm-uninstall operator-crd-apply operator-helm-install operator-helm-uninstall operator-chart-package operator-chart-push charts-package charts-push test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run e2e-resilience e2e-external-replication e2e-multisite e2e-multisite-setup e2e-multisite-test e2e-multisite-teardown e2e-migration e2e-migration-setup e2e-migration-test e2e-migration-teardown import import-init import-slapd import-toolkit import-operator import-exporter import-init-ol26 import-slapd-ol26 import-ol26 push-operator deliver deliver-operator deploy-operator clean show-tag helm-lint verify-generated test-init

## all: the seven pushable images plus slctl. build-ol26 is included so a
## release build carries the legacy OpenLDAP 2.6 pair too (ADR-021).
all: build-init build-slapd build-ol26 build-toolkit build-operator build-exporter build-slctl

## Stamp-file backed build targets (incremental)

$(STAMPS):
	mkdir -p $(STAMPS)

# Track the current GIT_TAG so builds re-trigger when the tag changes
# (e.g. new commit, dirty→clean transition). The file is only touched
# when its content actually changes.
$(STAMPS)/tag: FORCE | $(STAMPS)
	@if [ "$$(cat $@ 2>/dev/null)" != "$(IMAGE_TAG)" ]; then \
		echo "$(IMAGE_TAG)" > $@; \
	fi
.PHONY: FORCE

# The exporter's own tag stamp: its stream moves independently of IMAGE_TAG.
$(STAMPS)/exporter-tag: FORCE | $(STAMPS)
	@if [ "$$(cat $@ 2>/dev/null)" != "$(EXPORTER_IMAGE_TAG)" ]; then \
		echo "$(EXPORTER_IMAGE_TAG)" > $@; \
	fi

# OpenLDAP 2.7.1 packages, built once and consumed by both 2.7 images below.
$(STAMPS)/openldap-deb: $(OPENLDAP_DEB_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg RUN_UPSTREAM_TESTS=$(RUN_UPSTREAM_TESTS) \
		-t $(OPENLDAP_DEB_IMAGE) images/openldap-deb/
	@touch $@

$(STAMPS)/init: $(INIT_SRCS) $(STAMPS)/openldap-deb $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg OPENLDAP_DEB_IMAGE=$(OPENLDAP_DEB_IMAGE) \
		-t $(INIT_IMAGE) images/slapd-init/
	@touch $@

$(STAMPS)/slapd: $(SLAPD_SRCS) $(STAMPS)/openldap-deb $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg OPENLDAP_DEB_IMAGE=$(OPENLDAP_DEB_IMAGE) \
		-t $(SLAPD_IMAGE) images/slapd/
	@touch $@

$(STAMPS)/init-ol26: $(INIT_OL26_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/slapd-init/Containerfile.ol26 \
		-t $(INIT_IMAGE_OL26) images/slapd-init/
	@touch $@

$(STAMPS)/slapd-ol26: $(SLAPD_OL26_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/slapd/Containerfile.ol26 \
		-t $(SLAPD_IMAGE_OL26) images/slapd/
	@touch $@

# ── Debug variants (ADR-free: a diagnostic build, not a shipped artifact) ───
# Each needs its own package build, so each is a full ~10 min cycle. Nothing
# else depends on these stamps, so the normal loop never pays for them.

$(STAMPS)/openldap-deb-debug: $(OPENLDAP_DEB_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg RUN_UPSTREAM_TESTS=$(RUN_UPSTREAM_TESTS) \
		--build-arg BUILD_VARIANT=debug \
		-t $(OPENLDAP_DEB_IMAGE_DEBUG) images/openldap-deb/
	@touch $@

$(STAMPS)/slapd-debug: $(SLAPD_SRCS) $(STAMPS)/openldap-deb-debug $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg OPENLDAP_DEB_IMAGE=$(OPENLDAP_DEB_IMAGE_DEBUG) \
		--build-arg BUILD_VARIANT=debug \
		-t $(SLAPD_IMAGE_DEBUG) images/slapd/
	@touch $@

$(STAMPS)/openldap-deb-asan: $(OPENLDAP_DEB_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg RUN_UPSTREAM_TESTS=$(RUN_UPSTREAM_TESTS) \
		--build-arg BUILD_VARIANT=asan \
		-t $(OPENLDAP_DEB_IMAGE_ASAN) images/openldap-deb/
	@touch $@

$(STAMPS)/slapd-asan: $(SLAPD_SRCS) $(STAMPS)/openldap-deb-asan $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build \
		--build-arg OPENLDAP_DEB_IMAGE=$(OPENLDAP_DEB_IMAGE_ASAN) \
		--build-arg BUILD_VARIANT=asan \
		-t $(SLAPD_IMAGE_ASAN) images/slapd/
	@touch $@

$(STAMPS)/toolkit: $(TOOLKIT_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -t $(TOOLKIT_IMAGE) images/slapd-toolkit/
	@touch $@

$(STAMPS)/operator: $(OPERATOR_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/operator/Containerfile -t $(OPERATOR_IMAGE) .
	@touch $@

$(STAMPS)/exporter: $(EXPORTER_SRCS) $(STAMPS)/exporter-tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/slapd-exporter/Containerfile -t $(EXPORTER_IMAGE) .
	@touch $@

## Phony aliases so `make build-operator` etc. still work
build-openldap-deb: $(STAMPS)/openldap-deb
build-init: $(STAMPS)/init
build-slapd: $(STAMPS)/slapd
build-init-ol26: $(STAMPS)/init-ol26
build-slapd-ol26: $(STAMPS)/slapd-ol26
build-ol26: build-init-ol26 build-slapd-ol26
build-toolkit: $(STAMPS)/toolkit
build-operator: $(STAMPS)/operator
build-exporter: $(STAMPS)/exporter

## Debug variants of the slapd image. Deliberately absent from `all` and `push`.
build-openldap-deb-debug: $(STAMPS)/openldap-deb-debug
build-slapd-debug: $(STAMPS)/slapd-debug
build-openldap-deb-asan: $(STAMPS)/openldap-deb-asan
build-slapd-asan: $(STAMPS)/slapd-asan

build-slctl:
	cd operator && go build -o ../bin/slctl ./cmd/slctl/

install-slctl: build-slctl
	sudo install -m 0755 bin/slctl /usr/local/bin/slctl

## Push to container registry. Seven images: the 2.7 slapd pair (plain tags), the
## 2.6 pair (-ol26), toolkit, operator, and the slapd exporter at its own
## EXPORTER_IMAGE_TAG. The openldap-deb build artifact carrier is local-only and
## deliberately not pushed.
push: build-init build-slapd build-ol26 build-toolkit build-operator build-exporter
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)
	$(CONTAINER_ENGINE) push $(INIT_IMAGE_OL26)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE_OL26)
	$(CONTAINER_ENGINE) push $(TOOLKIT_IMAGE)
	$(CONTAINER_ENGINE) push $(OPERATOR_IMAGE)
	$(CONTAINER_ENGINE) push $(EXPORTER_IMAGE)

## Import images into k8s node CRI via SSH.
## Checks kubectl node image list first — skips import if the tag is already present.
## No per-context stamps needed: the content-based tag (:GIT_TAG) is the check.

import-init: build-init
	$(call import-if-needed,$(INIT_IMAGE))

import-slapd: build-slapd
	$(call import-if-needed,$(SLAPD_IMAGE))

import-toolkit: build-toolkit
	$(call import-if-needed,$(TOOLKIT_IMAGE))

import-operator: build-operator
	$(call import-if-needed,$(OPERATOR_IMAGE))

import-exporter: build-exporter
	$(call import-if-needed,$(EXPORTER_IMAGE))

import: import-init import-slapd import-toolkit import-operator import-exporter

## Legacy 2.6 pair — separate aggregate so the normal loop doesn't pay for it.
## Use it to run an e2e cycle against -ol26 images (ADR-021).
import-init-ol26: build-init-ol26
	$(call import-if-needed,$(INIT_IMAGE_OL26))

import-slapd-ol26: build-slapd-ol26
	$(call import-if-needed,$(SLAPD_IMAGE_OL26))

import-ol26: import-init-ol26 import-slapd-ol26

## Debug variants: deliver the one slapd image, leave slapd-init on the plain
## tag. Same OpenLDAP version, so the two images still agree on everything the
## runtime contract covers.
import-slapd-debug: build-slapd-debug
	$(call import-if-needed,$(SLAPD_IMAGE_DEBUG))

import-slapd-asan: build-slapd-asan
	$(call import-if-needed,$(SLAPD_IMAGE_ASAN))

push-slapd-debug: build-slapd-debug
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE_DEBUG)

push-slapd-asan: build-slapd-asan
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE_ASAN)

## Push a single image (per-image counterparts of the aggregate `push`,
## so `deliver-operator` resolves under the default DELIVERY=push).
push-operator: build-operator
	$(CONTAINER_ENGINE) push $(OPERATOR_IMAGE)

## Delivery: dispatch to push or import based on DELIVERY variable
deliver: $(DELIVERY)
deliver-operator: $(DELIVERY)-operator

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

## verify-generated: regenerate the CRDs, the RBAC role and the deepcopy, and
## fail if any checked-in copy (operator/config, charts/operator/crds,
## zz_generated.deepcopy.go) changes. Same class of check as `go mod tidy`:
## nothing used to notice a stale tree. See hack/verify-generated.sh.
verify-generated:
	@./hack/verify-generated.sh

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
	rm -rf .stamps .charts bin/ *.tar
