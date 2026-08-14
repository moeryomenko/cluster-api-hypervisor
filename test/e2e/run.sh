#!/usr/bin/env bash
#
# run.sh — full-lab e2e orchestration for cluster-api-hypervisor.
#
# Brings up the management plane (or consumes an external one), installs the
# provider quadlet through the committed mgmt bootstrap (test/e2e/mgmt), applies
# the committed ClusterClass + example Cluster to the management cluster, waits
# for the workload Machines to become Ready, runs the workload smoke checks
# (test/e2e/smoke.sh), and tears everything down again via a trap.
#
# Environment validation happens before any heavy work: no cluster, VM, or
# quadlet is started unless every variable below is valid.
#
# Environment:
#   MANAGEMENT_KUBECONFIG  management-cluster kubeconfig. When set, it must
#                          name an existing, readable, non-empty file and is
#                          used directly; the management plane is left
#                          untouched and is not torn down on exit. When unset
#                          or empty, the harness falls back to the committed
#                          management-plane bootstrap (test/e2e/mgmt), driven
#                          by its state directory (MGMT_STATE_DIR, default
#                          /var/lib/k8slab/mgmt); the state must be provisioned
#                          with the admin kubeconfig at
#                          <state>/kubeconfigs/admin.conf.
#   MGMT_STATE_DIR         management-plane state directory for the fallback
#                          (default /var/lib/k8slab/mgmt).
#   IMAGE                  provider image reference (default
#                          cluster-api-hypervisor:dev, the Makefile tag). A set
#                          value must be a syntactically plausible container
#                          reference (no whitespace).
#   BASE_IMAGE             k8labs base image path (default
#                          build/k8labs-base.qcow2, resolved against the working
#                          directory the harness is invoked from). Must name an
#                          existing, readable, regular file.
#   FIRMWARE               CLOUDHV.fd path (default build/CLOUDHV.fd, resolved
#                          against the working directory the harness is invoked
#                          from). Must name an existing, readable, regular
#                          file.
#   STATE_DIR              provider state directory (default /var/lib/k8slab).
#                          Must name an existing, writable directory.
#   OUT_DIR                provider release layout directory (default
#                          <repo>/out). Must name an existing directory
#                          containing the three provider release directories
#                          infrastructure-hypervisor/v0.1.0,
#                          bootstrap-hypervisor/v0.1.0, and
#                          control-plane-hypervisor/v0.1.0.
#   SMOKE                  run the workload smoke checks (test/e2e/smoke.sh)
#                          after the Machines are Ready; 0 disables them,
#                          anything else enables them (default 1). When
#                          smoke.sh is not present yet the checks are skipped
#                          with a note.
#   WAIT_TIMEOUT           seconds to wait for the workload Machines to become
#                          Ready (default 1800).
#
# Exit codes:
#   0  full-lab run completed (including teardown) or --help was requested
#   1  environment validation failure (before any heavy work) or orchestration
#      failure
#
# Usage:
#   test/e2e/run.sh [--help|-h]

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"
readonly REPO_ROOT

# Defaults pinned by the environment contract (the Makefile tag and the
# provider environment contract values from docs/install-contract.md).
readonly IMAGE_DEFAULT="cluster-api-hypervisor:dev"
readonly BASE_IMAGE_DEFAULT="build/k8labs-base.qcow2"
readonly FIRMWARE_DEFAULT="build/CLOUDHV.fd"
readonly STATE_DIR_DEFAULT="/var/lib/k8slab"
readonly MGMT_STATE_DIR_DEFAULT="/var/lib/k8slab/mgmt"
readonly MGMT_BOOTSTRAP_DIR="test/e2e/mgmt"

# Provider release layout validated by OUT_DIR (the layout `make components
# OUT_DIR=` emits; the version directory is pinned by the release contract).
readonly OUT_DIR_DEFAULT="${REPO_ROOT}/out"
readonly OUT_PROVIDER_DIRS=(
  "infrastructure-hypervisor"
  "bootstrap-hypervisor"
  "control-plane-hypervisor"
)
readonly OUT_PROVIDER_VERSION="v0.1.0"

