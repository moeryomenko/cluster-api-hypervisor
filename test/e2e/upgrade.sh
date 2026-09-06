#!/usr/bin/env bash
#
# upgrade.sh — Kubernetes version upgrade scenario for the
# cluster-api-hypervisor full-lab e2e. Applies a HypervisorUpgradePlan for the
# target version and waits until the plan reports Completed: the control plane
# replaces its Machine first (etcd snapshot/restore carries the cluster state
# across the replacement), then the workers roll through their
# MachineDeployments. A plan that reports Failed fails the scenario with the
# plan's failure reason and message.
#
# The harness (test/e2e/run.sh) hands the management kubeconfig to its
# helpers as the KUBECONFIG environment variable and as the first positional
# argument; either invocation shape is accepted and a positional argument
# wins, mirroring smoke.sh and scale.sh. The target version comes from the
# TO_VERSION environment variable or from the second positional argument.
#
# Every wait runs against its own budget (UPGRADE_WAIT_TIMEOUT, default 3600s
# — an upgrade replaces every VM in the cluster, so the budget is larger than
# the scale scenario's) and, on expiry, the script exits non-zero with an
# error line that names the step that timed out ("preflight", "plan",
# "control-plane", "workers", or "nodes").
#
# Usage:
#   KUBECONFIG=<mgmt-kubeconfig> TO_VERSION=<version> \
#     bash test/e2e/upgrade.sh [<mgmt-kubeconfig> [<version>]]
#
# Environment:
#   KUBECONFIG          management-cluster kubeconfig (overridden by $1)
#   TO_VERSION          target Kubernetes version, v-prefixed semver (overridden by $2)
#   CLUSTER_NAME        workload Cluster name (default k8labs)
#   CLUSTER_NAMESPACE   workload Cluster namespace (default default)
#   UPGRADE_WAIT_TIMEOUT
#                       per-step wait budget in seconds (default 3600)
#
# Exit codes:
#   0  the upgrade converged on the target version
#   1  a step timed out (the error line names the step), the preflight
#      failed, or the plan reported Failed
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

# The version contract: the second positional argument overrides TO_VERSION.
if [[ -n "${2:-}" ]]; then
  TO_VERSION="${2}"
fi
: "${TO_VERSION:?target version is required: pass it as the second argument or set TO_VERSION}"
if [[ ! "${TO_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'ERROR: TO_VERSION must be v-prefixed semver (e.g. v1.38.0): %s\n' "${TO_VERSION}" >&2
  exit 1
fi

CLUSTER_NAME="${CLUSTER_NAME:-k8labs}"
CLUSTER_NAMESPACE="${CLUSTER_NAMESPACE:-default}"
UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_TIMEOUT:-3600}"
if [[ ! "${UPGRADE_WAIT_TIMEOUT}" =~ ^[0-9]+$ ]]; then
  printf 'ERROR: UPGRADE_WAIT_TIMEOUT must be a non-negative integer: %s\n' "${UPGRADE_WAIT_TIMEOUT}" >&2
  exit 1
fi

readonly PLAN_NAME="${CLUSTER_NAME}-upgrade-${TO_VERSION}"
readonly PLAN_GROUP="controlplane.cluster.x-k8s.io"
readonly PLAN_VERSION="v1alpha1"
readonly PLAN_KIND="HypervisorUpgradePlan"
readonly POLL_INTERVAL=5

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
  local deadline=$(( $(date +%s) + UPGRADE_WAIT_TIMEOUT ))
  until "${predicate}"; do
    if (( $(date +%s) >= deadline )); then
      printf 'ERROR: %s: timed out after %ss waiting for %s\n' \
        "${step}" "${UPGRADE_WAIT_TIMEOUT}" "${description}" >&2
      exit 1
    fi
    sleep "${POLL_INTERVAL}"
  done
}

# mgmt <args...> — kubectl against the management cluster. Failures are the
# caller's problem: predicates swallow them, steps abort on them.
mgmt() {
  kubectl --kubeconfig="${KUBECONFIG}" "$@"
}

printf '%s\n' "=== upgrade: ${CLUSTER_NAME}/${CLUSTER_NAMESPACE} to ${TO_VERSION} ==="

