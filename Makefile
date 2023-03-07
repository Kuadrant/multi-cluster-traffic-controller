
# Image URL to use all building/pushing image targets
TAG ?= latest

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.25.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

include ./hack/make/agent.make ./hack/make/controller.make ./hack/make/syncer.make ./hack/make/dependencies.make

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: clean
clean: clean-clusters ## Clean up temporary files.
	-rm -rf ./bin/*
	-rm -rf ./tmp

.PHONY: clean-clusters
clean-clusters: ## Clean up generated cluster config files.
	-rm -rf ./config/local-setup/clusters/mctc-*

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint against code.
	golangci-lint run ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: local-setup
local-setup: kind kustomize helm yq dev-tls ## Setup multi cluster traffic controller locally using kind.
	./hack/local-setup.sh

.PHONY: local-cleanup
local-cleanup: kind ## Cleanup resources created by local-setup
	./hack/local-cleanup.sh
	$(MAKE) clean

.PHONY: build
build: build-controller build-agent build-syncer## Build all binaries.

##@ Deployment
ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy-sample-applicationset
deploy-sample-applicationset:
	kubectl apply -f ./samples/argocd-applicationset/echo-applicationset.yaml

DEV_TLS_DIR = config/webhook-setup/control/tls
DEV_TLS_CRT ?= $(DEV_TLS_DIR)/tls.crt
DEV_TLS_KEY ?= $(DEV_TLS_DIR)/tls.key

.PHONY: dev-tls
dev-tls: $(DEV_TLS_CRT) ## Generate dev tls webhook cert if necessary.
$(DEV_TLS_CRT):
	openssl req -x509 -nodes -days 365 -newkey rsa:2048 -keyout $(DEV_TLS_KEY) -out $(DEV_TLS_CRT) -subj "/C=IE/O=Red Hat Ltd/OU=HCG/CN=webhook.172.32.0.2.nip.io" -addext "subjectAltName = DNS:webhook.172.32.0.2.nip.io"

.PHONY: clear-dev-tls
clear-dev-tls:
	-rm -f $(DEV_TLS_CRT)
	-rm -f $(DEV_TLS_KEY)

.PHONY: webhook-proxy
webhook-proxy: kustomize
	$(KUSTOMIZE) --load-restrictor LoadRestrictionsNone build config/webhook-setup/proxy | kubectl apply -f -
