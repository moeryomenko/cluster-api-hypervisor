# E2E suite runbook

The end-to-end suite for cluster-api-hypervisor verifies the full lab flow on a
single KVM host: a self-bootstrapped (or external) management plane, the
provider running as a podman user quadlet next to the k8netd network daemon, a
topology-driven workload Cluster created through the committed ClusterClass,
workload Machines that boot through k8netd vhost-user ports with per-VM passt
WAN instances, dataplane gates (API reachability via `https://127.0.0.1:6443`,
guest-to-guest and internet reachability from inside a guest), workload smoke
checks, and a cluster deletion that leaves the host clean while k8netd itself
stays up.

The suite is documentation-faithful: every environment variable, default, wait
budget, and step below is read from the scripts in this directory (`run.sh`,
`scale.sh`, `delete-cluster.sh`, `smoke.sh`, and `mgmt/`). Source lines are
listed for each claim so the runbook can be re-verified against the code.

## Scripts at a glance

| Script | Role |
|---|---|
| `test/e2e/run.sh` | Full-lab orchestration: lab-host guard + prerequisite gates, management plane up (or external), provider user quadlet, workload Cluster generated via clusterctl and applied, wait for workload Machines Ready, k8netd/passt dataplane gates, workload API gate via `https://127.0.0.1:6443`, guest reachability probes over SSH, workload smoke checks, teardown with host-cleanliness verification via a trap. |
| `test/e2e/smoke.sh` | Workload-cluster smoke checks (nodes, kube-system pods, Cilium, Gateway, CoreDNS, in-cluster DNS). Invoked by `run.sh`; also runnable standalone. |
| `test/e2e/scale.sh` | Worker scale scenario against a live lab: bump replicas, new Machine boots and the node registers, then delete the Machine and wait for the count to drop. |
| `test/e2e/delete-cluster.sh` | Cluster-deletion scenario against a live lab: delete the Cluster object, wait for Machine teardown, stop a self-bootstrapped management plane, verify the host network state is gone. |
| `test/e2e/mgmt/` | Self-bootstrapped management plane: `pki.sh` (PKI + kubeconfigs), `apply.sh` (core manifests, clusterctl config render, offline core override, `clusterctl init`, webhook caBundle patch, quadlets), `down.sh` (stop the plane), `units/`, `core/`. |

## LAB-HOST ONLY — never run casually

`run.sh` drives real KVM virtual machines, a real rootless network daemon, and
real passt processes: it boots 4 VMs, binds host ports 6443 and 22, and writes
daemon state under `/run/user/1000/k8snet/`. The script refuses to run unless
`E2E_LAB_HOST=1` is exported (`run.sh:147-149`, `run.sh:297-301`) — there is no
other bypass, and `--help` is the only exempt invocation. Never run it on a
workstation, in CI, or against a shared management plane.

## 1. Prerequisites

All scenarios run on one Linux host; nothing is containerized away from the
host's kernel, so the host must provide the virtualization and container
primitives the provider shells out to. `run.sh` fails fast naming the failed
prerequisite before any heavy work: no cluster, VM, or quadlet is started when
a gate fails (`check_prerequisites`, `run.sh:424-512`).

1. **The dedicated k8labs host, explicitly confirmed.** Export `E2E_LAB_HOST=1`
   or the script exits 1 immediately (`run.sh:297-301`).

2. **KVM-capable host with group access.** `/dev/kvm` must be writable by the
   invoking user and the user must be in the `kvm` group (P1,
   `run.sh:434-437`). The provider runs cloud-hypervisor subprocesses inside
   its container; the host side of that contract is the `/dev/kvm` device. A
   systemd host is required (user quadlets).

3. **passt installed** and runnable (`passt --version`, P2, `run.sh:439-441`).
   Each VM gets its own passt WAN instance spawned by k8netd (contract REQ-008
   of `.specs/k8netd-contract/spec.md`); the control-plane instance forwards
   host port 6443 to the control-plane VM's API server and host port 22 to its
   SSH port (`run.sh:142-145`).

4. **k8netd binary present and daemon running.** `k8netd` must be on PATH (P3,
   `run.sh:443-444`) and the `k8netd.service` **user** quadlet must be active
   (P4, `run.sh:446-449`). The control socket
   `/run/user/1000/k8snet/control.sock` must answer a JSON-RPC probe with the
   contract version accepted (P5, `run.sh:451-472`; the probe is compiled from
   an embedded stdlib-only Go program, `build_k8netd_probe`,
   `run.sh:516-616`).

5. **podman unprivileged with user quadlets usable** (P6, `run.sh:474-477`).
   Both `k8netd.service` and the provider unit (`mgmt-cluster-api-hypervisor`)
   are user units; every systemd gate in `run.sh` uses `systemctl --user`.

