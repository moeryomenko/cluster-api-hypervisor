#!/usr/bin/env bash
#
# scale_test.sh — verify the per-step contract of the scale scenario script
# (test/e2e/scale.sh).
#
# The full-lab harness (test/e2e/run.sh) runs the scale scenario against a
# live lab: the management cluster plus the workload cluster. That cannot
# happen here: there is no live cluster in this test. What this file pins
# instead is the per-step contract of scale.sh, by executing it against a stub
# kubectl on PATH that dispatches on its arguments and returns scripted canned
# outputs, then asserting scale.sh's decisions and its aggregate exit code.
#
# The kubeconfig contract mirrors the harness exactly: run.sh hands the
# management kubeconfig to its helpers as the KUBECONFIG environment variable
# and as the first positional argument. scale.sh accepts the same shapes: the
# management kubeconfig comes from KUBECONFIG or from $1 (a positional
# argument wins, exactly like smoke.sh), and the target worker replicas value
# comes from the REPLICAS environment variable or from $2. The stub kubectl
# records which kubeconfig every invocation used (a --kubeconfig flag, a
# --kubeconfig=value, or the KUBECONFIG environment), so the tests can assert
# that scale.sh really targets the management cluster it was given and that
# its node polls go through the workload kubeconfig extracted from the
# <cluster>-kubeconfig Secret.
#
# Per-step contract pinned (each wait's pass/fail semantics):
#   1. scale-up  — scale.sh patches the Cluster topology worker
#      machineDeployments replicas to the target, then polls the management
#      cluster for the worker-labeled Machines until the worker count reaches
#      the target (the new Machine has appeared). The stub models the bump as
#      a timeline: the first machine list serves the pre-bump baseline, later
#      lists serve the post-bump set, and once a `kubectl delete machine` has
#      been seen, lists serve the post-delete set.
#   2. VM boot   — for the new Machine, the linked HypervisorMachine must
#      report ready with status.addresses populated (the VM is up and
#      reachable).
#   3. node-ready — scale.sh extracts the workload kubeconfig from the
#      <cluster>-kubeconfig Secret and polls the workload cluster until every
#      node is Ready (control-plane + the target number of workers).
#   4. delete    — scale.sh deletes a worker Machine and polls the management
#      cluster until the worker Machine count drops back below the target
#      (the Machine is gone; the linked HypervisorMachine is served as gone
#      too, host-level VM/TAP/disk cleanup being verified by the lab).
#   5. timeout   — any wait that does not converge within scale.sh's own wait
#      budget (SCALE_WAIT_TIMEOUT, default 1800s) makes scale.sh exit non-zero
#      with an error line that names the step that timed out. The step names
#      pinned are "scale-up", "node-ready", and "delete".
#
# Aggregate contract: scale.sh exits 0 only when the whole scenario converges
# (scale-up, VM boot, node Ready, scale-down). Every timeout scenario keeps
# everything healthy except the step under test, so a non-zero exit is
# attributable to exactly that step and the output must name it.
#
# Exit codes of this test:
#   0  scale.sh satisfies the scale-scenario contract
#   1  contract violation (including scale.sh being absent: the red phase)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/scale_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
SCALE_SH="${SCRIPT_DIR}/scale.sh"
readonly SCALE_SH

# A single scale.sh run must return quickly against the stub kubectl; the
# timeout guards against a scale.sh that hangs instead of reporting.
readonly SCALE_TIMEOUT=60
# scale.sh's own per-step wait budget. Success scenarios allow a comfortable
# 10s (the stub converges after one or two polls); timeout scenarios shrink
# the budget to 3s so the timed-out step fails fast.
readonly SCALE_WAIT_OK=10
readonly SCALE_WAIT_SHORT=3
# The target worker replicas every scenario drives towards. The stub's
# timeline starts at three workers (w1..w3), converges to four (w4 appears
# after the bump), and drops back to three after the delete.
readonly TARGET_REPLICAS=4