# The example Cluster (templates/cluster-example.yaml) fixed identity.
readonly WORKLOAD_CLUSTER="k8labs"
readonly WORKLOAD_NAMESPACE="default"

readonly APISERVER_READY_TIMEOUT=300
readonly MACHINES_READY_TIMEOUT_DEFAULT=1800
readonly TEARDOWN_CLUSTER_TIMEOUT=300
readonly POLL_INTERVAL=5

# Runtime state resolved by validate_environment and consumed by the
# orchestration and the cleanup trap. The contract variables themselves
# (MANAGEMENT_KUBECONFIG, MGMT_STATE_DIR, IMAGE, BASE_IMAGE, FIRMWARE,
# STATE_DIR, OUT_DIR, SMOKE, WAIT_TIMEOUT) are deliberately NOT
# pre-initialized here:
# a top-level assignment would clobber the value the environment provides.
MGMT_KUBECONFIG=""
MGMT_EXTERNAL=false
BASE_IMAGE_RESOLVED=""
FIRMWARE_RESOLVED=""
STATE_DIR_RESOLVED=""
OUT_DIR_RESOLVED=""
PLANE_STARTED=false
CLUSTER_APPLIED=false
WORKLOAD_KUBECONFIG=""

log() { printf 'run: %s\n' "$*" >&2; }

die() {
  printf 'run: error: %s\n' "$*" >&2
  exit 1
}

# usage — print the full environment contract and exit. Must never depend on
# validation passing: --help works from a completely empty environment.
usage() {
  cat <<'EOF'
test/e2e/run.sh — full-lab e2e orchestration for cluster-api-hypervisor

Brings up the management plane (or consumes an external one), installs the
provider quadlet through the committed mgmt bootstrap (test/e2e/mgmt), applies
the committed ClusterClass + example Cluster, waits for the workload Machines
to be Ready, runs the workload smoke checks (test/e2e/smoke.sh), and tears
everything down again via a trap.

Environment validation happens before any heavy work: no cluster, VM, or
quadlet is started unless every variable below is valid.

Environment:

  MANAGEMENT_KUBECONFIG  management-cluster kubeconfig. When set, it must name
                         an existing, readable, non-empty file and is used
                         directly; the management plane is left untouched and
                         is not torn down on exit. When unset or empty, the
                         harness falls back to the committed management-plane
                         bootstrap (test/e2e/mgmt), driven by its state
                         directory (MGMT_STATE_DIR, default
                         /var/lib/k8slab/mgmt); the state must be provisioned
                         with the admin kubeconfig at
                         <state>/kubeconfigs/admin.conf.
  MGMT_STATE_DIR         management-plane state directory for the fallback
                         (default /var/lib/k8slab/mgmt).
  IMAGE                  provider image reference (default
                         cluster-api-hypervisor:dev). A set value must be a
                         syntactically plausible container reference (no
                         whitespace).
  BASE_IMAGE             k8labs base image path (default
                         build/k8labs-base.qcow2, resolved against the working
                         directory the harness is invoked from). Must name an
                         existing, readable, regular file.
  FIRMWARE               CLOUDHV.fd path (default build/CLOUDHV.fd, resolved
                         against the working directory the harness is invoked
                         from). Must name an existing, readable, regular file.
  STATE_DIR              provider state directory (default /var/lib/k8slab).
                         Must name an existing, writable directory.
  OUT_DIR                provider release layout directory (default
                         <repo>/out). Must name an existing directory
                         containing the three provider release directories
                         infrastructure-hypervisor/v0.1.0,
                         bootstrap-hypervisor/v0.1.0, and
                         control-plane-hypervisor/v0.1.0.
  SMOKE                  run the workload smoke checks (test/e2e/smoke.sh)
                         after the Machines are Ready; 0 disables them,
                         anything else enables them (default 1). When smoke.sh
                         is not present yet the checks are skipped with a note.
  WAIT_TIMEOUT           seconds to wait for the workload Machines to become
                         Ready (default 1800).

Options:
  -h, --help             print this help and exit

Exit codes:
  0  full-lab run completed (including teardown) or --help was requested
  1  environment validation failure (before any heavy work) or orchestration
     failure
EOF
  exit "${1:-0}"
}

