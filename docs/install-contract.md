# cluster-api-hypervisor — Install Contract

This document is the install contract for the cluster-api-hypervisor provider.
It defines everything needed to run the provider as a podman quadlet on a
single lab host against a bare management apiserver: the image reference, the
quadlet unit template, the environment/flags contract, the required mounts,
the required capabilities, and webhook certificate provisioning.

The consumer of this contract is the k8labs-side `mgmt` module (and any
operator following it by hand). It is self-contained: no other document is
required to construct and start the provider quadlet.

Every value below is read from the repository sources; the source lines are
listed so the contract can be re-verified. Source files:

- `internal/config/config.go` — environment variable resolution and defaults.
- `main.go` — manager command-line flags and fixed host-network constants.
- `Makefile` — image tag and build target.
- `Containerfile` — runtime image layout and bundled tool versions.
- `VERSIONS.md` — pinned tool versions (source of truth table).

---

## 1. Image reference and build

| Item | Value | Source |
|---|---|---|
| Image tag | `cluster-api-hypervisor:dev` | `Makefile:5` (`IMAGE ?= cluster-api-hypervisor:dev`) |
| Build target | `make image` | `Makefile:107-109` |
| Build command | `podman build -t cluster-api-hypervisor:dev -f Containerfile .` | `Makefile:109` |
| Entry point | `/usr/local/bin/cluster-api-hypervisor` | `Containerfile:52-54` |
| Base image | Alpine edge (digest-pinned) | `Containerfile:35` |

The image is local-only: it is built with podman into local container storage
and is never published to a registry. A quadlet references it as
`localhost/cluster-api-hypervisor:dev` (podman's unambiguous form of the built
tag).

Build prerequisites: podman and a Go toolchain matching `VERSIONS.md:8` for the
multi-stage build (`Containerfile:18-33`). The build does not require a
registry.

### 1.1 Tools bundled in the runtime image

The provider shells out to host tooling; the image therefore packages the
binaries so the environment defaults (section 3) resolve inside the container:

| Tool | Version pin | Source |
|---|---|---|
| cloud-hypervisor | 48.0-r0 | `Containerfile:40` / `VERSIONS.md:16` |
| qemu-img | 11.0.3-r1 | `Containerfile:41` / `VERSIONS.md:17` |
| squashfs-tools (mksquashfs) | 4.7.5-r0 | `Containerfile:42` / `VERSIONS.md:18` |
| dnsmasq | 2.93-r0 | `Containerfile:43` / `VERSIONS.md:19` |

The runtime is Alpine edge (x86_64); cloud-hypervisor is pulled from the
`edge/testing` repository (`Containerfile:45-50`).

---

## 2. Provider startup contract

The provider is a single process: one manager registers four controllers and
five admission webhooks, then runs until a shutdown signal arrives
(`main.go:201-277`). It resolves host-specific configuration from the
environment (section 3) and command-line flags (section 4). Exactly one
provider instance must run: leader election is disabled by design
(`main.go:234-238`), so running a second quadlet would duplicate host-side
work (bridge, NAT, VM processes) without arbitration.

At startup the provider:

- Loads the management cluster REST config from `--kubeconfig` when set,
  otherwise the controller-runtime default loading rules (`main.go:283-292`).
- Resolves the environment into the provider configuration; a malformed
  `HYPERVISOR_NETWORK_CIDR` aborts startup (`main.go:214-218`,
  `internal/config/config.go:99-101`).
- Serves the admission webhooks on `--webhook-port` with the TLS material from
  `--webhook-cert-dir` (`main.go:227-232`).
- Binds health and readiness endpoints on `--health-addr`: `/healthz` and
  `/readyz` (`main.go:317-325`).

Fixed host-network constants the operator should be aware of (not configurable,
read from `main.go:82-90` and `main.go:117`): the provider-owned bridge is
`k8sbr0`, the nftables table is `inet k8slab`, the lab gateway/DNS address is
`192.168.124.1`, and dnsmasq forwards to `1.1.1.1`/`8.8.8.8`.

