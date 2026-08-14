# cluster-api-hypervisor — clusterctl Runbook

How to provision a cluster with clusterctl using this provider's
static-resources packaging. clusterctl delivers the CRDs, RBAC, and webhook
configurations to the management cluster; the manager binary still runs as the
host quadlet described in [docs/install-contract.md](install-contract.md) — no
Deployment is shipped.

Every command below exists in the repository sources; source lines are listed
so the runbook can be re-verified. Source files:

- `Makefile` — the `components`, `components-check`, and `image` targets plus
  the `OUT_DIR` / `RELEASE_VERSION` knobs.
- `clusterctl.yaml` — the committed clusterctl configuration template (three
  hypervisor providers plus the overrides folder).
- `metadata.yaml` — the provider metadata (release series + contract version).
- `config/release/kustomization.yaml` — the release overlay that builds the
  shared component set.
- `templates/cluster-template.yaml` — the default-flavor cluster template.
- `test/e2e/mgmt/apply.sh` — the reference management-plane bootstrap that
  renders the configuration, assembles the offline core override, initializes
  the providers, and patches the webhook CA bundles.
- `test/e2e/run.sh` — the reference full-lab harness that generates and applies
  the workload Cluster.
- `tools/go.mod` — the Go `tool` directives for `clusterctl` and `kustomize`.

## 1. Purpose and model

| Aspect | Value | Source |
|---|---|---|
| What clusterctl delivers | CRDs, RBAC, webhook configurations | `config/release/kustomization.yaml:17-20` |
| What clusterctl does NOT deliver | A manager Deployment — the component set ships no Deployment/Service | contract `test/e2e/clusterctl_test.sh` |
| Manager runtime | Host quadlet per the install contract (unchanged) | `docs/install-contract.md` §5 |
| Provider types | Infrastructure, Bootstrap, Control-Plane — one binary, three provider entries | `clusterctl.yaml:26-35` |
| Contract version | v1beta1 | `metadata.yaml:3-6` |
| Release layout | `{basepath}/{provider-label}/{version}/{components.yaml}` per provider | `Makefile:80-92`, `clusterctl.yaml:26-35` |

`make components` builds the release tree once and writes the identical
component set to all three provider directories; the three hypervisor providers
share every object (see §10 for the label consequences).

## 2. Prerequisites

| Prerequisite | Detail | Source |
|---|---|---|
| KVM-capable host | `/dev/kvm` and the host-ops capability set (bridge/TAP/NAT/VM) | `docs/install-contract.md` §6 |
| podman with quadlet support | Manager runtime; `make image` builds with podman | `Makefile:107-109` |
| kubectl | All management-cluster operations | `test/e2e/run.sh:451` |
| Go toolchain | `go tool clusterctl` / `go tool kustomize` resolve through `tools/go.mod` | `tools/go.mod:11`, `tools/go.mod:14` |
| `make image` | Builds the provider image `cluster-api-hypervisor:dev` | `Makefile:107-109` |
| `make components` | Builds the provider release tree under `OUT_DIR` | `Makefile:80-92` |
| Base image + firmware | `build/k8labs-base.qcow2` and `build/CLOUDHV.fd` (or `HYPERVISOR_BASE_IMAGE` / `HYPERVISOR_FIRMWARE` pointing at your copies) | `docs/install-contract.md` §5.1 |

The provider image is local-only: build it with `make image` and never rely on
a registry (`docs/install-contract.md` §1).

## 3. Build the release tree

```sh
make components
```

The target builds `go tool kustomize build config/release` once and writes the
byte-identical output to each provider directory, copying `metadata.yaml` and
`templates/cluster-template.yaml` alongside (`Makefile:80-92`):

| Path | Contents |
|---|---|
| `out/infrastructure-hypervisor/v0.1.0/` | `infrastructure-components.yaml`, `metadata.yaml`, `cluster-template.yaml` |
| `out/bootstrap-hypervisor/v0.1.0/` | `bootstrap-components.yaml`, `metadata.yaml`, `cluster-template.yaml` |
| `out/control-plane-hypervisor/v0.1.0/` | `control-plane-components.yaml`, `metadata.yaml`, `cluster-template.yaml` |

`OUT_DIR` and `RELEASE_VERSION` are overridable (`Makefile:18-19`); the default
`out/` is gitignored. The three components files are identical (fully-shared
object set). `make components-check` is the idempotency gate: it rebuilds into
a scratch `OUT_DIR` and fails on any diff against the committed layout
(`Makefile:94-101`).

## 4. Install the clusterctl configuration

The committed `clusterctl.yaml` is a template: its three `file://` URLs and the
`overridesFolder` key carry absolute placeholder base paths
(`/var/lib/k8slab/out`, `/var/lib/k8slab/overrides`). Substitute them with your
real release layout and overrides directory, then install the result where
clusterctl reads it: `$XDG_CONFIG_HOME/cluster-api/clusterctl.yaml` (default
`~/.cluster-api/clusterctl.yaml`).

```sh
sed -e 's|/var/lib/k8slab/out|/path/to/out|g' \
    -e 's|/var/lib/k8slab/overrides|/path/to/overrides|g' \
    clusterctl.yaml > ~/.cluster-api/clusterctl.yaml
```

The rendered URLs must stay absolute: clusterctl resolves local repositories as
`{basepath}/{provider-label}/{version}/{components.yaml}` with `file://` URLs
(`clusterctl.yaml:26-37`). The management-plane bootstrap performs the same
substitution when rendering into its hermetic state directory
(`test/e2e/mgmt/apply.sh:149-166`).

## 5. Initialize the providers

```sh
clusterctl init --infrastructure hypervisor --bootstrap hypervisor --control-plane hypervisor --skip-cert-manager
```