# absolute_path <path> — print the path resolved against the working directory
# when relative; the contract resolves the relative defaults (build/...) from
# the directory the harness is invoked from.
absolute_path() {
  local p="$1"
  if [[ "${p}" == /* ]]; then
    printf '%s\n' "${p}"
  else
    printf '%s/%s\n' "${PWD}" "${p}"
  fi
}

# validate_environment — check every contract variable before any heavy work.
# Every failure exits 1 naming the exact variable (and the offending path).
validate_environment() {
  # 1. MANAGEMENT_KUBECONFIG, or the mgmt bootstrap fallback.
  if [[ -n "${MANAGEMENT_KUBECONFIG:-}" ]]; then
    if [[ -f "${MANAGEMENT_KUBECONFIG}" && -r "${MANAGEMENT_KUBECONFIG}" && -s "${MANAGEMENT_KUBECONFIG}" ]]; then
      MGMT_KUBECONFIG="${MANAGEMENT_KUBECONFIG}"
      MGMT_EXTERNAL=true
    else
      die "MANAGEMENT_KUBECONFIG must name an existing, readable, non-empty file: ${MANAGEMENT_KUBECONFIG}"
    fi
  else
    MGMT_STATE_DIR="${MGMT_STATE_DIR:-${MGMT_STATE_DIR_DEFAULT}}"
    local admin_kubeconfig="${MGMT_STATE_DIR}/kubeconfigs/admin.conf"
    if [[ -f "${admin_kubeconfig}" && -r "${admin_kubeconfig}" && -s "${admin_kubeconfig}" ]]; then
      MGMT_KUBECONFIG="${admin_kubeconfig}"
      MGMT_EXTERNAL=false
    else
      die "MANAGEMENT_KUBECONFIG is not set and the mgmt bootstrap fallback is not provisioned (expected ${admin_kubeconfig}; provision the state via ${MGMT_BOOTSTRAP_DIR}/pki.sh)"
    fi
  fi

  # 2. IMAGE — unset/empty defaults to the Makefile tag; a set value must be
  #    a syntactically plausible container reference (no whitespace).
  IMAGE="${IMAGE:-${IMAGE_DEFAULT}}"
  if [[ "${IMAGE}" =~ [[:space:]] ]]; then
    die "IMAGE must be a syntactically plausible container image reference (no whitespace): ${IMAGE}"
  fi

  # 3. BASE_IMAGE — existing, readable, regular file.
  BASE_IMAGE_RESOLVED="$(absolute_path "${BASE_IMAGE:-${BASE_IMAGE_DEFAULT}}")"
  if [[ ! -f "${BASE_IMAGE_RESOLVED}" ]]; then
    die "BASE_IMAGE must name an existing file: ${BASE_IMAGE_RESOLVED}"
  fi
  if [[ ! -r "${BASE_IMAGE_RESOLVED}" ]]; then
    die "BASE_IMAGE must be readable: ${BASE_IMAGE_RESOLVED}"
  fi

  # 4. FIRMWARE — existing, readable, regular file.
  FIRMWARE_RESOLVED="$(absolute_path "${FIRMWARE:-${FIRMWARE_DEFAULT}}")"
  if [[ ! -f "${FIRMWARE_RESOLVED}" ]]; then
    die "FIRMWARE must name an existing file: ${FIRMWARE_RESOLVED}"
  fi
  if [[ ! -r "${FIRMWARE_RESOLVED}" ]]; then
    die "FIRMWARE must be readable: ${FIRMWARE_RESOLVED}"
  fi

  # 5. STATE_DIR — existing, writable directory, not a regular file.
  STATE_DIR_RESOLVED="$(absolute_path "${STATE_DIR:-${STATE_DIR_DEFAULT}}")"
  if [[ -e "${STATE_DIR_RESOLVED}" && ! -d "${STATE_DIR_RESOLVED}" ]]; then
    die "STATE_DIR must be a directory, not a regular file: ${STATE_DIR_RESOLVED}"
  fi
  if [[ ! -d "${STATE_DIR_RESOLVED}" ]]; then
    die "STATE_DIR must name an existing directory: ${STATE_DIR_RESOLVED}"
  fi
  if [[ ! -w "${STATE_DIR_RESOLVED}" ]]; then
    die "STATE_DIR must be writable: ${STATE_DIR_RESOLVED}"
  fi

  # 6. OUT_DIR — existing directory holding the provider release layout (the
  #    three v0.1.0 provider directories) so `go tool clusterctl generate
  #    cluster` can resolve the cluster template from the local repository.
  OUT_DIR_RESOLVED="$(absolute_path "${OUT_DIR:-${OUT_DIR_DEFAULT}}")"
  if [[ -e "${OUT_DIR_RESOLVED}" && ! -d "${OUT_DIR_RESOLVED}" ]]; then
    die "OUT_DIR must be a directory, not a regular file: ${OUT_DIR_RESOLVED}"
  fi
  if [[ ! -d "${OUT_DIR_RESOLVED}" ]]; then
    die "OUT_DIR must name an existing directory: ${OUT_DIR_RESOLVED}"
  fi
  for provider_dir in "${OUT_PROVIDER_DIRS[@]}"; do
    if [[ ! -d "${OUT_DIR_RESOLVED}/${provider_dir}/${OUT_PROVIDER_VERSION}" ]]; then
      die "OUT_DIR incomplete: missing ${OUT_DIR_RESOLVED}/${provider_dir}/${OUT_PROVIDER_VERSION}"
    fi
  done

  log "environment validated: kubeconfig=${MGMT_KUBECONFIG} image=${IMAGE} base_image=${BASE_IMAGE_RESOLVED} firmware=${FIRMWARE_RESOLVED} state_dir=${STATE_DIR_RESOLVED} out_dir=${OUT_DIR_RESOLVED}"
}

# require_cmd <name> — fail with a clear message when a tool the orchestration
# body needs is missing.
require_cmd() {
  local name="$1"
  command -v "${name}" >/dev/null 2>&1 \
    || die "required tool not found: ${name} (install it or fix PATH)"
}

# mgmt_up — bring the management plane up through the committed bootstrap
# unless an external kubeconfig was given; the mgmt apply flow installs the
# provider quadlet (test/e2e/mgmt/units/cluster-api-hypervisor.quadlet) so the
# provider runs against the management apiserver.
mgmt_up() {
  if [[ "${MGMT_EXTERNAL}" == true ]]; then
    log "using external management kubeconfig ${MGMT_KUBECONFIG}"
    return 0
  fi
  log "bringing the management plane up via ${MGMT_BOOTSTRAP_DIR}/apply.sh"
  export MGMT_STATE_DIR
  bash "${SCRIPT_DIR}/mgmt/apply.sh"
  PLANE_STARTED=true
  log "management plane is up (state: ${MGMT_STATE_DIR})"
}

# wait_for_apiserver_ready — poll the management apiserver until /readyz
# answers ok (the bare plane needs a moment for etcd + apiserver to come up).
wait_for_apiserver_ready() {
  local deadline=$(( $(date +%s) + APISERVER_READY_TIMEOUT ))
  log "waiting for the management apiserver to be ready"
  until kubectl get --kubeconfig="${MGMT_KUBECONFIG}" --raw='/readyz' >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      die "management apiserver did not become ready within ${APISERVER_READY_TIMEOUT}s"
    fi
    sleep 2
  done
  log "management apiserver is ready"
}

# wait_for_provider — poll the provider quadlet service (fallback plane only)
# until systemd reports it active.
wait_for_provider() {
  [[ "${MGMT_EXTERNAL}" == false ]] || return 0
  local deadline=$(( $(date +%s) + APISERVER_READY_TIMEOUT ))
  log "waiting for the provider quadlet (mgmt-cluster-api-hypervisor) to start"
  until systemctl is-active --quiet mgmt-cluster-api-hypervisor; do
    if (( $(date +%s) >= deadline )); then
      die "provider quadlet mgmt-cluster-api-hypervisor did not become active (check 'systemctl status mgmt-cluster-api-hypervisor')"
    fi
    sleep 2
  done
  log "provider quadlet is active"
}

# apply_templates — generate the workload Cluster with clusterctl and apply it
# to the management cluster. clusterctl reads its configuration from
# $XDG_CONFIG_HOME/cluster-api/clusterctl.yaml; the fallback plane provisioned
# a hermetic config plus offline core overrides at <state>/clusterctl (see
# test/e2e/mgmt/apply.sh), so XDG_CONFIG_HOME points there, and with an
# external plane it points at an OUT_DIR-derived directory (clusterctl still
# works when the config file is absent). Declarative, so re-running converges.
apply_templates() {
  CLUSTER_APPLIED=true
  if [[ "${MGMT_EXTERNAL}" == false ]]; then
    export XDG_CONFIG_HOME="${MGMT_STATE_DIR}/clusterctl"
  else
    export XDG_CONFIG_HOME="${OUT_DIR_RESOLVED}/.clusterctl"
  fi
  mkdir -p "${XDG_CONFIG_HOME}"
  log "generating Cluster ${WORKLOAD_CLUSTER} via clusterctl and applying it to the management cluster"
  go tool clusterctl generate cluster "${WORKLOAD_CLUSTER}" \
    --namespace "${WORKLOAD_NAMESPACE}" \
    --infrastructure hypervisor \
    --kubernetes-version v1.32.13 \
    --control-plane-machine-count 1 \
    --worker-machine-count 3 \
    | kubectl apply --kubeconfig="${MGMT_KUBECONFIG}" -f -
  log "Cluster ${WORKLOAD_CLUSTER} generated and applied"
}

# wait_for_machines_ready — poll the management cluster until every workload
# Machine reports the CAPI Ready condition True.
wait_for_machines_ready() {
  local deadline=$(( $(date +%s) + WAIT_TIMEOUT ))
  local names="" total=0 ready=0 statuses=""
  log "waiting up to ${WAIT_TIMEOUT}s for the workload Machines to be Ready"
  while (( $(date +%s) < deadline )); do
    names="$(kubectl get machine -n "${WORKLOAD_NAMESPACE}" --kubeconfig="${MGMT_KUBECONFIG}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
    total="$(wc -w <<< "${names}")"
    ready=0
    if (( total > 0 )); then
      statuses="$(kubectl get machine -n "${WORKLOAD_NAMESPACE}" --kubeconfig="${MGMT_KUBECONFIG}" \
        -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null || true)"
      ready="$(grep -c '^True$' <<< "${statuses}" || true)"
      log "machines: ${ready}/${total} Ready"
    else
      log "machines: none created yet"
    fi
    if (( total > 0 && ready == total )); then
      log "all ${total} workload Machines are Ready"
      return 0
    fi
    sleep "${POLL_INTERVAL}"
  done
  die "timed out after ${WAIT_TIMEOUT}s waiting for the workload Machines to be Ready (${ready}/${total})"
}

# extract_workload_kubeconfig — pull the <cluster>-kubeconfig Secret written by
# the CAPI core controller out of the management cluster into a temp file.
extract_workload_kubeconfig() {
  local kubeconfig=""
  kubeconfig="$(mktemp)"
  kubectl get secret "${WORKLOAD_CLUSTER}-kubeconfig" -n "${WORKLOAD_NAMESPACE}" \
    --kubeconfig="${MGMT_KUBECONFIG}" \
    -o jsonpath='{.data.value}' \
    | base64 -d > "${kubeconfig}"
  chmod 600 "${kubeconfig}"
  printf '%s\n' "${kubeconfig}"
}

# run_smoke — run the workload smoke checks against the workload cluster.
# Disabled by SMOKE=0, or skipped with a note when smoke.sh is not present yet.
run_smoke() {
  if [[ "${SMOKE}" == "0" ]]; then
    log "smoke: disabled (SMOKE=0); skipping the workload smoke checks"
    return 0
  fi
  local smoke_script="${SCRIPT_DIR}/smoke.sh"
  if [[ ! -x "${smoke_script}" ]]; then
    log "smoke: test/e2e/smoke.sh not present yet; skipping the workload smoke checks"
    return 0
  fi
  WORKLOAD_KUBECONFIG="$(extract_workload_kubeconfig)"
  log "running the workload smoke checks (${smoke_script})"
  KUBECONFIG="${WORKLOAD_KUBECONFIG}" bash "${smoke_script}" "${WORKLOAD_KUBECONFIG}"
  log "workload smoke checks passed"
}

# cleanup — teardown trap: remove the temp workload kubeconfig, delete the
# workload Cluster, and bring the mgmt plane down when the fallback started it.
# Best-effort: a failure never changes the run's exit code.
cleanup() {
  local rc=$?
  trap - EXIT
  if [[ "${CLUSTER_APPLIED}" == true ]]; then
    log "teardown: deleting the workload Cluster ${WORKLOAD_CLUSTER}"
    kubectl delete cluster "${WORKLOAD_CLUSTER}" -n "${WORKLOAD_NAMESPACE}" \
      --kubeconfig="${MGMT_KUBECONFIG}" --timeout="${TEARDOWN_CLUSTER_TIMEOUT}s" \
      >/dev/null 2>&1 \
      || log "teardown: workload Cluster deletion did not complete (continuing)"
  fi
  if [[ -n "${WORKLOAD_KUBECONFIG}" && -f "${WORKLOAD_KUBECONFIG}" ]]; then
    rm -f -- "${WORKLOAD_KUBECONFIG}"
  fi
  if [[ "${PLANE_STARTED}" == true && "${MGMT_EXTERNAL}" == false ]]; then
    log "teardown: bringing the management plane down"
    bash "${SCRIPT_DIR}/mgmt/down.sh" >/dev/null 2>&1 \
      || log "teardown: mgmt-down did not complete (continuing)"
  fi
  exit "${rc}"
}

# orchestrate — the full-lab flow. Runs only after the environment validated.
orchestrate() {
  SMOKE="${SMOKE:-1}"
  WAIT_TIMEOUT="${WAIT_TIMEOUT:-${MACHINES_READY_TIMEOUT_DEFAULT}}"
  trap cleanup EXIT

  # The body needs go for the clusterctl tool (go tool clusterctl) and kubectl
  # for the management-cluster work; the fallback also needs the mgmt
  # bootstrap's own tooling (apply.sh checks those itself).
  require_cmd go
  require_cmd kubectl
  require_cmd base64
  require_cmd mktemp

  mgmt_up
  wait_for_apiserver_ready
  wait_for_provider
  apply_templates
  wait_for_machines_ready
  run_smoke
  log "full-lab run complete"
}

main() {
  local arg=""
  for arg in "$@"; do
    case "${arg}" in
      -h|--help) usage 0 ;;
      *) die "unexpected argument: ${arg} (use --help for usage)" ;;
    esac
  done
  validate_environment
  orchestrate
}

main "$@"