The CRDs, RBAC, and webhook configurations the controllers need can
alternatively be delivered by `clusterctl init` using the committed
`clusterctl.yaml` and the `make components` output instead of manual kubectl
apply of generated manifests; the quadlet of section 5 remains the manager
runtime either way (see [docs/clusterctl.md](clusterctl.md) for the clusterctl
runbook).

---

## 3. Environment variables (`HYPERVISOR_*`)

All host-specific paths and binaries are provider-level configuration passed
as environment variables; there are no spec fields for them. Every variable is
read in `Load` (`internal/config/config.go:82-104`); an unset or empty value
falls back to the default (`internal/config/config.go:107-112`).

| Environment variable | Default | Source (default / read) |
|---|---|---|
| `HYPERVISOR_BASE_IMAGE` | `build/k8labs-base.qcow2` | `config.go:64` / `config.go:88` |
| `HYPERVISOR_FIRMWARE` | `build/CLOUDHV.fd` | `config.go:65` / `config.go:89` |
| `HYPERVISOR_VM_DISKS_DIR` | `build/vm-disks` | `config.go:66` / `config.go:90` |
| `HYPERVISOR_SOCKET_DIR` | `/tmp/ch-capi` | `config.go:67` / `config.go:91` |
| `HYPERVISOR_STATE_DIR` | `/var/lib/k8slab` | `config.go:68` / `config.go:92` |
| `HYPERVISOR_CH_BINARY` | `cloud-hypervisor` | `config.go:69` / `config.go:93` |
| `HYPERVISOR_QEMU_IMG` | `qemu-img` | `config.go:70` / `config.go:94` |
| `HYPERVISOR_DNSMASQ` | `dnsmasq` | `config.go:71` / `config.go:95` |
| `HYPERVISOR_NETWORK_CIDR` | `192.168.124.0/24` | `config.go:72` / `config.go:96` |

Semantics:

- The three relative defaults (`build/...`) resolve against the provider
  process working directory, which inside the container is `/` (the runtime
  stage sets no `WORKDIR`). With the mounts of section 5 they correspond to
  `/build/k8labs-base.qcow2`, `/build/CLOUDHV.fd`, and `/build/vm-disks`; the
  unit template sets the absolute container-side values explicitly so the unit
  does not depend on the working directory.
- `HYPERVISOR_CH_BINARY`, `HYPERVISOR_QEMU_IMG`, and `HYPERVISOR_DNSMASQ` are
  resolved through `PATH` inside the container, where the image installs the
  pinned tools (section 1.1). Overriding one to a host path requires mounting
  that binary into the container.
- `HYPERVISOR_NETWORK_CIDR` is the only validated variable: it must parse as an
  IPv4 network or startup fails (`config.go:115-123`).
- No other path is validated at startup: paths are resolved lazily by the
  controllers, so a misconfigured mount surfaces as a reconcile failure, not a
  startup error (`config.go:79-81`).

---

## 4. Manager flags

The flags are defined in `initFlags` (`main.go:150-199`). All defaults below
are the exact values from `main.go`; the unit template sets them explicitly.

| Flag | Default | Purpose | Source |
|---|---|---|---|
| `--kubeconfig` | empty (in-cluster config or default loading rules) | Management cluster kubeconfig | `main.go:151-156` |
| `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Directory holding `tls.key` and `tls.crt` | `main.go:157-162` |
| `--webhook-port` | `9443` | Webhook server port | `main.go:163-168`, constant `main.go:72` |
| `--health-addr` | `:9440` | Health/readiness bind address | `main.go:169-174`, constant `main.go:75` |
| `--hypervisorcluster-concurrency` | `1` | HypervisorCluster reconcile concurrency | `main.go:175-180` |
| `--hypervisormachine-concurrency` | `1` | HypervisorMachine reconcile concurrency | `main.go:181-186` |
| `--hypervisorconfig-concurrency` | `1` | HypervisorConfig reconcile concurrency | `main.go:187-192` |
| `--hypervisorcontrolplane-concurrency` | `1` | HypervisorControlPlane reconcile concurrency | `main.go:193-198` |

Notes:

- Because the management plane is a bare apiserver with no in-cluster service
  account, `--kubeconfig` must point at a mounted admin kubeconfig for the
  management cluster (or `KUBECONFIG` must be set in the unit).
- `--health-addr` exposes the provider's own health/readiness endpoints, which
  the `mgmt` module can use for liveness.

---

## 5. Quadlet unit template

Place the unit at `/etc/containers/systemd/cluster-api-hypervisor.container`,
run `systemctl daemon-reload`, then `systemctl start cluster-api-hypervisor`.

```ini
[Unit]
Description=cluster-api-hypervisor provider quadlet
After=network-online.target
Wants=network-online.target