6. **The provider image.** Build it before the lab run so the provider quadlet
   can start:

   ```sh
   make image
   ```

   This builds the default tag `cluster-api-hypervisor:dev` (`Makefile:5`,
   `Makefile:108-109`). The image is local-only (never published); a quadlet
   references it as `localhost/cluster-api-hypervisor:dev`. The prerequisite
   gate accepts either reference (P8, `run.sh:479-481`).

7. **The k8labs base image and firmware.** The provider boots workload VMs from
   `build/k8labs-base.qcow2` with the `build/CLOUDHV.fd` firmware.
   `run.sh` requires both as existing, readable, regular files
   (`run.sh:332-339`, `run.sh:341-348`); the relative defaults resolve against
   the working directory the harness is invoked from (`run.sh:53-61`,
   doc block `run.sh:24-89`). Bake the base image with the k8labs image-baking
   pipeline so both artifacts exist before the first `run.sh` invocation, or
   point `BASE_IMAGE`/`FIRMWARE` at your copies.

8. **cloud-hypervisor.** The binary is bundled inside the provider image at a
   pinned version (`Containerfile:40-41`, `docs/install-contract.md:50`), and
   the provider shells out to `cloud-hypervisor` from inside the container
   (`docs/install-contract.md:110`). No separate host install is required; the
   host side is the `/dev/kvm` device.

9. **Free host ports.** Host port 6443 must be completely free and port 22 may
   be held only by the host sshd (P11, `run.sh:484-497`): the control-plane
   passt binds both for inbound forwards, and a stale listener or second
   cluster breaks the run.

10. **A clean management plane.** No stale workload Cluster `k8labs` may exist
    on the management plane (P12, `run.sh:499-508`).

11. **CLI tools.** `kubectl` is required by every scenario script
    (    `run.sh:1114`, `scale.sh:77-78`, `delete-cluster.sh:80-81`,
    `smoke.sh:35-36`). `run.sh` also needs `go`, `base64`, `mktemp`, `ss`,
    `ssh`, and a process-enumeration tool for the passt/cloud-hypervisor
    process-count gates (`run.sh:1113-1119`); `scale.sh` needs `base64` and
    `mktemp` (`scale.sh:79-82`). The mgmt bootstrap needs `openssl` for
    `pki.sh` and, optionally, `kubectl` to render kubeconfigs
    (`test/e2e/mgmt/pki.sh:161-184`).

12. **A guest SSH key.** The guest reachability probes run inside the
    control-plane guest over SSH through the passt-forwarded host port 22, so
    a private key must exist locally whose public part is provisioned into the
    guests (HypervisorConfig `SSHPublicKey`). Default `~/.ssh/id_k8labs`,
    override with `GUEST_SSH_KEY` (`run.sh:385-393`).

## 2. Environment variables

Every variable below is read from the scenario scripts; the default column is
the value the script applies when the variable is unset, and the source column
lists the script lines that define or apply it.

### `run.sh` contract

| Variable | Default | Meaning | Source |
|---|---|---|---|
| `E2E_LAB_HOST` | unset -> refuse | Lab-host-only guard. Must be exported as `1` or the script exits 1 before doing anything; `--help` is exempt. | constants `run.sh:147-149`; enforced `run.sh:297-301` |
| `SKIP_PREREQS` | unset -> gates enforced | Test-only escape hatch used by `harness_test.sh`. When exported as `1`, the lab-host prerequisite gates (P1-P12) are skipped after environment validation; the environment contract itself is still enforced in full. Exists because gate P1 checks `/dev/kvm` directly and cannot be satisfied by PATH stubs on a non-lab host. Never set this on a real lab run. | doc `run.sh:28-36`; gate skip `run.sh:426-433` |
| `MANAGEMENT_KUBECONFIG` | unset -> mgmt-bootstrap fallback | Management-cluster kubeconfig. Set: must name an existing, readable, non-empty file; the plane is treated as external and is not torn down on exit. Unset/empty: the harness falls back to the committed bootstrap (`test/e2e/mgmt`) driven by `MGMT_STATE_DIR`; the state must be provisioned with the admin kubeconfig at `<state>/kubeconfigs/admin.conf`. | doc `run.sh:26-47`; validation `run.sh:306-323`; fallback `run.sh:314-323` |
| `MGMT_STATE_DIR` | `/var/lib/k8slab/mgmt` | Management-plane state directory for the fallback bootstrap (created by `test/e2e/mgmt/pki.sh`). | default `run.sh:113`; applied `run.sh:315`; also `test/e2e/mgmt/apply.sh:116` |
| `IMAGE` | `cluster-api-hypervisor:dev` | Provider image reference (the Makefile tag). A set value must be a syntactically plausible container reference (no whitespace). | default `run.sh:110`; applied/checked `run.sh:326-330` |
| `BASE_IMAGE` | `build/k8labs-base.qcow2` | k8labs base image path, resolved against the invocation working directory. Must be an existing, readable, regular file. | default `run.sh:111`; applied `run.sh:333`; checks `run.sh:334-339` |
| `FIRMWARE` | `build/CLOUDHV.fd` | CLOUDHV.fd path, resolved against the invocation working directory. Must be an existing, readable, regular file. | default `run.sh:112`; applied `run.sh:342`; checks `run.sh:343-348` |
| `STATE_DIR` | `~/.local/state/k8slab` (`/tmp/k8slab-state` without HOME) | Provider state directory, mirroring the provider's user-writable default. Must be an existing, writable directory (not a regular file). | default `run.sh:116-120`; applied `run.sh:351`; checks `run.sh:352-361` |
| `OUT_DIR` | `<repo>/out` | Provider release layout directory. Must be an existing directory containing the three provider release directories `infrastructure-hypervisor/v0.1.0`, `bootstrap-hypervisor/v0.1.0`, and `control-plane-hypervisor/v0.1.0` (the layout `make components` emits), so `go tool clusterctl generate cluster` can resolve the cluster template from the local repository. | default `run.sh:124`; applied `run.sh:365`; checks `run.sh:366-377` |
| `K8NETD_SOCKET` | `/run/user/1000/k8snet/control.sock` | k8netd JSON-RPC control socket (the provider default `HYPERVISOR_K8NETD_SOCKET`). Must be an absolute path; liveness is checked by prerequisite P5. | default `run.sh:136`; applied/checked `run.sh:378-383` |
| `GUEST_SSH_KEY` | `~/.ssh/id_k8labs` | SSH private key for the guest probes; its public part must be provisioned into the guests. Must be an existing, readable, regular file. | applied/checked `run.sh:385-393` |
| `GUEST_SSH_USER` | `root` | SSH user for the guest probes. Must be non-empty. | applied/checked `run.sh:395-398` |
| `SMOKE` | `1` | Run the workload smoke checks (`smoke.sh`) after the dataplane gates pass. `0` disables them; any other value enables them. When `smoke.sh` is absent the checks are skipped with a note. | applied `run.sh:1107`; disabled check `run.sh:977-981`; absent check `run.sh:982-986` |
| `WAIT_TIMEOUT` | `1800` | Seconds to wait for the workload Machines to become Ready. | default `run.sh:158`; applied `run.sh:1108`; used `run.sh:757` |

