# E2E suite runbook

The end-to-end suite for cluster-api-hypervisor verifies the full lab flow on a
single KVM host: a self-bootstrapped (or external) management plane, the
provider running as a podman quadlet, a topology-driven workload Cluster
created through the committed ClusterClass, workload Machines that boot and
join, workload smoke checks, a worker scale scenario, and a full cluster
deletion that leaves the host network state clean.

The suite is documentation-faithful: every environment variable, default, wait
budget, and step below is read from the scripts in this directory (`run.sh`,
`scale.sh`, `delete-cluster.sh`, `smoke.sh`, and `mgmt/`). Source lines are
listed for each claim so the runbook can be re-verified against the code.

## Scripts at a glance

| Script | Role |
|---|---|
| `test/e2e/run.sh` | Full-lab orchestration: management plane up (or external), provider quadlet, workload Cluster generated via clusterctl and applied, wait for workload Machines Ready, workload smoke checks, teardown via a trap. |
| `test/e2e/smoke.sh` | Workload-cluster smoke checks (nodes, kube-system pods, Cilium, Gateway, CoreDNS, in-cluster DNS). Invoked by `run.sh`; also runnable standalone. |
| `test/e2e/scale.sh` | Worker scale scenario against a live lab: bump replicas, new Machine boots and the node registers, then delete the Machine and wait for the count to drop. |
| `test/e2e/delete-cluster.sh` | Cluster-deletion scenario against a live lab: delete the Cluster object, wait for Machine teardown, stop a self-bootstrapped management plane, verify the host network state is gone. |
| `test/e2e/mgmt/` | Self-bootstrapped management plane: `pki.sh` (PKI + kubeconfigs), `apply.sh` (core manifests, clusterctl config render, offline core override, `clusterctl init`, webhook caBundle patch, quadlets), `down.sh` (stop the plane), `units/`, `core/`. |

## 1. Prerequisites

All scenarios run on one Linux host; nothing is containerized away from the
host's kernel, so the host must provide the virtualization and container
primitives the provider shells out to.

1. **KVM-capable host.** The provider runs cloud-hypervisor subprocesses that
   need KVM device access and a full capability set (bridge/TAP/NAT/VM
   management). The quadlet runs privileged (`PodmanArgs=--privileged`, see
   `docs/install-contract.md`), so the host must expose `/dev/kvm` and permit
   running VMs (`ls /dev/kvm`). The mgmt bootstrap starts systemd services, so
   a systemd host is required.

2. **podman with quadlet support.** The management plane runs as quadlet
   services installed under `/etc/containers/systemd`
   (`test/e2e/mgmt/apply.sh:41`), and `make image` builds the provider image
   with podman (`Makefile:107-109`). `podman`, `systemctl`, and `go` are
   required tools of the bootstrap (`test/e2e/mgmt/apply.sh:132-135`).

3. **The provider image.** Build it before the lab run so the provider quadlet
   can start:

   ```sh
   make image
   ```

   This builds the default tag `cluster-api-hypervisor:dev` (`Makefile:5`,
   `Makefile:108-109`). The image is local-only (never published); a quadlet
   references it as `localhost/cluster-api-hypervisor:dev`.

4. **The k8labs base image and firmware.** The provider boots workload VMs from
   `build/k8labs-base.qcow2` with the `build/CLOUDHV.fd` firmware
   (`docs/install-contract.md:105-106`). `run.sh` requires both as existing,
   readable, regular files (`run.sh:231-237`, `run.sh:240-246`); the relative
   defaults resolve against the working directory the harness is invoked from
   (`run.sh:31-38`, `run.sh:156-162`). Bake the base image with the k8labs
   image-baking pipeline so both artifacts exist before the first `run.sh`
   invocation, or point `BASE_IMAGE`/`FIRMWARE` at your copies.

5. **cloud-hypervisor.** The binary is bundled inside the provider image at a
   pinned version (`Containerfile:40-41`, `docs/install-contract.md:50`), and
   the provider shells out to `cloud-hypervisor` from inside the container
   (`docs/install-contract.md:110`). No separate host install is required; the
   host side is the `/dev/kvm` device.

