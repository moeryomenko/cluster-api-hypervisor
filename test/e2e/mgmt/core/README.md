# CAPI core controller manifests — v1.13.5 (pinned release series)

Self-contained manifests for the Cluster API core controller that the
management-plane bootstrap applies to the bare apiserver. All files were taken
verbatim from the official v1.13.5 release artifacts (no edits, no re-render):

- `metadata.yaml` — `metadata.yaml` from the v1.13.5 GitHub release
  (`https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.5/metadata.yaml`).
  Declares the release series with `major: 1, minor: 13, contract: v1beta2`.
- `crds/*.yaml` — the ten core CustomResourceDefinitions required by the
  management-plane contract, extracted from the `core-components.yaml` release
  artifact: the eight `cluster.x-k8s.io` kinds (Cluster, Machine, MachineSet,
  MachineDeployment, MachineHealthCheck, MachineDrainRule, MachinePool,
  ClusterClass) plus the two `addons.cluster.x-k8s.io` kinds
  (ClusterResourceSet, ClusterResourceSetBinding).
- `rbac.yaml` — the core controller's RBAC: the `capi-manager` ServiceAccount
  (namespace `capi-system`), the `capi-leader-election-role` Role and binding,
  the `capi-aggregated-manager-role` ClusterRole (the aggregation entry point
  provider ClusterRoles bind into), the `capi-manager-role` ClusterRole, and
  the `capi-manager-rolebinding` ClusterRoleBinding.
- `manager.yaml` — the `capi-system` Namespace, the `capi-webhook-service`
  Service, and the `capi-controller-manager` Deployment pinned to image tag
  `registry.k8s.io/cluster-api/cluster-api-controller:v1.13.5`.

## What is intentionally not shipped

The release `core-components.yaml` also contains artifacts this plane does not
use; they were deliberately excluded to keep the applied set minimal:

- The `runtime.cluster.x-k8s.io` and `ipam.cluster.x-k8s.io` CRDs
  (ExtensionConfig, IPAddressClaim, IPAddress). The bootstrap contract covers
  only the cluster/addons kinds; RuntimeSDK and IPAM providers are not part of
  this management plane.
- The `capi-webhook-service-cert` Secret. `manager.yaml` still mounts it into
  the Deployment (the `cert` volume with `secretName: capi-webhook-service-cert`
  at `manager.yaml:109-112`), but the Secret is not shipped because
  cert-manager is excluded (see below). The reference is inert on the bare
  plane: no kubelet exists to schedule the Deployment, so the mount never
  resolves and nothing fails.
- The cert-manager resources (the `capi-serving-cert` Certificate and the
  `capi-selfsigned-issuer` Issuer). The spec's non-objectives forbid
  cert-manager anywhere; webhook serving certificates are provisioned by the
  install contract instead.
- The MutatingWebhookConfiguration and ValidatingWebhookConfiguration. They
  reference the cert-manager-provisioned certificate Secret and would fail
  against a cert-manager-free plane. The core controller runs fine without
  them (webhook admission is not wired on the management plane); if webhook
  admission is later desired, the configs must be regenerated with an
  externally provisioned CA bundle.

## Regenerating

```sh
# Requires network access to github.com.
CAPI_VERSION=v1.13.5
curl -fsSL -o /tmp/core-components.yaml \
  "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CAPI_VERSION}/core-components.yaml"
curl -fsSL -o /tmp/metadata.yaml \
  "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CAPI_VERSION}/metadata.yaml"
# Split core-components.yaml into crds/, rbac.yaml, manager.yaml (see the
# document boundaries: 10 CRDs + 6 RBAC docs + Namespace/Service/Deployment),
# then copy metadata.yaml over core/metadata.yaml.
```