Validation is all-or-nothing before any heavy work: no cluster, VM, or quadlet
is started unless every variable above is valid (`run.sh:20-22`,
`run.sh:305-402`), and the lab-host prerequisite gates run right after
(`run.sh:424-512`).

### `scale.sh` contract

| Variable | Default | Meaning | Source |
|---|---|---|---|
| `KUBECONFIG` | required, no default | Management-cluster kubeconfig; overridden by the first positional argument. | required `scale.sh:48`; override `scale.sh:45-47` |
| `REPLICAS` | required, no default | Target worker Machine count; overridden by the second positional argument. Must be a non-negative integer. | required `scale.sh:55`; override `scale.sh:52-54`; type check `scale.sh:56-59` |
| `CLUSTER_NAME` | `k8labs` | Workload Cluster name. | `scale.sh:61` |
| `CLUSTER_NAMESPACE` | `default` | Workload Cluster namespace. | `scale.sh:62` |
| `SCALE_WAIT_TIMEOUT` | `1800` | Per-step wait budget in seconds; must be a non-negative integer. | `scale.sh:63`; type check `scale.sh:64-67` |

### `delete-cluster.sh` contract

| Variable | Default | Meaning | Source |
|---|---|---|---|
| `KUBECONFIG` | required, no default | Management-cluster kubeconfig; overridden by the first positional argument. | required `delete-cluster.sh:61`; override `delete-cluster.sh:58-60` |
| `CLUSTER_NAME` | `k8labs` | Workload Cluster name. | `delete-cluster.sh:69` |
| `CLUSTER_NAMESPACE` | `default` | Workload Cluster namespace; overridden by the second positional argument. | `delete-cluster.sh:70`; override `delete-cluster.sh:66-68` |
| `MANAGEMENT_KUBECONFIG` | unset -> self-bootstrapped plane | When set and non-empty, the plane is external and the mgmt-down step is skipped. Unset/empty: the mgmt-down step runs. | semantics `delete-cluster.sh:33-36`; decision `delete-cluster.sh:139-149` |
| `MGMT_DOWN_SH` | `<script-dir>/mgmt/down.sh` | mgmt-down script used by the self-bootstrapped plane path. | `delete-cluster.sh:72`; used `delete-cluster.sh:142-148` |
| `DELETE_WAIT_TIMEOUT` | `1800` | Per-step wait budget in seconds; must be a non-negative integer. | `delete-cluster.sh:71`; type check `delete-cluster.sh:73-76` |

### `smoke.sh` contract

| Variable | Default | Meaning | Source |
|---|---|---|---|
| `KUBECONFIG` | required, no default | Workload-cluster kubeconfig; overridden by the first positional argument. | required `smoke.sh:32`; override `smoke.sh:29-31` |