# Step 1: preflight — the plan CRD must be installed, the Cluster must exist,
# and the HypervisorCluster must register a base image for the target
# version. Failing here is a usage error, not a timeout: the lab cannot run
# this upgrade.
printf '%s\n' "--- preflight: checking the upgrade prerequisites ---"
mgmt get crd hypervisorupgradeplans.controlplane.cluster.x-k8s.io >/dev/null 2>&1 \
  || { printf 'ERROR: preflight: the HypervisorUpgradePlan CRD is not installed on the management cluster\n' >&2; exit 1; }
mgmt get cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" >/dev/null 2>&1 \
  || { printf 'ERROR: preflight: Cluster %s/%s not found\n' "${CLUSTER_NAMESPACE}" "${CLUSTER_NAME}" >&2; exit 1; }
FROM_VERSION="$(mgmt get cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
  -o jsonpath='{.spec.topology.version}' 2>/dev/null || true)"
if [[ -z "${FROM_VERSION}" ]]; then
  printf 'ERROR: preflight: Cluster %s/%s carries no topology version\n' "${CLUSTER_NAMESPACE}" "${CLUSTER_NAME}" >&2
  exit 1
fi
if [[ "${FROM_VERSION}" == "${TO_VERSION}" ]]; then
  printf 'ERROR: preflight: Cluster %s/%s already runs %s\n' "${CLUSTER_NAMESPACE}" "${CLUSTER_NAME}" "${TO_VERSION}" >&2
  exit 1
fi
REGISTERED_IMAGES="$(mgmt get hypervisorcluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
  -o jsonpath='{.spec.images[*].version}' 2>/dev/null || true)"
if [[ " ${REGISTERED_IMAGES} " != *" ${TO_VERSION} "* ]]; then
  printf 'ERROR: preflight: no base image registered for %s on HypervisorCluster %s/%s (registered: %s)\n' \
    "${TO_VERSION}" "${CLUSTER_NAMESPACE}" "${CLUSTER_NAME}" "${REGISTERED_IMAGES}" >&2
  exit 1
fi
printf '%s\n' "preflight: ${FROM_VERSION} -> ${TO_VERSION}, base image registered"

# Step 2: plan — apply the HypervisorUpgradePlan and wait until it leaves the
# in-flight phases. A Failed plan fails the scenario with the plan's own
# failure reason and message, so the lab failure points at the cause.
printf '%s\n' "--- plan: applying HypervisorUpgradePlan ${PLAN_NAME} ---"
mgmt apply -f - >/dev/null 2>&1 <<EOF \
  || { printf 'ERROR: plan: failed to apply HypervisorUpgradePlan %s\n' "${PLAN_NAME}" >&2; exit 1; }
apiVersion: ${PLAN_GROUP}/${PLAN_VERSION}
kind: ${PLAN_KIND}
metadata:
  name: ${PLAN_NAME}
  namespace: ${CLUSTER_NAMESPACE}
spec:
  clusterName: ${CLUSTER_NAME}
  toVersion: ${TO_VERSION}
EOF

plan_phase() {
  mgmt get "${PLAN_KIND}" "${PLAN_NAME}" -n "${CLUSTER_NAMESPACE}" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true
}
plan_settled_ok() {
  local phase=""
  phase="$(plan_phase)"
  [[ "${phase}" == "Completed" || "${phase}" == "Failed" ]]
}
wait_for "plan" "the HypervisorUpgradePlan ${PLAN_NAME} to settle" plan_settled_ok
if [[ "$(plan_phase)" == "Failed" ]]; then
  REASON="$(mgmt get "${PLAN_KIND}" "${PLAN_NAME}" -n "${CLUSTER_NAMESPACE}" \
    -o jsonpath='{.status.failureReason}' 2>/dev/null || true)"
  MESSAGE="$(mgmt get "${PLAN_KIND}" "${PLAN_NAME}" -n "${CLUSTER_NAMESPACE}" \
    -o jsonpath='{.status.failureMessage}' 2>/dev/null || true)"
  printf 'ERROR: plan: HypervisorUpgradePlan %s failed (%s): %s\n' "${PLAN_NAME}" "${REASON}" "${MESSAGE}" >&2
  exit 1
