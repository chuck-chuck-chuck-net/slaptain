REGISTRY ?= ghcr.io/chuck-chuck-chuck-net
PROJECT ?= slaptain
NAMESPACE ?= slaptain
NAMESPACE_TESTING ?= slaptain-testing
CONTAINER_ENGINE ?= podman

# Image tag: exact git tag if on one, otherwise short commit hash.
# Appends -dirty when the working tree has uncommitted changes, so a rebuild
# after local edits produces a distinct tag that won't match what's already
# on the nodes — triggering a re-import.
GIT_TAG := $(shell if [ -n "$$(git describe --tags --exact-match 2>/dev/null)" ]; then \
                   git describe --tags --exact-match; \
               else \
                   hash=$$(git rev-parse --short HEAD); \
                   if ! git diff --quiet HEAD 2>/dev/null; then \
                       echo "$${hash}-dirty"; \
                   else \
                       echo "$$hash"; \
                   fi; \
               fi)

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

INIT_IMAGE       = $(REGISTRY)/$(PROJECT)/slapd-init:$(GIT_TAG)
SLAPD_IMAGE      = $(REGISTRY)/$(PROJECT)/slapd:$(GIT_TAG)
TOOLKIT_IMAGE    = $(REGISTRY)/$(PROJECT)/slapd-toolkit:$(GIT_TAG)
OPERATOR_IMAGE   = $(REGISTRY)/$(PROJECT)/operator:$(GIT_TAG)
E2E_RUNNER_IMAGE = $(REGISTRY)/$(PROJECT)/e2e-runner:$(GIT_TAG)

# Helm chart OCI registry. Charts land under <registry>/<project>/charts/<name>.
# Pull example: helm pull oci://ghcr.io/chuck-chuck-chuck-net/slaptain/charts/slaptain-operator --version X.Y.Z
CHART_REGISTRY ?= oci://$(REGISTRY)/$(PROJECT)/charts
CHART_OUT      := .charts

# Stamp-file directory for incremental builds.
STAMPS := .stamps

# Source file dependencies per image
INIT_SRCS    := $(shell find images/slapd-init -type f)
SLAPD_SRCS   := $(shell find images/slapd -type f)
TOOLKIT_SRCS := $(shell find images/slapd-toolkit -type f)
OPERATOR_SRCS := $(shell find images/operator -type f) $(shell find operator -type f -name '*.go') operator/go.mod operator/go.sum
E2E_SRCS     := $(shell find images/e2e-runner -type f) $(shell find tests/e2e -type f)

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

.PHONY: all build-init build-slapd build-toolkit build-operator build-e2e-runner build-slctl install-slctl push push-e2e-runner gencert helm-install helm-deploy helm-uninstall cluster-helm-install cluster-helm-uninstall operator-helm-install operator-helm-uninstall operator-chart-package operator-chart-push test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run e2e-resilience e2e-external-replication e2e-in-cluster e2e-multisite e2e-multisite-setup e2e-multisite-test e2e-multisite-teardown import import-init import-slapd import-toolkit import-operator import-e2e-runner deliver deliver-operator deliver-e2e-runner deploy-operator clean show-tag

all: build-init build-slapd build-toolkit build-operator build-e2e-runner build-slctl

## Stamp-file backed build targets (incremental)

$(STAMPS):
	mkdir -p $(STAMPS)

# Track the current GIT_TAG so builds re-trigger when the tag changes
# (e.g. new commit, dirty→clean transition). The file is only touched
# when its content actually changes.
$(STAMPS)/tag: FORCE | $(STAMPS)
	@if [ "$$(cat $@ 2>/dev/null)" != "$(GIT_TAG)" ]; then \
		echo "$(GIT_TAG)" > $@; \
	fi
.PHONY: FORCE

