# cluster-api-hypervisor

Cluster API provider set for k8labs: infrastructure, bootstrap, and control
plane providers that manage cloud-hypervisor VMs on a Linux host.

- Module: `github.com/moeryomenko/cluster-api-hypervisor`
- Layout: kubebuilder v3 conventions (PROJECT, `api/`, `controllers/`,
  `internal/`, `main.go`, `config/`)
- Tools: Go `tool` directives in `tools/go.mod` (controller-gen, golangci-lint,
  gotestsum, golines, gofumpt, goimports, setup-envtest)

## Make targets

| Target | Purpose |
| ------ | ------- |
| `make build` | Build the `cluster-api-hypervisor` manager binary |
| `make test` | Run all tests (gotestsum, race, coverage) |
| `make lint` | Run golangci-lint |
| `make fmt` | Format Go sources (gofmt, golines/gofumpt, goimports) |
| `make vet` | Run `go vet ./...` |
| `make tidy` | Tidy module dependencies and workspace |
| `make generate` | Run controller-gen codegen (CRDs, RBAC, webhooks, deepcopy) |
| `make generate-check` | Gate on generate idempotency |
| `make check` | lint + vet + test (CI gate) |
| `make envtest` | Download envtest binaries for the target Kubernetes version |
| `make image` | Build container image (placeholder, later phase) |
| `make clean` | Remove build artifacts and coverage output |
| `make help` | Print this help |