fi
printf '%s\n' "plan: HypervisorUpgradePlan ${PLAN_NAME} Completed"

# Step 3: control-plane — the Cluster topology and the control-plane Machine
# must report the target version (the plan gates the workers on this, so by
# the time it completes this already holds; the check pins the state for the
# scenario log).
printf '%s\n' "--- control-plane: verifying the control plane runs ${TO_VERSION} ---"
control_plane_ok() {
  local topology="" cp_version=""
  topology="$(mgmt get cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAMESPACE}" \
    -o jsonpath='{.spec.topology.version}' 2>/dev/null || true)"
  cp_version="$(mgmt get machine -n "${CLUSTER_NAMESPACE}" \
    -l "cluster.x-k8s.io/cluster-name=${CLUSTER_NAME},cluster.x-k8s.io/control-plane" \
    -o jsonpath='{.items[0].spec.version}' 2>/dev/null || true)"
  [[ "${topology}" == "${TO_VERSION}" && "${cp_version}" == "${TO_VERSION}" ]]
}
wait_for "control-plane" "the control plane to report ${TO_VERSION}" control_plane_ok
printf '%s\n' "control-plane: topology and control-plane Machine report ${TO_VERSION}"

# Step 4: workers — every worker Machine must report the target version.
printf '%s\n' "--- workers: verifying the workers run ${TO_VERSION} ---"
workers_ok() {
  local versions=""
  versions="$(mgmt get machine -n "${CLUSTER_NAMESPACE}" \
    -l "cluster.x-k8s.io/cluster-name=${CLUSTER_NAME}" \
    -o jsonpath='{.items[?(@.metadata.labels.cluster\.x-k8s\.io/control-plane==null)].spec.version}' \
    2>/dev/null || true)"
  if [[ -z "${versions}" ]]; then
    return 1
  fi
  # The versions come back space-separated; split on spaces explicitly
  # because the script-wide IFS has no space.
  local IFS=$' \t\n'
  local version=""
  for version in ${versions}; do
    if [[ "${version}" != "${TO_VERSION}" ]]; then
      return 1
    fi
  done
  return 0
}
wait_for "workers" "every worker Machine to report ${TO_VERSION}" workers_ok
printf '%s\n' "workers: every worker Machine reports ${TO_VERSION}"

# Step 5: nodes — extract the workload kubeconfig and poll until every node
# is Ready and its kubelet reports the target version.
printf '%s\n' "--- nodes: waiting for the workload nodes on ${TO_VERSION} ---"
WORKLOAD_KC="$(mktemp)"
trap 'rm -f -- "${WORKLOAD_KC}"' EXIT
mgmt get secret "${CLUSTER_NAME}-kubeconfig" -n "${CLUSTER_NAMESPACE}" \
  -o jsonpath='{.data.value}' 2>/dev/null \
  | base64 -d > "${WORKLOAD_KC}" 2>/dev/null \
  || { printf 'ERROR: failed to extract the workload kubeconfig from the %s-kubeconfig Secret\n' "${CLUSTER_NAME}" >&2; exit 1; }
chmod 600 "${WORKLOAD_KC}"

nodes_ok() {
  local nodes=""
  nodes="$(kubectl get nodes --kubeconfig="${WORKLOAD_KC}" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status} {.status.nodeInfo.kubeletVersion}{"\n"}{end}' \
    2>/dev/null || true)"
  if [[ -z "${nodes}" ]]; then
    return 1
  fi
  local line="" ready="" kubelet=""
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    ready="$(awk '{print $2}' <<< "${line}" || true)"
    kubelet="$(awk '{print $3}' <<< "${line}" || true)"
    if [[ "${ready}" != "True" || "${kubelet}" != "${TO_VERSION}" ]]; then
      return 1
    fi
  done <<< "${nodes}"
  return 0
}
wait_for "nodes" "every workload node to be Ready on ${TO_VERSION}" nodes_ok
printf '%s\n' "nodes: every workload node Ready on ${TO_VERSION}"

printf '%s\n' "PASS: upgrade scenario complete (${FROM_VERSION} -> ${TO_VERSION})"
exit 0