# The fixture worker-machine lists shared by the timeout scenarios.
readonly WORKERS_BASELINE="w1 k8labs Running 30m v1.35.4
w2 k8labs Running 30m v1.35.4
w3 k8labs Running 30m v1.35.4"
readonly WORKERS_NEVER_SCALED="${WORKERS_BASELINE}"
readonly NODES_WITH_UNREADY_WORKER="cp1 Ready control-plane 45m v1.35.4
w1 Ready <none> 30m v1.35.4
w2 Ready <none> 30m v1.35.4
w3 Ready <none> 30m v1.35.4
w4 NotReady <none> 1m v1.35.4"

problems=0
SCRATCH=""
SCALE_RC=0

log() { printf 'scale_test: %s\n' "$*" >&2; }

ok() { printf 'scale_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'scale_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'scale_test: %s\n' "$message" >&2
  exit "$code"
}

# install_stub_kubectl <dir> — write an executable stub kubectl into <dir>.
# The stub dispatches on its arguments and prints canned outputs driven by the
# STUB_* environment variables; every variable defaults to an all-healthy
# cluster. It models the scale timeline with two pieces of state persisted in
# STUB_STATE_DIR: a counter of machine-list calls (the first list is the
# pre-bump baseline, later lists are the post-bump set) and a delete-seen
# marker (once `kubectl delete machine` runs, machine lists serve the
# post-delete set and the deleted Machine's objects report gone). Each
# invocation is appended to "$STUB_LOG" (when set) together with the
# kubeconfig it used. The dispatch matches resource-name substrings, not flag
# positions, because the scale steps invoke kubectl with inconsistent flag
# ordering.
install_stub_kubectl() {
  local dir="$1"
  cat > "${dir}/kubectl" <<'STUB'
#!/usr/bin/env bash
set -u

stub_default_workers_baseline="w1 k8labs Running 30m v1.35.4
w2 k8labs Running 30m v1.35.4
w3 k8labs Running 30m v1.35.4"
stub_default_workers_bump="${stub_default_workers_baseline}
w4 k8labs Running 1m v1.35.4"
stub_default_workers_after="${stub_default_workers_baseline}"
stub_default_cp="cp1 k8labs Running 45m v1.35.4"
stub_default_nodes="cp1 Ready control-plane 45m v1.35.4
w1 Ready <none> 30m v1.35.4
w2 Ready <none> 30m v1.35.4
w3 Ready <none> 30m v1.35.4
w4 Ready <none> 1m v1.35.4"
stub_default_nodes_after="${stub_default_cp}
w1 Ready <none> 30m v1.35.4
w2 Ready <none> 30m v1.35.4
w3 Ready <none> 30m v1.35.4"
stub_default_secret="YXBpVmVyc2lvbjogdjEKa2luZDogQ29uZmlnCmNsdXN0ZXJzOiBbXQp1c2VyczogW10KY29udGV4dHM6IFtdCmN1cnJlbnQtY29udGV4dDogInN0dWIiCg=="

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

# Normalize "machines" to "machine" for substring dispatch; the plural form is
# accepted for the same resources.
args="$*"
dispatch="${args//machines/machine}"

# The first non-flag token after the resource verb is the object name.
machine_name=""
found_verb=0
for arg in "$@"; do
  if [[ "${found_verb}" -eq 0 ]]; then
    if [[ "${arg}" == "machine" || "${arg}" == "machines" ]]; then
      found_verb=1
    fi
    continue
  fi
  if [[ "${arg}" != -* ]]; then
    machine_name="${arg}"
    break
  fi
done

node_name=""
found_verb=0
for arg in "$@"; do
  if [[ "${found_verb}" -eq 0 ]]; then
    if [[ "${arg}" == "node" || "${arg}" == "nodes" ]]; then
      found_verb=1
    fi
    continue
  fi
  if [[ "${arg}" != -* ]]; then
    node_name="${arg}"
    break
  fi
done

# stub_workers — the worker Machine list for the current timeline phase:
# post-delete once a delete was seen, otherwise the pre-bump baseline on the
# very first list call and the post-bump set afterwards.
stub_workers() {
  if [[ "${delete_seen}" -eq 1 ]]; then
    printf '%s\n' "${STUB_WORKERS_AFTER-${stub_default_workers_after}}"
    return 0
  fi
  local count=0
  if [[ -n "${state_dir}" && -f "${state_dir}/list-count" ]]; then
    count="$(<"${state_dir}/list-count")"
  fi
  if [[ "${count}" -eq 0 ]]; then
    printf '%s\n' "${STUB_WORKERS_BASELINE-${stub_default_workers_baseline}}"
  else
    printf '%s\n' "${STUB_WORKERS_BUMP-${stub_default_workers_bump}}"
  fi
  if [[ -n "${state_dir}" ]]; then
    printf '%s' "$((count + 1))" > "${state_dir}/list-count"
  fi
}

all_machine_list() {
  printf '%s\n' "${STUB_CP_MACHINE-${stub_default_cp}}"
  stub_workers
}

# emit_machine_list <list> — print a machine list in the format the caller
# asked for: names or per-machine Ready conditions for jsonpath queries,
# "machine/<name>" lines for -o name, and the plain table otherwise.
emit_machine_list() {
  local list="$1"
  if [[ "${dispatch}" == *jsonpath* ]]; then
    if [[ "${dispatch}" == *'status.conditions'* || "${dispatch}" == *'Ready'* ]]; then
      printf '%s\n' "${list}" | awk '{print "True"}'
    else
      printf '%s\n' "${list}" | awk '{print $1}'
    fi
    return 0
  fi
  if [[ "${dispatch}" == *'-o name'* ]]; then
    printf '%s\n' "${list}" | awk '{print "machine/"$1}'
    return 0
  fi
  printf '%s\n' "${list}"
}

# emit_node_list <list> — print a node list in the requested format: node
# names or Ready statuses for jsonpath queries (the node table's second column
# is the Ready status), "node/<name>" lines for -o name, plain otherwise.
emit_node_list() {
  local list="$1"
  if [[ "${dispatch}" == *jsonpath* ]]; then
    if [[ "${dispatch}" == *'status.conditions'* || "${dispatch}" == *'Ready'* ]]; then
      printf '%s\n' "${list}" | awk '{print $2}'
    else
      printf '%s\n' "${list}" | awk '{print $1}'
    fi
    return 0
  fi
  if [[ "${dispatch}" == *'-o name'* ]]; then
    printf '%s\n' "${list}" | awk '{print "node/"$1}'
    return 0
  fi
  printf '%s\n' "${list}"
}

case "${dispatch}" in
  *'patch cluster'*)
    exit "${STUB_PATCH_RC-0}"
    ;;
  *'delete machine'*)
    if [[ -n "${state_dir}" ]]; then
      touch "${state_dir}/delete-seen"
    fi
    exit "${STUB_DELETE_RC-0}"
    ;;
  *'get secret'*'kubeconfig'*)
    printf '%s\n' "${STUB_SECRET_VALUE-${stub_default_secret}}"
    exit 0
    ;;
  *'get hypervisormachine'*)
    if [[ "${delete_seen}" -eq 1 && "${STUB_DELETED_MACHINE_RC-1}" -ne 0 ]]; then
      exit 1
    fi
    case "${dispatch}" in
      *'status.addresses'*)
        printf '%s\n' "${STUB_HM_ADDRESSES-192.168.124.41}"
        ;;
      *)
        printf '%s\n' "${STUB_HM_READY-true}"
        ;;
    esac
    exit 0
    ;;
  *'get node '*)
    if [[ "${delete_seen}" -eq 1 && "${STUB_DELETED_MACHINE_RC-1}" -ne 0 ]]; then
      exit 1
    fi
    if [[ "${dispatch}" == *jsonpath* ]]; then
      if [[ "${dispatch}" == *'Ready'* || "${dispatch}" == *'status.conditions'* ]]; then
        printf '%s\n' "${STUB_NODE_READY-True}"
      else
        printf '%s\n' "${node_name}"
      fi
      exit 0
    fi
    printf '%s\n' "${node_name:-w4} Ready <none> 1m v1.35.4"
    exit 0
    ;;
  *'get nodes'*)
    if [[ "${delete_seen}" -eq 1 && "${STUB_DELETED_MACHINE_RC-1}" -ne 0 ]]; then
      emit_node_list "${STUB_NODES_AFTER-${stub_default_nodes_after}}"
    else
      emit_node_list "${STUB_NODES-${stub_default_nodes}}"
    fi
    exit 0
    ;;
  *'get machine'*'infrastructureRef'*)
    printf '%s\n' "${STUB_MACHINE_INFRAREF-${machine_name}}"
    exit 0
    ;;
  *'get machine'*'-l '*)
    emit_machine_list "$(stub_workers)"
    exit 0
    ;;
  *'get machine'*)
    if [[ -n "${machine_name}" ]]; then
      if [[ "${delete_seen}" -eq 1 && "${STUB_DELETED_MACHINE_RC-1}" -ne 0 ]]; then
        exit 1
      fi
      if [[ "${dispatch}" == *jsonpath* ]]; then
        if [[ "${dispatch}" == *'status.phase'* ]]; then
          printf '%s\n' "Running"
        else
          printf '%s\n' "${machine_name}"
        fi
        exit 0
      fi
      printf '%s\n' "${machine_name} k8labs Running 1m v1.35.4"
      exit 0
    fi
    emit_machine_list "$(all_machine_list)"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "${dir}/kubectl"
}

