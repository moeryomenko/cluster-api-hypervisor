# cluster-api-hypervisor — Install Contract

This document defines the production boundary for cluster-api-hypervisor. The
provider manager is a Kubernetes controller and webhook server. The HostAgent
is the separately deployed, authenticated host-lifecycle service. The manager
must never be given direct access to KVM, Cloud Hypervisor, k8netd, VM disks,
firmware, artifact directories, host state, or host lifecycle sockets.

The installation must preserve offline and immutable validation. It must not
introduce mutable firmware URLs, `latest` references, or firmware download
behavior.

## 1. Build the manager image

Build the local manager image before installing its Quadlet:

```sh
make image
```

The committed Quadlet uses `localhost/cluster-api-hypervisor:dev`. The image
contains the manager binary and the pinned runtime tools defined in
`Containerfile` and `VERSIONS.md`; only the HostAgent directly invokes host
lifecycle tools in production.

## 2. Responsibility boundary

| Component | Owns | Must not own |
|---|---|---|
| Manager | Kubernetes reconciliation, admission webhooks, management API access, authenticated HostAgent RPC requests | KVM, VM processes, Cloud Hypervisor APIs, k8netd control, IPAM, port publication, firmware files, disk artifacts, host state |
| HostAgent | Host networking, IPAM, port publication/release, artifact preparation and verification, Cloud Hypervisor lifecycle, VM teardown | Management-plane controller reconciliation or webhook serving |

The manager talks to the HostAgent exclusively through mutually authenticated
TLS. A reachable address alone is insufficient: the manager needs its client
certificate directory, the HostAgent CA, and the expected TLS server name.
Host lifecycle data remains on the host side of this boundary.

## 3. Manager configuration

The manager uses the usual Kubernetes and webhook flags plus these HostAgent
connection flags:

| Flag | Purpose |
|---|---|
| `--agent-address` | HostAgent gRPC endpoint |
| `--agent-server-name` | Expected HostAgent TLS server name |
| `--agent-client-cert-dir` | Read-only directory holding the manager mTLS client identity |
| `--agent-ca` | Read-only HostAgent trust-root file |

The Kubernetes deployment uses the in-cluster HostAgent service endpoint. The
user Quadlet uses its locally provisioned endpoint. Operators must select an
address and server name that match their HostAgent certificate. Do not put
private key values, certificates, or tokens in a Quadlet `Exec=` or
`Environment=` directive.

The manager also receives its management kubeconfig and webhook serving
certificate directory as read-only inputs. The webhook directory contains the
pre-provisioned `tls.crt` and `tls.key` expected by the controller manager.

### Firmware integrity input

`HYPERVISOR_FIRMWARE_SHA256` is an operator-supplied, no-default SHA-256
expectation for the selected firmware. Leaving it unset does not manufacture a
checksum from local bytes. The value must come from an independent,
authoritative integrity source selected by the operator; this repository does
not provide a firmware URL or a fallback digest.

The manager includes this expected checksum in the HostAgent VM request. The
HostAgent verifies the owned regular firmware file against it immediately
before Cloud Hypervisor `vm.create`. A missing, invalid, mismatched,
symlinked, or out-of-root firmware file prevents VM creation.

Other `HYPERVISOR_*` settings that identify host lifecycle paths or binaries
belong to the HostAgent deployment, not the manager Quadlet. In particular,
the manager must not configure a k8netd socket, Cloud Hypervisor socket path,
disk directory, host state directory, or KVM device.

## 4. User Quadlet

Install `deploy/cluster-api-hypervisor.container` with:

```sh
make install-quadlet
systemctl --user daemon-reload
systemctl --user start cluster-api-hypervisor.service
```

The unit runs only the provider manager. It has one `Exec=` directive because
Podman 6.x Quadlet treats repeated `Exec=` directives as last-wins. It depends
on the management-plane `capishim-pod.service`, not on `k8netd.service`.

The manager unit may mount only these read-only inputs:

| Host source | Container target | Consumer |
|---|---|---|
| Provisioned webhook certificate directory | `/tmp/k8s-webhook-server/serving-certs` | `--webhook-cert-dir` |
| Provisioned management kubeconfig | `/etc/kubernetes/mgmt/hypervisor.kubeconfig` | `--kubeconfig` |
| Provisioned manager mTLS identity directory | `/tls/agent-client` | `--agent-client-cert-dir` |
| Provisioned HostAgent CA file | `/tls/agent-ca/ca.crt` | `--agent-ca` |

For the committed user Quadlet, these sources are under the capishim state
root. The Agent client identity and CA are provisioned independently; the
Quadlet never creates them or exposes their contents.

Do not add `/dev/kvm`, `--privileged`, `NET_ADMIN`, relaxed seccomp, `/build`,
`/state`, `/tmp/ch-capi`, `/run/user/.../k8snet`, k8netd sockets, Cloud
Hypervisor sockets, artifact mounts, or `HYPERVISOR_*` environment variables
to this unit. Network host mode exists only so the local management apiserver
can reach the manager webhook and health endpoints; it does not grant
host-lifecycle ownership.

## 5. Kubernetes manager deployment

`config/manager/deployment.yaml` follows the same boundary. Its manager
container mounts only the webhook certificate, manager HostAgent mTLS client
identity, and HostAgent CA as read-only Kubernetes Secrets. It drops all Linux
capabilities, disallows privilege escalation, uses a read-only root filesystem,
and runs as a non-root user.

The Kubernetes deployment does not mount a host kubeconfig because it uses its
service account. It still requires correctly provisioned webhook and HostAgent
mTLS secrets before the manager can serve or reconcile successfully.

## 6. HostAgent deployment requirements

Deploy the HostAgent separately on the host(s) that own VM lifecycle. The
Agent, not the manager, receives the host-side mounts and permissions required
for KVM, k8netd, artifacts, VM state, and Cloud Hypervisor.

Give the Agent an explicit owned firmware root with `--firmware-root`. The
firmware supplied in a VM request must resolve to a regular file within that
root. Configure the expected firmware checksum through the manager's
independent integrity input described above; do not replace it with a digest
computed from the Agent's local firmware file.

## 7. Webhook certificates

Provision webhook serving material before starting the manager. The material
must include `tls.crt` and `tls.key`, and the management apiserver must trust
the corresponding CA through its webhook configuration. Keep private material
in the mounted credential source with restrictive access; never print it in
logs or copy it into configuration files.

When using `clusterctl`, generate and validate components deterministically
before applying them. Offline validation remains authoritative for immutable
artifacts; an installation must fail closed rather than substitute mutable or
unverified inputs.
