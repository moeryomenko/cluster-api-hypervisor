#!/usr/bin/env bash
#
# run.sh — full-lab e2e orchestration for cluster-api-hypervisor.
#
# Brings up the management plane (or consumes an external one), installs the
# provider quadlet through the committed mgmt bootstrap (test/e2e/mgmt),
# applies the committed ClusterClass + example Cluster to the management
# cluster, waits for the workload Machines to become Ready, verifies the
# k8netd dataplane end to end (network created through real k8netd, per-VM
# passt instances, API reachability via https://127.0.0.1:6443, guest-to-guest
# and internet reachability from inside the control-plane guest), runs the
# workload smoke checks (test/e2e/smoke.sh), tears the workload Cluster down,
# and verifies the teardown left the host clean while k8netd itself stays up.
#
# The script refuses to run unless E2E_LAB_HOST=1 is exported: it drives real
# KVM virtual machines, a real rootless network daemon, and real passt
# processes, binds host ports 6443 and 22, and writes daemon state under
# /run/user/1000/k8snet/. Run it only on the dedicated k8labs host.
#
# Environment validation and lab-host prerequisite gates happen before any
# heavy work: no cluster, VM, or quadlet is started unless every variable and
# every prerequisite below is satisfied.
#
# Environment:
#   E2E_LAB_HOST           lab-host-only guard. Must be exported as 1 or the
#                          script exits 1 before doing anything (--help is
#                          exempt). There is no other way to bypass the guard.
#   SKIP_PREREQS           test-only escape hatch. When exported as 1, the
#                          lab-host prerequisite gates (P1-P12) are skipped
#                          after environment validation; the environment
#                          contract itself is still enforced in full. This
#                          exists for test/e2e/harness_test.sh, which drives
#                          the flow with stubbed tooling on hosts without
#                          KVM, passt, or k8netd (gate P1 checks /dev/kvm
#                          directly and cannot be satisfied by PATH stubs).
#                          Never set this on a real lab run.
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
#   STATE_DIR              provider state directory (default
#                          ~/.local/state/k8slab, mirroring the provider's
#                          user-writable default; /tmp/k8slab-state when HOME
#                          is unset). Must name an existing, writable
#                          directory.
#   OUT_DIR                provider release layout directory (default
#                          <repo>/out). Must name an existing directory
#                          containing the three provider release directories
#                          infrastructure-hypervisor/v0.1.0,
#                          bootstrap-hypervisor/v0.1.0, and
#                          control-plane-hypervisor/v0.1.0.
#   K8NETD_SOCKET          k8netd JSON-RPC control socket (default
#                          /run/user/1000/k8snet/control.sock, the provider
#                          default HYPERVISOR_K8NETD_SOCKET). Must be an
#                          absolute path; liveness is checked by the
#                          prerequisites.
#   GUEST_SSH_KEY          SSH private key for the guest reachability probes.
#                          The key's public part must be provisioned into the
#                          guests (HypervisorConfig SSHPublicKey). Default
#                          ~/.ssh/id_k8labs; must name an existing, readable,
#                          regular file.
#   GUEST_SSH_USER         SSH user for the guest probes (default root).
#   SMOKE                  run the workload smoke checks (test/e2e/smoke.sh)
#                          after the dataplane gates pass; 0 disables them,
#                          anything else enables them (default 1). When
#                          smoke.sh is not present yet the checks are skipped
#                          with a note.
#   WAIT_TIMEOUT           seconds to wait for the workload Machines to become
#                          Ready (default 1800).
#
# Exit codes:
#   0  full-lab run completed (including teardown) or --help was requested
#   1  lab-host guard refusal, environment validation failure, prerequisite
#      failure (all before any heavy work), or orchestration failure
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
readonly MGMT_STATE_DIR_DEFAULT="/var/lib/k8slab/mgmt"
readonly MGMT_BOOTSTRAP_DIR="test/e2e/mgmt"

# STATE_DIR default mirrors the provider's user-writable default
# (internal/config defaultStateDir): $HOME/.local/state/k8slab, falling back
# to /tmp/k8slab-state when HOME is unset.
STATE_DIR_DEFAULT="${HOME:+${HOME}/.local/state/k8slab}"
STATE_DIR_DEFAULT="${STATE_DIR_DEFAULT:-/tmp/k8slab-state}"

# Provider release layout validated by OUT_DIR (the layout `make components
# OUT_DIR=` emits; the version directory is pinned by the release contract).
readonly OUT_DIR_DEFAULT="${REPO_ROOT}/out"
readonly OUT_PROVIDER_DIRS=(
  "infrastructure-hypervisor"
  "bootstrap-hypervisor"
  "control-plane-hypervisor"
)
readonly OUT_PROVIDER_VERSION="v0.1.0"

# k8netd contract constants (.specs/k8netd-contract/spec.md, K8NETD-CTR-001):
# the control socket default matches HYPERVISOR_K8NETD_SOCKET, the RPC version
# matches internal/k8netd's k8netdVersion, and both units are user units
# (rootless install contract, REQ-008).
readonly K8NETD_SOCKET_DEFAULT="/run/user/1000/k8snet/control.sock"
# The contract RPC version ("1.0") is pinned inside build_k8netd_probe's
# embedded Go program, matching internal/k8netd's k8netdVersion.
readonly K8NETD_UNIT="k8netd.service"
readonly PROVIDER_UNIT="mgmt-cluster-api-hypervisor"