6. **CLI tools.** `kubectl` is required by every scenario script
   (`run.sh:451`, `scale.sh:77-78`, `delete-cluster.sh:80-81`, `smoke.sh:35-36`).
   `run.sh` also needs `go`, `base64`, and `mktemp` (`run.sh:450-453`);
   `scale.sh` needs `base64` and `mktemp` (`scale.sh:79-82`). The mgmt
   bootstrap needs `openssl` for `pki.sh` and, optionally, `kubectl` to render
   kubeconfigs (`test/e2e/mgmt/pki.sh:161-184`).

## 2. Environment variables

Every variable below is read from the scenario scripts; the default column is
the value the script applies when the variable is unset, and the source column
lists the script lines that define or apply it.

### `run.sh` contract

| Variable | Default | Meaning | Source |
|---|---|---|---|
| `MANAGEMENT_KUBECONFIG` | unset -> mgmt-bootstrap fallback | Management-cluster kubeconfig. Set: must name an existing, readable, non-empty file; the plane is treated as external and is not torn down on exit. Unset/empty: the harness falls back to the committed bootstrap (`test/e2e/mgmt`) driven by `MGMT_STATE_DIR`; the state must be provisioned with the admin kubeconfig at `<state>/kubeconfigs/admin.conf`. | doc `run.sh:15-24`; validation `run.sh:205-221`; fallback `run.sh:212-221` |
| `MGMT_STATE_DIR` | `/var/lib/k8slab/mgmt` | Management-plane state directory for the fallback bootstrap (created by `test/e2e/mgmt/pki.sh`). | default `run.sh:78`; applied `run.sh:213`; also `test/e2e/mgmt/apply.sh:116` |
| `IMAGE` | `cluster-api-hypervisor:dev` | Provider image reference (the Makefile tag). A set value must be a syntactically plausible container reference (no whitespace). | default `run.sh:74`; applied/checked `run.sh:225-228` |
| `BASE_IMAGE` | `build/k8labs-base.qcow2` | k8labs base image path, resolved against the invocation working directory. Must be an existing, readable, regular file. | default `run.sh:75`; applied `run.sh:231`; checks `run.sh:232-237` |
| `FIRMWARE` | `build/CLOUDHV.fd` | CLOUDHV.fd path, resolved against the invocation working directory. Must be an existing, readable, regular file. | default `run.sh:76`; applied `run.sh:240`; checks `run.sh:241-246` |
| `STATE_DIR` | `/var/lib/k8slab` | Provider state directory. Must be an existing, writable directory (not a regular file). | default `run.sh:77`; applied `run.sh:249`; checks `run.sh:250-258` |
| `OUT_DIR` | `<repo>/out` | Provider release layout directory. Must be an existing directory containing the three provider release directories `infrastructure-hypervisor/v0.1.0`, `bootstrap-hypervisor/v0.1.0`, and `control-plane-hypervisor/v0.1.0` (the layout `make components` emits), so `go tool clusterctl generate cluster` can resolve the cluster template from the local repository. | default `run.sh:83`; applied `run.sh:263`; checks `run.sh:264-274` |
| `SMOKE` | `1` | Run the workload smoke checks (`smoke.sh`) after the Machines are Ready. `0` disables them; any other value enables them. When `smoke.sh` is absent the checks are skipped with a note. | applied `run.sh:443`; disabled check `run.sh:402-404`; absent check `run.sh:406-410` |
| `WAIT_TIMEOUT` | `1800` | Seconds to wait for the workload Machines to become Ready. | default `run.sh:96`; applied `run.sh:444`; used `run.sh:362` |

Validation is all-or-nothing before any heavy work: no cluster, VM, or quadlet
is started unless every variable above is valid (`run.sh:11-13`, `run.sh:203`).

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
namespace `default` (`run.sh:92-93`), the worker MachineDeployment is `md-0`
(`scale.sh:74`), and the management endpoint is `https://127.0.0.1:6443`
(`test/e2e/mgmt/pki.sh:45`).

## 3. Run order

The suite is three phases. Phase 1 is self-contained: `run.sh` brings the lab
up and tears it down. Phases 2 and 3 are scenario scripts that operate on a
live lab (management plane up, Cluster applied, workload Machines Ready) and
are documented after the teardown note that explains how the lab stays alive
for them.

### Phase 1 — full lab (`run.sh`)

```sh
bash test/e2e/run.sh
```

