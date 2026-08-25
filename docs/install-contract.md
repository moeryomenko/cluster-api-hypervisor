# cluster-api-hypervisor — Install Contract

This document is the install contract for the cluster-api-hypervisor provider.
It defines everything needed to run the provider as a rootless podman quadlet
on a single lab host against a bare management apiserver: the image reference,
the two user quadlet units (the k8netd network daemon and the provider), the
environment/flags contract, the required mounts, the security model, and
webhook certificate provisioning.

The consumer of this contract is the k8labs-side `mgmt` module (and any
operator following it by hand). It is self-contained: no other document is
required to construct and start the two user quadlets.

Every value below is read from the repository sources; the source lines are
listed so the contract can be re-verified. Source files:

- `internal/config/config.go` — environment variable resolution and defaults.
- `main.go` — manager command-line flags and startup wiring.
- `internal/k8netd/client.go` — the k8netd control-socket client (startup
  ordering tolerance).
- `Makefile` — image tag and build target.
- `Containerfile` — runtime image layout and bundled tool versions.
- `VERSIONS.md` (repo root) — pinned tool versions (source of truth table).
- `test/e2e/mgmt/units/cluster-api-hypervisor.quadlet` — the reference
  management-plane provider unit (environment and Exec directives).
- `.specs/k8netd-contract/spec.md` — the external k8netd contract (transfers
  to the k8netd repository); defines the `K8NETD_*` configuration surface.

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
tag). Rootless podman builds and stores the image in the lab user's container
storage; no root step is involved.

Build prerequisites: podman and a Go toolchain matching `VERSIONS.md:8` for the
multi-stage build (`Containerfile:18-33`). The build does not require a
registry.

### 1.1 Tools bundled in the runtime image

The provider shells out to host tooling; the image therefore packages the
binaries so the environment defaults (section 3) resolve inside the container:

| Tool | Version pin | Source |
|---|---|---|
| cloud-hypervisor | 48.0-r0 | `Containerfile:40` / `VERSIONS.md:18` |
| qemu-img | 11.0.3-r1 | `Containerfile:41` / `VERSIONS.md:19` |
| squashfs-tools (mksquashfs) | 4.7.5-r0 | `Containerfile:42` / `VERSIONS.md:20` |

The runtime is Alpine edge (x86_64); cloud-hypervisor is pulled from the
`edge/testing` repository (`Containerfile:45-50`).

---

## 2. Provider startup contract

The provider is a single process: one manager registers four controllers and
five admission webhooks, then runs until a shutdown signal arrives
(`main.go:198-274`). It resolves host-specific configuration from the
environment (section 3) and command-line flags (section 4). Exactly one
provider instance must run: leader election is disabled by design
(`main.go:231-236`), so running a second quadlet would duplicate host-side
work (VM processes, k8netd port allocations) without arbitration.

At startup the provider:

- Loads the management cluster REST config from `--kubeconfig` when set,
  otherwise the controller-runtime default loading rules (`main.go:280-289`).
- Resolves the environment into the provider configuration; a malformed
  `HYPERVISOR_NETWORK_CIDR` aborts startup (`main.go:211-215`,
  `internal/config/config.go:115-117`).
- Serves the admission webhooks on `--webhook-port` with the TLS material from
  `--webhook-cert-dir` (`main.go:226-229`).
- Binds health and readiness endpoints on `--health-addr`: `/healthz` and
  `/readyz` (`main.go:314-322`).

Host networking is owned by k8netd, not the provider. All cluster network
lifecycle — the per-cluster L2 segment, IPAM, DHCP, DNS forwarding, and the
per-VM passt WAN — is driven through JSON-RPC calls on the k8netd Unix control
socket (`HYPERVISOR_K8NETD_SOCKET`, section 3): the cluster controller issues
CreateNetwork/DeleteNetwork, the machine controller issues
CreatePort/AttachPort/AllocateIP and DetachPort/DeletePort per VM. The
workload control plane is reached at `https://127.0.0.1:6443`; reachability is
provided by the control-plane VM's passt instance forwarding host port 6443
(k8netd contract REQ-008). The provider performs no host network operation
itself.

