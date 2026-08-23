#!/usr/bin/env bash
#
# delete_test.sh — verify the per-step contract of the cluster-deletion
# scenario script (test/e2e/delete-cluster.sh).
#
# The full-lab harness (test/e2e/run.sh) tears a workload cluster down on a
# live lab: delete the Cluster object, wait for the controllers to tear down
# the Machines (and with them the VMs, TAPs, and disks), bring the management
# plane down, and verify the host network state is gone. That cannot happen
# here: there is no live cluster in this test. What this file pins instead is
# the per-step contract of delete-cluster.sh, by executing it against a stub
# kubectl on PATH that dispatches on its arguments and returns scripted canned
# outputs, plus sentinel host binaries (ip, pgrep, nft, dnsmasq) that record
# every invocation, and a stub mgmt-down script, then asserting
# delete-cluster.sh's decisions and its aggregate exit code.
#
# The kubeconfig contract mirrors the harness exactly: run.sh hands the
# management kubeconfig to its helpers as the KUBECONFIG environment variable
# and as the first positional argument. delete-cluster.sh accepts the same
# shapes: the management kubeconfig comes from KUBECONFIG or from $1 (a
# positional argument wins, exactly like smoke.sh and scale.sh). The workload
# namespace is optional: it comes from CLUSTER_NAMESPACE (default "default")
# or from $2, and the Cluster name from CLUSTER_NAME (default "k8labs", the
# committed example Cluster). The stub kubectl records which kubeconfig every
# invocation used (a --kubeconfig flag, a --kubeconfig=value, or the
# KUBECONFIG environment), so the tests can assert that delete-cluster.sh
# really targets the management cluster it was given and that it plumbs the
# workload namespace through to every management-cluster call.
#
# Per-step contract pinned (each step's pass/fail semantics):
#   1. cluster-delete — delete-cluster.sh deletes the workload Cluster object
#      (and the workload namespace) from the management cluster, then polls
#      the Cluster object until it is gone. The stub models the deletion as a
#      timeline: a `kubectl delete cluster` marks a delete-seen flag in
#      STUB_STATE_DIR, and once the flag is set the Cluster object reports
#      gone (exit 1); machine lists flip to the post-deletion empty set.
#   2. machine-teardown — delete-cluster.sh polls the management cluster
#      until no workload Machine remains (the controllers tear down
#      VMs/TAPs/disks as part of the CAPI deletion; the script's gate is the
#      object teardown, and the linked HypervisorMachine objects are served
#      as gone too). Host-level bridge/dnsmasq/NAT removal is verified by the
#      lab, not by this step.
#   3. mgmt-down — when the management plane was self-bootstrapped (the
#      MANAGEMENT_KUBECONFIG environment variable is unset or empty),
#      delete-cluster.sh runs the mgmt-down script to stop the quadlets
#      cleanly; when the plane was external (MANAGEMENT_KUBECONFIG set and
#      non-empty), the step is skipped entirely. The script resolves the
#      mgmt-down script through MGMT_DOWN_SH (default
#      <script-dir>/mgmt/down.sh); the tests always inject a recording stub so
#      a faulty implementation can never reach the real mgmt/down.sh.
#   4. no-host-net-tooling — after the k8netd migration delete-cluster.sh
#      performs no host network operation at all: the whole scenario must
#      converge with sentinel ip/pgrep/nft/dnsmasq binaries on PATH whose
#      invocation log stays empty. Any bridge/dnsmasq/nftables tooling
#      invocation is a contract violation (the provider talks to k8netd over
#      the control socket instead; there is no leftover host network state
#      for the script to inspect).
#
# Aggregate contract: delete-cluster.sh exits 0 only when the whole scenario
# converges (Cluster gone, Machines gone, mgmt-down handled per the plane
# origin, and no host network tooling invoked). Every failure scenario keeps
# everything healthy except the step under test, so a non-zero exit is
# attributable to exactly that step and the output must name it.
#
# Exit codes of this test:
#   0  delete-cluster.sh satisfies the cluster-deletion scenario contract
#   1  contract violation (including delete-cluster.sh being absent: the red
#      phase)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/delete_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
DELETE_SH="${SCRIPT_DIR}/delete-cluster.sh"
readonly DELETE_SH

