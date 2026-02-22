REGISTRY ?= registry.internal
PROJECT ?= slaptain
NAMESPACE ?= slaptain
CONTAINER_ENGINE ?= podman
APKO ?= apko

INIT_IMAGE = $(REGISTRY)/$(PROJECT)/slapd-init:latest
SLAPD_IMAGE = $(REGISTRY)/$(PROJECT)/slapd:latest

.PHONY: all build-init build-slapd clean search-perl push deploy helm-install helm-uninstall

all: build-init build-slapd

build-init:
	$(CONTAINER_ENGINE) build -t slapd-init:latest -f images/Containerfile.init images/
	$(CONTAINER_ENGINE) tag slapd-init:latest $(INIT_IMAGE)

build-slapd:
	$(APKO) build images/slapd.yaml slapd:latest slapd.tar --arch x86_64
	$(CONTAINER_ENGINE) load -i slapd.tar
	$(CONTAINER_ENGINE) tag slapd:latest-amd64 $(SLAPD_IMAGE)

push: build-init build-slapd
	$(CONTAINER_ENGINE) push $(INIT_IMAGE)
	$(CONTAINER_ENGINE) push $(SLAPD_IMAGE)

deploy:
	sed -e "s|registry.internal/slaptain/slapd-init:latest|$(INIT_IMAGE)|g" \
	    -e "s|registry.internal/slaptain/slapd:latest|$(SLAPD_IMAGE)|g" \
	    test-ldap.yaml | kubectl apply -n $(NAMESPACE) -f -

helm-install: push
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
