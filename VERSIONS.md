# Version Pins

Pinned component versions for this repository. Every value below is read from
the source of truth listed in the table — keep them in sync when bumping.

| Component | Version | Source of truth |
|---|---|---|
| Go toolchain | 1.26.0 | `go.mod:3` / `tools/go.mod:3` (`go` directive) |
| sigs.k8s.io/cluster-api (CAPI) | v1.13.5 | `go.mod:14` |
| sigs.k8s.io/controller-runtime | v0.23.3 | `go.mod:15` |
| k8s.io/api | v0.35.4 | `go.mod:10` |
| k8s.io/apimachinery | v0.35.4 | `go.mod:12` |
| k8s.io/client-go | v0.35.4 | `go.mod:13` |
| controller-gen (sigs.k8s.io/controller-tools) | v0.20.1 | `tools/go.mod:271` (tool directive at `tools/go.mod:12`) |
| clusterctl (sigs.k8s.io/cluster-api/cmd/clusterctl) | v1.13.5 | `tools/go.mod:295` (tool directive at `tools/go.mod:11`) |
| kustomize (sigs.k8s.io/kustomize/kustomize/v5) | v5.8.1 | `tools/go.mod:302` (tool directive at `tools/go.mod:14`) |
| envtest k8s binaries | 1.35.0 | `Makefile:11` (`ENVTEST_K8S_VERSION`) |
| cloud-hypervisor | 48.0-r0 | `Containerfile:40` (`CLOUD_HYPERVISOR_VERSION`) |
| qemu-img | 11.0.3-r1 | `Containerfile:41` (`QEMU_IMG_VERSION`) |
| squashfs-tools | 4.7.5-r0 | `Containerfile:42` (`SQUASHFS_TOOLS_VERSION`) |

Source files:

- `go.mod` — root Go module (direct dependencies).
- `tools/go.mod` — Go `tool` directives and tool module pins.
- `Makefile` — envtest Kubernetes binary version for the test suite.
- `Containerfile` — Alpine package pins for the runtime image tools.
