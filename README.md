# cluster-api-hypervisor

Cluster API provider set for k8labs: infrastructure, bootstrap, and control
plane providers that manage cloud-hypervisor VMs on a Linux host.

- Module: `github.com/moeryomenko/cluster-api-hypervisor`
- Layout: kubebuilder v3 conventions (PROJECT, `api/`, `controllers/`,
  `internal/`, `main.go`, `config/`)
- Tools: Go `tool` directives in `tools/go.mod` (clusterctl, controller-gen,
  golangci-lint, gotestsum, golines, gofumpt, goimports, kustomize,
  setup-envtest)

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
| `make components` | Build the clusterctl provider release tree under OUT_DIR |
| `make components-check` | Fail if a second make components changes the release tree |
| `make check` | lint + vet + test (CI gate) |
| `make envtest` | Download envtest binaries for the target Kubernetes version |
| `make image` | Build container image (placeholder, later phase) |
| `make clean` | Remove build artifacts and coverage output |
| `make help` | Print this help |

## clusterctl

The provider is clusterctl-installable through its static-resources packaging:
`make components` builds the provider release tree under `out/` and the
committed `clusterctl.yaml` registers the three hypervisor providers
(infrastructure, bootstrap, control-plane) as local repositories. clusterctl
delivers the CRDs, RBAC, and webhook configurations to the management cluster;
the manager still runs as the host quadlet described in
[docs/install-contract.md](docs/install-contract.md). See
[docs/clusterctl.md](docs/clusterctl.md) for the full runbook.

## Version Pins

Pinned component versions (Go modules, envtest k8s binaries, and container
image tools) are documented in [VERSIONS.md](VERSIONS.md) — keep them in sync
when bumping.