# A single delete-cluster.sh run must return quickly against the stub kubectl;
# the timeout guards against a delete-cluster.sh that hangs instead of
# reporting.
readonly DELETE_TIMEOUT=60
# delete-cluster.sh's own per-step wait budget. Success scenarios allow a
# comfortable 10s (the stub converges after one or two polls); timeout
# scenarios shrink the budget to 3s so the timed-out step fails fast.
readonly DELETE_WAIT_OK=10
readonly DELETE_WAIT_SHORT=3
# The workload Cluster identity every scenario drives towards, mirroring the
# harness pins (run.sh: WORKLOAD_CLUSTER=k8labs, WORKLOAD_NAMESPACE=default).
readonly CLUSTER_NAME="k8labs"
readonly DEFAULT_NAMESPACE="default"

problems=0
SCRATCH=""
DELETE_RC=0

log() { printf 'delete_test: %s\n' "$*" >&2; }

ok() { printf 'delete_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'delete_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'delete_test: %s\n' "$message" >&2
  exit "$code"
}

# install_stub_kubectl <dir> — write an executable stub kubectl into <dir>.
# The stub dispatches on its arguments and prints canned outputs driven by the
# STUB_* environment variables; every variable defaults to a healthy deletion
# scenario (the Cluster object and Machines exist until a delete is seen, then
# both report gone). It models the deletion timeline with one piece of state
# persisted in STUB_STATE_DIR: a delete-seen marker (once `kubectl delete
# cluster` runs, the Cluster object reports gone and machine lists serve the
# post-deletion empty set). Each invocation is appended to "$STUB_LOG" (when
# set) together with the kubeconfig it used. The dispatch matches
# resource-name substrings, not flag positions, because the deletion steps
# invoke kubectl with inconsistent flag ordering.
install_stub_kubectl() {
  local dir="$1"
  cat > "${dir}/kubectl" <<'STUB'
#!/usr/bin/env bash
set -u

stub_default_machines_before="cp1
w1
w2
w3"
stub_default_machines_after=""

# Record the invocation and the kubeconfig it used.
log="${STUB_LOG:-}"
if [[ -n "${log}" ]]; then
  printf 'kubectl %s\n' "$*" >> "${log}"
  kc=""
  prev=""
  for arg in "$@"; do
    if [[ "${prev}" == "--kubeconfig" ]]; then
      kc="${arg}"
      break
    fi
    case "${arg}" in
      --kubeconfig=*) kc="${arg#--kubeconfig=}"; break ;;
    esac
    prev="${arg}"
  done
  if [[ -z "${kc}" ]]; then
    kc="${KUBECONFIG:-}"
  fi
  if [[ -n "${kc}" ]]; then
    printf 'kubeconfig=%s\n' "${kc}" >> "${log}"
  fi
fi

state_dir="${STUB_STATE_DIR:-}"
delete_seen=0
if [[ -n "${state_dir}" && -f "${state_dir}/delete-seen" ]]; then
  delete_seen=1
fi

# Normalize resource plurals for substring dispatch; the plural forms are
# accepted for the same resources.
args="$*"
dispatch="${args//machines/machine}"
dispatch="${dispatch//clusters/cluster}"
dispatch="${dispatch//namespaces/namespace}"

# The first non-flag token after the resource verb is the object name.
cluster_name=""
found_verb=0
for arg in "$@"; do
  if [[ "${found_verb}" -eq 0 ]]; then
    if [[ "${arg}" == "cluster" || "${arg}" == "clusters" ]]; then
      found_verb=1
    fi
    continue
  fi
  if [[ "${arg}" != -* ]]; then
    cluster_name="${arg}"
    break
  fi
done

# emit_machine_list <list> — print a machine list in the requested format:
# "machine/<name>" lines for -o name, plain names otherwise. An empty list
# prints nothing.
emit_machine_list() {
  local list="$1"
  if [[ -z "${list}" ]]; then
    return 0
  fi
  if [[ "${dispatch}" == *'-o name'* ]]; then
    printf '%s\n' "${list}" | awk '{print "machine/"$1}'
    return 0
  fi
  printf '%s\n' "${list}"
}