The three hypervisor providers register from the local repositories of your
configuration; `--skip-cert-manager` keeps the bootstrap offline (the webhook
certificates are static, provisioned per `docs/install-contract.md` §7, so no
cert-manager is involved). The repository drives the same command through
`go tool clusterctl init` (`test/e2e/mgmt/apply.sh:197-205`).

The core Cluster API provider is initialized by default. To stay fully
offline, pin the core version and pre-assemble a local override so clusterctl
never fetches the upstream core components: the reference bootstrap assembles
`<state>/clusterctl/overrides/cluster-api/v1.13.5/core-components.yaml` from the
committed core manifests and passes `--core cluster-api:v1.13.5`
(`test/e2e/mgmt/apply.sh:168-205`); the same files feed the `overridesFolder`
key of the rendered configuration (`clusterctl.yaml:37`).

## 6. Patch the webhook CA bundles

The components ship the ten admission webhooks with `failurePolicy: Fail` and
an empty `caBundle` — the webhook configurations are url-rewritten to
`https://127.0.0.1:9443/<path>` and the serving certificate is trusted through
the management CA (`config/release/kustomization.yaml:32-144`). Until the
bundle is set, the first admission of a Hypervisor object fails. After
`clusterctl init`, patch the management CA into every webhook entry of both
configurations (the same trust root the webhook serving certs are signed by,
`docs/install-contract.md` §7):

```sh
CA=$(base64 -w0 /path/to/mgmt/ca.pem)
kubectl patch mutating-webhook-configuration --type=json \
  -p '[{"op":"replace","path":"/webhooks/0/clientConfig/caBundle","value":"'"$CA"'"}]'
kubectl patch validating-webhook-configuration --type=json \
  -p '[{"op":"replace","path":"/webhooks/0/clientConfig/caBundle","value":"'"$CA"'"}]'
```

The reference bootstrap builds the per-index JSON patch for every webhook of
both configurations from `<state>/pki/ca.pem` (`test/e2e/mgmt/apply.sh:207-228`);
patching an identical bundle is a no-op, keeping the step idempotent.

## 7. Create a cluster

The default flavor template (`templates/cluster-template.yaml`) contains a
ClusterClass document plus a topology Cluster parameterized with `${VARIABLE}`
markers (`templates/cluster-template.yaml:41-101`). Generate and apply it:

```sh
clusterctl generate cluster <name> --infrastructure hypervisor \
  --kubernetes-version v1.32.13 \
  --control-plane-machine-count 1 \
  --worker-machine-count 3 \
  | kubectl apply -f -
```

clusterctl substitutes `${CLUSTER_NAME}`, `${NAMESPACE}`, `${KUBERNETES_VERSION}`,
`${CONTROL_PLANE_MACHINE_COUNT}`, and `${WORKER_MACHINE_COUNT}` from the flags
and the configuration; the literal defaults the markers stand for are the
k8labs example (`templates/cluster-template.yaml:8-24`). The reference harness
runs the identical generate-and-apply pipe (with `--namespace default`) through
`go tool clusterctl generate cluster` (`test/e2e/run.sh:339-356`).

## 8. Wait and inspect

The ClusterClass reconciles the topology into Machines, HypervisorMachines, and
a HypervisorCluster; the provider boots the VMs through cloud-hypervisor on the
host:

```sh
kubectl get machines
kubectl get hypervisormachines
kubectl get hypervisorclusters
```

`machines` comes from the core Cluster API provider; the hypervisor kinds from
the provider CRDs (`cluster.x-k8s.io/v1alpha1`). Once the control plane is up,
fetch the workload kubeconfig from the `<cluster>-kubeconfig` Secret written by
the core controllers (the reference harness does exactly this,
`test/e2e/run.sh:388-397`):

```sh
kubectl get secret <name>-kubeconfig -n <namespace> -o jsonpath='{.data.value}' | base64 -d > kubeconfig
```

## 9. Teardown

```sh
kubectl delete cluster <name>
```

Deleting the Cluster object tears down the Machines and, through them, the VMs,
TAPs, and disks (the reference harness deletes the Cluster on exit,
`test/e2e/run.sh:423-429`). Do NOT use `clusterctl delete`: the fully-shared
object set carries only the infrastructure provider label
(`config/release/kustomization.yaml:7-13`, `config/release/kustomization.yaml:22-26`),
so clusterctl delete of the bootstrap or control-plane providers would find no
labeled objects; Cluster-object deletion is the supported teardown path.

## 10. Limitations

- **Static-resources model**: no Deployment is shipped (`test/e2e/clusterctl_test.sh`
  pins "no Deployment/Service"), so `clusterctl init --wait-providers` has
  nothing to wait on — the manager must run as the host quadlet.
- **Host-ops prerequisites**: the manager quadlet requires `Network=host`,
  privileged mode, `NET_ADMIN`, KVM access (`/dev/kvm`), and the host bind
  mounts for the base image, firmware, VM disks, state, sockets, webhook certs,
  and kubeconfig (`docs/install-contract.md` §5-§6).
- **Contract version**: v1beta1, declared in `metadata.yaml:3-6` (clusterctl
  1.13 accepts it through the compatibility window).
- **Fully-shared object labels**: every object in every provider's components
  file carries `cluster.x-k8s.io/provider: infrastructure-hypervisor`; the
  installer order Core -> Bootstrap -> Control-Plane -> Infrastructure makes the
  infrastructure provider deterministically own the shared set, and teardown is
  Cluster-object deletion rather than clusterctl delete (§9).
- **Offline e2e reference**: the full-lab harness provisions the plane with
  `clusterctl init` against locally assembled core overrides and never touches
  the network (`test/e2e/mgmt/apply.sh:168-205`); the runbook's §5 init shows
  the same flags without the offline scaffolding.