Fixed identities shared by the scripts: the workload Cluster is `k8labs` in
namespace `default` (`run.sh:153-154`), the expected topology is 1 control
plane + 3 workers = 4 Machines (`run.sh:155`), the passt inbound forwards bind
host ports 6443 and 22 (`run.sh:144-145`), the worker MachineDeployment is
`md-0` (`scale.sh:74`), and the management endpoint is
`https://127.0.0.1:6443` (`test/e2e/mgmt/pki.sh:45`).

## 3. Run order

The suite is three phases. Phase 1 is self-contained: `run.sh` brings the lab
up, verifies it, tears it down, and verifies the host converged. Phases 2 and
3 are scenario scripts that operate on a live lab (management plane up,
Cluster applied, workload Machines Ready) and are documented after the
teardown note that explains how the lab stays alive for them.

### Phase 1 — full lab (`run.sh`)

```sh
E2E_LAB_HOST=1 bash test/e2e/run.sh
```

Invoke from the repository root so the relative `build/` defaults resolve, or
override `BASE_IMAGE`/`FIRMWARE` with absolute paths. Without
`E2E_LAB_HOST=1` the script exits 1 before touching anything
(`run.sh:297-301`). The orchestration is (`orchestrate`, `run.sh:1106-1138`):

1. **Prerequisites** (`run.sh:1122`, `check_prerequisites` at
   `run.sh:424-512`): the fail-fast gates P1-P12 from the prerequisites
   section above, including the live JSON-RPC probe of the k8netd control
   socket (P5) and the free-host-port check (P11). Nothing has been started
   yet when these run.
2. **mgmt up** (`run.sh:1123`, `mgmt_up` at `run.sh:630-642`). With
   `MANAGEMENT_KUBECONFIG` set, the external plane is used as-is. Otherwise the
   committed bootstrap runs (`test/e2e/mgmt/apply.sh`): it applies the CAPI
   core manifests from `test/e2e/mgmt/core/` (`apply.sh:143-147`), renders the
   clusterctl configuration from the committed `clusterctl.yaml` template into
   the state directory (`apply.sh:149-166`), assembles the offline core-CAPI
   override from the committed core manifests (`apply.sh:168-187`), initializes
   the Cluster API providers with `go tool clusterctl init` against the
   rendered config (`apply.sh:197-205`), patches the management CA into the
   admission webhook configurations (`apply.sh:207-228`), installs the quadlet
   units from `test/e2e/mgmt/units/` as `mgmt-etcd`,
   `mgmt-kube-apiserver`, `mgmt-cluster-api-core`, and
   `mgmt-cluster-api-hypervisor` services (`apply.sh:230-243`), then starts
   them (`apply.sh:245-258`).
3. **Management apiserver ready** (`run.sh:1124`,
   `wait_for_apiserver_ready` at `run.sh:644-658`).
4. **Provider connected to k8netd** (`run.sh:1125-1126`): wait for the
   provider user quadlet `mgmt-cluster-api-hypervisor` to be active
   (`wait_for_provider` at `run.sh:660-674`), then assert its journal shows no
   persistent k8netd connection-retry errors once active
   (`check_provider_journal` at `run.sh:676-690`); the provider client's
   connection backoff absorbs the start-order race between the two user
   quadlets.
5. **Cluster via clusterctl** — generate the workload Cluster with
   `go tool clusterctl generate cluster` and apply it to the management
   cluster (`run.sh:1127`, `apply_templates` at `run.sh:693-713`): the pinned
   flags are `--namespace default --infrastructure hypervisor
   --kubernetes-version v1.32.13 --control-plane-machine-count 1
   --worker-machine-count 3`, piped into `kubectl apply --kubeconfig=<admin>
   -f -`; the rendered manifest comes from the cluster template shipped in the
   provider release tree (`templates/cluster-template.yaml`).
6. **Network created through real k8netd** (`run.sh:1128-1129`): wait for the
   HypervisorCluster to report InfrastructureReady=True, which only happens
   after CreateNetwork succeeded (`wait_for_infrastructure_ready` at
   `run.sh:715-735`), then call GetNetwork over the control socket and require
   the CIDR/gateway back (`verify_network_created` at `run.sh:737-753`).
7. **Machines Ready** — poll until every workload Machine reports the CAPI
   `Ready` condition `True` (`run.sh:1130`, `wait_for_machines_ready` at
   `run.sh:755-783`).
8. **Dataplane gates** (`run.sh:1131-1133`): extract the workload kubeconfig
   from the `k8labs-kubeconfig` Secret (`extract_workload_kubeconfig` at
   `run.sh:863-877`), inventory the Machines and their HypervisorMachine
   infrastructure refs with their reserved internal IPs (`collect_machines`
   at `run.sh:785-826`), then verify (`verify_dataplane` at
   `run.sh:828-861`): a k8netd port socket exists per machine under
   `/run/user/1000/k8snet/<machine>.sock`, exactly one passt process runs per
   attached port (per-VM passt, never a shared instance), and every Node
   registered with the reserved internal IP of its machine (DHCP honored the
   AllocateIP reservation).