case "${dispatch}" in
  *'delete cluster'*)
    if [[ -n "${state_dir}" ]]; then
      touch "${state_dir}/delete-seen"
    fi
    exit "${STUB_DELETE_CLUSTER_RC-0}"
    ;;
  *'delete namespace'*)
    exit "${STUB_DELETE_NS_RC-0}"
    ;;
  *'get cluster'*)
    if [[ "${delete_seen}" -eq 1 && "${STUB_CLUSTER_GONE_RC-1}" -ne 0 ]]; then
      exit 1
    fi
    if [[ "${dispatch}" == *'-o name'* ]]; then
      printf '%s\n' "cluster/${cluster_name:-k8labs}"
    else
      printf '%s\n' "${cluster_name:-k8labs} Active 10m"
    fi
    exit 0
    ;;
  *'get hypervisormachine'*)
    if [[ "${delete_seen}" -eq 1 ]]; then
      exit 1
    fi
    printf '%s\n' "hm-${cluster_name:-k8labs}"
    exit 0
    ;;
  *'get machine'*)
    if [[ "${delete_seen}" -eq 1 ]]; then
      emit_machine_list "${STUB_MACHINES_AFTER-${stub_default_machines_after}}"
    else
      emit_machine_list "${STUB_MACHINES_BEFORE-${stub_default_machines_before}}"
    fi
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "${dir}/kubectl"
}

# install_stub_net_sentinels <dir> — write executable sentinel ip, pgrep,
# nft, and dnsmasq binaries into <dir>. After the k8netd migration the
# deletion scenario must not touch any host network tooling: each sentinel
# appends its invocation to "$STUB_NET_LOG" (when set) and exits 7 so that,
# even if a script invoked one inside an if-condition, the invocation is
# still recorded and detectable. The tests assert the log stays empty.
install_stub_net_sentinels() {
  local dir="$1"
  local tool=""
  for tool in ip pgrep nft dnsmasq; do
    cat > "${dir}/${tool}" <<'STUB'
#!/usr/bin/env bash
set -u
net_log="${STUB_NET_LOG:-}"
if [[ -n "${net_log}" ]]; then
  printf '%s: %s\n' "$(basename "$0")" "$*" >> "${net_log}"
fi
exit 7
STUB
    chmod +x "${dir}/${tool}"
  done
}

# install_stub_down <dir> — write an executable stub mgmt-down script into
# <dir>. It records every invocation to "$STUB_DOWN_LOG" (when set) and exits
# per STUB_DOWN_RC (default 0), standing in for the real mgmt/down.sh so the
# tests can assert whether the mgmt-down step ran without ever touching the
# host's quadlet state.
install_stub_down() {
  local dir="$1"
  cat > "${dir}/down.sh" <<'STUB'
#!/usr/bin/env bash
set -u
down_log="${STUB_DOWN_LOG:-}"
if [[ -n "${down_log}" ]]; then
  printf 'down: %s\n' "$*" >> "${down_log}"
fi
exit "${STUB_DOWN_RC-0}"
STUB
  chmod +x "${dir}/down.sh"
}

# new_scenario <label> — create a scratch fixture: a stub kubectl, sentinel
# host binaries, and a stub mgmt-down on PATH, plus a stub state directory
# and a minimal management kubeconfig. Prints the scratch directory path.
new_scenario() {
  local label="$1"
  local scratch=""
  scratch="$(mktemp -d "${SCRATCH}/${label}.XXXXXX")"
  install_stub_kubectl "${scratch}"
  install_stub_net_sentinels "${scratch}"
  install_stub_down "${scratch}"
  mkdir -p "${scratch}/stub-state"
  printf '%s' "${scratch}"
}

# make_kubeconfig <dir> — write a minimal non-empty management kubeconfig
# fixture into <dir>; the stub kubectl never reads it, but delete-cluster.sh
# may stat it. Prints the kubeconfig path.
make_kubeconfig() {
  local dir="$1"
  local kc="${dir}/mgmt-kubeconfig"
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Config' \
    'clusters: []' \
    'users: []' \
    'contexts: []' \
    'current-context: ""' > "${kc}"
  printf '%s' "${kc}"
}

