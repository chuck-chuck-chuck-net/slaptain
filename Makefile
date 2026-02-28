REGISTRY ?= registry.internal
PROJECT ?= slaptain
NAMESPACE ?= slaptain
NAMESPACE_TESTING ?= slaptain-testing
CONTAINER_ENGINE ?= podman

INIT_IMAGE    = $(REGISTRY)/$(PROJECT)/slapd-init:latest
SLAPD_IMAGE   = $(REGISTRY)/$(PROJECT)/slapd:latest
TOOLKIT_IMAGE = $(REGISTRY)/$(PROJECT)/slapd-toolkit:latest
OPERATOR_IMAGE = $(REGISTRY)/$(PROJECT)/operator:latest

.PHONY: all build-init build-slapd build-toolkit build-operator push gencert helm-install helm-deploy helm-uninstall cluster-helm-install cluster-helm-uninstall operator-helm-install operator-helm-uninstall test test-uninstall operator-generate operator-manifests operator-sync-crd e2e e2e-run clean

all: build-init build-slapd build-toolkit build-operator

build-init:
	$(CONTAINER_ENGINE) build -t $(INIT_IMAGE) images/slapd-init/

build-slapd:
	$(CONTAINER_ENGINE) build -t $(SLAPD_IMAGE) images/slapd/

build-toolkit:
	$(CONTAINER_ENGINE) build -t $(TOOLKIT_IMAGE) images/slapd-toolkit/

build-operator:
	$(CONTAINER_ENGINE) build -f images/operator/Containerfile -t $(OPERATOR_IMAGE) .

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

gencert:
	kubectl get namespace $(NAMESPACE_TESTING) >/dev/null 2>&1 || kubectl create namespace $(NAMESPACE_TESTING)
	cd tests && ./gencert.sh -n $(NAMESPACE_TESTING) -t slapd -s slapd slapd-tls

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

clean:
	rm -f *.tar