9. **Workload API via https://127.0.0.1:6443** (`run.sh:1134`,
   `verify_workload_api` at `run.sh:880-912`): the kubeconfig server URL must
   be exactly `https://127.0.0.1:6443`, `/readyz` must answer `ok` through the
   passt forward (TLS SAN includes 127.0.0.1), and all 4 nodes must be Ready.
10. **Guest reachability** (`run.sh:1135`, `verify_guest_reachability` at
    `run.sh:946-973`): probed from inside the control-plane guest over SSH
    through the passt-forwarded host port 22 (`ssh_guest` at `run.sh:914-924`;
    the host is not on the cluster L2 segment, so VM-to-VM and internet
    reachability can only be observed from inside a guest — this is the
    documented probe mechanism, chosen over the `kubectl exec` fallback):
    ping to every worker internal IP, ping to the gateway, DNS resolution of
    a public name through the gateway resolver, and HTTPS egress through the
    per-VM passt WAN.
11. **Smoke** — run the workload smoke checks (`run.sh:1136`, `run_smoke` at
    `run.sh:976-993`), unless `SMOKE=0`.
12. **Teardown with verification** (`run.sh:1137`, `teardown_and_verify` at
    `run.sh:1002-1076`): delete the workload Cluster, wait until no Machine
    remains, then prove the host converged — no cloud-hypervisor process, no
    port sockets, GetNetwork answers `not_found`, no passt process, host port
    6443 released, and `k8netd.service` still active (cluster teardown must
    not stop the independent daemon). Finally remove the temp kubeconfig and
    bring the management plane down when the harness started it.

`run.sh` requires `go`, `kubectl`, `base64`, `mktemp`, `ss`, `ssh`, and a
process-enumeration tool for the process-count gates on PATH
(`run.sh:1113-1119`). Exit codes: `0` full-lab run completed
(including teardown) or `--help`; `1` lab-host guard refusal, environment
validation failure, prerequisite failure (all before any heavy work), or
orchestration failure (`run.sh:91-94`). `test/e2e/run.sh --help` prints the
full contract without touching anything.

### Teardown semantics (read before Phase 2/3)

Phase 1 now includes its own verified teardown: by the time `run.sh` exits 0,
the workload Cluster, its VMs, port sockets, and passt processes are gone and
host port 6443 is released (`run.sh:1002-1076`). With an external management
plane the Cluster is still deleted; only the plane itself is left running
(`run.sh:1062-1073`). So Phases 2 and 3 run against a lab kept up for the
scenario, not against the leftovers of a completed `run.sh` session. If the
run fails before or during the deliberate teardown, the EXIT trap
(`cleanup` at `run.sh:1079-1103`) best-effort deletes the Cluster, removes the
temp kubeconfig, and brings a self-bootstrapped plane down — without changing
the exit code.

### Phase 2 — worker scale (`scale.sh`)

```sh
KUBECONFIG=/path/to/mgmt/admin.conf REPLICAS=4 bash test/e2e/scale.sh
# or: bash test/e2e/scale.sh /path/to/mgmt/admin.conf 4
```

The management kubeconfig comes from `KUBECONFIG` or `$1` (a positional
argument wins); the target worker count from `REPLICAS` or `$2`
(`scale.sh:45-59`). The steps are (`scale.sh:121-215`):

1. **scale-up** — patch the Cluster topology worker `machineDeployments`
   replicas to the target (`scale.sh:130-134`; the merge-patch shape matches
   the documented recipe in `templates/README.md`), then wait until the
   worker-labeled Machine count reaches the target (`scale.sh:136-139`).
   Workers are selected by `cluster.x-k8s.io/deployment-name=md-0`
   (`scale.sh:74`), which separates them from the control-plane Machine.
2. **vm-boot** — identify the new Machine (the first worker not in the
   pre-bump baseline, `scale.sh:143-154`) and wait for its linked
   `HypervisorMachine` to report `status.ready=true` with
   `status.addresses` populated (`scale.sh:159-175`).
3. **node-ready** — extract the workload kubeconfig from the
   `<cluster>-kubeconfig` Secret (`scale.sh:181-187`) and wait until
   `1 + REPLICAS` nodes (control plane + workers) are Ready
   (`scale.sh:189-200`).
4. **delete** — delete the new Machine and wait until the worker Machine count
   drops below the target (`scale.sh:206-215`). Host-level VM/disk cleanup
   after the delete is verified by the lab, not by this script
   (`scale.sh:7-9`).

Every wait shares the `SCALE_WAIT_TIMEOUT` budget; on expiry the script exits
non-zero naming the step that timed out: `scale-up`, `node-ready`, or `delete`
(`scale.sh:17-19`, `scale.sh:88-99`). Requires `kubectl`, `base64`, `mktemp`
(`scale.sh:77-82`). Exit codes: `0` scenario converged; `1` timeout or failure
(`scale.sh:32-34`).

### Phase 3 — cluster deletion (`delete-cluster.sh`)