# new_scenario <label> — create a scratch fixture: a stub kubectl on PATH, a
# stub state directory, and a minimal management kubeconfig. Prints the
# scratch directory path.
new_scenario() {
  local label="$1"
  local scratch=""
  scratch="$(mktemp -d "${SCRATCH}/${label}.XXXXXX")"
  install_stub_kubectl "${scratch}"
  mkdir -p "${scratch}/stub-state"
  printf '%s' "${scratch}"
}

# make_kubeconfig <dir> — write a minimal non-empty management kubeconfig
# fixture into <dir>; the stub kubectl never reads it, but scale.sh may stat
# it. Prints the kubeconfig path.
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

# run_scale <scratch> <kubeconfig> [mode] [VAR=value ...] — execute scale.sh
# against the stub kubectl installed in <scratch>. The target replicas is
# always TARGET_REPLICAS. mode "harness" mirrors the full-lab harness:
# KUBECONFIG env plus both positionals (<kubeconfig> <replicas>). mode "env"
# passes only the KUBECONFIG and REPLICAS environment variables. mode "poskc"
# passes the kubeconfig only as the first positional (no KUBECONFIG env) with
# REPLICAS from the environment. Extra VAR=value arguments become additional
# environment assignments for scale.sh and the stub kubectl; a passed
# SCALE_WAIT_TIMEOUT=... overrides the default 10s budget. scale.sh's combined
# output is written to <scratch>/scale.out and SCALE_RC holds its exit status
# (124 when the run exceeds SCALE_TIMEOUT).
run_scale() {
  local scratch="$1" kubeconfig="$2" mode="${3:-harness}"
  local log="${scratch}/kubectl.log"
  local -a env_extra=()
  local arg="" rc=0
  local wait_timeout="${SCALE_WAIT_OK}"
  for arg in "${@:4}"; do
    if [[ "${arg}" == SCALE_WAIT_TIMEOUT=* ]]; then
      wait_timeout="${arg#SCALE_WAIT_TIMEOUT=}"
    fi
    env_extra+=("${arg}")
  done
  case "${mode}" in
    env)
      timeout "${SCALE_TIMEOUT}" env "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" \
        REPLICAS="${TARGET_REPLICAS}" STUB_LOG="${log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        SCALE_WAIT_TIMEOUT="${wait_timeout}" \
        "${SCALE_SH}" >"${scratch}/scale.out" 2>&1 || rc=$?
      ;;
    poskc)
      timeout "${SCALE_TIMEOUT}" env -u KUBECONFIG "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" REPLICAS="${TARGET_REPLICAS}" \
        STUB_LOG="${log}" STUB_STATE_DIR="${scratch}/stub-state" \
        SCALE_WAIT_TIMEOUT="${wait_timeout}" \
        "${SCALE_SH}" "${kubeconfig}" >"${scratch}/scale.out" 2>&1 || rc=$?
      ;;
    *)
      timeout "${SCALE_TIMEOUT}" env "${env_extra[@]}" \
        PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" \
        REPLICAS="${TARGET_REPLICAS}" STUB_LOG="${log}" \
        STUB_STATE_DIR="${scratch}/stub-state" \
        SCALE_WAIT_TIMEOUT="${wait_timeout}" \
        "${SCALE_SH}" "${kubeconfig}" "${TARGET_REPLICAS}" \
        >"${scratch}/scale.out" 2>&1 || rc=$?
      ;;
  esac
  SCALE_RC="${rc}"
}