# passt inbound forwards on the lab host (contract REQ-008 defaults: the
# control-plane passt forwards host 6443 -> guest 6443 and host 22 -> guest 22).
readonly HOST_API_PORT=6443
readonly HOST_SSH_PORT=22

# Lab-host-only guard: the scenario must be explicitly confirmed.
readonly LAB_HOST_GUARD_VAR="E2E_LAB_HOST"
readonly LAB_HOST_GUARD_VALUE="1"

# The example Cluster (templates/cluster-example.yaml) fixed identity and the
# topology the ClusterClass generates: 1 control plane + 3 workers.
readonly WORKLOAD_CLUSTER="k8labs"
readonly WORKLOAD_NAMESPACE="default"
readonly EXPECTED_MACHINE_COUNT=4

readonly APISERVER_READY_TIMEOUT=300
readonly MACHINES_READY_TIMEOUT_DEFAULT=1800
readonly TEARDOWN_CLUSTER_TIMEOUT=300
readonly POLL_INTERVAL=5
readonly SSH_PROBE_TIMEOUT=120

# Runtime state resolved by validate_environment and consumed by the
# orchestration and the cleanup trap. The contract variables themselves are
# deliberately NOT pre-initialized here: a top-level assignment would clobber
# the value the environment provides.
MGMT_KUBECONFIG=""
MGMT_EXTERNAL=false
BASE_IMAGE_RESOLVED=""
FIRMWARE_RESOLVED=""
STATE_DIR_RESOLVED=""
OUT_DIR_RESOLVED=""
K8NETD_SOCKET_RESOLVED=""
GUEST_SSH_KEY_RESOLVED=""
PLANE_STARTED=false
CLUSTER_APPLIED=false
WORKLOAD_KUBECONFIG=""
PROBE_DIR=""
PROBE_BIN=""
NETWORK_GATEWAY=""
TEARDOWN_DONE=false
declare -a MACHINE_INFRA_NAMES=()
declare -a MACHINE_INTERNAL_IPS=()
declare -a MACHINE_IS_CONTROL_PLANE=()
declare -a WORKER_IPS=()

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
to be Ready, verifies the k8netd dataplane (real k8netd network, per-VM passt,
API via https://127.0.0.1:6443, guest-to-guest and internet probes over SSH
into the control-plane guest), runs the workload smoke checks
(test/e2e/smoke.sh), tears the workload Cluster down, and verifies the host is
clean afterwards while k8netd stays up.

The script refuses to run unless E2E_LAB_HOST=1 is exported: it drives real
KVM VMs, k8netd, and passt, and binds host ports 6443 and 22. Run it only on
the dedicated k8labs host.

Environment validation happens before any heavy work: no cluster, VM, or
quadlet is started unless every variable below is valid.

Environment:

  E2E_LAB_HOST           lab-host-only guard; must be exported as 1.
  SKIP_PREREQS           test-only escape hatch (harness_test.sh); when
                         exported as 1 the lab-host prerequisite gates
                         (P1-P12) are skipped after environment validation.
                         Never set this on a real lab run.
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
  STATE_DIR              provider state directory (default
                         ~/.local/state/k8slab; /tmp/k8slab-state when HOME is
                         unset). Must name an existing, writable directory.
  OUT_DIR                provider release layout directory (default
                         <repo>/out). Must name an existing directory
                         containing the three provider release directories
                         infrastructure-hypervisor/v0.1.0,
                         bootstrap-hypervisor/v0.1.0, and
                         control-plane-hypervisor/v0.1.0.
  K8NETD_SOCKET          k8netd JSON-RPC control socket (default
                         /run/user/1000/k8snet/control.sock). Must be an
                         absolute path; liveness is checked by the
                         prerequisites.
  GUEST_SSH_KEY          SSH private key for the guest probes (default
                         ~/.ssh/id_k8labs). Must name an existing, readable,
                         regular file whose public part is provisioned into
                         the guests.
  GUEST_SSH_USER         SSH user for the guest probes (default root).
  SMOKE                  run the workload smoke checks (smoke.sh) after the
                         dataplane gates pass; 0 disables them, anything else
                         enables them (default 1). When smoke.sh is not
                         present yet the checks are skipped with a note.
  WAIT_TIMEOUT           seconds to wait for the workload Machines to become
                         Ready (default 1800).

Options:
  -h, --help             print this help and exit

Exit codes:
  0  full-lab run completed (including teardown) or --help was requested
  1  lab-host guard refusal, environment validation failure, prerequisite
     failure (all before any heavy work), or orchestration failure
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