# run_delete <scratch> <kubeconfig> [mode] [namespace] [VAR=value ...] —
# execute delete-cluster.sh against the stub kubectl installed in <scratch>.
# mode "harness" mirrors the full-lab harness: KUBECONFIG env plus both
# positionals (<kubeconfig> [<namespace>]). mode "env" passes only the
# KUBECONFIG and CLUSTER_NAMESPACE environment variables. mode "poskc" passes
# the kubeconfig only as the first positional (no KUBECONFIG env). mode
# "selfboot" mirrors the harness shape but unsets MANAGEMENT_KUBECONFIG so the
# plane is treated as self-bootstrapped. The management-plane origin is pinned
# by MANAGEMENT_KUBECONFIG: set and non-empty means an external plane (the
# mgmt-down step must be skipped), unset or empty means the plane was
# self-bootstrapped (the mgmt-down step must run through MGMT_DOWN_SH). Extra
# VAR=value arguments become additional environment assignments for
# delete-cluster.sh and the stubs; a passed DELETE_WAIT_TIMEOUT=... overrides
# the default 10s budget. delete-cluster.sh's combined output is written to
# <scratch>/delete.out and DELETE_RC holds its exit status (124 when the run
# exceeds DELETE_TIMEOUT).
run_delete() {
  local scratch="$1" kubeconfig="$2" mode="${3:-harness}" ns="${4:-}"
  local log="${scratch}/kubectl.log" down_log="${scratch}/down.log"
  local down_stub="${scratch}/down.sh"
  local -a env_extra=()
  local arg="" rc=0
  local wait_timeout="${DELETE_WAIT_OK}"
  for arg in "${@:5}"; do
    if [[ "${arg}" == DELETE_WAIT_TIMEOUT=* ]]; then
      wait_timeout="${arg#DELETE_WAIT_TIMEOUT=}"
    fi
    env_extra+=("${arg}")
  done
  case "${mode}" in
    env)
      timeout "${DELETE_TIMEOUT}" env "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" \
        MANAGEMENT_KUBECONFIG="${kubeconfig}" CLUSTER_NAME="${CLUSTER_NAME}" \
        CLUSTER_NAMESPACE="${ns:-${DEFAULT_NAMESPACE}}" \
        DELETE_WAIT_TIMEOUT="${wait_timeout}" MGMT_DOWN_SH="${down_stub}" \
        STUB_LOG="${log}" STUB_DOWN_LOG="${down_log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        STUB_NET_LOG="${scratch}/net.log" \
        "${DELETE_SH}" >"${scratch}/delete.out" 2>&1 || rc=$?
      ;;
    poskc)
      timeout "${DELETE_TIMEOUT}" env -u KUBECONFIG "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" MANAGEMENT_KUBECONFIG="${kubeconfig}" \
        CLUSTER_NAME="${CLUSTER_NAME}" CLUSTER_NAMESPACE="${ns:-${DEFAULT_NAMESPACE}}" \
        DELETE_WAIT_TIMEOUT="${wait_timeout}" MGMT_DOWN_SH="${down_stub}" \
        STUB_LOG="${log}" STUB_DOWN_LOG="${down_log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        STUB_NET_LOG="${scratch}/net.log" \
        "${DELETE_SH}" "${kubeconfig}" >"${scratch}/delete.out" 2>&1 || rc=$?
      ;;
    selfboot)
      timeout "${DELETE_TIMEOUT}" env -u MANAGEMENT_KUBECONFIG "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" \
        CLUSTER_NAME="${CLUSTER_NAME}" CLUSTER_NAMESPACE="${ns:-${DEFAULT_NAMESPACE}}" \
        DELETE_WAIT_TIMEOUT="${wait_timeout}" MGMT_DOWN_SH="${down_stub}" \
        STUB_LOG="${log}" STUB_DOWN_LOG="${down_log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        STUB_NET_LOG="${scratch}/net.log" \
        "${DELETE_SH}" "${kubeconfig}" >"${scratch}/delete.out" 2>&1 || rc=$?
      ;;
    *)
      timeout "${DELETE_TIMEOUT}" env "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" \
        MANAGEMENT_KUBECONFIG="${kubeconfig}" CLUSTER_NAME="${CLUSTER_NAME}" \
        CLUSTER_NAMESPACE="${ns:-${DEFAULT_NAMESPACE}}" \
        DELETE_WAIT_TIMEOUT="${wait_timeout}" MGMT_DOWN_SH="${down_stub}" \
        STUB_LOG="${log}" STUB_DOWN_LOG="${down_log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        STUB_NET_LOG="${scratch}/net.log" \
        "${DELETE_SH}" "${kubeconfig}" "${ns}" >"${scratch}/delete.out" 2>&1 || rc=$?
      ;;
  esac
  DELETE_RC="${rc}"
}