# assert_scale_passed <scratch> <display> — scale.sh must exit 0 and its
# output must not report a wait timeout.
assert_scale_passed() {
  local scratch="$1" display="$2"
  local out="" rc="${SCALE_RC}"
  out="$(<"${scratch}/scale.out")"
  if [[ "${rc}" -ne 0 ]]; then
    missing "scale.sh exited ${rc}; expected 0 for ${display}"
    printf 'scale_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if grep -Eqi 'timeout' <<< "${out}"; then
    missing "scale.sh reported a wait timeout for ${display}"
    printf 'scale_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  ok "scale.sh exited 0 for ${display}"
}

# assert_scale_timeout <scratch> <display> <step> — scale.sh must exit
# non-zero (but not be killed by the outer guard), and an error line must name
# the timed-out step: a line mentioning the timeout that also contains the
# step identifier ("scale-up", "node-ready", or "delete").
assert_scale_timeout() {
  local scratch="$1" display="$2" step="$3"
  local out="" rc="${SCALE_RC}"
  out="$(<"${scratch}/scale.out")"
  if [[ "${rc}" -eq 0 ]]; then
    missing "scale.sh exited 0; expected non-zero when ${display}"
    printf 'scale_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if [[ "${rc}" -eq 124 ]]; then
    missing "scale.sh exceeded the ${SCALE_TIMEOUT}s test guard when ${display} (did it honor SCALE_WAIT_TIMEOUT?)"
    printf 'scale_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if ! grep -Ei '(timeout|timed out)' <<< "${out}" | grep -Fq -- "${step}"; then
    missing "scale.sh exited ${rc} but did not name the '${step}' step when ${display}"
    printf 'scale_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  ok "scale.sh exited ${rc} and named the '${step}' step when ${display}"
}

