#!/usr/bin/env bash
#
# scale.sh — worker scale scenario for the cluster-api-hypervisor full-lab
# e2e. Drives the CAPI Cluster topology through a scale-up (bump the worker
# replicas, the new Machine appears, its VM boots and the kubelet registers so
# the node becomes Ready) and a scale-down (delete a worker Machine and wait
# until the worker Machine count drops back below the target). Host-level
# VM/TAP/disk cleanup after the delete is verified by the lab harness, not by
# this script.
#
# The harness (test/e2e/run.sh) hands the management kubeconfig to its
# helpers as the KUBECONFIG environment variable and as the first positional
# argument; either invocation shape is accepted and a positional argument
# wins, mirroring smoke.sh. The target worker replicas value comes from the
# REPLICAS environment variable or from the second positional argument.
#
# Every wait runs against its own budget (SCALE_WAIT_TIMEOUT, default 1800s)
# and, on expiry, the script exits non-zero with an error line that names the
# step that timed out ("scale-up", "node-ready", or "delete").
#
# Usage:
#   KUBECONFIG=<mgmt-kubeconfig> REPLICAS=<workers> \
#     bash test/e2e/scale.sh [<mgmt-kubeconfig> [<workers>]]
#
# Environment:
#   KUBECONFIG          management-cluster kubeconfig (overridden by $1)
#   REPLICAS            target worker Machine count (overridden by $2)
#   CLUSTER_NAME        workload Cluster name (default k8labs)
#   CLUSTER_NAMESPACE   workload Cluster namespace (default default)
#   SCALE_WAIT_TIMEOUT  per-step wait budget in seconds (default 1800)
#
# Exit codes:
#   0  the whole scale scenario converged
#   1  a step timed out (the error line names the step) or failed
#
# shellcheck disable=SC2329 # wait predicates are invoked indirectly by name through wait_for

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

# The kubeconfig contract: a positional argument overrides the environment so
# both harness shapes (KUBECONFIG env alone, or env plus the first argument)
# work.
if [[ -n "${1:-}" ]]; then
  KUBECONFIG="${1}"
fi
: "${KUBECONFIG:?management kubeconfig is required: pass it as the first argument or set KUBECONFIG}"
export KUBECONFIG

# The replicas contract: the second positional argument overrides REPLICAS.
if [[ -n "${2:-}" ]]; then
  REPLICAS="${2}"
fi
: "${REPLICAS:?target worker replicas is required: pass it as the second argument or set REPLICAS}"
if [[ ! "${REPLICAS}" =~ ^[0-9]+$ ]]; then
  printf 'ERROR: REPLICAS must be a non-negative integer: %s\n' "${REPLICAS}" >&2
  exit 1
fi

CLUSTER_NAME="${CLUSTER_NAME:-k8labs}"
CLUSTER_NAMESPACE="${CLUSTER_NAMESPACE:-default}"
SCALE_WAIT_TIMEOUT="${SCALE_WAIT_TIMEOUT:-1800}"
if [[ ! "${SCALE_WAIT_TIMEOUT}" =~ ^[0-9]+$ ]]; then
  printf 'ERROR: SCALE_WAIT_TIMEOUT must be a non-negative integer: %s\n' "${SCALE_WAIT_TIMEOUT}" >&2
  exit 1
fi

# The worker MachineDeployment from the committed example Cluster topology
# (templates/cluster-example.yaml). CAPI labels every Machine a
# MachineDeployment creates with cluster.x-k8s.io/deployment-name=<md-name>,
# which is what separates the worker Machines from the control-plane Machine
# when counting against the target.
readonly WORKER_SELECTOR="cluster.x-k8s.io/deployment-name=md-0"
readonly POLL_INTERVAL=2

command -v kubectl >/dev/null 2>&1 \
  || { printf 'ERROR: kubectl is required on PATH\n' >&2; exit 1; }
