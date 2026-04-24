# Include ODC common make targets
DEV_KIT_VERSION := v1.0.2
-include common.mk
common.mk:
	curl --fail -sSL https://raw.githubusercontent.com/opendefensecloud/dev-kit/$(DEV_KIT_VERSION)/common.mk -o common.mk.download && \
	mv common.mk.download $@

APIGEN ?= $(LOCAlGOBIN)/apigen

KCP ?= $(LOCALBIN)/kcp
KCP_VERSION ?= 0.30.0

IMG_REGISTRY ?= ghcr.io/opendefense
IMG_TAG ?= latest
CONTROLLER_IMG ?= $(IMG_REGISTRY)/dependency-controller:$(IMG_TAG)
WEBHOOK_IMG ?= $(IMG_REGISTRY)/dependency-controller:$(IMG_TAG)

TIMESTAMP := $(shell date '+%Y%m%d%H%M%S')
DEV_TAG ?= dev.$(TIMESTAMP)
export DEV_TAG

##@ Development

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy methods.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: $(CONTROLLER_GEN) $(APIGEN) ## Generate CRDs and convert to kcp APIResourceSchemas + APIExport.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=config/crds
	$(APIGEN) --input-dir=config/crds --output-dir=config/kcp
	cp config/crds/* charts/dependency-controller/files/
	cp config/kcp/apiresourceschema-*.yaml charts/dependency-controller/files/
	cp config/kcp/apiexport-*.yaml charts/dependency-controller/files/

.PHONY: fmt
fmt: $(ADDLICENSE) $(GOLANGCI_LINT) ## Add license headers and format code.
	git ls-files | grep '.*\.go$$' | xargs $(ADDLICENSE) -c 'BWI GmbH and Dependency Controller contributors' -l apache -s=only
	$(GO) fmt ./...
	$(GOLANGCI_LINT) run --fix

.PHONY: lint
lint: lint-no-golangci golangci-lint ## Run all linters.

.PHONY: lint-no-golangci
lint-no-golangci: $(ADDLICENSE) ## Run linters except golangci-lint (license headers + shellcheck).
	git ls-files | grep '.*\.go$$' | xargs $(ADDLICENSE) -c 'BWI GmbH and Dependency Controller contributors' -l apache -s=only -check

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

##@ Build

.PHONY: build
build: generate ## Build the controller and webhook binaries.
	$(GO) build -o $(LOCALBIN)/dependency-controller ./cmd/controller/
	$(GO) build -o $(LOCALBIN)/dependency-webhook ./cmd/webhook/

.PHONY: run
run-controller: generate ## Run the controller from source.
	$(GO) run ./cmd/controller/

.PHONY: run-webhook
run-webhook: generate ## Run the webhook server from source.
	$(GO) run ./cmd/webhook/

.PHONY: docker-build
docker-build: ## Build the Docker image.
	$(DOCKER) build --platform linux/amd64,linux/arm64 -t $(CONTROLLER_IMG) .

.PHONY: docker-push
docker-push: ## Push the Docker image.
	$(DOCKER) push $(CONTROLLER_IMG)

.PHONY: helm-package
helm-package: manifests ## Package Helm chart.
	helm package charts/dependency-controller

##@ Testing

.PHONY: test
test: $(GINKGO) $(KCP) generate ## Run all tests (excludes e2e).
	TEST_KCP_ASSETS=$(LOCALBIN) $(GINKGO) -r -cover --fail-fast --require-suite -covermode count --output-dir=$(BUILD_PATH) -coverprofile=coverprofile --skip-package=test/e2e $(testargs)

.PHONY: test-e2e
test-e2e: $(GINKGO) ## Run e2e tests (kind + kcp + helm).
	$(GINKGO) -r --fail-fast -v --timeout 30m ./test/e2e/ $(testargs)

.PHONY: e2e-cleanup
clean-e2e: ## Remove kind cluster from e2e tests.
	-$(KIND) delete cluster --name dep-ctrl-e2e 2>/dev/null

##@ Tool Dependencies

.PHONY: $(KCP)
$(KCP): $(LOCALBIN) ## Download kcp binary locally if necessary.
	@test -s $(LOCALBIN)/kcp && $(LOCALBIN)/kcp --version 2>&1 | grep -q "$(KCP_VERSION)" || (\
	echo "Downloading kcp v$(KCP_VERSION) for $(OS)/$(ARCH)..."; \
	curl -fsSL -o $(LOCALBIN)/kcp.tar.gz "https://github.com/kcp-dev/kcp/releases/download/v$(KCP_VERSION)/kcp_$(KCP_VERSION)_$(OS)_$(ARCH).tar.gz"; \
	tar -xzf $(LOCALBIN)/kcp.tar.gz -C $(LOCALBIN) --strip-components=1 bin/; \
	rm -f $(LOCALBIN)/kcp.tar.gz; \
	chmod +x $(LOCALBIN)/kcp)