```sh
KUBECONFIG=/path/to/mgmt/admin.conf bash test/e2e/delete-cluster.sh
# or: bash test/e2e/delete-cluster.sh /path/to/mgmt/admin.conf
```

The management kubeconfig comes from `KUBECONFIG` or `$1`; the namespace from
`CLUSTER_NAMESPACE` or `$2` (`delete-cluster.sh:58-70`). The steps are
(`delete-cluster.sh:117-152`):

1. **cluster-delete** — delete the workload Cluster object and the workload
   namespace, then poll until the Cluster object is gone
   (`delete-cluster.sh:121-127`). CAPI controllers tear down the Machines and,
   through them, the VMs and disks.
2. **machine-teardown** — poll until no workload Machine remains
   (`delete-cluster.sh:129-134`).
3. **mgmt-down** — stop the management plane only when it was
   self-bootstrapped (`MANAGEMENT_KUBECONFIG` unset/empty); an external plane
   keeps running and the step is skipped (`delete-cluster.sh:136-149`). The
   down script resolves through `MGMT_DOWN_SH` (default
   `<script-dir>/mgmt/down.sh`).

Every wait shares the `DELETE_WAIT_TIMEOUT` budget; on expiry the script exits
non-zero naming the step that timed out: `cluster-delete` or
`machine-teardown` (`delete-cluster.sh:20-22`, `delete-cluster.sh:87-98`).
Requires `kubectl` (`delete-cluster.sh:80-81`). Exit codes: `0` scenario
converged; `1` timeout or failure (`delete-cluster.sh:41-44`). The script
performs no host network operation: after the k8netd migration the provider
tears the cluster network down through the k8netd control socket, so no host
network tooling is needed or invoked.

### One-session suite recipe

Run the whole suite against a lab kept alive for the scenario phases
(management state provisioned with `pki.sh`, plane started with `apply.sh`):

```sh
# Phase 1: full-lab run (bring-up, gates, smoke, verified teardown) — self-contained.
E2E_LAB_HOST=1 bash test/e2e/run.sh

# Keep a lab alive for the scenario phases:
bash test/e2e/mgmt/pki.sh /var/lib/k8slab/mgmt
MGMT_STATE_DIR=/var/lib/k8slab/mgmt bash test/e2e/mgmt/apply.sh
KC=/var/lib/k8slab/mgmt/kubeconfigs/admin.conf   # the fallback admin kubeconfig (run.sh:316)
XDG_CONFIG_HOME=/var/lib/k8slab/mgmt/clusterctl \
  go tool clusterctl generate cluster k8labs --namespace default \
  --infrastructure hypervisor --kubernetes-version v1.32.13 \
  --control-plane-machine-count 1 --worker-machine-count 3 \
  | kubectl --kubeconfig="$KC" apply -f -
# wait for the workload Machines to be Ready (the same wait run.sh performs)

# Phase 2: worker scale.
KUBECONFIG="$KC" REPLICAS=4 bash test/e2e/scale.sh

# Phase 3: cluster deletion (mgmt-down included because the plane was
# self-bootstrapped here).
KUBECONFIG="$KC" bash test/e2e/delete-cluster.sh
```

`apply.sh` is idempotent (`apply.sh:27-31`), so re-running it converges;
clusterctl generate + kubectl apply is declarative (`run.sh:686-692`). After
Phase 3 the lab is down; the deletion scenario itself touches no host network
state (the provider tears the cluster network down through k8netd).

## 4. Expected timings

Every wait below is a budget, not a delay: the scripts poll until the
condition converges or the budget expires and the run fails naming the step.
On a cold lab the machine-ready wait dominates the wall clock.

| Phase | Wait | Budget | Poll interval | Source |
|---|---|---|---|---|
| run.sh | management apiserver `/readyz` | 300 s | 2 s | `run.sh:157`, `run.sh:644-658` |
| run.sh | provider user quadlet `mgmt-cluster-api-hypervisor` active | 300 s | 2 s | `run.sh:660-674` |
| run.sh | HypervisorCluster InfrastructureReady=True | 300 s | 5 s | `run.sh:715-735` |
| run.sh | workload Machines Ready | `WAIT_TIMEOUT`, default 1800 s | 5 s | `run.sh:158`, `run.sh:160`, `run.sh:755-783` |
| run.sh | SSH into the control-plane guest | 120 s | 5 s | `run.sh:161`, `run.sh:926-944` |
| run.sh | teardown: workload Cluster deletion + Machine drain | 300 s | 5 s | `run.sh:159`, `run.sh:1004-1022` |
| scale.sh | scale-up, vm-boot, node-ready, delete | `SCALE_WAIT_TIMEOUT`, default 1800 s per step | 2 s | `scale.sh:63`, `scale.sh:75`, `scale.sh:88-99` |
| delete-cluster.sh | cluster-delete, machine-teardown | `DELETE_WAIT_TIMEOUT`, default 1800 s per step | 1 s | `delete-cluster.sh:71`, `delete-cluster.sh:78`, `delete-cluster.sh:87-98` |
| smoke.sh | coredns rollout status | 60 s | — | `smoke.sh:120` |
| smoke.sh | DNS probe pods reach Running | up to 30 x 2 s | 2 s | `smoke.sh:140-148` |