command -v base64 >/dev/null 2>&1 \
  || { printf 'ERROR: base64 is required on PATH\n' >&2; exit 1; }
command -v mktemp >/dev/null 2>&1 \
  || { printf 'ERROR: mktemp is required on PATH\n' >&2; exit 1; }

# wait_for <step> <description> <predicate> — poll the predicate function
# until it exits 0. On expiry, print an error line that names the step and
# exit non-zero. The predicate must never abort the script itself: a
# transient API error is a reason to keep polling, not to fail the run.
wait_for() {
  local step="$1" description="$2" predicate="$3"
  local deadline=$(( $(date +%s) + SCALE_WAIT_TIMEOUT ))
  until "${predicate}"; do
    if (( $(date +%s) >= deadline )); then
      printf 'ERROR: %s: timed out after %ss waiting for %s\n' \
        "${step}" "${SCALE_WAIT_TIMEOUT}" "${description}" >&2
      exit 1
    fi
    sleep "${POLL_INTERVAL}"
  done
}

# worker_machine_names — the worker Machine names via the worker label
# selector. Every kubectl failure is swallowed so a transient API error
# yields an empty list (a reason to keep polling).
worker_machine_names() {
  kubectl get machine -n "${CLUSTER_NAMESPACE}" -l "${WORKER_SELECTOR}" \
    --kubeconfig="${KUBECONFIG}" --no-headers 2>/dev/null \
    | awk '{print $1}' || true
}

# worker_machine_count — the number of worker Machines currently present.
worker_machine_count() {
  local names=""
  names="$(worker_machine_names)"
  if [[ -z "${names}" ]]; then
    printf '%s\n' "0"
    return 0
  fi
  wc -l <<< "${names}"
}

printf '%s\n' "=== scale: target worker replicas ${REPLICAS} on cluster ${CLUSTER_NAME}/${CLUSTER_NAMESPACE} ==="

# Step 1: scale-up — remember the pre-bump workers, patch the Cluster
# topology worker replicas, then wait until the worker Machine count reaches
# the target (the new Machine has appeared). The merge patch replaces the
# machineDeployments array, so the full item (name, class, replicas) is sent,
# exactly like the documented scaling recipe in templates/README.md.
printf '%s\n' "--- scale-up: bumping the worker replicas to ${REPLICAS} ---"
BASELINE="$(worker_machine_names)"
kubectl patch cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
  --kubeconfig="${KUBECONFIG}" --type=merge \
  -p "{\"spec\":{\"topology\":{\"workers\":{\"machineDeployments\":[{\"name\":\"md-0\",\"class\":\"default-worker\",\"replicas\":${REPLICAS}}]}}}}" \
  >/dev/null 2>&1 \
  || { printf 'ERROR: failed to patch the Cluster topology replicas\n' >&2; exit 1; }

scale_up_ok() {
  [[ "$(worker_machine_count)" -eq "${REPLICAS}" ]]
}
wait_for "scale-up" "the worker Machine count to reach ${REPLICAS}" scale_up_ok

# Identify the new Machine: the first worker Machine not present before the
# bump.
NEW_MACHINE=""
for name in $(worker_machine_names); do
  if ! grep -Fq -- "${name}" <<< "${BASELINE}"; then
    NEW_MACHINE="${name}"
    break
  fi
done
if [[ -z "${NEW_MACHINE}" ]]; then
  printf 'ERROR: scale-up: no new worker Machine appeared after the replicas bump\n' >&2
  exit 1
fi
printf '%s\n' "scale-up: new worker Machine ${NEW_MACHINE} appeared"

# Step 2: VM boot — the new Machine's linked HypervisorMachine must report
# ready with status.addresses populated (the VM is up and reachable). A boot
# hang is reported as part of the scale-up step.
printf '%s\n' "--- vm-boot: waiting for ${NEW_MACHINE}'s HypervisorMachine ---"
HM_NAME="$(kubectl get machine "${NEW_MACHINE}" -n "${CLUSTER_NAMESPACE}" \
  --kubeconfig="${KUBECONFIG}" -o jsonpath='{.spec.infrastructureRef.name}' 2>/dev/null || true)"
