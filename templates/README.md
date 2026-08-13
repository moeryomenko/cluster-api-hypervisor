# Templates

This directory ships the topology templates for cluster-api-hypervisor:

- `clusterclass.yaml` — the ClusterClass (`cluster.x-k8s.io/v1beta1`) that
  wires together the four provider kinds of this repository:
  - infrastructure ref -> `HypervisorCluster`
  - control plane class -> `HypervisorControlPlane` with a
    `HypervisorMachineTemplate` for control-plane Machines
  - worker MachineDeployment class -> `HypervisorConfig` (bootstrap) +
    `HypervisorMachineTemplate` (infrastructure)
- `cluster-example.yaml` — a topology-driven example `Cluster` that consumes
  the ClusterClass with a single control-plane replica and a three-replica
  worker MachineDeployment (k8labs defaults: cp1 + w1..w3).

## Usage

Apply the ClusterClass first, then the example Cluster (optionally edit
`metadata.name`/`namespace` in `cluster-example.yaml` before applying):

```sh
kubectl apply -f templates/clusterclass.yaml
kubectl apply -f templates/cluster-example.yaml
```

Once the Cluster converges, inspect the resulting Machines:

```sh
kubectl get machines
kubectl get hypervisormachines
kubectl get hypervisorclusters
```

To scale workers, patch the topology's MachineDeployment replicas:

```sh
kubectl patch cluster k8labs --type=merge \
  -p '{"spec":{"topology":{"workers":{"machineDeployments":[{"name":"md-0","class":"default-worker","replicas":4}]}}}}'
```

Deleting the Cluster object tears down its Machines and infrastructure:

```sh
kubectl delete cluster k8labs
```