The CRDs, RBAC, and webhook configurations the controllers need can
alternatively be delivered by `clusterctl init` using the committed
`clusterctl.yaml` and the `make components` output instead of manual kubectl
apply of generated manifests; the quadlets of section 5 remain the manager
runtime either way (see [docs/clusterctl.md](clusterctl.md) for the clusterctl
runbook).

---

## 3. Environment variables (`HYPERVISOR_*`)

All host-specific paths and binaries are provider-level configuration passed
as environment variables; there are no spec fields for them. Every variable is
read in `Load` (`internal/config/config.go:108-131`); an unset or empty value
falls back to the default (`internal/config/config.go:73-82`,
`internal/config/config.go:84-97`). The single exception is
`HYPERVISOR_SSH_PUBLIC_KEY_FILE`, which has no default and stays empty when
unset.

| Environment variable | Default | Source (default / read) |
|---|---|---|
| `HYPERVISOR_BASE_IMAGE` | `build/k8labs-base.qcow2` | `config.go:74` / `config.go:114` |
| `HYPERVISOR_FIRMWARE` | `build/CLOUDHV.fd` | `config.go:75` / `config.go:115` |
| `HYPERVISOR_VM_DISKS_DIR` | `build/vm-disks` | `config.go:76` / `config.go:116` |
| `HYPERVISOR_SOCKET_DIR` | `/tmp/ch-capi` | `config.go:77` / `config.go:117` |
| `HYPERVISOR_STATE_DIR` | `$HOME/.local/state/k8slab` | `config.go:84-97` / `config.go:118` |
| `HYPERVISOR_CH_BINARY` | `cloud-hypervisor` | `config.go:78` / `config.go:119` |
| `HYPERVISOR_QEMU_IMG` | `qemu-img` | `config.go:79` / `config.go:120` |
| `HYPERVISOR_K8NETD_SOCKET` | `/run/user/1000/k8snet/control.sock` | `config.go:80` / `config.go:121` |
| `HYPERVISOR_NETWORK_CIDR` | `192.168.124.0/24` | `config.go:81` / `config.go:122` |
| `HYPERVISOR_SSH_PUBLIC_KEY_FILE` | *(none — feature off when unset)* | `config.go:123` |

Semantics:

- The three relative defaults (`build/...`) resolve against the provider
  process working directory, which inside the container is `/` (the runtime
  stage sets no `WORKDIR`). With the mounts of section 5 they correspond to
  `/build/k8labs-base.qcow2`, `/build/CLOUDHV.fd`, and `/build/vm-disks`; the
  unit template sets the absolute container-side values explicitly so the unit
  does not depend on the working directory.
- `HYPERVISOR_STATE_DIR` resolves to a user-writable location by default:
  `$HOME/.local/state/k8slab` when `HOME` is set, `$XDG_STATE_HOME/k8slab`
  otherwise, falling back to `/tmp/k8slab-state` in minimal environments
  (`config.go:84-97`). Every host path in this contract lives under the lab
  user's home or another user-writable location; nothing is written to
  system-owned directories.
- `HYPERVISOR_CH_BINARY` and `HYPERVISOR_QEMU_IMG` are resolved through `PATH`
  inside the container, where the image installs the pinned tools
  (section 1.1). Overriding one to a host path requires mounting that binary
  into the container.
- `HYPERVISOR_K8NETD_SOCKET` is the Unix control socket of the k8netd daemon;
  the default assumes the lab user's uid is 1000 (adjust the path when it is
  not). The directory holding it is mounted into the container (section 5.1).
- `HYPERVISOR_NETWORK_CIDR` is the only validated variable: it must parse as an
  IPv4 network or startup fails (`config.go:126-128`).
- No other path is validated at startup: paths are resolved lazily by the
  controllers, so a misconfigured mount surfaces as a reconcile failure, not a
  startup error (`config.go:105-107`).