# assert_kubeconfig_shape <scratch> <kubeconfig> <display> — scale.sh must
# exit 0, the stub must have observed the management kubeconfig being used for
# the management-cluster work, and at least two distinct kubeconfigs must
# appear overall: the management one plus the workload kubeconfig extracted
# from the <cluster>-kubeconfig Secret and used for the node polls.
assert_kubeconfig_shape() {
  local scratch="$1" kubeconfig="$2" display="$3"
  local kclog="" distinct=0 rc="${SCALE_RC}"
  if [[ "${rc}" -ne 0 ]]; then
    missing "scale.sh exited ${rc}; expected 0 for ${display}"
    printf 'scale_test: output:\n%s\n' "$(<"${scratch}/scale.out")" >&2
    return 1
  fi
  kclog=""
  if [[ -f "${scratch}/kubectl.log" ]]; then
    kclog="$(<"${scratch}/kubectl.log")"
  fi
  if ! grep -Fq -- "kubeconfig=${kubeconfig}" <<< "${kclog}"; then
    missing "the stub kubectl never observed the management kubeconfig ${kubeconfig} for ${display}"
    printf 'scale_test: kubectl log:\n%s\n' "${kclog}" >&2
    return 1
  fi
  distinct="$(grep '^kubeconfig=' <<< "${kclog}" | sort -u | wc -l || true)"
  if [[ "${distinct}" -lt 2 ]]; then
    missing "scale.sh never used the workload kubeconfig from the <cluster>-kubeconfig Secret for ${display} (saw ${distinct} distinct kubeconfigs)"
    printf 'scale_test: kubectl log:\n%s\n' "${kclog}" >&2
    return 1
  fi
  ok "scale.sh targeted the management kubeconfig and the Secret-derived workload kubeconfig for ${display}"
}

# --- scenarios --------------------------------------------------------------

test_scale_up_success() {
  log "scenario: scale-up, VM boot, node Ready, and scale-down all converge"
  local scratch="" kc=""
  scratch="$(new_scenario scale-up)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" harness
  assert_scale_passed "${scratch}" "an all-healthy scale scenario" || :
  assert_kubeconfig_shape "${scratch}" "${kc}" "an all-healthy scale scenario" || :
  rm -rf -- "${scratch}"
}

