#!/usr/bin/env bash
#
# upgrade_test.sh — verify the per-step contract of the upgrade scenario
# script (test/e2e/upgrade.sh).
#
# The full-lab harness (test/e2e/run.sh) runs the upgrade scenario against a
# live lab: the management cluster plus the workload cluster. That cannot
# happen here: there is no live cluster in this test. What this file pins
# instead is the per-step contract of upgrade.sh, by executing it against a
# stub kubectl on PATH that dispatches on its arguments and returns scripted
# canned outputs, then asserting upgrade.sh's decisions and its aggregate
# exit code.
#
# The kubeconfig contract mirrors the harness exactly: run.sh hands the
# management kubeconfig to its helpers as the KUBECONFIG environment variable
# and as the first positional argument. upgrade.sh accepts the same shapes:
# the management kubeconfig comes from KUBECONFIG or from $1 (a positional
# argument wins), and the target version comes from TO_VERSION or from $2.
#
# Per-step contract pinned (each wait's pass/fail semantics):
#   1. preflight — upgrade.sh requires the HypervisorUpgradePlan CRD, the
#      Cluster with a topology version different from the target, and a base
#      image registered for the target version. Any failure exits non-zero
#      with an error line naming "preflight".
#   2. plan      — upgrade.sh applies the HypervisorUpgradePlan and polls its
#      status phase until it settles on Completed or Failed. A Failed plan
#      exits non-zero with the plan's failure reason and message; a plan that
#      never settles within the wait budget exits non-zero naming "plan".
#   3. nodes     — upgrade.sh extracts the workload kubeconfig from the
#      <cluster>-kubeconfig Secret and polls until every node is Ready on the
#      target kubelet version.
#
# Aggregate contract: upgrade.sh exits 0 only when the whole scenario
# converges. Every failure scenario keeps everything healthy except the step
# under test, so a non-zero exit is attributable to exactly that step and the
# output must name it.
#
# Exit codes of this test:
#   0  upgrade.sh satisfies the upgrade-scenario contract
#   1  contract violation (including upgrade.sh being absent: the red phase)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/upgrade_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
UPGRADE_SH="${SCRIPT_DIR}/upgrade.sh"
readonly UPGRADE_SH

# A single upgrade.sh run must return quickly against the stub kubectl; the
# timeout guards against an upgrade.sh that hangs instead of reporting.
readonly UPGRADE_TIMEOUT=60
# upgrade.sh's own per-step wait budget. Success scenarios allow a comfortable
# 15s (the stub converges after one or two polls); timeout scenarios shrink
# the budget to 3s so the timed-out step fails fast.
readonly UPGRADE_WAIT_OK=15
readonly UPGRADE_WAIT_SHORT=3
# The version every scenario upgrades to. The stub serves the Cluster
# topology at v1.37.0 and the plan/nodes converge on TARGET_VERSION.
readonly TARGET_VERSION="v1.38.0"

problems=0
SCRATCH=""
UPGRADE_RC=0

log() { printf 'upgrade_test: %s\n' "$*" >&2; }

ok() { printf 'upgrade_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'upgrade_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'upgrade_test: %s\n' "$message" >&2
  exit "$code"
}