# assert_delete_passed <scratch> <display> — delete-cluster.sh must exit 0 and
# its output must not report a wait timeout.
assert_delete_passed() {
  local scratch="$1" display="$2"
  local out="" rc="${DELETE_RC}"
  out="$(<"${scratch}/delete.out")"
  if [[ "${rc}" -ne 0 ]]; then
    missing "delete-cluster.sh exited ${rc}; expected 0 for ${display}"
    printf 'delete_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if grep -Eqi 'timeout' <<< "${out}"; then
    missing "delete-cluster.sh reported a wait timeout for ${display}"
    printf 'delete_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  ok "delete-cluster.sh exited 0 for ${display}"
}

# assert_delete_timeout <scratch> <display> <step> — delete-cluster.sh must
# exit non-zero (but not be killed by the outer guard), and an error line must
# name the timed-out step: a line mentioning the timeout that also contains
# the step identifier ("cluster-delete" or "machine-teardown").
assert_delete_timeout() {
  local scratch="$1" display="$2" step="$3"
  local out="" rc="${DELETE_RC}"
  out="$(<"${scratch}/delete.out")"
  if [[ "${rc}" -eq 0 ]]; then
    missing "delete-cluster.sh exited 0; expected non-zero when ${display}"
    printf 'delete_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if [[ "${rc}" -eq 124 ]]; then
    missing "delete-cluster.sh exceeded the ${DELETE_TIMEOUT}s test guard when ${display} (did it honor DELETE_WAIT_TIMEOUT?)"
    printf 'delete_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if ! grep -Ei '(timeout|timed out)' <<< "${out}" | grep -Fq -- "${step}"; then
    missing "delete-cluster.sh exited ${rc} but did not name the '${step}' step when ${display}"
    printf 'delete_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  ok "delete-cluster.sh exited ${rc} and named the '${step}' step when ${display}"
}

# assert_no_down_calls <scratch> <display> — the stub mgmt-down must never
# have been invoked (the mgmt-down step is skipped for an external plane).
assert_no_down_calls() {
  local scratch="$1" display="$2"
  local down_log="${scratch}/down.log"
  if [[ -f "${down_log}" && -s "${down_log}" ]]; then
    missing "delete-cluster.sh invoked mgmt-down for ${display}; expected the mgmt-down step to be skipped with an external management kubeconfig"
    printf 'delete_test: down log:\n%s\n' "$(<"${down_log}")" >&2
    return 1
  fi
  ok "mgmt-down was not invoked for ${display}"
}

# assert_down_called_once <scratch> <display> — the stub mgmt-down must have
# been invoked exactly once (the mgmt-down step runs for a self-bootstrapped
# plane).
assert_down_called_once() {
  local scratch="$1" display="$2"
  local down_log="${scratch}/down.log" count=0
  if [[ ! -f "${down_log}" ]]; then
    missing "delete-cluster.sh never invoked mgmt-down for ${display}; expected exactly one invocation for a self-bootstrapped management plane"
    return 1
  fi
  count="$(wc -l < "${down_log}")"
  if [[ "${count}" -ne 1 ]]; then
    missing "delete-cluster.sh invoked mgmt-down ${count} times for ${display}; expected exactly one invocation"
    return 1
  fi
  ok "mgmt-down was invoked exactly once for ${display}"
}