For a healthy run, expect the phases to take well under the budgets — the
budgets exist to fail fast with a named step when something does not converge.

## 5. Re-running against an existing management plane

`run.sh` consumes an external plane when `MANAGEMENT_KUBECONFIG` names an
existing, readable, non-empty kubeconfig (`run.sh:306-313`); the mgmt
bootstrap is then skipped entirely (`run.sh:630-635`) and the plane is left
running on exit (`run.sh:1062-1073`). This is the supported way to re-run the
full-lab flow against a plane you already operate:

```sh
export E2E_LAB_HOST=1
export MANAGEMENT_KUBECONFIG=/path/to/existing/admin.conf
bash test/e2e/run.sh
```

With an external plane, `run.sh` still generates and applies the workload
Cluster via clusterctl, waits for Ready, runs the dataplane and guest gates,
runs smoke, and still deletes the workload Cluster on exit — only the plane
itself survives (`run.sh:1004-1076`). If the external plane lacks the Cluster
API core CRDs, the initialized hypervisor providers (clusterctl init), or the
k8netd control socket the scenarios expect, `run.sh` will fail at the
prerequisite, apply, or wait step; the committed bootstrap (`test/e2e/mgmt`)
is the reference environment the suite is verified against.

`scale.sh` and `delete-cluster.sh` always take the management kubeconfig
explicitly (`KUBECONFIG`/`$1`) and do not care how the plane came up. For the
deletion scenario, `MANAGEMENT_KUBECONFIG` set and non-empty skips the
mgmt-down step (`delete-cluster.sh:139-141`) — the right choice when the plane
is external.

## 6. Contract tests (no live cluster required)

Each scenario script has a contract test that runs without a live cluster.
The scenario scripts are executed against stub tooling on `PATH` (or, for the
harness, against a controlled environment; for the k8netd stub test, against a
compiled in-process responder) and the tests assert the scripts' decisions and
aggregate exit codes. All seven are plain scripts with no arguments:

```sh
test/e2e/mgmt/mgmt_test.sh     # mgmt bootstrap contract
test/e2e/harness_test.sh       # run.sh environment contract
test/e2e/smoke_test.sh         # smoke.sh per-check contract
test/e2e/scale_test.sh         # scale.sh per-step contract
test/e2e/delete_test.sh        # delete-cluster.sh per-step contract
test/e2e/k8netd_stub_test.sh   # k8netd stub socket + no-host-tool contract
test/e2e/clusterctl_test.sh    # make components release-layout contract
```

| Test | What it pins | How it runs without a cluster |
|---|---|---|
| `test/e2e/mgmt/mgmt_test.sh` | `pki.sh` state layout, quadlet units, core manifests, `apply.sh`/`down.sh` lifecycle, and the clusterctl rewire contract (config render, offline core override, `go tool clusterctl init` invocation, webhook caBundle patch) | Executes `pki.sh` against a scratch state directory; every other assertion checks the committed files directly, including `test_apply_rewire` against `apply.sh` and the committed `clusterctl.yaml` template (`mgmt_test.sh:48-50`, `mgmt_test.sh:483-533`). |
| `test/e2e/harness_test.sh` | `run.sh` validates every contract variable (including `OUT_DIR`) before heavy work, its errors name the exact variable, and `apply_templates` pipes `go tool clusterctl generate cluster` into `kubectl apply -f -` | Runs `run.sh` with `env -i` (only `PATH`/`HOME` plus explicit assignments) and a 30 s timeout, feeding fake-but-valid fixture files; every scenario keeps all variables valid except the one under test (`harness_test.sh:150-166`, `harness_test.sh:189-217`, `harness_test.sh:224-236`). Every invocation exports the lab-host guard like a real operator (`E2E_LAB_HOST=1`) plus the documented test-only prerequisite skip (`SKIP_PREREQS=1`, gate P1 checks `/dev/kvm` directly and cannot be PATH-stubbed), and a fixture `GUEST_SSH_KEY`. The apply flow drives the full orchestrate against stubbed tooling on `PATH`: `go` (also emits a fake k8netd probe for the `go build` dispatch), `kubectl` (readyz, apply stdin capture, fixed 4-machine inventory, delete flips the stub lab to torn-down), plus process-listener, socket-listener, guest-SSH, systemd-user-unit, and journal stubs, and real unix-socket inodes for the per-machine port sockets (`harness_test.sh:627-860`). Never starts a cluster, VM, or quadlet (`harness_test.sh:74-80`). |
| `test/e2e/smoke_test.sh` | Per-check pass/fail semantics of `smoke.sh` (nodes, kube-system, Cilium, Gateway, CoreDNS, DNS regressions) plus the aggregate exit code | Runs `smoke.sh` against a stub `kubectl` on `PATH` that dispatches on its arguments and returns scripted canned outputs; each run is bounded by a 60 s timeout (`smoke_test.sh:7-12`, `smoke_test.sh:63`). |
| `test/e2e/scale_test.sh` | Per-step contract of `scale.sh` (scale-up, VM boot, node-ready, delete, timeout naming) | Runs `scale.sh` against a stub `kubectl` modeling a timeline (pre-bump baseline, post-bump set, post-delete set) through `STUB_*` variables; success scenarios use a 10 s wait budget, timeout scenarios 3 s (`scale_test.sh:6-11`, `scale_test.sh:73-80`). |
| `test/e2e/delete_test.sh` | Per-step contract of `delete-cluster.sh` (cluster-delete, machine-teardown, mgmt-down) plus the no-host-tool guarantee: sentinel host binaries on `PATH` whose invocation log must stay empty | Runs `delete-cluster.sh` against a stub `kubectl` plus sentinel host binaries that record every invocation, and a stub mgmt-down injected through `MGMT_DOWN_SH` (`delete_test.sh:9-15`). |
| `test/e2e/k8netd_stub_test.sh` | The stubbed k8netd control socket (JSON-RPC 2.0 envelope, typed error codes, the ten-method inventory wired in `internal/k8netd/client.go`) and the no-host-tool contract of the scenario scripts and this README | Compiles an embedded standard-library-only Go responder/probe pair into a scratch directory (`GOPROXY=off`) and drives round-trips over a real Unix socket; the host-tool assertions are static greps over the scripts and this file (`k8netd_stub_test.sh:124-321`, `k8netd_stub_test.sh:485-512`). |
| `test/e2e/clusterctl_test.sh` | `make components` release layout: the three provider directories, byte-identical components files, the eleven-object inventory, url-rewritten webhook clientConfigs, and the provider labels | Runs `make components OUT_DIR=<scratch>` from the repo root and asserts the emitted files directly; never starts a cluster, VM, or quadlet (`clusterctl_test.sh:50-52`, `clusterctl_test.sh:368-377`). |

