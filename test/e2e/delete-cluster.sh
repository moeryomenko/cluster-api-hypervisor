#!/usr/bin/env bash
#
# delete-cluster.sh — cluster-deletion scenario for the cluster-api-hypervisor
# full-lab e2e. Deleting the workload Cluster object drives the CAPI
# controllers to tear down the workload Machines and, through them, the
# VMs and disks; this script gates that teardown at the object level
# (the Cluster object gone, then no workload Machine remains) and brings
# the management plane down through the mgmt-down script when the plane
# was self-bootstrapped. It performs no host network operation: after the
# k8netd migration the provider tears the cluster network down through the
# k8netd control socket, so there is no host-side network state to inspect.
#
# The harness (test/e2e/run.sh) hands the management kubeconfig to its
# helpers as the KUBECONFIG environment variable and as the first positional
# argument; either invocation shape is accepted and a positional argument
# wins, mirroring smoke.sh and scale.sh. The workload namespace is optional:
# it comes from CLUSTER_NAMESPACE (default "default") or from the second
# positional argument.
#
# Every wait runs against its own budget (DELETE_WAIT_TIMEOUT, default
# 1800s) and, on expiry, the script exits non-zero with an error line that
# names the step that timed out ("cluster-delete" or "machine-teardown").
#
# Usage:
#   KUBECONFIG=<mgmt-kubeconfig> \
#     bash test/e2e/delete-cluster.sh [<mgmt-kubeconfig> [<namespace>]]
#
# Environment:
#   KUBECONFIG             management-cluster kubeconfig (overridden by $1)
#   CLUSTER_NAME           workload Cluster name (default k8labs)
#   CLUSTER_NAMESPACE      workload Cluster namespace (default default,
#                          overridden by $2)
#   MANAGEMENT_KUBECONFIG  management-plane kubeconfig; when set and non-empty
#                          the plane is external and the mgmt-down step is
#                          skipped (unset or empty: self-bootstrapped plane,
#                          the mgmt-down step runs)
#   MGMT_DOWN_SH           mgmt-down script (default
#                          <script-dir>/mgmt/down.sh)
#   DELETE_WAIT_TIMEOUT    per-step wait budget in seconds (default 1800)
#
# Exit codes:
#   0  the whole deletion scenario converged
#   1  a step timed out (the error line names the step) or a step failed;
#      no host network tooling is ever invoked (none is part of the contract)
#
# shellcheck disable=SC2329 # wait predicates are invoked indirectly by name through wait_for

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR

# The kubeconfig contract: a positional argument overrides the environment so
# both harness shapes (KUBECONFIG env alone, or env plus the first argument)
# work.
if [[ -n "${1:-}" ]]; then
  KUBECONFIG="${1}"
fi
: "${KUBECONFIG:?management kubeconfig is required: pass it as the first argument or set KUBECONFIG}"
export KUBECONFIG

# The workload identity contract: the second positional argument overrides
# CLUSTER_NAMESPACE (an empty second argument is ignored).
if [[ -n "${2:-}" ]]; then
  CLUSTER_NAMESPACE="${2}"
fi
CLUSTER_NAME="${CLUSTER_NAME:-k8labs}"
CLUSTER_NAMESPACE="${CLUSTER_NAMESPACE:-default}"
DELETE_WAIT_TIMEOUT="${DELETE_WAIT_TIMEOUT:-1800}"
MGMT_DOWN_SH="${MGMT_DOWN_SH:-${SCRIPT_DIR}/mgmt/down.sh}"
if [[ ! "${DELETE_WAIT_TIMEOUT}" =~ ^[0-9]+$ ]]; then
  printf 'ERROR: DELETE_WAIT_TIMEOUT must be a non-negative integer: %s\n' "${DELETE_WAIT_TIMEOUT}" >&2
  exit 1
fi

readonly POLL_INTERVAL=1

command -v kubectl >/dev/null 2>&1 \
  || { printf 'ERROR: kubectl is required on PATH\n' >&2; exit 1; }

# wait_for <step> <description> <predicate> — poll the predicate function
# until it exits 0. On expiry, print an error line that names the step and
# exit non-zero. The predicate must never abort the script itself: a
# transient API error is a reason to keep polling, not to fail the run.
wait_for() {
  local step="$1" description="$2" predicate="$3"
  local deadline=$(( $(date +%s) + DELETE_WAIT_TIMEOUT ))
  until "${predicate}"; do
    if (( $(date +%s) >= deadline )); then
      printf 'ERROR: %s: timed out after %ss waiting for %s\n' \
        "${step}" "${DELETE_WAIT_TIMEOUT}" "${description}" >&2
      exit 1
    fi
    sleep "${POLL_INTERVAL}"
  done
}

# cluster_gone_ok — the workload Cluster object is gone: the get fails. A
# transient API error is a reason to keep polling, not to fail the run.
cluster_gone_ok() {
  ! kubectl get cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
    --kubeconfig="${KUBECONFIG}" -o name >/dev/null 2>&1
}

# machines_gone_ok — no workload Machine remains. Every kubectl failure is
# swallowed so a transient API error yields an empty list (a reason to keep
# polling).
machines_gone_ok() {
  local names=""
  names="$(kubectl get machine -n "${CLUSTER_NAMESPACE}" \
    --kubeconfig="${KUBECONFIG}" -o name 2>/dev/null || true)"
  [[ -z "${names}" ]]
}

printf '%s\n' "=== delete: cluster ${CLUSTER_NAME}/${CLUSTER_NAMESPACE} ==="

# Step 1: cluster-delete — delete the workload Cluster object and the
# workload namespace, then poll the Cluster object until it is gone.
printf '%s\n' "--- cluster-delete: deleting the Cluster object and namespace ---"
kubectl delete cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
  --kubeconfig="${KUBECONFIG}" >/dev/null 2>&1 \
  || { printf 'ERROR: cluster-delete: failed to delete the Cluster object\n' >&2; exit 1; }
kubectl delete namespace "${CLUSTER_NAMESPACE}" --kubeconfig="${KUBECONFIG}" >/dev/null 2>&1 || true
wait_for "cluster-delete" "the Cluster object to be gone" cluster_gone_ok
printf '%s\n' "cluster-delete: Cluster object gone"

# Step 2: machine-teardown — poll the management cluster until no workload
# Machine remains. The CAPI controllers tear down the VMs and disks as part
# of the deletion; this script's gate is the object teardown.
printf '%s\n' "--- machine-teardown: waiting for the workload Machines ---"
wait_for "machine-teardown" "the workload Machines to be gone" machines_gone_ok
printf '%s\n' "machine-teardown: no workload Machines remain"

# Step 3: mgmt-down — stop the management plane only when it was
# self-bootstrapped. An external plane (MANAGEMENT_KUBECONFIG set and
# non-empty) keeps running, so the step is skipped entirely.
if [[ -n "${MANAGEMENT_KUBECONFIG:-}" ]]; then
  printf '%s\n' "mgmt-down: skipping (external management kubeconfig)"
else
  if [[ ! -f "${MGMT_DOWN_SH}" ]]; then
    printf 'ERROR: mgmt-down: management plane down script not found: %s\n' "${MGMT_DOWN_SH}" >&2
    exit 1
  fi
  printf '%s\n' "mgmt-down: stopping the management plane"
  bash "${MGMT_DOWN_SH}" >/dev/null 2>&1 \
    || { printf 'ERROR: mgmt-down: the management plane did not stop cleanly\n' >&2; exit 1; }
fi

printf '%s\n' "PASS: cluster-deletion scenario complete"
exit 0