# assert_no_net_tooling <scratch> <display> — the sentinel ip/pgrep/nft/
# dnsmasq binaries must never have been invoked: delete-cluster.sh performs
# no host network operation (the k8netd migration removed the host-tool
# contract entirely).
assert_no_net_tooling() {
  local scratch="$1" display="$2"
  local net_log="${scratch}/net.log"
  if [[ -f "${net_log}" && -s "${net_log}" ]]; then
    missing "delete-cluster.sh invoked host network tooling for ${display}; the k8netd migration requires no bridge/dnsmasq/nftables tooling at all"
    printf 'delete_test: net sentinel log:\n%s\n' "$(<"${net_log}")" >&2
    return 1
  fi
  ok "no host network tooling was invoked for ${display}"
}

# assert_contract_observations <scratch> <kubeconfig> <namespace> — the
# management kubeconfig must have been observed by the stub kubectl, the
# workload namespace must appear in the invocations (including a namespace
# deletion).
assert_contract_observations() {
  local scratch="$1" kubeconfig="$2" ns="${3:-${DEFAULT_NAMESPACE}}"
  local kclog=""
  if [[ "${DELETE_RC}" -ne 0 ]]; then
    missing "delete-cluster.sh exited ${DELETE_RC}; expected 0"
    printf 'delete_test: output:\n%s\n' "$(<"${scratch}/delete.out")" >&2
    return 1
  fi
  kclog=""
  if [[ -f "${scratch}/kubectl.log" ]]; then
    kclog="$(<"${scratch}/kubectl.log")"
  fi
  if ! grep -Fq -- "kubeconfig=${kubeconfig}" <<< "${kclog}"; then
    missing "the stub kubectl never observed the management kubeconfig ${kubeconfig}"
    printf 'delete_test: kubectl log:\n%s\n' "${kclog}" >&2
    return 1
  fi
  if ! grep -Fq -- "delete namespace" <<< "${kclog}"; then
    missing "the stub kubectl never observed a workload namespace deletion"
    printf 'delete_test: kubectl log:\n%s\n' "${kclog}" >&2
    return 1
  fi
  if ! grep -Fq -- "${ns}" <<< "${kclog}"; then
    missing "the stub kubectl never observed the workload namespace ${ns}"
    printf 'delete_test: kubectl log:\n%s\n' "${kclog}" >&2
    return 1
  fi
  ok "delete-cluster.sh targeted the management kubeconfig ${kubeconfig} and the namespace ${ns}"
}

# --- scenarios --------------------------------------------------------------

test_delete_success() {
  log "scenario: Cluster deletion, machine teardown, and host cleanup all converge"
  local scratch="" kc=""

  # Harness shape: KUBECONFIG env plus positionals, exactly how run.sh would
  # invoke the scenario helper.
  scratch="$(new_scenario delete-success)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" harness
  assert_delete_passed "${scratch}" "an all-healthy deletion scenario" || :
  assert_contract_observations "${scratch}" "${kc}" "default" || :
  assert_no_down_calls "${scratch}" "an all-healthy deletion scenario with an external management kubeconfig" || :
  assert_no_net_tooling "${scratch}" "an all-healthy deletion scenario" || :
  rm -rf -- "${scratch}"

  # Environment-only shape: no positionals at all.
  scratch="$(new_scenario delete-success-env)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" env
  assert_delete_passed "${scratch}" "the environment-only invocation shape" || :
  assert_contract_observations "${scratch}" "${kc}" "default" || :
  assert_no_net_tooling "${scratch}" "the environment-only invocation shape" || :
  rm -rf -- "${scratch}"

  # Positional-kubeconfig shape: the management kubeconfig arrives only as the
  # first positional argument.
  scratch="$(new_scenario delete-success-poskc)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" poskc
  assert_delete_passed "${scratch}" "the positional-kubeconfig invocation shape" || :
  assert_contract_observations "${scratch}" "${kc}" "default" || :
  assert_no_net_tooling "${scratch}" "the positional-kubeconfig invocation shape" || :
  rm -rf -- "${scratch}"

  # The optional workload namespace arrives as the second positional argument.
  scratch="$(new_scenario delete-success-ns)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" harness "lab-ns"
  assert_delete_passed "${scratch}" "the harness shape with an explicit workload namespace" || :
  assert_contract_observations "${scratch}" "${kc}" "lab-ns" || :
  assert_no_net_tooling "${scratch}" "the harness shape with an explicit workload namespace" || :
  rm -rf -- "${scratch}"
}