test_machine_not_created_timeout() {
  log "scenario: the new Machine never appears"
  local scratch="" kc=""
  scratch="$(new_scenario machine-never-appears)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" harness \
    "SCALE_WAIT_TIMEOUT=${SCALE_WAIT_SHORT}" \
    "STUB_WORKERS_BUMP=${WORKERS_NEVER_SCALED}"
  assert_scale_timeout "${scratch}" "the worker Machine count never reaches the target" "scale-up" || :
  rm -rf -- "${scratch}"
}

test_node_not_ready_timeout() {
  log "scenario: the Machine appears but the node never becomes Ready"
  local scratch="" kc=""
  scratch="$(new_scenario node-never-ready)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" harness \
    "SCALE_WAIT_TIMEOUT=${SCALE_WAIT_SHORT}" \
    "STUB_NODES=${NODES_WITH_UNREADY_WORKER}" \
    "STUB_NODE_READY=False"
  assert_scale_timeout "${scratch}" "the new worker node never reports Ready" "node-ready" || :
  rm -rf -- "${scratch}"
}

test_machine_delete_timeout() {
  log "scenario: the deleted Machine never disappears"
  local scratch="" kc=""
  scratch="$(new_scenario machine-delete-timeout)"
  kc="$(make_kubeconfig "${scratch}")"
  # The stub must serve the post-delete worker list with the deleted Machine
  # still present, and keep serving the Machine itself, so no poll can
  # converge.
  run_scale "${scratch}" "${kc}" harness \
    "SCALE_WAIT_TIMEOUT=${SCALE_WAIT_SHORT}" \
    "STUB_WORKERS_AFTER=${WORKERS_NEVER_SCALED}
w4 k8labs Running 1m v1.35.4" \
    "STUB_DELETED_MACHINE_RC=0"
  assert_scale_timeout "${scratch}" "the deleted Machine never disappears" "delete" || :
  rm -rf -- "${scratch}"
}

test_kubeconfig_shape() {
  log "scenario: scale.sh consumes the management kubeconfig the harness passes"
  local scratch="" kc=""

  # Harness shape: KUBECONFIG env plus both positionals, exactly how run.sh
  # would invoke the scenario helper.
  scratch="$(new_scenario kubeconfig-harness)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" harness
  assert_kubeconfig_shape "${scratch}" "${kc}" "the harness invocation shape (KUBECONFIG env plus positionals)" || :
  rm -rf -- "${scratch}"

  # Environment-only shape: no positionals at all.
  scratch="$(new_scenario kubeconfig-env)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" env
  assert_kubeconfig_shape "${scratch}" "${kc}" "the environment-only invocation shape (KUBECONFIG and REPLICAS)" || :
  rm -rf -- "${scratch}"

  # Positional-kubeconfig shape: the management kubeconfig arrives only as the
  # first positional argument, the replicas value from the environment.
  scratch="$(new_scenario kubeconfig-positional)"
  kc="$(make_kubeconfig "${scratch}")"
  run_scale "${scratch}" "${kc}" poskc
  assert_kubeconfig_shape "${scratch}" "${kc}" "the positional-kubeconfig invocation shape" || :
  rm -rf -- "${scratch}"
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'scale_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  # The scale script under test is created by the implementation; until then
  # the red phase is this explicit, readable failure.
  if [[ ! -e "${SCALE_SH}" ]]; then
    fail "scale scenario script not found: ${SCALE_SH} (the implementation must provide it)" 1
  fi
  if [[ ! -f "${SCALE_SH}" ]]; then
    fail "scale scenario script path is not a regular file: ${SCALE_SH}" 1
  fi
  if [[ ! -x "${SCALE_SH}" ]]; then
    fail "scale scenario script is not executable: ${SCALE_SH}" 1
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

  test_scale_up_success || :
  test_machine_not_created_timeout || :
  test_node_not_ready_timeout || :
  test_machine_delete_timeout || :
  test_kubeconfig_shape || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "scale-scenario contract check failed: ${problems} problem(s)" 1
  fi
  log "scale-scenario contract satisfied"
}

main "$@"