Invoke from the repository root so the relative `build/` defaults resolve, or
override `BASE_IMAGE`/`FIRMWARE` with absolute paths. The orchestration is
(`run.sh:442-462`):

1. **mgmt up** (`run.sh:455`, `mgmt_up` at `run.sh:291-301`). With
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
2. **Provider** — wait for the provider quadlet `mgmt-cluster-api-hypervisor`
   to be active (`run.sh:456`, `wait_for_provider` at `run.sh:319-330`;
   fallback plane only, `run.sh:320`).
3. **Cluster via clusterctl** — generate the workload Cluster with
   `go tool clusterctl generate cluster` and apply it to the management
   cluster (`run.sh:457`, `apply_templates` at `run.sh:339-356`): the pinned
   flags are `--namespace default --infrastructure hypervisor
   --kubernetes-version v1.32.13 --control-plane-machine-count 1
   --worker-machine-count 3`, piped into `kubectl apply --kubeconfig=<admin>
   -f -`; the rendered manifest comes from the cluster template shipped in the
   provider release tree (`templates/cluster-template.yaml`).
4. **Machines Ready** — poll until every workload Machine reports the CAPI
   `Ready` condition `True` (`run.sh:458`, `wait_for_machines_ready` at
   `run.sh:360-384`).
5. **Smoke** — extract the workload kubeconfig from the
   `k8labs-kubeconfig` Secret (`run.sh:388-397`) and run the workload smoke
   checks (`run.sh:459`, `run_smoke` at `run.sh:401-415`), unless `SMOKE=0`.
6. **Teardown** — on exit, the trap (`run.sh:420-439`) deletes the workload
   Cluster, removes the temp kubeconfig, and brings the management plane down
   (`test/e2e/mgmt/down.sh`) when the harness started it.

`run.sh` requires `go`, `kubectl`, `base64`, and `mktemp` on PATH
(`run.sh:450-453`). Exit codes: `0` full-lab run completed (including
teardown) or `--help`; `1` environment validation failure or orchestration
failure (`run.sh:56-58`). `test/e2e/run.sh --help` prints the full contract
without touching anything.

### Teardown semantics (read before Phase 2/3)

`run.sh` always deletes the workload Cluster on exit — with an external
management plane the Cluster is still deleted (`run.sh:423-429`), only the
plane itself is left running (`run.sh:433-437`). So Phases 2 and 3 run against
a lab kept up for the scenario, not against the leftovers of a completed
`run.sh` session.

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
   drops below the target (`scale.sh:206-215`). Host-level VM/TAP/disk cleanup
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
(`delete-cluster.sh:117-172`):

1. **cluster-delete** — delete the workload Cluster object and the workload
   namespace, then poll until the Cluster object is gone
   (`delete-cluster.sh:121-127`). CAPI controllers tear down the Machines and,
   through them, the VMs, TAPs, and disks.
2. **machine-teardown** — poll until no workload Machine remains
   (`delete-cluster.sh:129-134`).
3. **mgmt-down** — stop the management plane only when it was
   self-bootstrapped (`MANAGEMENT_KUBECONFIG` unset/empty); an external plane
   keeps running and the step is skipped (`delete-cluster.sh:136-149`). The
   down script resolves through `MGMT_DOWN_SH` (default
   `<script-dir>/mgmt/down.sh`).
4. **leftover** — verify the host network state is gone: the `k8sbr0` bridge,
   the `dnsmasq` process, and the `inet k8slab` nftables table must all be
   absent; any survivor exits non-zero naming the leftover item
   (`delete-cluster.sh:151-169`).

Every wait shares the `DELETE_WAIT_TIMEOUT` budget; on expiry the script exits
non-zero naming the step that timed out: `cluster-delete` or
`machine-teardown` (`delete-cluster.sh:20-22`, `delete-cluster.sh:87-98`).
Requires `kubectl` (`delete-cluster.sh:80-81`). Exit codes: `0` scenario
converged; `1` timeout, failure, or a leftover host artifact
(`delete-cluster.sh:41-44`).

### One-session suite recipe

Run the whole suite against a lab kept alive for the scenario phases
(management state provisioned with `pki.sh`, plane started with `apply.sh`):

