NAME := cluster-api-hypervisor
BINARY := $(NAME)
COVER_FILE ?= coverage.out
RACE_DETECTOR ?= -race
COUNT ?= 1
TEST ?= $(shell go list ./...)
IMPORT_PATH := $(shell go list -m -f {{.Path}} | head -1)
ENVTEST_K8S_VERSION ?= 1.35.0

.PHONY: default
default: help

.PHONY: build
build: ## Build the manager binary
	@go build -o $(BINARY) .

.PHONY: test
test: ## Run all tests with race detector and coverage
	@go tool gotestsum --format-hide-empty-pkg -f testname -- $(RACE_DETECTOR) -count $(COUNT) $(TEST) -timeout=3m -coverprofile=$(COVER_FILE)
	@go tool cover -func=$(COVER_FILE) | grep ^total

.PHONY: cover
cover: test ## Open coverage report in browser
	@go tool cover -html=$(COVER_FILE)

.PHONY: lint
lint: ## Run linter
	@go tool golangci-lint run -v

.PHONY: fmt
fmt: ## Format Go source files
	@gofmt -s -w .
	@git ls-files -m -o --exclude-standard -- '*.go' | xargs -r -I{} go tool golines --base-formatter=gofumpt --ignore-generated --tab-len=1 --max-len=120 -w {}
	@git ls-files -m -o --exclude-standard -- '*.go' | xargs -r -I{} go tool goimports -local $(IMPORT_PATH) -w {}

.PHONY: vet
vet: ## Run go vet
	@go vet ./...

.PHONY: tidy
tidy: ## Tidy go module dependencies
	@go mod tidy -v
	@go work sync

.PHONY: generate
generate: ## Run controller-gen codegen (no-op until api/ exists)
	@if [ ! -d api ]; then \
		echo "api/ does not exist yet; skipping code generation"; \
	else \
		go tool controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."; \
		go tool controller-gen rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases; \
	fi

.PHONY: generate-check
generate-check: ## Fail if a second generate changes tracked files
	@$(MAKE) generate
	@$(MAKE) generate
	@git diff --no-ext-diff --exit-code

.PHONY: envtest
envtest: ## Download envtest binaries for the target Kubernetes version
	@KUBEBUILDER_ENVTEST_KUBERNETES_VERSION=$(ENVTEST_K8S_VERSION) go tool setup-envtest use -p path

.PHONY: image
image: ## Build container image (stub; implemented in a later phase)
	@echo "image build not implemented yet"

.PHONY: clean
clean: ## Remove build artifacts and coverage output
	@rm -f $(BINARY) $(COVER_FILE)

.PHONY: check
check: lint vet test ## Run lint, vet, and test (CI gate)

.PHONY: help
help: ## Print this help message
	@echo "Usage: make <target>"
	@echo ""
	@echo "Targets:"
	@grep -F -h '##' $(MAKEFILE_LIST) \
		| grep -F -v fgrep \
		| sort \
		| grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
