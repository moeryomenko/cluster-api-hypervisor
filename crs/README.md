# ClusterResourceSet delivery for cluster ops

`crs/` ships the Cluster API ClusterResourceSet (CRS) that delivers the
cluster-ops manifests — rbac, cilium (including its gateway-api CRDs and the
metrics-server manifest), coredns — to the workload cluster, plus the script
that turns manifest directories into the ConfigMaps the CRS references.

## Delivery contract

- The CRS (`clusterresourceset.yaml`) is `addons.cluster.x-k8s.io/v1beta1`,
  kind `ClusterResourceSet`, named `cluster-ops`.
- `spec.clusterSelector.matchLabels` pins `cluster.x-k8s.io/cluster-name` to
  the workload Cluster name. An empty selector matches no Cluster, so the
  placeholder value in the manifest must be replaced with the actual workload
  Cluster name before apply (or the selector is set programmatically on
  Cluster creation).
- `spec.strategy` is `ApplyOnce`: the manifests are applied exactly once after
  the workload cluster is provisioned. `Reconcile` is not part of the delivery
  contract.
- `spec.resources` references ConfigMaps by name; each resource kind is
  `ConfigMap` (the cluster-ops manifests are delivered as ConfigMaps, not
  Secrets). The CRS and its ConfigMaps must live in the same namespace.

## Generating the ConfigMaps

`populate.sh` reads a directory of manifest YAML files and writes a core `v1`
ConfigMap YAML stream to stdout. Each manifest file becomes one data entry
keyed by its file name; the ConfigMap name is derived from the file name
(extension dropped, lowercased, non-alphanumeric runs collapsed to dashes).

The source directory is the first positional argument, or the
`CRS_MANIFEST_DIR` environment variable:

```sh
crs/populate.sh <k8labs>/rbac
CRS_MANIFEST_DIR=<k8labs>/rbac crs/populate.sh
```

The CRS references the ConfigMaps produced for the k8labs manifest sets:

```sh
crs/populate.sh <k8labs>/rbac          | kubectl apply -f -   # rbac set
crs/populate.sh <k8labs>/cilium/install | kubectl apply -f -  # cilium CRDs + agent
crs/populate.sh <k8labs>/cilium        | kubectl apply -f -   # cilium L2 + metrics-server
crs/populate.sh <k8labs>/coredns       | kubectl apply -f -   # coredns
```

The stream contains nothing but ConfigMap documents, so it can be piped
straight into `kubectl apply`. Diagnostics go to stderr; stdout carries the
stream only.

## Apply ordering

The rbac manifests must be applied before the cilium manifests: the kubelet
requires the bootstrap RBAC before it can run the cilium agent pods, and the
remaining rbac entries (system-nodes, system-admin, kube-apiserver-proxy)
grant the cluster-level access the control plane and cilium rely on.

The ClusterResourceSet controller applies the referenced resources in the
order they appear in `spec.resources`, so the list in
`clusterresourceset.yaml` is dependency-ordered: the rbac set first, then the
cilium gateway-api CRDs and agent manifests, then metrics-server, then
coredns.

Two details complete the picture and are the mitigation for the CRS ordering
risk:

- Within one ConfigMap, the controller applies the data entries in sorted
  (alphabetical) key order, so intra-set ordering is encoded in the file
  names: the k8labs sets are prefix-numbered (`00-`, `01-`, ...) exactly so
  that alphabetical order equals apply order.
- Within a single YAML document, the controller sorts objects by kind creation
  priority (namespaces and CRDs first, then configmaps and service accounts,
  then workloads), so a document holding multiple kinds converges in the right
  order.

If per-set granularity is not needed, the alternative mitigation is a single
combined ConfigMap whose data is one ordered multi-document YAML stream: the
controller applies each data value as one YAML document in sorted key order,
so a single key (for example `00-cluster-ops.yaml`) keeps the whole set in one
document, sorted internally by kind priority.

The end-to-end verification gate checks the result: after the workload cluster
is provisioned, the cilium daemonset and the coredns deployment reach Ready
without host-side make targets.