Exit codes are uniform across the contract tests: `0` the script satisfies its
contract, `1` contract violation (including the script under test being
absent), `2` prerequisite problem (missing tool, unexpected arguments). The
contract tests ship under `test/e2e/` and run directly as plain scripts
(`bash test/e2e/<name>_test.sh`, no arguments); they are not wired into the
Makefile or CI. They require only the tools the scripts themselves require
(`kubectl` for the harness and stub-based tests, `openssl` for the mgmt test,
`go` for the k8netd stub test, `make`/`grep`/`awk`/`cmp` for the clusterctl
test).

## 7. Troubleshooting pointers

- **`run.sh` exits 1 immediately**: either `E2E_LAB_HOST=1` is missing
  (`run.sh:297-301`) or an environment variable is invalid — the error names
  the offending variable and path (`run.sh:305-402`); check the env table
  above against the invocation.
- **`run.sh` fails at a prerequisite**: the error names the failed gate P1-P12
  (`run.sh:424-512`). Common causes: the user is not in the `kvm` group (P1),
  passt or k8netd missing from PATH (P2/P3), `k8netd.service` not started
  (P4, `systemctl --user start k8netd.service`), a stale control socket that
  does not answer the JSON-RPC probe (P5), something already bound on host
  port 6443 (P11), or a leftover workload Cluster on the management plane
  (P12).
- **A wait times out**: the error line names the step (`scale.sh:92-95`,
  `delete-cluster.sh:91-94`, `run.sh:755-783`); the budgets are configurable
  (`WAIT_TIMEOUT`, `SCALE_WAIT_TIMEOUT`, `DELETE_WAIT_TIMEOUT`).
- **The provider quadlet does not start**: check
  `systemctl --user status mgmt-cluster-api-hypervisor` and
  `journalctl --user` for the unit (`run.sh:660-674`); confirm the provider
  image exists (`podman images | grep cluster-api-hypervisor`) and the
  `build/` artifacts are in place. Persistent k8netd connection-retry lines in
  the journal fail the run explicitly (`run.sh:676-690`).
- **Guest probes fail**: the probes need SSH access to the control-plane guest
  through the forwarded host port 22; check `GUEST_SSH_KEY`/`GUEST_SSH_USER`
  (`run.sh:385-398`) and that the key's public part is provisioned into the
  guests (HypervisorConfig `SSHPublicKey`).
- **Smoke fails**: each failing check names itself as `FAIL: <check>` and the
  aggregate exit code is non-zero (`smoke.sh:186-192`); the Cilium exec check
  is WARN-only and does not fail the run (`smoke.sh:92-102`).
- **Delete fails**: `delete-cluster.sh` exits non-zero naming the timed-out
  step; it performs no host network operation, so a failure points at the
  object teardown or the mgmt-down step, never at host tooling
  (`delete-cluster.sh:41-44`).