- `HYPERVISOR_SSH_PUBLIC_KEY_FILE` is optional and has no default: when set it
  names a file inside the provider container (e.g. `/build/ssh-lab.pub` on the
  mounted build directory) whose trimmed content is the SSH public key
  injected into machines whose HypervisorConfig leaves `spec.sshPublicKey`
  empty. A non-empty spec key always wins. When the variable is unset, or the
  named file is missing or empty, machine provisioning fails with an error
  naming the variable.

---

## 4. Manager flags

The flags are defined in `initFlags` (`main.go:147-196`). All defaults below
are the exact values from `main.go`; the unit template sets them explicitly.

| Flag | Default | Purpose | Source |
|---|---|---|---|
| `--kubeconfig` | empty (in-cluster config or default loading rules) | Management cluster kubeconfig | `main.go:148-153` |
| `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Directory holding `tls.key` and `tls.crt` | `main.go:154-159` |
| `--webhook-port` | `9443` | Webhook server port | `main.go:160-165`, constant `main.go:69` |
| `--health-addr` | `:9440` | Health/readiness bind address | `main.go:166-171`, constant `main.go:72` |
| `--hypervisorcluster-concurrency` | `1` | HypervisorCluster reconcile concurrency | `main.go:172-177` |
| `--hypervisormachine-concurrency` | `1` | HypervisorMachine reconcile concurrency | `main.go:178-183` |
| `--hypervisorconfig-concurrency` | `1` | HypervisorConfig reconcile concurrency | `main.go:184-189` |
| `--hypervisorcontrolplane-concurrency` | `1` | HypervisorControlPlane reconcile concurrency | `main.go:190-195` |

Notes:

- Because the management plane is a bare apiserver with no in-cluster service
  account, `--kubeconfig` must point at a mounted admin kubeconfig for the
  management cluster (or `KUBECONFIG` must be set in the unit).
- `--health-addr` exposes the provider's own health/readiness endpoints, which
  the `mgmt` module can use for liveness.

---

## 5. Quadlet units

The install consists of exactly two **user** quadlets, both installed into the
lab user's quadlet directory `~/.config/containers/systemd/` (or
`$XDG_CONFIG_HOME/containers/systemd/`):

1. `k8netd.service` — the rootless userspace network daemon (vhost-user L2
   switch, IPAM, DHCP, DNS, per-VM passt WAN). Its binary, image, and unit are
   owned by the k8netd repository; this contract pins only its install shape
   and environment surface.
2. `cluster-api-hypervisor.service` — the provider manager (generated by
   podman from the `cluster-api-hypervisor.container` quadlet below).

Both units run on the systemd **user** bus. One-time preparation, performed as
the lab user unless marked otherwise:

```sh
# One-time, as an administrator (the single privileged bootstrap step):
# add the lab user to the kvm group so cloud-hypervisor can use /dev/kvm.
usermod -aG kvm lab   # the lab user then logs out and back in

# Everything below runs as the lab user.
loginctl enable-linger            # keep user services outside login sessions
mkdir -p ~/.local/state/k8slab/build/vm-disks \
         ~/.local/state/k8slab/webhook-certs \
         ~/.local/state/k8slab/kubeconfigs \
         /tmp/ch-capi \
         /run/user/$(id -u)/k8snet
mkdir -p ~/.config/containers/systemd
# Write the two unit files of sections 5.0.1 and 5.0.2 into
# ~/.config/containers/systemd/, then:
systemctl --user daemon-reload
systemctl --user start k8netd.service cluster-api-hypervisor.service
```

Startup ordering: the provider unit declares `After=`/`Wants=` on
`k8netd.service`, and the provider additionally tolerates k8netd starting
later — the k8netd client dials the control socket with bounded exponential
backoff (10 ms doubling to a 100 ms cap, bounded by the call context or a
default window) before failing (`internal/k8netd/client.go:259-299`), so a
restart of either unit converges without manual intervention.

### 5.0.1 k8netd unit

The k8netd repository owns the daemon's image and full unit; the install
shape this contract depends on is a user quadlet carrying the `K8NETD_*`
configuration variables defined by the k8netd contract
(`.specs/k8netd-contract/spec.md` section 2, transferring to the k8netd
repository):

```ini
# ~/.config/containers/systemd/k8netd.container
[Unit]
Description=k8netd rootless network daemon
Wants=network-online.target
After=network-online.target