# require_lab_host — refuse to run unless the operator explicitly confirmed
# the dedicated lab host via E2E_LAB_HOST=1. This is the first gate after
# argument parsing; --help is exempt because documentation mutates nothing.
require_lab_host() {
  if [[ "${!LAB_HOST_GUARD_VAR:-}" != "${LAB_HOST_GUARD_VALUE}" ]]; then
    die "refusing to run: ${LAB_HOST_GUARD_VAR} must be exported as ${LAB_HOST_GUARD_VALUE} — this scenario boots real VMs through k8netd and passt and binds host ports ${HOST_API_PORT} and ${HOST_SSH_PORT}; run it only on the dedicated k8labs host (see test/e2e/README.md)"
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

  # 7. K8NETD_SOCKET — absolute path; liveness is checked by the
  #    prerequisites once the daemon is expected to be running.
  K8NETD_SOCKET_RESOLVED="${K8NETD_SOCKET:-${K8NETD_SOCKET_DEFAULT}}"
  if [[ "${K8NETD_SOCKET_RESOLVED}" != /* ]]; then
    die "K8NETD_SOCKET must be an absolute path: ${K8NETD_SOCKET_RESOLVED}"
  fi

  # 8. GUEST_SSH_KEY — existing, readable, regular file (the public part must
  #    be provisioned into the guests via HypervisorConfig SSHPublicKey).
  GUEST_SSH_KEY_RESOLVED="$(absolute_path "${GUEST_SSH_KEY:-${HOME}/.ssh/id_k8labs}")"
  if [[ ! -f "${GUEST_SSH_KEY_RESOLVED}" ]]; then
    die "GUEST_SSH_KEY must name an existing file: ${GUEST_SSH_KEY_RESOLVED}"
  fi
  if [[ ! -r "${GUEST_SSH_KEY_RESOLVED}" ]]; then
    die "GUEST_SSH_KEY must be readable: ${GUEST_SSH_KEY_RESOLVED}"
  fi

  # 9. GUEST_SSH_USER — non-empty.
  GUEST_SSH_USER="${GUEST_SSH_USER:-root}"
  if [[ -z "${GUEST_SSH_USER}" ]]; then
    die "GUEST_SSH_USER must not be empty"
  fi

  log "environment validated: kubeconfig=${MGMT_KUBECONFIG} image=${IMAGE} base_image=${BASE_IMAGE_RESOLVED} firmware=${FIRMWARE_RESOLVED} state_dir=${STATE_DIR_RESOLVED} out_dir=${OUT_DIR_RESOLVED} k8netd_socket=${K8NETD_SOCKET_RESOLVED} guest_ssh_key=${GUEST_SSH_KEY_RESOLVED}"
}

# require_cmd <name> — fail with a clear message when a tool the orchestration
# body needs is missing.
require_cmd() {
  local name="$1"
  command -v "${name}" >/dev/null 2>&1 \
    || die "required tool not found: ${name} (install it or fix PATH)"
}

# listener_addresses <port> — print the local addresses of listening TCP
# sockets bound to exactly <port> (one per line), or nothing.
listener_addresses() {
  local port="$1"
  ss -ltn 2>/dev/null | awk -v suffix=":${port}" 'NR > 1 && $4 ~ suffix"$" {print $4}' || true
}

# check_prerequisites — lab-host gates P1-P12 (TASK-021 scenario design),
# evaluated after the environment validated and before any heavy work: no
# cluster, VM, or quadlet is started when any gate fails. P7 (base image +
# firmware) and P9 (release tree) are covered by validate_environment above;
# P10 (tools) is enforced here plus in orchestrate.
check_prerequisites() {
  # Test-mode escape hatch (SKIP_PREREQS in the header): harness_test.sh
  # drives the flow with stubbed tooling where the lab-host gates cannot pass
  # (P1 checks /dev/kvm directly, which no PATH stub can satisfy). The
  # environment contract above still applied in full.
  if [[ "${SKIP_PREREQS:-}" == "1" ]]; then
    log "SKIP_PREREQS=1: skipping the lab-host prerequisite gates (test mode)"
    return 0
  fi

  # P1: KVM available to the lab user.
  if [[ ! -w /dev/kvm ]] || ! id -nG | grep -qw kvm; then
    die "prerequisite P1 failed: /dev/kvm must be writable and the lab user must be in the kvm group"
  fi

  # P2: passt installed.
  command -v passt >/dev/null 2>&1 || die "prerequisite P2 failed: passt not found on PATH"
  passt --version >/dev/null 2>&1 || die "prerequisite P2 failed: passt --version did not run"

  # P3: k8netd binary present.
  command -v k8netd >/dev/null 2>&1 || die "prerequisite P3 failed: k8netd not found on PATH"

  # P4: k8netd user quadlet running.
  if [[ "$(systemctl --user is-active "${K8NETD_UNIT}" 2>/dev/null || true)" != "active" ]]; then
    die "prerequisite P4 failed: user unit ${K8NETD_UNIT} is not active (check 'systemctl --user status ${K8NETD_UNIT}')"
  fi

  # P5: control socket live, not stale — the JSON-RPC probe must answer; a
  # not_found for the (not yet created) workload network proves the socket is
  # live and the contract version is accepted.
  if [[ ! -S "${K8NETD_SOCKET_RESOLVED}" ]]; then
    die "prerequisite P5 failed: k8netd control socket is not a socket: ${K8NETD_SOCKET_RESOLVED}"
  fi
  build_k8netd_probe
  local probe_out="" probe_rc=0
  probe_out="$(k8netd_get_network)" || probe_rc=$?
  case "${probe_rc}" in
    0) log "prerequisite P5: k8netd control socket answered (unexpected network present: ${probe_out})" ;;
    4)
      if [[ "${probe_out}" == err\ not_found* ]]; then
        log "prerequisite P5: k8netd control socket answered, contract version accepted"
      else
        die "prerequisite P5 failed: k8netd rejected the probe: ${probe_out}"
      fi
      ;;
    *)
      die "prerequisite P5 failed: k8netd control socket did not answer: ${probe_out}"
      ;;
  esac

  # P6: podman unprivileged + user quadlets reachable.
  podman info >/dev/null 2>&1 || die "prerequisite P6 failed: podman info did not succeed (unprivileged podman required)"
  systemctl --user list-units --no-pager >/dev/null 2>&1 \
    || die "prerequisite P6 failed: systemctl --user is not usable"

  # P8: provider image built (the quadlet references the localhost tag).
  if ! podman image exists "${IMAGE}" && ! podman image exists "localhost/${IMAGE}"; then
    die "prerequisite P8 failed: provider image not built (neither ${IMAGE} nor localhost/${IMAGE}); run make image"
  fi

  # P11: host ports free for the single-cluster scope. Port 6443 must be
  # completely free; port 22 may be held only by the host sshd.
  if [[ -n "$(listener_addresses "${HOST_API_PORT}")" ]]; then
    die "prerequisite P11 failed: host port ${HOST_API_PORT} is already bound (stale passt or second cluster?)"
  fi
  local ssh_listeners="" ssh_owner_lines=""
  ssh_listeners="$(listener_addresses "${HOST_SSH_PORT}")"
  if [[ -n "${ssh_listeners}" ]]; then
    ssh_owner_lines="$(ss -ltnp 2>/dev/null | awk -v suffix=":${HOST_SSH_PORT}" 'NR > 1 && $4 ~ suffix"$"' || true)"
    if grep -q 'users:' <<< "${ssh_owner_lines}" && ! grep -q 'sshd' <<< "${ssh_owner_lines}"; then
      die "prerequisite P11 failed: host port ${HOST_SSH_PORT} is held by a non-sshd process"
    fi
    log "prerequisite P11: host port ${HOST_SSH_PORT} held (owner not visible to this user; assumed sshd)"
  fi

  # P12: no stale workload Cluster on the management plane.
  local stale_rc=0 stale_err=""
  stale_err="$(kubectl get cluster "${WORKLOAD_CLUSTER}" -n "${WORKLOAD_NAMESPACE}" \
    --kubeconfig="${MGMT_KUBECONFIG}" 2>&1 >/dev/null)" || stale_rc=$?
  if (( stale_rc == 0 )); then
    die "prerequisite P12 failed: stale workload Cluster ${WORKLOAD_CLUSTER} already exists on the management plane"
  fi
  if ! grep -Eq 'not found|NotFound' <<< "${stale_err}"; then
    die "prerequisite P12 failed: could not confirm the absence of Cluster ${WORKLOAD_CLUSTER}: ${stale_err}"
  fi

  log "lab-host prerequisites satisfied (P1-P12)"
}

# build_k8netd_probe — compile the embedded stdlib-only JSON-RPC probe into a
# scratch directory (same pattern as test/e2e/k8netd_stub_test.sh). Sets
# PROBE_BIN. The scratch directory is removed by the cleanup trap.
build_k8netd_probe() {
  if [[ -n "${PROBE_BIN}" && -x "${PROBE_BIN}" ]]; then
    return 0
  fi
  PROBE_DIR="$(mktemp -d)"
  cat > "${PROBE_DIR}/k8netd_probe.go" <<'PROBEGO'
// k8netd JSON-RPC 2.0 probe used by test/e2e/run.sh gates. Standard library
// only, built on demand with `go build` (same pattern as
// test/e2e/k8netd_stub_test.sh). Prints exactly one line to stdout:
//
//	ok <result-json>      on a successful response
//	err <code> <message>  on a typed RPC error (not_found, ...)
//	transport <message>   when the socket cannot be reached or the reply
//	                      cannot be decoded
//
// Exit codes: 0 answered (ok or typed error), 3 transport failure,
// 2 usage error.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// rpcVersion is the k8netd contract version sent with every request; keep in
// sync with internal/k8netd's k8netdVersion constant.
const rpcVersion = "1.0"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Version string          `json:"version"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: k8netd_probe <socket> <method> <params-json>")
		os.Exit(2)
	}
	conn, err := net.DialTimeout("unix", os.Args[1], 5*time.Second)
	if err != nil {
		fmt.Printf("transport %v\n", err)
		os.Exit(3)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		fmt.Printf("transport set deadline: %v\n", err)
		os.Exit(3)
	}
	req, err := json.Marshal(request{
		JSONRPC: "2.0",
		ID:      1,
		Version: rpcVersion,
		Method:  os.Args[2],
		Params:  json.RawMessage(os.Args[3]),
	})
	if err != nil {
		fmt.Printf("transport encode request: %v\n", err)
		os.Exit(3)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		fmt.Printf("transport write request: %v\n", err)
		os.Exit(3)
	}
	var resp response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		fmt.Printf("transport decode response: %v\n", err)
		os.Exit(3)
	}
	if resp.Error != nil {
		fmt.Printf("err %s %s\n", resp.Error.Code, resp.Error.Message)
		os.Exit(4)
	}
	fmt.Printf("ok %s\n", string(resp.Result))
}
PROBEGO
  go build -o "${PROBE_DIR}/k8netd_probe" "${PROBE_DIR}/k8netd_probe.go" \
    || die "failed to build the k8netd JSON-RPC probe (go toolchain required)"
  PROBE_BIN="${PROBE_DIR}/k8netd_probe"
}

# k8netd_get_network — call GetNetwork for the workload Cluster over the
# control socket. Prints the probe's one-line answer; the exit code follows
# the probe (0 answered-ok, 4 typed error, 3 transport failure).
k8netd_get_network() {
  # Build the probe on first use so SKIP_PREREQS runs (which never enter
  # check_prerequisites) still have it; build_k8netd_probe is idempotent.
  build_k8netd_probe
  "${PROBE_BIN}" "${K8NETD_SOCKET_RESOLVED}" GetNetwork \
    "{\"name\":\"${WORKLOAD_CLUSTER}\"}"
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

# wait_for_provider — poll the provider user quadlet until systemd reports it
# active. Both the provider and k8netd run as user units (rootless install
# contract); the provider client tolerates k8netd starting later via its
# connection backoff.
wait_for_provider() {
  local deadline=$(( $(date +%s) + APISERVER_READY_TIMEOUT ))
  log "waiting for the provider user quadlet (${PROVIDER_UNIT}) to start"
  until systemctl --user is-active --quiet "${PROVIDER_UNIT}"; do
    if (( $(date +%s) >= deadline )); then
      die "provider user quadlet ${PROVIDER_UNIT} did not become active (check 'systemctl --user status ${PROVIDER_UNIT}')"
    fi
    sleep 2
  done
  log "provider user quadlet is active"
}

# check_provider_journal — gate G3b: once the provider quadlet is active, its
# journal must not show persistent k8netd connection-retry errors (a couple of
# startup races are tolerated; a steady stream means the provider cannot reach
# the control socket).
check_provider_journal() {
  local journal="" retry_lines=0
  journal="$(journalctl --user -u "${PROVIDER_UNIT}" -n 50 --no-pager 2>/dev/null || true)"
  retry_lines="$(grep -Eic 'dial.*(refused|timeout|unreachable)|connect.*(refused|fail(ed)?)|retrying' <<< "${journal}" || true)"
  if (( retry_lines > 2 )); then
    die "provider journal shows persistent k8netd connection-retry errors (${retry_lines} of the last 50 lines; check 'journalctl --user -u ${PROVIDER_UNIT}')"
  fi
  log "provider journal shows no persistent k8netd connection errors"
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

# wait_for_infrastructure_ready — gate G4a: poll until the workload
# HypervisorCluster reports InfrastructureReady=True, which the cluster
# controller sets only after CreateNetwork succeeded on real k8netd.
wait_for_infrastructure_ready() {
  local deadline=$(( $(date +%s) + APISERVER_READY_TIMEOUT ))
  local condition=""
  log "waiting for HypervisorCluster ${WORKLOAD_CLUSTER} InfrastructureReady=True"
  while :; do
    condition="$(kubectl get hypervisorcluster "${WORKLOAD_CLUSTER}" -n "${WORKLOAD_NAMESPACE}" \
      --kubeconfig="${MGMT_KUBECONFIG}" \
      -o jsonpath='{.status.conditions[?(@.type=="InfrastructureReady")].status}' 2>/dev/null || true)"
    if [[ "${condition}" == "True" ]]; then
      log "HypervisorCluster ${WORKLOAD_CLUSTER} InfrastructureReady=True (network created through k8netd)"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "HypervisorCluster ${WORKLOAD_CLUSTER} did not report InfrastructureReady=True within ${APISERVER_READY_TIMEOUT}s (last: ${condition:-none})"
    fi
    sleep "${POLL_INTERVAL}"
  done
}

# verify_network_created — gate G4b: GetNetwork over the control socket must
# return the workload network with a CIDR and gateway; the gateway is kept
# for the guest gateway probe (G7b).
verify_network_created() {
  local probe_out="" probe_rc=0
  probe_out="$(k8netd_get_network)" || probe_rc=$?
  if (( probe_rc != 0 )); then
    die "GetNetwork(${WORKLOAD_CLUSTER}) failed after InfrastructureReady: ${probe_out}"
  fi
  if ! grep -q '"cidr"' <<< "${probe_out}" || ! grep -q '"gateway"' <<< "${probe_out}"; then
    die "GetNetwork(${WORKLOAD_CLUSTER}) result lacks cidr/gateway: ${probe_out}"
  fi
  NETWORK_GATEWAY="$(grep -o '"gateway":"[^"]*"' <<< "${probe_out}" | head -1 | cut -d'"' -f4)"
  if [[ -z "${NETWORK_GATEWAY}" ]]; then
    die "could not extract the gateway from the GetNetwork result: ${probe_out}"
  fi
  log "k8netd network ${WORKLOAD_CLUSTER} present (gateway ${NETWORK_GATEWAY})"
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

# collect_machines — fill MACHINE_INFRA_NAMES, MACHINE_INTERNAL_IPS and
# MACHINE_IS_CONTROL_PLANE from the CAPI Machines and their HypervisorMachine
# infrastructure refs, and derive WORKER_IPS. Dies when the inventory does not
# match the expected topology.
collect_machines() {
  local rows="" infra_name="" cp_label="" i=0 ip=""
  MACHINE_INFRA_NAMES=()
  MACHINE_INTERNAL_IPS=()
  MACHINE_IS_CONTROL_PLANE=()
  WORKER_IPS=()
  rows="$(kubectl get machine -n "${WORKLOAD_NAMESPACE}" --kubeconfig="${MGMT_KUBECONFIG}" \
    -o jsonpath='{range .items[*]}{.status.infrastructureRef.name}{"\t"}{.metadata.labels.cluster\.x-k8s\.io/control-plane}{"\n"}{end}' 2>/dev/null || true)"
  while IFS=$'\t' read -r infra_name cp_label; do
    [[ -n "${infra_name}" ]] || continue
    MACHINE_INFRA_NAMES+=("${infra_name}")
    MACHINE_IS_CONTROL_PLANE+=("$([[ -n "${cp_label}" ]] && echo true || echo false)")
  done <<< "${rows}"

  if (( ${#MACHINE_INFRA_NAMES[@]} != EXPECTED_MACHINE_COUNT )); then
    die "expected ${EXPECTED_MACHINE_COUNT} workload Machines, found ${#MACHINE_INFRA_NAMES[@]}"
  fi

  for i in "${!MACHINE_INFRA_NAMES[@]}"; do
    infra_name="${MACHINE_INFRA_NAMES[${i}]}"
    ip="$(kubectl get hypervisormachine "${infra_name}" -n "${WORKLOAD_NAMESPACE}" \
      --kubeconfig="${MGMT_KUBECONFIG}" \
      -o jsonpath='{range .status.addresses[*]}{.type}{"\t"}{.address}{"\n"}{end}' 2>/dev/null \
      | awk '$1 == "MachineInternalIP" {print $2; exit}' || true)"
    if [[ -z "${ip}" ]]; then
      die "HypervisorMachine ${infra_name} has no MachineInternalIP in status.addresses"
    fi
    MACHINE_INTERNAL_IPS+=("${ip}")
    if [[ "${MACHINE_IS_CONTROL_PLANE[${i}]}" != true ]]; then
      WORKER_IPS+=("${ip}")
    fi
  done
  log "machines inventoried: ${#MACHINE_INFRA_NAMES[@]} total, ${#WORKER_IPS[@]} workers"
}

# verify_dataplane — gates G5b-G5e after the Machines are Ready:
#   G5b  every HypervisorMachine publishes its AllocateIP-assigned
#        MachineInternalIP (checked by collect_machines),
#   G5c  a k8netd port socket exists per machine,
#   G5d  exactly one passt process runs per attached port (per-VM passt,
#        contract REQ-008 — never a shared instance),
#   G5e  every Node registered with the reserved internal IP of its machine
#        (DHCP honored the AllocateIP reservation).
verify_dataplane() {
  local socket_dir="" i=0 name="" passt_pids="" passt_count=0
  local node_ips="" machine_ip=""

  socket_dir="$(dirname -- "${K8NETD_SOCKET_RESOLVED}")"

  # G5c: one port socket per machine.
  for name in "${MACHINE_INFRA_NAMES[@]}"; do
    if [[ ! -S "${socket_dir}/${name}.sock" ]]; then
      die "port socket missing for machine ${name}: ${socket_dir}/${name}.sock"
    fi
  done
  log "port sockets present for all ${#MACHINE_INFRA_NAMES[@]} machines"

  # G5d: exactly one passt process per attached port.
  passt_pids="$(pgrep -x passt || true)"
  passt_count="$(wc -w <<< "${passt_pids}")"
  if (( passt_count != ${#MACHINE_INFRA_NAMES[@]} )); then
    die "expected exactly one passt process per attached port (${#MACHINE_INFRA_NAMES[@]}), found ${passt_count}"
  fi
  log "per-VM passt process count verified: ${passt_count}"

  # G5e: node InternalIPs match the reserved machine IPs.
  node_ips="$(kubectl get nodes --kubeconfig="${WORKLOAD_KUBECONFIG}" \
    -o jsonpath='{range .items[*].status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}' 2>/dev/null || true)"
  for machine_ip in "${MACHINE_INTERNAL_IPS[@]}"; do
    if ! grep -qx "${machine_ip}" <<< "${node_ips}"; then
      die "no Node registered with the reserved internal IP ${machine_ip} (DHCP reservation not honored?)"
    fi
  done
  log "node InternalIPs match the reserved machine IPs"
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

# verify_workload_api — gates G6a-G6c against the workload kubeconfig:
#   G6a  the kubeconfig server URL is exactly https://127.0.0.1:<api-port>
#       (published by the control-plane endpoint reconcile, REQ-006),
#   G6b  the workload apiserver answers /readyz through the passt forward
#       (TLS SAN includes 127.0.0.1),
#   G6c  all expected Nodes are Ready.
verify_workload_api() {
  local expected_server="https://127.0.0.1:${HOST_API_PORT}"
  local server="" readyz="" names="" total=0 ready=0 statuses=""

  server="$(kubectl config view --kubeconfig="${WORKLOAD_KUBECONFIG}" --minify \
    -o jsonpath='{.clusters[0].cluster.server}')"
  if [[ "${server}" != "${expected_server}" ]]; then
    die "workload kubeconfig server is ${server}, want ${expected_server}"
  fi
  log "workload kubeconfig targets ${expected_server}"

  readyz="$(kubectl get --kubeconfig="${WORKLOAD_KUBECONFIG}" --raw='/readyz' 2>/dev/null || true)"
  if [[ "${readyz}" != "ok" ]]; then
    die "workload apiserver ${expected_server} did not answer /readyz with ok (got: ${readyz:-<empty>})"
  fi
  log "workload apiserver reachable via ${expected_server}"

  names="$(kubectl get nodes --kubeconfig="${WORKLOAD_KUBECONFIG}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
  total="$(wc -w <<< "${names}")"
  statuses="$(kubectl get nodes --kubeconfig="${WORKLOAD_KUBECONFIG}" \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null || true)"
  ready="$(grep -c '^True$' <<< "${statuses}" || true)"
  if (( total != EXPECTED_MACHINE_COUNT || ready != EXPECTED_MACHINE_COUNT )); then
    die "expected ${EXPECTED_MACHINE_COUNT} Ready nodes, found ${ready}/${total}"
  fi
  log "all ${EXPECTED_MACHINE_COUNT} workload nodes are Ready"
}

# ssh_guest — run a command inside the control-plane guest over SSH through
# the passt-forwarded host port 22 (contract REQ-008 forwards host 22 to the
# control-plane VM's port 22). This is the documented probe mechanism for the
# guest-side gates: the host is not on the cluster L2 segment, so VM-to-VM and
# internet reachability can only be observed from inside a guest.
ssh_guest() {
  ssh -i "${GUEST_SSH_KEY_RESOLVED}" -p "${HOST_SSH_PORT}" \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=10 \
    "${GUEST_SSH_USER}@127.0.0.1" "$@"
}

# wait_for_guest_ssh — poll SSH into the control-plane guest until it answers
# or the budget expires (the guest agent needs a moment after the node turns
# Ready).
wait_for_guest_ssh() {
  local deadline=$(( $(date +%s) + SSH_PROBE_TIMEOUT ))
  log "waiting for SSH access to the control-plane guest (127.0.0.1:${HOST_SSH_PORT})"
  until ssh_guest true >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      die "SSH into the control-plane guest did not come up within ${SSH_PROBE_TIMEOUT}s (key: ${GUEST_SSH_KEY_RESOLVED}, user: ${GUEST_SSH_USER})"
    fi
    sleep "${POLL_INTERVAL}"
  done
  log "SSH access to the control-plane guest is up"
}

# verify_guest_reachability — gates G7a/G7b/G8a/G8b, probed from inside the
# control-plane guest:
#   G7a  ping succeeds to every worker internal IP (L2 known-unicast
#        forwarding between ports),
#   G7b  ping to the gateway answers (gateway ARP function),
#   G8a  DNS resolves a public name via the guest resolver (k8netd DNS
#        forwarder on the gateway),
#   G8b  HTTPS to a public site egresses through the per-VM passt WAN.
verify_guest_reachability() {
  local ip=""
  wait_for_guest_ssh

  # G7a: VM-to-VM reachability.
  for ip in "${WORKER_IPS[@]}"; do
    ssh_guest "ping -c 1 -W 5 ${ip}" >/dev/null 2>&1 \
      || die "guest-to-guest ping to worker ${ip} failed"
  done
  log "guest-to-guest ping succeeded to all ${#WORKER_IPS[@]} workers"

  # G7b: gateway ARP function.
  ssh_guest "ping -c 1 -W 5 ${NETWORK_GATEWAY}" >/dev/null 2>&1 \
    || die "ping to the gateway ${NETWORK_GATEWAY} failed from the control-plane guest"
  log "gateway ${NETWORK_GATEWAY} answers pings from the guest"

  # G8a: DNS forwarding through the gateway resolver.
  ssh_guest "getent hosts www.example.com" >/dev/null 2>&1 \
    || die "DNS resolution of www.example.com failed inside the control-plane guest"
  log "guest DNS resolution works"

  # G8b: internet egress through the per-VM passt WAN (curl preferred, busybox
  # wget as the fallback for slim guest images).
  ssh_guest "curl -fsS --max-time 15 https://www.example.com -o /dev/null 2>/dev/null || wget -q -T 15 -O /dev/null https://www.example.com" >/dev/null 2>&1 \
    || die "HTTPS egress to www.example.com failed inside the control-plane guest"
  log "guest internet egress works"
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
  log "running the workload smoke checks (${smoke_script})"
  KUBECONFIG="${WORKLOAD_KUBECONFIG}" bash "${smoke_script}" "${WORKLOAD_KUBECONFIG}"
  log "workload smoke checks passed"
}

# teardown_and_verify — deliberate teardown phase (scenario step S10): delete
# the workload Cluster, wait for the Machines to disappear, then prove the
# host converged:
#   G10a  no workload Machine remains,
#   G10b  no cloud-hypervisor process remains,
#   G10c  port sockets removed and GetNetwork answers not_found,
#   G10d  no passt process remains and host port 6443 is released,
#   G10e  k8netd itself stays active (independent quadlet; cluster teardown
#         must not stop the daemon).
# Any gate failure fails the run naming the gate; the EXIT trap still handles
# the residual cleanup when this function is never reached.
teardown_and_verify() {
  local deadline=0 names="" probe_out="" probe_rc=0 socket_dir=""
  socket_dir="$(dirname -- "${K8NETD_SOCKET_RESOLVED}")"

  log "teardown: deleting the workload Cluster ${WORKLOAD_CLUSTER}"
  kubectl delete cluster "${WORKLOAD_CLUSTER}" -n "${WORKLOAD_NAMESPACE}" \
    --kubeconfig="${MGMT_KUBECONFIG}" --timeout="${TEARDOWN_CLUSTER_TIMEOUT}s" \
    || die "teardown: workload Cluster deletion did not complete within ${TEARDOWN_CLUSTER_TIMEOUT}s"

  # G10a: no workload Machine remains.
  deadline=$(( $(date +%s) + TEARDOWN_CLUSTER_TIMEOUT ))
  while :; do
    names="$(kubectl get machine -n "${WORKLOAD_NAMESPACE}" --kubeconfig="${MGMT_KUBECONFIG}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
    [[ -z "${names}" ]] && break
    if (( $(date +%s) >= deadline )); then
      die "teardown G10a failed: workload Machines still present after ${TEARDOWN_CLUSTER_TIMEOUT}s: ${names}"
    fi
    sleep "${POLL_INTERVAL}"
  done
  log "teardown G10a: no workload Machine remains"

  # G10b: no cloud-hypervisor process remains.
  deadline=$(( $(date +%s) + TEARDOWN_CLUSTER_TIMEOUT ))
  while :; do
    [[ -z "$(pgrep -x cloud-hypervisor || true)" ]] && break
    if (( $(date +%s) >= deadline )); then
      die "teardown G10b failed: cloud-hypervisor processes still running after ${TEARDOWN_CLUSTER_TIMEOUT}s"
    fi
    sleep "${POLL_INTERVAL}"
  done
  log "teardown G10b: no cloud-hypervisor process remains"

  # G10c: port sockets removed; the network is gone from k8netd.
  for name in "${MACHINE_INFRA_NAMES[@]}"; do
    if [[ -S "${socket_dir}/${name}.sock" ]]; then
      die "teardown G10c failed: port socket still present: ${socket_dir}/${name}.sock"
    fi
  done
  probe_out="$(k8netd_get_network)" || probe_rc=$?
  if (( probe_rc != 4 )) || [[ "${probe_out}" != err\ not_found* ]]; then
    die "teardown G10c failed: GetNetwork(${WORKLOAD_CLUSTER}) did not answer not_found: ${probe_out}"
  fi
  log "teardown G10c: port sockets removed, k8netd network deleted"

  # G10d: no passt process remains; host port 6443 released.
  if [[ -n "$(pgrep -x passt || true)" ]]; then
    die "teardown G10d failed: passt processes still running"
  fi
  if [[ -n "$(listener_addresses "${HOST_API_PORT}")" ]]; then
    die "teardown G10d failed: host port ${HOST_API_PORT} is still bound"
  fi
  log "teardown G10d: no passt process remains, host port ${HOST_API_PORT} released"

  # G10e: k8netd survives the cluster deletion.
  if [[ "$(systemctl --user is-active "${K8NETD_UNIT}" 2>/dev/null || true)" != "active" ]]; then
    die "teardown G10e failed: ${K8NETD_UNIT} is no longer active after cluster deletion"
  fi
  log "teardown G10e: ${K8NETD_UNIT} still active"

  if [[ -n "${WORKLOAD_KUBECONFIG}" && -f "${WORKLOAD_KUBECONFIG}" ]]; then
    rm -f -- "${WORKLOAD_KUBECONFIG}"
    WORKLOAD_KUBECONFIG=""
  fi
  if [[ "${PLANE_STARTED}" == true && "${MGMT_EXTERNAL}" == false ]]; then
    log "teardown: bringing the management plane down"
    bash "${SCRIPT_DIR}/mgmt/down.sh" >/dev/null 2>&1 \
      || log "teardown: mgmt-down did not complete (continuing)"
  fi
  TEARDOWN_DONE=true
}

# cleanup — teardown trap: safety net for abnormal exits. When the deliberate
# teardown phase already ran (TEARDOWN_DONE) there is nothing left to do;
# otherwise remove the temp workload kubeconfig, best-effort delete the
# workload Cluster, and bring the mgmt plane down when the fallback started
# it. Best-effort: a failure here never changes the run's exit code.
cleanup() {
  local rc=$?
  trap - EXIT
  rm -rf -- "${PROBE_DIR}"
  if [[ "${TEARDOWN_DONE}" == true ]]; then
    exit "${rc}"
  fi
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

# orchestrate — the full-lab flow. Runs only after the lab-host guard, the
# environment validation, and the prerequisite gates passed.
orchestrate() {
  SMOKE="${SMOKE:-1}"
  WAIT_TIMEOUT="${WAIT_TIMEOUT:-${MACHINES_READY_TIMEOUT_DEFAULT}}"

  # The body needs go for the clusterctl tool (go tool clusterctl) and the
  # k8netd probe build, kubectl for the cluster work, base64/mktemp for the
  # kubeconfig extraction, and ss/pgrep/ssh for the dataplane and guest gates.
  require_cmd go
  require_cmd kubectl
  require_cmd base64
  require_cmd mktemp
  require_cmd ss
  require_cmd pgrep
  require_cmd ssh

  check_prerequisites
  mgmt_up
  wait_for_apiserver_ready
  wait_for_provider
  check_provider_journal
  apply_templates
  wait_for_infrastructure_ready
  verify_network_created
  wait_for_machines_ready
  WORKLOAD_KUBECONFIG="$(extract_workload_kubeconfig)"
  collect_machines
  verify_dataplane
  verify_workload_api
  verify_guest_reachability
  run_smoke
  teardown_and_verify
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
  require_lab_host
  trap cleanup EXIT
  validate_environment
  orchestrate
}

main "$@"
