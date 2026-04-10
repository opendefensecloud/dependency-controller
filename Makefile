
# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Set MAKEFLAGS to suppress entering/leaving directory messages
MAKEFLAGS += --no-print-directory

BUILD_PATH ?= $(shell pwd)
LOCALBIN ?= $(BUILD_PATH)/bin

OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)

GO ?= go
SHELLCHECK ?= shellcheck
DOCKER ?= docker
KIND ?= kind
KUBECTL ?= kubectl

KCP ?= $(LOCALBIN)/kcp
GINKGO ?= $(LOCALBIN)/ginkgo
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
ADDLICENSE ?= $(LOCALBIN)/addlicense
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
APIGEN ?= $(LOCALBIN)/apigen

KCP_VERSION ?= 0.30.0
GINKGO_VERSION ?= $(shell go list -json -m -u github.com/onsi/ginkgo/v2 | jq -r '.Version')
GOLANGCI_LINT_VERSION ?= v2.10.1
ADDLICENSE_VERSION ?= v1.1.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1

CONTROLLER_IMG ?= dependency-controller:latest

TIMESTAMP := $(shell date '+%Y%m%d%H%M%S')
DEV_TAG ?= dev.$(TIMESTAMP)
export DEV_TAG

##@ General

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: clean
clean: ## Remove build artifacts and tool binaries.
	rm -rf $(LOCALBIN)

##@ Development

.PHONY: generate
generate: controller-gen ## Generate deepcopy methods.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: controller-gen apigen ## Generate CRDs and convert to kcp APIResourceSchemas + APIExport.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=config/crds
	$(APIGEN) --input-dir=config/crds --output-dir=config/kcp

.PHONY: fmt
fmt: addlicense golangci-lint ## Add license headers and format code.
	find . -not -path '*/.*' -not -path '*/bin/*' -name '*.go' -exec $(ADDLICENSE) -c 'Open Defense and dependency-controller contributors' -l apache -s=only {} +
	$(GO) fmt ./...
	$(GOLANGCI_LINT) run --fix

.PHONY: mod
mod: ## Run go mod tidy, download, verify.
	@$(GO) mod tidy
	@$(GO) mod download
	@$(GO) mod verify

.PHONY: lint
lint: golangci-lint ## Run linters.
	$(GOLANGCI_LINT) run -v

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

##@ Build

.PHONY: build
build: generate ## Build the controller binary.
	$(GO) build -o $(LOCALBIN)/dependency-controller ./cmd/controller/

.PHONY: run
run: generate ## Run the controller from source.
	$(GO) run ./cmd/controller/

.PHONY: docker-build
docker-build: ## Build the Docker image.
	$(DOCKER) build -t $(CONTROLLER_IMG) .

.PHONY: docker-push
docker-push: ## Push the Docker image.
	$(DOCKER) push $(CONTROLLER_IMG)

##@ Testing

.PHONY: test
test: generate ginkgo ## Run all tests.
	$(GINKGO) -r -cover --fail-fast --require-suite -covermode count --output-dir=$(BUILD_PATH) -coverprofile=coverprofile $(testargs)

.PHONY: test-unit
test-unit: generate ## Run unit tests only (no e2e).
	$(GO) test ./internal/... ./api/... -cover $(testargs)

.PHONY: test-e2e
test-e2e: generate ginkgo kcp ## Run e2e tests against a local kcp instance.
	TEST_KCP_ASSETS=$(LOCALBIN) $(GINKGO) -r --fail-fast -v ./test/e2e/ $(testargs)

##@ Tool Dependencies

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen locally if necessary.
	@test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Download golangci-lint locally if necessary.
	@test -s $(LOCALBIN)/golangci-lint && $(LOCALBIN)/golangci-lint --version | grep -q $(GOLANGCI_LINT_VERSION) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: ginkgo
ginkgo: $(LOCALBIN) ## Download ginkgo locally if necessary.
	@test -s $(LOCALBIN)/ginkgo && $(LOCALBIN)/ginkgo version | grep -q $(subst v,,$(GINKGO_VERSION)) || \
	GOBIN=$(LOCALBIN) go install github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION)

.PHONY: addlicense
addlicense: $(LOCALBIN) ## Download addlicense locally if necessary.
	@test -s $(LOCALBIN)/addlicense && grep -q $(ADDLICENSE_VERSION) $(LOCALBIN)/.addlicense-version 2>/dev/null || \
	GOBIN=$(LOCALBIN) go install github.com/google/addlicense@$(ADDLICENSE_VERSION); \
	echo $(ADDLICENSE_VERSION) > $(LOCALBIN)/.addlicense-version

.PHONY: apigen
apigen: $(LOCALBIN) ## Download apigen locally if necessary.
	@test -s $(LOCALBIN)/apigen || \
	GOBIN=$(LOCALBIN) go install github.com/kcp-dev/sdk/cmd/apigen@v$(KCP_VERSION)

.PHONY: kcp
kcp: $(LOCALBIN) ## Download kcp binary locally if necessary.
	@test -s $(LOCALBIN)/kcp && $(LOCALBIN)/kcp --version 2>&1 | grep -q "$(KCP_VERSION)" || (\
	echo "Downloading kcp v$(KCP_VERSION) for $(OS)/$(ARCH)..."; \
	curl -fsSL -o $(LOCALBIN)/kcp.tar.gz "https://github.com/kcp-dev/kcp/releases/download/v$(KCP_VERSION)/kcp_$(KCP_VERSION)_$(OS)_$(ARCH).tar.gz"; \
	tar -xzf $(LOCALBIN)/kcp.tar.gz -C $(LOCALBIN) --strip-components=1 bin/; \
	rm -f $(LOCALBIN)/kcp.tar.gz; \
	chmod +x $(LOCALBIN)/kcp)