[Container]
Image=localhost/cluster-api-hypervisor:dev
Network=host
PodmanArgs=--privileged
AddCapability=NET_ADMIN

# Read-only provider inputs: baked base image, firmware, and the writable
# per-machine disk directory (see section 5.1).
Mount=type=bind,source=/var/lib/k8slab/build,target=/build
# Provider state: dnsmasq data, nftables state, cluster PKI artifacts.
Mount=type=bind,source=/var/lib/k8slab,target=/var/lib/k8slab
# cloud-hypervisor control sockets: the provider writes one socket tree per
# VM under this directory.
Mount=type=bind,source=/tmp/ch-capi,target=/tmp/ch-capi
# Webhook serving certificate and key (provisioned in section 7).
Mount=type=bind,source=/etc/cluster-api-hypervisor/webhook-certs,target=/tmp/k8s-webhook-server/serving-certs
# Management apiserver kubeconfig.
Mount=type=bind,source=/etc/kubernetes/mgmt/admin.conf,target=/etc/kubernetes/mgmt/admin.conf,ro

Environment=HYPERVISOR_BASE_IMAGE=/build/k8labs-base.qcow2
Environment=HYPERVISOR_FIRMWARE=/build/CLOUDHV.fd
Environment=HYPERVISOR_VM_DISKS_DIR=/build/vm-disks
Environment=HYPERVISOR_SOCKET_DIR=/tmp/ch-capi
Environment=HYPERVISOR_STATE_DIR=/var/lib/k8slab
Environment=HYPERVISOR_CH_BINARY=cloud-hypervisor
Environment=HYPERVISOR_QEMU_IMG=qemu-img
Environment=HYPERVISOR_DNSMASQ=dnsmasq
Environment=HYPERVISOR_NETWORK_CIDR=192.168.124.0/24

Exec=--kubeconfig=/etc/kubernetes/mgmt/admin.conf
Exec=--webhook-cert-dir=/tmp/k8s-webhook-server/serving-certs
Exec=--webhook-port=9443
Exec=--health-addr=:9440
Exec=--hypervisorcluster-concurrency=1
Exec=--hypervisormachine-concurrency=1
Exec=--hypervisorconfig-concurrency=1
Exec=--hypervisorcontrolplane-concurrency=1