test_cluster_delete_timeout() {
  log "scenario: the Cluster object never disappears"
  local scratch="" kc=""
  scratch="$(new_scenario cluster-delete-timeout)"
  kc="$(make_kubeconfig "${scratch}")"
  # The delete command succeeds (the delete-seen marker is set) but the stub
  # keeps serving the Cluster object, so no poll can converge.
  run_delete "${scratch}" "${kc}" harness \
    "DELETE_WAIT_TIMEOUT=${DELETE_WAIT_SHORT}" \
    "STUB_CLUSTER_GONE_RC=0"
  assert_delete_timeout "${scratch}" "the Cluster object never reports deleted" "cluster-delete" || :
  rm -rf -- "${scratch}"
}

test_no_host_net_tooling() {
  log "scenario: the deletion converges without any host network tooling"
  local scratch="" kc=""
  scratch="$(new_scenario no-host-net-tooling)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" harness
  assert_delete_passed "${scratch}" "a scenario with sentinel ip/pgrep/nft/dnsmasq on PATH" || :
  assert_no_net_tooling "${scratch}" "a scenario with sentinel ip/pgrep/nft/dnsmasq on PATH" || :
  rm -rf -- "${scratch}"
}

test_mgmt_down_external() {
  log "scenario: the mgmt-down step is skipped for an external management plane"
  local scratch="" kc=""

  # An external plane (MANAGEMENT_KUBECONFIG set): the whole scenario
  # converges and the stub mgmt-down is never invoked.
  scratch="$(new_scenario mgmt-down-external)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" harness
  assert_delete_passed "${scratch}" "an external-management deletion scenario" || :
  assert_no_down_calls "${scratch}" "an external management kubeconfig" || :
  rm -rf -- "${scratch}"

  # The other half of the mgmt-down contract: a self-bootstrapped plane (no
  # MANAGEMENT_KUBECONFIG) must run mgmt-down exactly once through the
  # injected stub.
  scratch="$(new_scenario mgmt-down-selfboot)"
  kc="$(make_kubeconfig "${scratch}")"
  run_delete "${scratch}" "${kc}" selfboot
  assert_delete_passed "${scratch}" "a self-bootstrapped deletion scenario" || :
  assert_down_called_once "${scratch}" "a self-bootstrapped management plane" || :
  rm -rf -- "${scratch}"
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'delete_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  # The cluster-deletion script under test is created by the implementation;
  # until then the red phase is this explicit, readable failure.
  if [[ ! -e "${DELETE_SH}" ]]; then
    fail "cluster-deletion scenario script not found: ${DELETE_SH} (the implementation must provide it)" 1
  fi
  if [[ ! -f "${DELETE_SH}" ]]; then
    fail "cluster-deletion scenario script path is not a regular file: ${DELETE_SH}" 1
  fi
  if [[ ! -x "${DELETE_SH}" ]]; then
    fail "cluster-deletion scenario script is not executable: ${DELETE_SH}" 1
  fi

  command -v env >/dev/null 2>&1 \
    || fail "env (coreutils) is required to isolate the stub environment" 2
  command -v mktemp >/dev/null 2>&1 \
    || fail "mktemp (coreutils) is required for the scratch fixtures" 2
  command -v timeout >/dev/null 2>&1 \
    || fail "timeout (coreutils) is required for the run guard" 2

  SCRATCH="$(mktemp -d)"
  trap 'rm -rf -- "${SCRATCH}"' EXIT
  log "scratch root ${SCRATCH}"

  test_delete_success || :
  test_cluster_delete_timeout || :
  test_no_host_net_tooling || :
  test_mgmt_down_external || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "cluster-deletion scenario contract check failed: ${problems} problem(s)" 1
  fi
  log "cluster-deletion scenario contract satisfied"
}

main "$@"