```sh
# Phase 1: full-lab run (bring-up, smoke, teardown) — self-contained.
bash test/e2e/run.sh

# Keep a lab alive for the scenario phases:
bash test/e2e/mgmt/pki.sh /var/lib/k8slab/mgmt
MGMT_STATE_DIR=/var/lib/k8slab/mgmt bash test/e2e/mgmt/apply.sh
KC=/var/lib/k8slab/mgmt/kubeconfigs/admin.conf   # the fallback admin kubeconfig (run.sh:214)
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
clusterctl generate + kubectl apply is declarative (`run.sh:332-338`). After
Phase 3 the lab is down and the host network state is verified gone.

## 4. Expected timings

Every wait below is a budget, not a delay: the scripts poll until the
condition converges or the budget expires and the run fails naming the step.
On a cold lab the machine-ready wait dominates the wall clock.

| Phase | Wait | Budget | Poll interval | Source |
|---|---|---|---|---|
| run.sh | management apiserver `/readyz` | 300 s | 2 s | `run.sh:95`, `run.sh:305-315` |
| run.sh | provider quadlet `mgmt-cluster-api-hypervisor` active | 300 s | 2 s | `run.sh:319-330` |
| run.sh | workload Machines Ready | `WAIT_TIMEOUT`, default 1800 s | 5 s | `run.sh:96`, `run.sh:98`, `run.sh:360-384` |
| run.sh | teardown: workload Cluster deletion | 300 s | — | `run.sh:97`, `run.sh:423-429` |
| scale.sh | scale-up, vm-boot, node-ready, delete | `SCALE_WAIT_TIMEOUT`, default 1800 s per step | 2 s | `scale.sh:63`, `scale.sh:75`, `scale.sh:88-99` |
| delete-cluster.sh | cluster-delete, machine-teardown | `DELETE_WAIT_TIMEOUT`, default 1800 s per step | 1 s | `delete-cluster.sh:71`, `delete-cluster.sh:78`, `delete-cluster.sh:87-98` |
| smoke.sh | coredns rollout status | 60 s | — | `smoke.sh:120` |
| smoke.sh | DNS probe pods reach Running | up to 30 x 2 s | 2 s | `smoke.sh:140-148` |

For a healthy run, expect the phases to take well under the budgets — the
budgets exist to fail fast with a named step when something does not converge.

## 5. Re-running against an existing management plane

`run.sh` consumes an external plane when `MANAGEMENT_KUBECONFIG` names an
existing, readable, non-empty kubeconfig (`run.sh:205-211`); the mgmt
bootstrap is then skipped entirely (`run.sh:291-295`) and the plane is left
running on exit (`run.sh:433-437`). This is the supported way to re-run the
full-lab flow against a plane you already operate:

```sh
export MANAGEMENT_KUBECONFIG=/path/to/existing/admin.conf
bash test/e2e/run.sh
```

With an external plane, `run.sh` still generates and applies the workload
Cluster via clusterctl, waits for Ready, runs smoke, and still deletes the
workload Cluster on exit — only the plane itself survives (`run.sh:423-437`).
If the external plane lacks the Cluster API core CRDs, the initialized
hypervisor providers (clusterctl init), or the host network setup the
scenarios expect, `run.sh` will fail at the apply or wait step; the committed
bootstrap (`test/e2e/mgmt`) is the reference environment the suite is verified
against.

`scale.sh` and `delete-cluster.sh` always take the management kubeconfig
explicitly (`KUBECONFIG`/`$1`) and do not care how the plane came up. For the
deletion scenario, `MANAGEMENT_KUBECONFIG` set and non-empty skips the
mgmt-down step (`delete-cluster.sh:139-141`) — the right choice when the plane
is external.

## 6. Contract tests (no live cluster required)

Each scenario script has a contract test that runs without a live cluster.
The scenario scripts are executed against stub tooling on `PATH` (or, for the
harness, against a controlled environment) and the tests assert the scripts'
decisions and aggregate exit codes. All five are plain scripts with no
arguments:

```sh
test/e2e/mgmt/mgmt_test.sh     # mgmt bootstrap contract
test/e2e/harness_test.sh       # run.sh environment contract
test/e2e/smoke_test.sh         # smoke.sh per-check contract
test/e2e/scale_test.sh         # scale.sh per-step contract
test/e2e/delete_test.sh        # delete-cluster.sh per-step contract
```

| Test | What it pins | How it runs without a cluster |
|---|---|---|
| `test/e2e/mgmt/mgmt_test.sh` | `pki.sh` state layout, quadlet units, core manifests, `apply.sh`/`down.sh` lifecycle, and the clusterctl rewire contract (config render, offline core override, `go tool clusterctl init` invocation, webhook caBundle patch) | Executes `pki.sh` against a scratch state directory; every other assertion checks the committed files directly, including `test_apply_rewire` against `apply.sh` and the committed `clusterctl.yaml` template (`mgmt_test.sh:48-50`, `mgmt_test.sh:483-533`). |
| `test/e2e/harness_test.sh` | `run.sh` validates every contract variable (including `OUT_DIR`) before heavy work, its errors name the exact variable, and `apply_templates` pipes `go tool clusterctl generate cluster` into `kubectl apply -f -` | Runs `run.sh` with `env -i` (only `PATH`/`HOME` plus explicit assignments) and a 30 s timeout, feeding fake-but-valid fixture files; every scenario keeps all variables valid except the one under test (`harness_test.sh:150-166`, `harness_test.sh:189-217`, `harness_test.sh:224-236`); the apply flow runs against stubbed `go` and `kubectl` binaries (`harness_test.sh:609-705`). Never starts a cluster, VM, or quadlet (`harness_test.sh:74-80`). |
| `test/e2e/smoke_test.sh` | Per-check pass/fail semantics of `smoke.sh` (nodes, kube-system, Cilium, Gateway, CoreDNS, DNS regressions) plus the aggregate exit code | Runs `smoke.sh` against a stub `kubectl` on `PATH` that dispatches on its arguments and returns scripted canned outputs; each run is bounded by a 60 s timeout (`smoke_test.sh:7-12`, `smoke_test.sh:63`). |
| `test/e2e/scale_test.sh` | Per-step contract of `scale.sh` (scale-up, VM boot, node-ready, delete, timeout naming) | Runs `scale.sh` against a stub `kubectl` modeling a timeline (pre-bump baseline, post-bump set, post-delete set) through `STUB_*` variables; success scenarios use a 10 s wait budget, timeout scenarios 3 s (`scale_test.sh:6-11`, `scale_test.sh:73-80`). |
| `test/e2e/delete_test.sh` | Per-step contract of `delete-cluster.sh` (cluster-delete, machine-teardown, mgmt-down, leftover) | Runs `delete-cluster.sh` against a stub `kubectl` plus stub `ip`/`pgrep`/`nft` and a stub mgmt-down injected through `MGMT_DOWN_SH` (`delete_test.sh:9-15`). |

Exit codes are uniform across the contract tests: `0` the script satisfies its
contract, `1` contract violation (including the script under test being
absent), `2` prerequisite problem (missing tool, unexpected arguments). The
contract tests ship under `test/e2e/` and run directly as plain scripts
(`bash test/e2e/<name>_test.sh`, no arguments); they are not wired into the
Makefile or CI. They require only the tools the scripts themselves require
(`kubectl` for the harness and stub-based tests, `openssl` for the mgmt test).

## 7. Troubleshooting pointers

- **`run.sh` exits 1 before any heavy work**: the error names the offending
  variable and path (`run.sh:203-277`); check the env table above against the
  invocation.
- **A wait times out**: the error line names the step (`scale.sh:92-95`,
  `delete-cluster.sh:91-94`); the budgets are configurable
  (`WAIT_TIMEOUT`, `SCALE_WAIT_TIMEOUT`, `DELETE_WAIT_TIMEOUT`).
- **The provider quadlet does not start**: check
  `systemctl status mgmt-cluster-api-hypervisor` and `journalctl` for the unit
  (`run.sh:323-325`); confirm the provider image exists
  (`podman images | grep cluster-api-hypervisor`) and the `build/` artifacts
  are in place.
- **Smoke fails**: each failing check names itself as `FAIL: <check>` and the
  aggregate exit code is non-zero (`smoke.sh:186-192`); the Cilium exec check
  is WARN-only and does not fail the run (`smoke.sh:92-102`).
- **Delete leaves host state behind**: `delete-cluster.sh` exits non-zero
  naming `k8sbr0`, `dnsmasq`, or `k8slab` (`delete-cluster.sh:151-169`).
