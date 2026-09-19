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
#             CI-built image. slaptain publishes by hand and has no such
#             constraint — it adopts the spelling so the two projects read the
#             same, and so the defaults already fit if slaptain gains the same
#             workflows. It also makes a registry listing self-describing:
#             `0.2.0` is a release, `sha-09ecf10` is a build of a commit.
IMAGE_TAG := $(if $(IS_VERSION_TAG),$(GIT_TAG:v%=%),sha-$(GIT_TAG))

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

# OpenLDAP 2.7.1 .deb build. Local-only image (localhost/ prefix — never pushed):
# it carries nothing but /debs and is consumed by both the slapd and slapd-init
# 2.7 Containerfiles, so the package build runs once per tag for both.
OPENLDAP_DEB_IMAGE = localhost/$(PROJECT)/openldap-deb:$(IMAGE_TAG)
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
INIT_SRCS        := images/slapd-init/Containerfile images/slapd-init/bootstrap.sh
INIT_OL26_SRCS   := images/slapd-init/Containerfile.ol26 images/slapd-init/bootstrap.sh
SLAPD_SRCS       := images/slapd/Containerfile
SLAPD_OL26_SRCS  := images/slapd/Containerfile.ol26
TOOLKIT_SRCS := $(shell find images/slapd-toolkit -type f)
OPERATOR_SRCS := $(shell find images/operator -type f) $(shell find operator -type f -name '*.go') operator/go.mod operator/go.sum

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

.PHONY: all build-openldap-deb build-init build-slapd build-init-ol26 build-slapd-ol26 build-ol26 build-toolkit build-operator build-slctl install-slctl push gencert helm-deploy cluster-helm-install cluster-helm-uninstall operator-crd-apply operator-helm-install operator-helm-uninstall operator-chart-package operator-chart-push charts-package charts-push test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run e2e-resilience e2e-external-replication e2e-multisite e2e-multisite-setup e2e-multisite-test e2e-multisite-teardown e2e-migration e2e-migration-setup e2e-migration-test e2e-migration-teardown import import-init import-slapd import-toolkit import-operator import-init-ol26 import-slapd-ol26 import-ol26 push-operator deliver deliver-operator deploy-operator clean show-tag

## all: the six pushable images plus slctl. build-ol26 is included so a
## release build carries the legacy OpenLDAP 2.6 pair too (ADR-021).
all: build-init build-slapd build-ol26 build-toolkit build-operator build-slctl

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

$(STAMPS)/toolkit: $(TOOLKIT_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -t $(TOOLKIT_IMAGE) images/slapd-toolkit/
	@touch $@

$(STAMPS)/operator: $(OPERATOR_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/operator/Containerfile -t $(OPERATOR_IMAGE) .
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

build-slctl:
	cd operator && go build -o ../bin/slctl ./cmd/slctl/

install-slctl: build-slctl
	sudo install -m 0755 bin/slctl /usr/local/bin/slctl

## Push to container registry. Six images: the 2.7 slapd pair (plain tags), the
## 2.6 pair (-ol26), toolkit and operator. The openldap-deb build artifact
## carrier is local-only and deliberately not pushed.
push: build-init build-slapd build-ol26 build-toolkit build-operator
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)
	$(CONTAINER_ENGINE) push $(INIT_IMAGE_OL26)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE_OL26)
	$(CONTAINER_ENGINE) push $(TOOLKIT_IMAGE)
	$(CONTAINER_ENGINE) push $(OPERATOR_IMAGE)

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

import: import-init import-slapd import-toolkit import-operator

## Legacy 2.6 pair — separate aggregate so the normal loop doesn't pay for it.
## Use it to run an e2e cycle against -ol26 images (ADR-021).
import-init-ol26: build-init-ol26
	$(call import-if-needed,$(INIT_IMAGE_OL26))

import-slapd-ol26: build-slapd-ol26
	$(call import-if-needed,$(SLAPD_IMAGE_OL26))

import-ol26: import-init-ol26 import-slapd-ol26

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

## operator-helm-install: deploy/upgrade the operator. Depends on
## operator-crd-apply so CRDs are always current — `helm upgrade` won't do it
## (see operator-crd-apply), which otherwise leaves a new/changed CRD missing or
## stale after an upgrade. This is the t3e iteration-loop command:
##   make operator-helm-install CONTEXT=t3e GIT_TAG=<pushed-tag>
operator-helm-install: operator-crd-apply
	$(HELM) upgrade --install slaptain ./charts/operator \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(REGISTRY)/$(PROJECT)/operator \
		--set image.tag=$(IMAGE_TAG)

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

show-tag: ## Print the git tag and the image/chart tag derived from it
	@echo "git:    $(GIT_TAG)"
	@echo "image:  $(IMAGE_TAG)"
	@echo "chart:  $(CHART_VERSION)"

clean:
	rm -rf .stamps .charts bin/ *.tar