if [[ -z "${HM_NAME}" ]]; then
  printf 'ERROR: no infrastructure reference for Machine %s\n' "${NEW_MACHINE}" >&2
  exit 1
fi
hm_ready_ok() {
  local ready="" addresses=""
  ready="$(kubectl get hypervisormachine "${HM_NAME}" -n "${CLUSTER_NAMESPACE}" \
    --kubeconfig="${KUBECONFIG}" -o jsonpath='{.status.ready}' 2>/dev/null || true)"
  addresses="$(kubectl get hypervisormachine "${HM_NAME}" -n "${CLUSTER_NAMESPACE}" \
    --kubeconfig="${KUBECONFIG}" -o jsonpath='{.status.addresses}' 2>/dev/null || true)"
  [[ "${ready}" == "true" && -n "${addresses}" ]]
}
wait_for "scale-up" "the HypervisorMachine ${HM_NAME} to be ready with addresses" hm_ready_ok
printf '%s\n' "vm-boot: HypervisorMachine ${HM_NAME} ready with addresses"

# Step 3: node-ready — extract the workload kubeconfig from the
# <cluster>-kubeconfig Secret and poll the workload cluster until every node
# is Ready (the control-plane plus the target number of workers).
printf '%s\n' "--- node-ready: waiting for the workload nodes ---"
WORKLOAD_KC="$(mktemp)"
trap 'rm -f -- "${WORKLOAD_KC}"' EXIT
kubectl get secret "${CLUSTER_NAME}-kubeconfig" -n "${CLUSTER_NAMESPACE}" \
  --kubeconfig="${KUBECONFIG}" -o jsonpath='{.data.value}' 2>/dev/null \
  | base64 -d > "${WORKLOAD_KC}" 2>/dev/null \
  || { printf 'ERROR: failed to extract the workload kubeconfig from the %s-kubeconfig Secret\n' "${CLUSTER_NAME}" >&2; exit 1; }
chmod 600 "${WORKLOAD_KC}"

EXPECTED_NODES=$((1 + REPLICAS))
nodes_ready_ok() {
  local nodes="" not_ready=""
  nodes="$(kubectl get nodes --kubeconfig="${WORKLOAD_KC}" --no-headers 2>/dev/null || true)"
  if [[ -z "${nodes}" ]]; then
    return 1
  fi
  not_ready="$(printf '%s\n' "${nodes}" | awk '{if($2!="Ready"){print $1}}' || true)"
  [[ "$(wc -l <<< "${nodes}")" -eq "${EXPECTED_NODES}" && -z "${not_ready}" ]]
}
wait_for "node-ready" "${EXPECTED_NODES} workload nodes to be Ready" nodes_ready_ok
printf '%s\n' "node-ready: ${EXPECTED_NODES} workload nodes Ready"

# Step 4: delete — remove the new worker Machine and wait until the worker
# Machine count drops back below the target (the Machine is gone; the linked
# HypervisorMachine is served as gone too, host-level VM/TAP/disk cleanup
# being verified by the lab).
printf '%s\n' "--- delete: removing worker Machine ${NEW_MACHINE} ---"
kubectl delete machine "${NEW_MACHINE}" -n "${CLUSTER_NAMESPACE}" \
  --kubeconfig="${KUBECONFIG}" >/dev/null 2>&1 \
  || { printf 'ERROR: failed to delete Machine %s\n' "${NEW_MACHINE}" >&2; exit 1; }

delete_ok() {
  [[ "$(worker_machine_count)" -lt "${REPLICAS}" ]]
}
wait_for "delete" "the worker Machine count to drop below ${REPLICAS}" delete_ok
printf '%s\n' "delete: worker Machine count dropped below ${REPLICAS}"

printf '%s\n' "PASS: scale scenario complete"
exit 0