$(STAMPS)/init: $(INIT_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -t $(INIT_IMAGE) images/slapd-init/
	@touch $@

$(STAMPS)/slapd: $(SLAPD_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -t $(SLAPD_IMAGE) images/slapd/
	@touch $@

$(STAMPS)/toolkit: $(TOOLKIT_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -t $(TOOLKIT_IMAGE) images/slapd-toolkit/
	@touch $@

$(STAMPS)/operator: $(OPERATOR_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/operator/Containerfile -t $(OPERATOR_IMAGE) .
	@touch $@

$(STAMPS)/e2e: $(E2E_SRCS) $(STAMPS)/tag | $(STAMPS)
	$(CONTAINER_ENGINE) build -f images/e2e-runner/Containerfile -t $(E2E_RUNNER_IMAGE) .
	@touch $@

## Phony aliases so `make build-operator` etc. still work
build-init: $(STAMPS)/init
build-slapd: $(STAMPS)/slapd
build-toolkit: $(STAMPS)/toolkit
build-operator: $(STAMPS)/operator
build-e2e-runner: $(STAMPS)/e2e

build-slctl:
	cd operator && go build -o ../bin/slctl ./cmd/slctl/

install-slctl: build-slctl
	sudo install -m 0755 bin/slctl /usr/local/bin/slctl

## Push to container registry
push: build-init build-slapd build-toolkit build-operator build-e2e-runner
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)
	$(CONTAINER_ENGINE) push $(TOOLKIT_IMAGE)
	$(CONTAINER_ENGINE) push $(OPERATOR_IMAGE)
	$(CONTAINER_ENGINE) push $(E2E_RUNNER_IMAGE)

push-e2e-runner: build-e2e-runner
	$(CONTAINER_ENGINE) push $(E2E_RUNNER_IMAGE)

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

import-e2e-runner: build-e2e-runner
	$(call import-if-needed,$(E2E_RUNNER_IMAGE))

import: import-init import-slapd import-toolkit import-operator import-e2e-runner

## Delivery: dispatch to push or import based on DELIVERY variable
deliver: $(DELIVERY)
deliver-operator: $(DELIVERY)-operator
deliver-e2e-runner: $(DELIVERY)-e2e-runner

## Operator dev fast-path: build + deliver + helm upgrade + restart
deploy-operator: deliver-operator operator-helm-install
	$(KUBECTL) rollout restart deployment/slaptain-operator -n $(NAMESPACE)

operator-generate:
	$(MAKE) -C operator generate

operator-manifests:
	$(MAKE) -C operator manifests
	$(MAKE) operator-sync-crd

operator-sync-crd:
	cp operator/config/crd/bases/*.yaml charts/operator/crds/

gencert:
	$(KUBECTL) get namespace $(NAMESPACE_TESTING) >/dev/null 2>&1 || $(KUBECTL) create namespace $(NAMESPACE_TESTING)
	cd tests && ./gencert.sh $(if $(CONTEXT),-c $(CONTEXT)) -n $(NAMESPACE_TESTING) -t slapd -s slapd -H slapd-headless slapd-tls

helm-install:
	$(HELM) upgrade --install slapd ./charts/slapd \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		$(HELM_VALUES_SLAPD)

helm-deploy: deliver gencert helm-install ## Full pipeline: build images, deliver, generate certs, deploy

helm-uninstall:
	$(HELM) uninstall slapd --namespace $(NAMESPACE_TESTING)

## Test resources: SlapdSchema + SlapdDatabase + readpw Secret.
## Set TEST_RESOURCES to "example" or "lab" (default: lab).
TEST_RESOURCES ?= lab

testing-apply:
	$(KUBECTL) apply -n $(NAMESPACE_TESTING) -f tests/resources/$(TEST_RESOURCES)/

testing-delete:
	$(KUBECTL) delete -n $(NAMESPACE_TESTING) -f tests/resources/$(TEST_RESOURCES)/ --ignore-not-found

## Legacy aliases (deprecated — use testing-apply / testing-delete).
testing-helm-install: testing-apply
testing-helm-uninstall: testing-delete

cluster-helm-install:
	$(HELM) upgrade --install slapd ./charts/slapd-cluster \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		--set images.slapd.tag=$(GIT_TAG) \
		--set images.init.tag=$(GIT_TAG) \
		$(HELM_VALUES_SLAPD_CLUSTER)

cluster-helm-uninstall:
	$(HELM) uninstall slapd --namespace $(NAMESPACE_TESTING)

operator-helm-install:
	$(HELM) upgrade --install slaptain-operator ./charts/operator \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(REGISTRY)/$(PROJECT)/operator \
		--set image.tag=$(GIT_TAG) \
		$(HELM_VALUES)

operator-helm-uninstall:
	$(HELM) uninstall slaptain-operator --namespace $(NAMESPACE)

## operator-chart-package: package the operator Helm chart into $(CHART_OUT)/
## CRDs are synced first so the packaged chart includes the current CRD.
operator-chart-package: operator-sync-crd
	@mkdir -p $(CHART_OUT)
	$(HELM) package ./charts/operator -d $(CHART_OUT)

## operator-chart-push: push the packaged operator chart to $(CHART_REGISTRY).
## Requires `helm registry login` against $(REGISTRY) first.
operator-chart-push: operator-chart-package
	@chart_version=$$($(HELM) show chart ./charts/operator | awk '/^version:/ {print $$2}'); \
	tgz=$(CHART_OUT)/slaptain-operator-$$chart_version.tgz; \
	echo "Pushing $$tgz to $(CHART_REGISTRY)"; \
	$(HELM) push $$tgz $(CHART_REGISTRY)

## toolkit-install: deploy the slapd-toolkit debug pod (ldap-utils, python3, ldap3).
toolkit-install:
	$(HELM) upgrade --install toolkit ./charts/slapd-toolkit \
		--namespace $(NAMESPACE_TESTING) --create-namespace

toolkit-uninstall:
	$(HELM) uninstall toolkit --namespace $(NAMESPACE_TESTING)

## e2e: full setup + test run + teardown
e2e: e2e-run

## e2e-run: run tests against an already-installed cluster (skips helm setup/teardown)
e2e-run:
	cd tests/e2e && go test -v ./... --ginkgo.v

## e2e-resilience: run all tests including slow pod-restart and warm-start tests
e2e-resilience:
	cd tests/e2e && E2E_RESILIENCE=1 go test -v ./... --ginkgo.v --ginkgo.timeout=30m

## e2e-external-replication: run cross-cluster external replication tests
e2e-external-replication:
	cd tests/e2e && E2E_EXTERNAL_REPL=1 go test -v ./... --ginkgo.v --ginkgo.timeout=10m --ginkgo.label-filter=external-replication

## e2e-in-cluster: build + deliver e2e runner image, deploy as a Job, stream logs, report result
e2e-in-cluster: deliver-e2e-runner
	$(KUBECTL) delete job e2e-runner -n $(NAMESPACE_TESTING) --ignore-not-found
	sed 's|__E2E_RUNNER_IMAGE__|$(E2E_RUNNER_IMAGE)|g; s|__NAMESPACE_TESTING__|$(NAMESPACE_TESTING)|g' \
		tests/e2e-runner-rbac.yaml | $(KUBECTL) apply -f -
	sed 's|__E2E_RUNNER_IMAGE__|$(E2E_RUNNER_IMAGE)|g; s|__NAMESPACE_TESTING__|$(NAMESPACE_TESTING)|g' \
		tests/e2e-runner-job.yaml | $(KUBECTL) apply -f -
	@echo "Waiting for e2e-runner pod to start..."
	@until $(KUBECTL) logs -n $(NAMESPACE_TESTING) -f job/e2e-runner 2>/dev/null; do sleep 2; done
	@$(KUBECTL) wait job/e2e-runner -n $(NAMESPACE_TESTING) \
		--for=condition=complete --timeout=30s 2>/dev/null \
		|| (echo "FAIL: e2e-runner job did not complete successfully" && exit 1)

## e2e-multisite: full multi-site setup + test + teardown (pass CONTEXTS="s1 s2 s3")
e2e-multisite:
	./tests/e2e-multisite.sh all $(CONTEXTS)

## e2e-multisite-setup: deploy multi-site infrastructure
e2e-multisite-setup:
	./tests/e2e-multisite.sh setup $(CONTEXTS)

## e2e-multisite-test: run tests against existing multi-site deployment
e2e-multisite-test:
	./tests/e2e-multisite.sh test $(CONTEXTS)

## e2e-multisite-teardown: remove multi-site infrastructure
e2e-multisite-teardown:
	./tests/e2e-multisite.sh teardown $(CONTEXTS)

show-tag: ## Print the current GIT_TAG used for image tagging
	@echo $(GIT_TAG)

clean:
	rm -rf .stamps .charts bin/ *.tar
