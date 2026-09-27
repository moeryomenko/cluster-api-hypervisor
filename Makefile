NAME := cluster-api-hypervisor
BINARY := $(NAME)
# IMAGE is a literal tag (no make functions) so the image content contract
# test (test/image_contract.sh) can resolve it directly.
IMAGE ?= cluster-api-hypervisor:dev
COVER_FILE ?= coverage.out
RACE_DETECTOR ?= -race
COUNT ?= 1
TEST ?= $(shell go list ./...)
IMPORT_PATH := $(shell go list -m -f {{.Path}} | head -1)
ENVTEST_K8S_VERSION ?= 1.35.0

# Release tree knobs for the clusterctl provider packaging targets. OUT_DIR is
# the release root (the default out/ is gitignored); callers such as the
# contract test and the e2e scripts override it with a scratch directory.
# RELEASE_VERSION names the version directory under each provider folder and
# must stay in sync with the releaseSeries in metadata.yaml.
OUT_DIR ?= out
RELEASE_VERSION ?= v0.1.0
# RELEASE_IMAGE is deliberately local: release-gate verifies an image without
# registry credentials or a push.
RELEASE_IMAGE ?= localhost/$(NAME):release-gate
ROOT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

# ENVTEST_ASSETS resolves the envtest binary directory used by the test
# targets. A pre-set KUBEBUILDER_ASSETS wins; otherwise setup-envtest returns
# (downloading on first use) the binary directory for the pinned Kubernetes
# version. The version is passed positionally because setup-envtest ignores
# the KUBEBUILDER_ENVTEST_KUBERNETES_VERSION env var and would otherwise
# select the latest release (observed 1.36.2) instead of the 1.35.x series
# the envtest harness targets. Recursive expansion defers setup-envtest until
# a recipe actually needs the path.
ENVTEST_ASSETS = $(if $(KUBEBUILDER_ASSETS),$(KUBEBUILDER_ASSETS),$(shell go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path))

.PHONY: default
default: help

.PHONY: build
build: ## Build the manager binary
	@go build -o $(BINARY) .

.PHONY: test
test: ## Run all tests with race detector and coverage
	@KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go tool gotestsum --format-hide-empty-pkg -f testname -- $(RACE_DETECTOR) -count $(COUNT) $(TEST) -timeout=15m -coverprofile=$(COVER_FILE)
	@go tool cover -func=$(COVER_FILE) | grep ^total

.PHONY: cover
cover: test ## Open coverage report in browser
	@go tool cover -html=$(COVER_FILE)

.PHONY: lint
lint: ## Run linter
	@go tool golangci-lint run -v --fix

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

.PHONY: proto-fmt
proto-fmt: ## Format HostAgent protobuf source
	@go tool buf format -w api/agent/v1/agent.proto

.PHONY: proto
proto: ## Regenerate checked-in HostAgent protobuf and gRPC bindings
	@protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative api/agent/v1/agent.proto

.PHONY: proto-check
proto-check: ## Fail if regenerating HostAgent bindings would change their current content
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	cd "$(ROOT_DIR)"; \
	protoc --go_out="$$tmp" --go_opt=paths=source_relative --go-grpc_out="$$tmp" --go-grpc_opt=paths=source_relative api/agent/v1/agent.proto; \
	cmp -s "$$tmp/api/agent/v1/agent.pb.go" api/agent/v1/agent.pb.go; \
	cmp -s "$$tmp/api/agent/v1/agent_grpc.pb.go" api/agent/v1/agent_grpc.pb.go

.PHONY: generate
generate: ## Run controller-gen codegen (deepcopy, CRDs, RBAC, webhook manifests)
	@go tool controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	@go tool controller-gen crd:crdVersions=v1 rbac:roleName=manager-role webhook paths="./api/..." paths="./controllers/..." paths="./internal/webhook/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

.PHONY: generate-check
generate-check: ## Fail if generation would change generated files
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	for path in Makefile go.mod go.sum go.work go.work.sum tools api config controllers internal hack; do \
		cp -a "$(ROOT_DIR)/$$path" "$$tmp/$$path"; \
	done; \
	$(MAKE) -C "$$tmp" generate; \
	diff -ru "$(ROOT_DIR)/api" "$$tmp/api"; \
	diff -ru "$(ROOT_DIR)/config" "$$tmp/config"


.PHONY: components
components: ## Build the clusterctl provider release tree under OUT_DIR
	@set -e; \
	tmp=$$(mktemp); \
	trap 'rm -f "$$tmp"' EXIT; \
	go tool kustomize build config/release > "$$tmp"; \
	for provider in infrastructure bootstrap control-plane; do \
		dir="$(OUT_DIR)/$${provider}-hypervisor/$(RELEASE_VERSION)"; \
		mkdir -p "$$dir"; \
		cp "$$tmp" "$$dir/$${provider}-components.yaml"; \
		cp metadata.yaml "$$dir/metadata.yaml"; \
		cp templates/cluster-template.yaml "$$dir/cluster-template.yaml"; \
	done

.PHONY: components-check
components-check: ## Fail if a second make components changes the release tree
	@set -e; \
	test -d "$(OUT_DIR)" || { echo "components-check: $(OUT_DIR) missing; run make components first" >&2; exit 1; }; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(MAKE) components OUT_DIR="$$tmp"; \
	diff -r "$$tmp" "$(OUT_DIR)"

.PHONY: release-gate
release-gate: ## Run the immutable local release and image gate
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	go mod verify; \
	(cd "$(ROOT_DIR)/tools" && go mod verify && go mod download); \
	$(MAKE) provenance-check; \
	$(MAKE) prepare; \
	$(MAKE) proto-check; \
	$(MAKE) vet; \
	$(MAKE) test COVER_FILE="$$tmp/coverage.out"; \
	$(MAKE) generate-check; \
	$(MAKE) components OUT_DIR="$$tmp/components-first"; \
	$(MAKE) components OUT_DIR="$$tmp/components-second"; \
	diff -ru "$$tmp/components-first" "$$tmp/components-second"; \
	$(MAKE) image IMAGE="$(RELEASE_IMAGE)"; \
	IMAGE="$(RELEASE_IMAGE)" "$(ROOT_DIR)/test/image_contract.sh"

.PHONY: envtest
envtest: ## Run the envtest suite (controllers CRD contract + helpers) against the pinned k8s binaries
	@KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go tool gotestsum --format-hide-empty-pkg -f testname -- $(RACE_DETECTOR) -count $(COUNT) ./controllers/... ./test/helpers/... -timeout=15m

.PHONY: prepare
prepare: ## Materialize digest-locked artifacts and write a byte inventory
	@python3 hack/prepare_artifacts.py prepare

.PHONY: provenance-check
provenance-check: ## Verify immutable container and release provenance
	@python3 hack/check_provenance.py

.PHONY: prepare-check
prepare-check: provenance-check ## Verify every prepared artifact byte, checksum, and inventory entry
	@python3 hack/prepare_artifacts.py check

.PHONY: offline-recreate
offline-recreate: ## Recreate the locked artifact tree without network access
	@python3 hack/prepare_artifacts.py offline-recreate

.PHONY: image
image: prepare-check ## Build the provider container image with podman
	@podman build -t $(IMAGE) -f Containerfile .

.PHONY: install-quadlet
install-quadlet: ## Install the provider quadlet into the user systemd directory
	@install -D -m 0644 deploy/cluster-api-hypervisor.container $(HOME)/.config/containers/systemd/cluster-api-hypervisor.container

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