[Service]
Restart=always
```

Host paths in the `Mount=` lines are examples; the operator defines the actual
layout. The rule is: every host path listed in section 5.1 must be bind-mounted
into the container, and every `HYPERVISOR_*` value must name a path that exists
inside the container filesystem.

### 5.1 Required mounts

| Contract item | Host source (example) | Container target | Consumed by |
|---|---|---|---|
| Base image | `/var/lib/k8slab/build/k8labs-base.qcow2` | `/build/k8labs-base.qcow2` | `HYPERVISOR_BASE_IMAGE` |
| Firmware | `/var/lib/k8slab/build/CLOUDHV.fd` | `/build/CLOUDHV.fd` | `HYPERVISOR_FIRMWARE` |
| vm-disks dir | `/var/lib/k8slab/build/vm-disks` | `/build/vm-disks` | `HYPERVISOR_VM_DISKS_DIR` |
| State dir | `/var/lib/k8slab` | `/var/lib/k8slab` | `HYPERVISOR_STATE_DIR` |
| Socket dir | `/tmp/ch-capi` | `/tmp/ch-capi` | `HYPERVISOR_SOCKET_DIR` |
| Webhook certs | `/etc/cluster-api-hypervisor/webhook-certs` | `/tmp/k8s-webhook-server/serving-certs` | `--webhook-cert-dir` |
| Management kubeconfig | `/etc/kubernetes/mgmt/admin.conf` | `/etc/kubernetes/mgmt/admin.conf` | `--kubeconfig` |

The base image, firmware, and vm-disks directory are all satisfied by the
single `/build` bind: the image and firmware are read-only inputs, while
vm-disks must be writable because the provider converts/resizes root disks
there with `qemu-img`. The state dir keeps dnsmasq and cluster PKI artifacts
between provider restarts; the socket dir holds the per-VM cloud-hypervisor
API sockets; the webhook certs and management kubeconfig are the TLS and
authentication inputs described in sections 4 and 7.

---

## 6. Capabilities

| Setting | Rationale |
|---|---|
| `Network=host` | The provider owns the host network stack for the lab: it creates and enslaves the `k8sbr0` bridge and per-Machine TAPs via netlink, runs dnsmasq as a subprocess answering on the host `:53`, and manages the `inet k8slab` nftables table. These operations target the host network namespace, so the container must share it. |
| `AddCapability=NET_ADMIN` | Netlink bridge/TAP management (creation, enslaving, address assignment) and nftables rule installation require `NET_ADMIN` in the shared network namespace. |
| `PodmanArgs=--privileged` | Quadlet has no `Privileged=` key for `[Container]` units, so privileged mode is passed through as a podman argument. The provider spawns cloud-hypervisor subprocesses that need host device access (KVM) and full capability set for the host-ops model (bridge/NAT/VM management). This is the "privileged as needed" capability the provider's host-ops model requires; `NET_ADMIN` is listed explicitly so the requirement is visible even though privileged mode implies it. |

---

## 7. Webhook certificate provisioning

The five admission webhooks are served over TLS from the directory named by
`--webhook-cert-dir`, expecting `tls.key` and `tls.crt` (`main.go:157-162`).
No cert-manager is involved; the certificates are static, provisioned before
the quadlet starts.

Provisioning flow (implemented by the `mgmt` module's install script; a
reference script is out of scope here):

1. **Generate a CA**: create a self-signed CA key and certificate
   (`ca.key`, `ca.crt`). This CA is the trust root for the webhook serving
   certs and is not rotated per restart.
2. **Generate a serving certificate**: create a key and a certificate signed
   by the CA, valid for the address the apiserver uses to reach the webhook.
   Because the provider runs with `Network=host` on the apiserver's own host,
   the SANs must include at least `127.0.0.1` and `localhost`, plus the lab
   host IP/hostname if the webhook `clientConfig` uses it. Write the
   certificate as `tls.crt` and the key as `tls.key` into the directory that
   will be mounted at `--webhook-cert-dir`.
3. **Patch the webhook configurations**: base64-encode `ca.crt` and set it as
   `caBundle` in every entry of the `ValidatingWebhookConfiguration` and
   `MutatingWebhookConfiguration` manifests (generated into `config/webhook`
   by `make generate`, `Makefile:66-72`). The apiserver uses this bundle to
   verify the webhook's serving certificate.
4. **Mount and start**: the quadlet bind-mounts the certificate directory into
   the container (section 5.1); the provider starts its webhook server with
   the provisioned material. The webhook endpoints then answer with the
   provisioned serving cert, and the apiserver's bundle validates it.

After provisioning, the install contract is satisfied when the provider
quadlet starts against the bare management apiserver, the CRDs reconcile, and
the webhook endpoints answer with the provisioned certificate.

### 7.1 clusterctl delivery path

When the webhook configurations are delivered by `clusterctl init` instead of
kubectl apply of generated manifests, the components ship the webhooks with
`failurePolicy: Fail` and an empty `caBundle` (url-rewritten to
`https://127.0.0.1:9443/<path>`; see [docs/clusterctl.md](clusterctl.md)). The
step 3 caBundle patch above is then performed after `clusterctl init`, patching
the management CA into every webhook entry of both configurations — the
reference management-plane bootstrap does exactly this
(`test/e2e/mgmt/apply.sh:298-319`).