[Container]
Image=localhost/k8netd:dev
# Socket directory and persisted daemon state live under the user's runtime
# and state homes; the vhost-user port sockets are served from here.
Volume=%h/.local/state/k8slab/k8netd:/var/lib/k8netd

Environment=K8NETD_SOCKET_DIR=/run/user/1000/k8snet
Environment=K8NETD_UPSTREAM_DNS=1.1.1.1,8.8.8.8
Environment=K8NETD_MTU=1500
Environment=K8NETD_PASST_FORWARDS=6443,22

[Service]
Restart=always
```

The variable set above mirrors the contract's documented configuration
surface — socket directory (default `/run/user/1000/k8snet/`), upstream DNS
resolvers (default `1.1.1.1`, `8.8.8.8`), MTU (default `1500`), and passt port
forwards (default `6443`, `22`). The authoritative names and defaults are
owned by the k8netd repository once the contract spec transfers there; this
unit must be kept in sync with that definition.

### 5.0.2 Provider unit template

```ini
# ~/.config/containers/systemd/cluster-api-hypervisor.container
[Unit]
Description=cluster-api-hypervisor provider quadlet
After=network-online.target k8netd.service
Wants=network-online.target k8netd.service

[Container]
Image=localhost/cluster-api-hypervisor:dev
Network=host
Device=/dev/kvm

# Read-only provider inputs: baked base image, firmware, and the writable
# per-machine disk directory (see section 5.1).
Mount=type=bind,source=%h/.local/state/k8slab/build,target=/build
# Provider state: cluster PKI artifacts between provider restarts.
Mount=type=bind,source=%h/.local/state/k8slab,target=/state
# k8netd control and vhost-user port sockets (HYPERVISOR_K8NETD_SOCKET).
Mount=type=bind,source=/run/user/1000/k8snet,target=/run/user/1000/k8snet
# cloud-hypervisor control sockets: the provider writes one socket tree per
# VM under this directory.
Mount=type=bind,source=/tmp/ch-capi,target=/tmp/ch-capi
# Webhook serving certificate and key (provisioned in section 7).
Mount=type=bind,source=%h/.local/state/k8slab/webhook-certs,target=/tmp/k8s-webhook-server/serving-certs
# Management apiserver kubeconfig.
Mount=type=bind,source=%h/.local/state/k8slab/kubeconfigs/admin.conf,target=/etc/kubernetes/mgmt/admin.conf,ro

Environment=HYPERVISOR_BASE_IMAGE=/build/k8labs-base.qcow2
Environment=HYPERVISOR_FIRMWARE=/build/CLOUDHV.fd
Environment=HYPERVISOR_VM_DISKS_DIR=/build/vm-disks
Environment=HYPERVISOR_SOCKET_DIR=/tmp/ch-capi
Environment=HYPERVISOR_STATE_DIR=/state
Environment=HYPERVISOR_CH_BINARY=cloud-hypervisor
Environment=HYPERVISOR_QEMU_IMG=qemu-img
Environment=HYPERVISOR_K8NETD_SOCKET=/run/user/1000/k8snet/control.sock
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

The environment and Exec blocks match the reference management-plane unit
(`test/e2e/mgmt/units/cluster-api-hypervisor.quadlet:38-55`); the reference
unit renders the `<state>` prefix of its host paths from the management
state directory at install time. Host paths in the `Mount=` lines are
examples; the operator defines the actual layout. The rule is: every host path
listed in section 5.1 must be bind-mounted into the container, and every
`HYPERVISOR_*` value must name a path that exists inside the container
filesystem. Adjust the `/run/user/1000/...` paths when the lab user's uid is
not 1000 (both the mount and `HYPERVISOR_K8NETD_SOCKET`).

### 5.1 Required mounts

