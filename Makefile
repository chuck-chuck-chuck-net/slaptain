REGISTRY ?= registry.internal
PROJECT ?= slaptain
NAMESPACE ?= slaptain
CONTAINER_ENGINE ?= podman

INIT_IMAGE = $(REGISTRY)/$(PROJECT)/slapd-init:latest
SLAPD_IMAGE = $(REGISTRY)/$(PROJECT)/slapd:latest

.PHONY: all build-init build-slapd push deploy gencert helm-install helm-uninstall test test-uninstall clean

all: build-init build-slapd

build-init:
	$(CONTAINER_ENGINE) build -t $(INIT_IMAGE) images/slapd-init/

build-slapd:
	$(CONTAINER_ENGINE) build -t $(SLAPD_IMAGE) images/slapd/

push: build-init build-slapd
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)

deploy:
	sed -e "s|registry.internal/slaptain/slapd-init:latest|$(INIT_IMAGE)|g" \
	    -e "s|registry.internal/slaptain/slapd:latest|$(SLAPD_IMAGE)|g" \
	    test-ldap.yaml | kubectl apply -n $(NAMESPACE) -f -

gencert:
	cd tests && ./gencert.sh -n $(NAMESPACE) -t slapd -s slapd slapd-tls

helm-install: push
	# Ensure cert exists if TLS is enabled (default)
	$(MAKE) gencert
	helm upgrade --install slapd ./charts/slapd \
		--namespace $(NAMESPACE) --create-namespace \
		--set global.registry=$(REGISTRY) \
		--set global.project=$(PROJECT) \
		$(HELM_VALUES)

helm-uninstall:
	helm uninstall slapd --namespace $(NAMESPACE)

test:
	helm upgrade --install slapd-test ./charts/slapd-test \
		--namespace $(NAMESPACE) --create-namespace

test-uninstall:
	helm uninstall slapd-test --namespace $(NAMESPACE)

clean:
	rm -f *.tar
