REGISTRY ?= ghcr.io/chuck-chuck-chuck-net
PROJECT ?= slaptain
NAMESPACE ?= slaptain
NAMESPACE_TESTING ?= slaptain-testing
CONTAINER_ENGINE ?= podman

INIT_IMAGE       = $(REGISTRY)/$(PROJECT)/slapd-init:latest
SLAPD_IMAGE      = $(REGISTRY)/$(PROJECT)/slapd:latest
TOOLKIT_IMAGE    = $(REGISTRY)/$(PROJECT)/slapd-toolkit:latest
OPERATOR_IMAGE   = $(REGISTRY)/$(PROJECT)/operator:latest
E2E_RUNNER_IMAGE = $(REGISTRY)/$(PROJECT)/e2e-runner:latest

.PHONY: all build-init build-slapd build-toolkit build-operator build-e2e-runner push push-e2e-runner gencert helm-install helm-deploy helm-uninstall cluster-helm-install cluster-helm-uninstall operator-helm-install operator-helm-uninstall test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run e2e-resilience e2e-external-replication e2e-in-cluster clean

all: build-init build-slapd build-toolkit build-operator

build-init:
	$(CONTAINER_ENGINE) build -t $(INIT_IMAGE) images/slapd-init/

build-slapd:
	$(CONTAINER_ENGINE) build -t $(SLAPD_IMAGE) images/slapd/

build-toolkit:
	$(CONTAINER_ENGINE) build -t $(TOOLKIT_IMAGE) images/slapd-toolkit/

build-operator:
	$(CONTAINER_ENGINE) build -f images/operator/Containerfile -t $(OPERATOR_IMAGE) .

build-e2e-runner:
	$(CONTAINER_ENGINE) build -f images/e2e-runner/Containerfile -t $(E2E_RUNNER_IMAGE) .

operator-generate:
	$(MAKE) -C operator generate

operator-manifests:
	$(MAKE) -C operator manifests
	$(MAKE) operator-sync-crd

operator-sync-crd:
	cp operator/config/crd/bases/*.yaml charts/operator/crds/

push: build-init build-slapd build-toolkit build-operator
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)
	$(CONTAINER_ENGINE) push $(TOOLKIT_IMAGE)
	$(CONTAINER_ENGINE) push $(OPERATOR_IMAGE)

push-e2e-runner: build-e2e-runner
	$(CONTAINER_ENGINE) push $(E2E_RUNNER_IMAGE)

gencert:
	kubectl get namespace $(NAMESPACE_TESTING) >/dev/null 2>&1 || kubectl create namespace $(NAMESPACE_TESTING)
	cd tests && ./gencert.sh -n $(NAMESPACE_TESTING) -t slapd -s slapd -H slapd-headless slapd-tls

helm-install:
	helm upgrade --install slapd ./charts/slapd \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		$(HELM_VALUES_SLAPD)

helm-deploy: push gencert helm-install ## Full pipeline: build images, generate certs, deploy

helm-uninstall:
	helm uninstall slapd --namespace $(NAMESPACE_TESTING)

ifeq ($(TOOLKIT_ONLY),true)
  HELM_SET_SLAPD_TESTING = --set bootstrap.enabled=false
endif

testing-helm-install:
	helm upgrade --install slapd-test ./charts/slapd-test \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		$(HELM_VALUES_SLAPD_TESTING) $(HELM_SET_SLAPD_TESTING)

testing-helm-uninstall:
	helm uninstall slapd-test --namespace $(NAMESPACE_TESTING)

cluster-helm-install:
	helm upgrade --install slapd ./charts/slapd-cluster \
		--namespace $(NAMESPACE_TESTING) --create-namespace \
		$(HELM_VALUES_SLAPD_CLUSTER)

cluster-helm-uninstall:
	helm uninstall slapd --namespace $(NAMESPACE_TESTING)

operator-helm-install:
	helm upgrade --install slaptain-operator ./charts/operator \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(REGISTRY)/$(PROJECT)/operator \
		$(HELM_VALUES)

operator-helm-uninstall:
	helm uninstall slaptain-operator --namespace $(NAMESPACE)

test:
	helm upgrade --install slapd-test ./charts/slapd-test \
		--namespace $(NAMESPACE_TESTING) --create-namespace

test-uninstall:
	helm uninstall slapd-test --namespace $(NAMESPACE_TESTING)

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

## e2e-in-cluster: build + push e2e runner image, deploy as a Job, stream logs, report result
e2e-in-cluster: push-e2e-runner
	kubectl delete job e2e-runner -n $(NAMESPACE_TESTING) --ignore-not-found
	sed 's|__E2E_RUNNER_IMAGE__|$(E2E_RUNNER_IMAGE)|g; s|__NAMESPACE_TESTING__|$(NAMESPACE_TESTING)|g' \
		tests/e2e-runner-rbac.yaml | kubectl apply -f -
	sed 's|__E2E_RUNNER_IMAGE__|$(E2E_RUNNER_IMAGE)|g; s|__NAMESPACE_TESTING__|$(NAMESPACE_TESTING)|g' \
		tests/e2e-runner-job.yaml | kubectl apply -f -
	@echo "Waiting for e2e-runner pod to start..."
	@until kubectl logs -n $(NAMESPACE_TESTING) -f job/e2e-runner 2>/dev/null; do sleep 2; done
	@kubectl wait job/e2e-runner -n $(NAMESPACE_TESTING) \
		--for=condition=complete --timeout=30s 2>/dev/null \
		|| (echo "FAIL: e2e-runner job did not complete successfully" && exit 1)

clean:
	rm -f *.tar