# install_stub_kubectl <dir> — write an executable stub kubectl into <dir>.
# The stub dispatches on its arguments and prints canned outputs driven by
# the STUB_* environment variables; every variable defaults to an all-healthy
# upgrade. Plan phase polling is a timeline: STUB_PLAN_PHASES lists the phases
# served in order (a call counter persisted in STUB_STATE_DIR walks the list
# and sticks at the last entry). Each invocation is appended to "$STUB_LOG"
# (when set).
install_stub_kubectl() {
  local dir="$1"
  cat > "${dir}/kubectl" <<'STUB'
#!/usr/bin/env bash
set -u

# Record the invocation.
if [[ -n "${STUB_LOG:-}" ]]; then
  printf 'kubectl %s\n' "$*" >> "${STUB_LOG}"
fi

args="$*"
state_dir="${STUB_STATE_DIR:-/tmp}"

# get nodes against the workload kubeconfig serves the node table.
if [[ "${args}" == *"get nodes"* ]]; then
  printf '%s\n' "${STUB_NODES:-"cp1 True v1.38.0"}"
  exit 0
fi

# get secret serves a base64 workload kubeconfig.
if [[ "${args}" == *"get secret"* ]]; then
  printf '%s' "${STUB_SECRET:-"YXBpVmVyc2lvbjogdjEKa2luZDogQ29uZmlnCmNsdXN0ZXJzOiBbXQp1c2VyczogW10KY29udGV4dHM6IFtdCmN1cnJlbnQtY29udGV4dDogInN0dWIiCg=="}"
  exit 0
fi

# The plan CRD presence gate.
if [[ "${args}" == *"get crd"* ]]; then
  exit "${STUB_CRD_RC:-0}"
fi

# Cluster topology version (preflight + control-plane step). Once the plan
# settles on Completed the topology controller has propagated the target
# version, so the stub serves the done version after the plan_done marker
# (written by the plan branch below) appears.
if [[ "${args}" == *"get cluster"* && "${args}" == *"topology.version"* ]]; then
  if [[ -f "${state_dir}/plan_done" ]]; then
    printf '%s' "${STUB_TOPOLOGY_VERSION_DONE:-v1.38.0}"
  else
    printf '%s' "${STUB_TOPOLOGY_VERSION:-v1.37.0}"
  fi
  exit 0
fi
if [[ "${args}" == *"get cluster "* ]]; then
  exit 0
fi

# Registered base images (preflight).
if [[ "${args}" == *"get hypervisorcluster"* ]]; then
  printf '%s' "${STUB_IMAGES:-v1.37.0 v1.38.0}"
  exit 0
fi

# Plan apply consumes the manifest from stdin.
if [[ "${args}" == *"apply"* ]]; then
  cat > /dev/null
  if [[ -n "${STUB_APPLIED_TO:-}" ]]; then
    printf '%s' "${args}" > "${STUB_APPLIED_TO}"
  fi
  exit 0
fi

# Plan status reads walk the phase timeline and stick at the last entry.
if [[ "${args}" == *"HypervisorUpgradePlan"* ]]; then
  counter="${state_dir}/plan_calls"
  calls=0
  if [[ -f "${counter}" ]]; then
    calls="$(cat "${counter}")"
  fi
  calls=$((calls + 1))
  printf '%s' "${calls}" > "${counter}"
  phases="${STUB_PLAN_PHASES:-Completed}"
  # shellcheck disable=SC2206
  phase_list=(${phases})
  index=$((calls - 1))
  if (( index >= ${#phase_list[@]} )); then
    index=$((${#phase_list[@]} - 1))
  fi
  phase="${phase_list[${index}]}"
  if [[ "${phase}" == "Completed" ]]; then
    touch "${state_dir}/plan_done"
  fi
  if [[ "${args}" == *"failureReason"* ]]; then
    printf '%s' "${STUB_FAILURE_REASON:-ImageNotRegistered}"
    exit 0
  fi
  if [[ "${args}" == *"failureMessage"* ]]; then
    printf '%s' "${STUB_FAILURE_MESSAGE:-no base image registered}"
    exit 0
  fi
  printf '%s' "${phase}"
  exit 0
fi

# Machine version reads: the worker filter (control-plane==null) serves the
# worker versions, any other control-plane mention serves the CP version.
if [[ "${args}" == *"get machine"* ]]; then
  if [[ "${args}" == *"control-plane==null"* ]]; then
    printf '%s' "${STUB_WORKER_VERSIONS:-v1.38.0 v1.38.0}"
    exit 0
  fi
  if [[ "${args}" == *"control-plane"* ]]; then
    printf '%s' "${STUB_CP_VERSION:-v1.38.0}"
    exit 0
  fi
  exit 0
fi

exit 0
STUB
  chmod +x "${dir}/kubectl"
}

# run_upgrade — execute upgrade.sh against the stub kubectl with the given
# environment, capturing combined output and exit code. The caller sets the
# STUB_* variables beforehand via export. The exit code rides on the last
# output line as UPGRADE_TEST_RC=<n>: run_upgrade always executes inside a
# command substitution (the caller captures its stdout), so a global the
# function sets would die with the subshell — only stdout escapes.
# Callers split the line back off with split_run_result.
run_upgrade() {
  local stub_bin="$1"
  shift
  local out="" rc=0
  out="$(PATH="${stub_bin}:${PATH}" timeout "${UPGRADE_TIMEOUT}" bash "${UPGRADE_SH}" "$@" 2>&1)" || rc=$?
  printf '%s\nUPGRADE_TEST_RC=%d\n' "${out}" "${rc}"
}

# split_run_result — split run_upgrade's trailing UPGRADE_TEST_RC line back
# into the UPGRADE_RC global and the scenario output in out.
split_run_result() {
  UPGRADE_RC="${out##*$'\n'UPGRADE_TEST_RC=}"
  out="${out%$'\n'UPGRADE_TEST_RC=*}"
}

# with_scratch — create an isolated scratch dir with a stub bin dir and a
# state dir, exporting STUB_STATE_DIR and STUB_LOG for the stub.
with_scratch() {
  SCRATCH="$(mktemp -d)"
  mkdir -p "${SCRATCH}/bin" "${SCRATCH}/state"
  export STUB_STATE_DIR="${SCRATCH}/state"
  export STUB_LOG="${SCRATCH}/kubectl.log"
  install_stub_kubectl "${SCRATCH}/bin"
  printf '%s' "${SCRATCH}/bin"
}

# reset_stub — clear every STUB_* override so scenarios start all-healthy.
# It also unsets the upgrade.sh inputs: prefix assignments before a function
# call persist in bash, so without this a TO_VERSION set by one scenario
# would leak into the next.
reset_stub() {
  unset KUBECONFIG TO_VERSION
  unset STUB_CRD_RC STUB_TOPOLOGY_VERSION STUB_IMAGES STUB_PLAN_PHASES
  unset STUB_FAILURE_REASON STUB_FAILURE_MESSAGE STUB_CP_VERSION
  unset STUB_WORKER_VERSIONS STUB_NODES STUB_SECRET STUB_APPLIED_TO
  export STUB_STATE_DIR="${SCRATCH}/state"
  export STUB_LOG="${SCRATCH}/kubectl.log"
  rm -f "${SCRATCH}/state/plan_calls" "${SCRATCH}/state/plan_done"
}

# expect_pass <name> — the scenario must exit 0 with a PASS line.
expect_pass() {
  local name="$1" out="$2"
  if (( UPGRADE_RC != 0 )); then
    missing "${name}: exit ${UPGRADE_RC}, want 0 (output: ${out})"
    return
  fi
  if [[ "${out}" != *"PASS: upgrade scenario complete"* ]]; then
    missing "${name}: no PASS line (output: ${out})"
    return
  fi
  ok "${name}"
}

# expect_fail_named <name> <step> — the scenario must exit non-zero with an
# error line naming the step.
expect_fail_named() {
  local name="$1" step="$2" out="$3"
  if (( UPGRADE_RC == 0 )); then
    missing "${name}: exit 0, want non-zero"
    return
  fi
  if [[ "${out}" != *"ERROR: ${step}"* ]]; then
    missing "${name}: error does not name step ${step} (output: ${out})"
    return
  fi
  ok "${name}"
}

main() {
  command -v timeout >/dev/null 2>&1 || fail "timeout is required on PATH" 2
  command -v mktemp >/dev/null 2>&1 || fail "mktemp is required on PATH" 2
  [[ -f "${UPGRADE_SH}" ]] || fail "upgrade.sh is absent: ${UPGRADE_SH}" 1
  [[ -x "${UPGRADE_SH}" ]] || fail "upgrade.sh is not executable: ${UPGRADE_SH}" 1

  local stub_bin=""
  # NB: called in the current shell (not a command substitution) so the
  # SCRATCH global it sets survives for reset_stub and the scenarios.
  with_scratch >/dev/null
  stub_bin="${SCRATCH}/bin"
  local out=""

  # 1. happy path — the whole scenario converges on the target version.
  export UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}"
  reset_stub
  out="$(run_upgrade "${stub_bin}" "/stub/mgmt.kubeconfig" "${TARGET_VERSION}")"
  split_run_result
  expect_pass "happy path converges" "${out}"

  # The happy path really applied a plan: the stub recorded an apply.
  export STUB_APPLIED_TO="${SCRATCH}/applied.txt"
  reset_stub
  export STUB_APPLIED_TO="${SCRATCH}/applied.txt"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  if [[ ! -f "${SCRATCH}/applied.txt" ]]; then
    missing "happy path: no kubectl apply recorded for the plan"
  else
    ok "happy path applies the plan"
  fi

  # 2. missing version — no TO_VERSION and no $2 exits non-zero.
  reset_stub
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" \
    run_upgrade "${stub_bin}")"
  split_run_result
  if (( UPGRADE_RC == 0 )); then
    missing "missing version: exit 0, want non-zero"
  else
    ok "missing version fails"
  fi

  # 3. malformed version is rejected before touching the cluster.
  reset_stub
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="soon" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  if (( UPGRADE_RC == 0 )); then
    missing "malformed version: exit 0, want non-zero"
  else
    ok "malformed version fails"
  fi

  # 4. preflight: CRD missing.
  reset_stub
  export STUB_CRD_RC=1
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "preflight without CRD" "preflight" "${out}"

  # 5. preflight: no base image for the target.
  reset_stub
  export STUB_IMAGES="v1.37.0"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "preflight without image" "preflight" "${out}"

  # 6. preflight: already on the target version.
  reset_stub
  export STUB_TOPOLOGY_VERSION="${TARGET_VERSION}"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "preflight already current" "preflight" "${out}"

  # 7. plan Failed — the scenario reports the plan's reason.
  reset_stub
  export STUB_PLAN_PHASES="RollingControlPlane Failed"
  export STUB_FAILURE_REASON="ImageNotRegistered"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_OK}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "failed plan" "plan" "${out}"
  if [[ "${out}" != *"ImageNotRegistered"* ]]; then
    missing "failed plan: output misses the failure reason (output: ${out})"
  else
    ok "failed plan surfaces the reason"
  fi

  # 8. plan never settles — the plan step times out and names itself.
  reset_stub
  export STUB_PLAN_PHASES="RollingControlPlane"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_SHORT}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "unsettled plan times out" "plan" "${out}"

  # 9. nodes never converge — every other step is healthy, so the timeout is
  # attributable to the nodes step.
  reset_stub
  export STUB_NODES="cp1 True v1.38.0
w1 True v1.37.0"
  out="$(KUBECONFIG=/stub/mgmt.kubeconfig TO_VERSION="${TARGET_VERSION}" \
    UPGRADE_WAIT_TIMEOUT="${UPGRADE_WAIT_SHORT}" run_upgrade "${stub_bin}")"
  split_run_result
  expect_fail_named "stale nodes time out" "nodes" "${out}"

  rm -rf "${SCRATCH}"

  if (( problems > 0 )); then
    fail "${problems} contract problem(s)" 1
  fi
  log "all upgrade.sh contract checks passed"
}

main "$@"