| Contract item | Host source (example) | Container target | Consumed by |
|---|---|---|---|
| Base image | `%h/.local/state/k8slab/build/k8labs-base.qcow2` | `/build/k8labs-base.qcow2` | `HYPERVISOR_BASE_IMAGE` |
| Firmware | `%h/.local/state/k8slab/build/CLOUDHV.fd` | `/build/CLOUDHV.fd` | `HYPERVISOR_FIRMWARE` |
| vm-disks dir | `%h/.local/state/k8slab/build/vm-disks` | `/build/vm-disks` | `HYPERVISOR_VM_DISKS_DIR` |
| State dir | `%h/.local/state/k8slab` | `/state` | `HYPERVISOR_STATE_DIR` |
| k8netd runtime dir | `/run/user/1000/k8snet` | `/run/user/1000/k8snet` | `HYPERVISOR_K8NETD_SOCKET` |
| Socket dir | `/tmp/ch-capi` | `/tmp/ch-capi` | `HYPERVISOR_SOCKET_DIR` |
| Webhook certs | `%h/.local/state/k8slab/webhook-certs` | `/tmp/k8s-webhook-server/serving-certs` | `--webhook-cert-dir` |
| Management kubeconfig | `%h/.local/state/k8slab/kubeconfigs/admin.conf` | `/etc/kubernetes/mgmt/admin.conf` | `--kubeconfig` |

Every host path above is owned and writable by the lab user: the
`%h/.local/state/k8slab` tree is created by the lab user (section 5),
`/tmp/ch-capi` sits under the world-writable `/tmp`, and
`/run/user/<uid>/k8snet` is the lab user's own XDG runtime directory. No
ownership change to a system directory is ever required.

The base image, firmware, and vm-disks directory are all satisfied by the
single `/build` bind: the image and firmware are read-only inputs, while
vm-disks must be writable because the provider converts/resizes root disks
there with `qemu-img`. The state dir keeps the cluster PKI artifacts between
provider restarts; the k8netd runtime dir carries the control socket and the
per-port vhost-user sockets the VMs attach through; the socket dir holds the
per-VM cloud-hypervisor API sockets; the webhook certs and management
kubeconfig are the TLS and authentication inputs described in sections 4
and 7.

---

## 6. Security model

The install is fully rootless: neither unit grants capabilities beyond the
lab user's own permissions, and neither performs a privileged host operation.

| Setting | Rationale |
|---|---|
| `Network=host` | The provider serves the admission webhooks on `:9443` and the health/readiness endpoints on `:9440`, and the management apiserver runs on the same host; sharing the host network namespace lets the apiserver reach both without port publishing. This is a serving convenience only — the provider creates no interfaces and touches no host network state. |
| `Device=/dev/kvm` | cloud-hypervisor subprocesses need KVM acceleration. Access is granted by the lab user's `kvm` group membership (section 5), not by capabilities. |
| No elevated capabilities | The provider performs no host network operation: the L2 segment, IPAM, DHCP, DNS, and WAN all live in the k8netd user-space daemon. Neither unit needs capabilities beyond the lab user's own permissions, so none are granted — no capability additions and no privileged mode. |

---

## 7. Webhook certificate provisioning

The five admission webhooks are served over TLS from the directory named by
`--webhook-cert-dir`, expecting `tls.key` and `tls.crt` (`main.go:154-159`).
No cert-manager is involved; the certificates are static, provisioned before
the quadlet starts.

Provisioning flow (implemented by the `mgmt` module's install script; a
reference script is out of scope here):

1. **Generate a CA**: create a self-signed CA key and certificate
   (`ca.key`, `ca.crt`). This CA is the trust root for the webhook serving
   certs and is not rotated per restart.
2. **Generate a serving certificate**: create a key and a certificate signed
   by the CA, valid for the address the apiserver uses to reach the webhook.
   Because the provider shares the host network namespace with the apiserver,
   the SANs must include at least `127.0.0.1` and `localhost`, plus the lab
   host IP/hostname if the webhook `clientConfig` uses it. Write the
   certificate as `tls.crt` and the key as `tls.key` into the directory that
   will be mounted at `--webhook-cert-dir` (the lab-user-owned
   `%h/.local/state/k8slab/webhook-certs` of section 5.1).
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
