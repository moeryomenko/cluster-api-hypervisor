#!/usr/bin/env bash
#
# smoke_test.sh — verify the per-check contract of the workload smoke checks
# (test/e2e/smoke.sh).
#
# The full-lab harness (test/e2e/run.sh) runs smoke.sh against the workload
# cluster after the workload Machines are Ready. Those checks cannot run here:
# there is no live workload cluster in this test. What this file pins instead
# is the per-check contract of smoke.sh, by executing it against a stub
# kubectl on PATH that dispatches on its arguments and returns scripted canned
# outputs, then asserting smoke.sh's pass/fail decisions and its aggregate
# exit code.
#
# The kubeconfig contract mirrors the harness exactly: run.sh hands the
# workload kubeconfig (extracted from the <cluster>-kubeconfig Secret) to
# smoke.sh as the KUBECONFIG environment variable and as the first positional
# argument. The stub kubectl records which kubeconfig every invocation used
# (a --kubeconfig flag, a --kubeconfig=value, or the KUBECONFIG environment),
# so the tests can assert that smoke.sh actually targets the kubeconfig it was
# given.
#
# Per-check contract pinned (each check's pass/fail semantics):
#   1. nodes Ready      — every node reports Ready; otherwise the check fails
#                         naming the offending nodes.
#   2. kube-system pods — every pod is Running or Completed; otherwise the
#                         check fails naming the offending pod:status pairs.
#   3. Cilium health    — NetworkUnavailable=False on every node, plus a
#                         cilium status check via pod exec.
#   4. Gateway          — the GatewayClass cilium exists and the Gateway is
#                         Programmed.
#   5. CoreDNS          — the coredns Deployment is Available and the kube-dns
#                         Service clusterIP is 10.96.0.10.
#   6. in-cluster DNS   — kubernetes.default.svc.cluster.local resolves to
#                         10.96.0.1, a negative lookup returns NXDOMAIN, and
#                         an external forward (example.com) resolves.
#
# Aggregate contract: smoke.sh exits 0 only when every check passes, and
# exits non-zero when any check fails, reporting each failing check.
#
# Every scenario keeps all checks healthy except the one under test, so a
# non-zero exit is attributable to exactly that check, and the output must
# name it.
#
# Exit codes of this test:
#   0  smoke.sh satisfies the smoke-check contract
#   1  contract violation (including smoke.sh being absent: the red phase)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/smoke_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
SMOKE_SH="${SCRIPT_DIR}/smoke.sh"
readonly SMOKE_SH

# A single smoke.sh run must return quickly against the stub kubectl; the
# timeout guards against a smoke.sh that hangs instead of reporting.
readonly SMOKE_TIMEOUT=60

problems=0
SCRATCH=""
SMOKE_RC=0

log() { printf 'smoke_test: %s\n' "$*" >&2; }

ok() { printf 'smoke_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'smoke_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'smoke_test: %s\n' "$message" >&2
  exit "$code"
}

# install_stub_kubectl <dir> — write an executable stub kubectl into <dir>.
# The stub dispatches on its arguments and prints canned outputs driven by the
# STUB_* environment variables; every variable defaults to an all-healthy
# cluster. Each invocation is appended to "$STUB_LOG" (when set) together with
# the kubeconfig it used. The dispatch matches resource-name substrings, not
# flag positions, because the smoke checks invoke kubectl with inconsistent
# flag ordering (the namespace flag sometimes precedes the verb).
install_stub_kubectl() {
  local dir="$1"
  cat > "${dir}/kubectl" <<'STUB'
#!/usr/bin/env bash
set -u

stub_default_nodes="cp1 Ready control-plane 12m v1.35.4
worker1 Ready <none> 10m v1.35.4"

stub_default_pods="coredns-5dc5f7b94d-abc12 1/1 Running 0 12m
cilium-8n2xk 1/1 Running 0 12m
cilium-operator-6c8f9b5d9f-def34 1/1 Running 0 12m
etcd-cp1 1/1 Running 0 12m
kube-apiserver-cp1 1/1 Running 0 12m
kube-controller-manager-cp1 1/1 Running 0 12m
kube-scheduler-cp1 1/1 Running 0 12m"

stub_default_nslookup="Server:         10.96.0.10
Address:        10.96.0.10#53

** server can't find smoke-does-not-exist.cluster.local: NXDOMAIN"

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

args="$*"
case "${args}" in
  *'get nodes --no-headers'*)
    printf '%s\n' "${STUB_NODES-${stub_default_nodes}}"
    exit 0
    ;;
  *'get nodes'*NetworkUnavailable*)
    printf '%s\n' "${STUB_NET_AVAIL-False False}"
    exit 0
    ;;
  *'get nodes'*)
    printf '%s\n' "${STUB_NODES-${stub_default_nodes}}"
    exit 0
    ;;
  *'get pods'*'k8s-app=cilium'*)
    printf '%s\n' "${STUB_CILIUM_POD-cilium-8n2xk}"
    exit 0
    ;;
  *'get pods'*'--no-headers'*)
    printf '%s\n' "${STUB_PODS-${stub_default_pods}}"
    exit 0
    ;;
  *'get gatewayclass cilium'*)
    exit "${STUB_GATEWAYCLASS_RC-0}"
    ;;
  *'get gateway'*Programmed*)
    printf '%s\n' "${STUB_GATEWAY_PROG-True}"
    exit 0
    ;;
  *'get svc'*'kube-dns'*)
    printf '%s\n' "${STUB_KUBEDNS_IP-10.96.0.10}"
    exit 0
    ;;
  *'get pod dns-probe'*|*'get pod dns-neg'*)
    printf '%s\n' "${STUB_PROBE_PHASE-Running}"
    exit 0
    ;;
  *'get pod '*)
    printf '%s\n' "${STUB_PROBE_PHASE-Running}"
    exit 0
    ;;
  *'rollout status deployment/coredns'*)
    exit "${STUB_COREDNS_RC-0}"
    ;;
  *'cilium status --brief'*)
    exit "${STUB_CILIUM_STATUS_RC-0}"
    ;;
  *'exec dns-probe'*'getent hosts example.com'*)
    printf '%s\n' "${STUB_GETENT_EXTERNAL-93.184.215.14}"
    exit 0
    ;;
  *'exec dns-probe'*'getent hosts'*)
    printf '%s\n' "${STUB_GETENT_CLUSTER-10.96.0.1}"
    exit 0
    ;;
  *'exec dns-neg'*'nslookup'*)
    printf '%s\n' "${STUB_NSLOOKUP-${stub_default_nslookup}}"
    exit 0
    ;;
  *'create namespace'*|*'run dns-probe'*|*'run dns-neg'*|*'delete'*)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "${dir}/kubectl"
}

# new_scenario <label> — create a scratch fixture: a stub kubectl on PATH and
# a minimal workload kubeconfig. Prints the scratch directory path.
new_scenario() {
  local label="$1"
  local scratch=""
  scratch="$(mktemp -d "${SCRATCH}/${label}.XXXXXX")"
  install_stub_kubectl "${scratch}"
  printf '%s' "${scratch}"
}

# make_kubeconfig <dir> — write a minimal non-empty workload kubeconfig fixture
# into <dir>; the stub kubectl never reads it, but smoke.sh may stat it.
# Prints the kubeconfig path.
make_kubeconfig() {
  local dir="$1"
  local kc="${dir}/workload-kubeconfig"
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Config' \
    'clusters: []' \
    'users: []' \
    'contexts: []' \
    'current-context: ""' > "${kc}"
  printf '%s' "${kc}"
}

# run_smoke <scratch> <kubeconfig> [mode] [VAR=value ...] — execute smoke.sh
# against the stub kubectl installed in <scratch>, with the workload kubeconfig
# passed exactly like the full-lab harness does: as the KUBECONFIG environment
# variable AND as the first positional argument. mode "env" passes only the
# KUBECONFIG environment variable. Extra VAR=value arguments become additional
# environment assignments for smoke.sh and the stub kubectl. smoke.sh's
# combined output is written to <scratch>/smoke.out and SMOKE_RC holds its exit
# status (124 when the run exceeds SMOKE_TIMEOUT).
run_smoke() {
  local scratch="$1" kubeconfig="$2" mode="${3:-harness}"
  local log="${scratch}/kubectl.log"
  local -a env_extra=()
  local arg=""
  local rc=0
  for arg in "${@:4}"; do
    env_extra+=("${arg}")
  done
  if [[ "${mode}" == "env" ]]; then
    timeout "${SMOKE_TIMEOUT}" env "${env_extra[@]}" \
      PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" STUB_LOG="${log}" \
      "${SMOKE_SH}" >"${scratch}/smoke.out" 2>&1 || rc=$?
  else
    timeout "${SMOKE_TIMEOUT}" env "${env_extra[@]}" \
      PATH="${scratch}:${PATH}" KUBECONFIG="${kubeconfig}" STUB_LOG="${log}" \
      "${SMOKE_SH}" "${kubeconfig}" >"${scratch}/smoke.out" 2>&1 || rc=$?
  fi
  SMOKE_RC="${rc}"
}

# assert_smoke_passed <scratch> <display> — smoke.sh must exit 0 and its output
# must not report any failing check.
assert_smoke_passed() {
  local scratch="$1" display="$2"
  local out="" rc="${SMOKE_RC}"
  out="$(<"${scratch}/smoke.out")"
  if [[ "${rc}" -ne 0 ]]; then
    missing "smoke.sh exited ${rc}; expected 0 for ${display}"
    printf 'smoke_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  if grep -Fq -- "FAIL:" <<< "${out}"; then
    missing "smoke.sh reported a failing check for ${display}"
    printf 'smoke_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  ok "smoke.sh exited 0 for ${display}"
}

# assert_smoke_failed <scratch> <display> <needle...> — smoke.sh must exit
# non-zero and its output must mention every needle (each failing check names
# itself).
assert_smoke_failed() {
  local scratch="$1" display="$2"
  shift 2
  local out="" rc="${SMOKE_RC}" needle="" all_present=1
  out="$(<"${scratch}/smoke.out")"
  if [[ "${rc}" -eq 0 ]]; then
    missing "smoke.sh exited 0; expected non-zero when ${display}"
    printf 'smoke_test: output:\n%s\n' "${out}" >&2
    return 1
  fi
  for needle in "$@"; do
    if ! grep -Fq -- "${needle}" <<< "${out}"; then
      missing "smoke.sh output does not mention '${needle}' when ${display}: ${out}"
      all_present=0
    fi
  done
  if [[ "${all_present}" -eq 1 ]]; then
    ok "smoke.sh exited ${rc} and reported: ${display}"
  fi
  [[ "${all_present}" -eq 1 ]]
}

# --- scenarios --------------------------------------------------------------

test_all_healthy() {
  log "scenario: all checks healthy"
  local scratch="" kc=""
  scratch="$(new_scenario healthy)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}"
  assert_smoke_passed "${scratch}" "an all-healthy cluster" || :
  rm -rf -- "${scratch}"
}

test_node_not_ready() {
  log "scenario: one node NotReady"
  local scratch="" kc=""
  scratch="$(new_scenario node-not-ready)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness \
    "STUB_NODES=cp1 Ready control-plane 12m v1.35.4
node-2 NotReady <none> 10m v1.35.4"
  assert_smoke_failed "${scratch}" "a node is NotReady" "node-2" || :
  rm -rf -- "${scratch}"
}

test_no_nodes() {
  log "scenario: no nodes at all"
  local scratch="" kc=""
  scratch="$(new_scenario no-nodes)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_NODES="
  assert_smoke_failed "${scratch}" "no nodes are found" "no nodes" || :
  rm -rf -- "${scratch}"
}

test_kube_system_pod_not_running() {
  log "scenario: a kube-system pod not Running"
  local scratch="" kc=""
  scratch="$(new_scenario kube-system-pod-pending)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness \
    "STUB_PODS=cilium-8n2xk 1/1 Running 0 12m
coredns-5dc5f7b94d-abc12 0/1 Pending 0 12m"
  assert_smoke_failed "${scratch}" "a kube-system pod is not Running" "Pending" || :
  rm -rf -- "${scratch}"
}

test_cilium_network_unavailable() {
  log "scenario: a node reports NetworkUnavailable=True"
  local scratch="" kc=""
  scratch="$(new_scenario cilium-network-unavailable)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_NET_AVAIL=False True"
  assert_smoke_failed "${scratch}" "a node has network unavailable" "network unavailable" || :
  rm -rf -- "${scratch}"
}

test_missing_gateway_class() {
  log "scenario: GatewayClass cilium missing"
  local scratch="" kc=""
  scratch="$(new_scenario missing-gatewayclass)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_GATEWAYCLASS_RC=1"
  assert_smoke_failed "${scratch}" "the GatewayClass cilium is missing" "GatewayClass" || :
  rm -rf -- "${scratch}"
}

test_gateway_not_programmed() {
  log "scenario: Gateway not Programmed"
  local scratch="" kc=""
  scratch="$(new_scenario gateway-not-programmed)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_GATEWAY_PROG=False"
  assert_smoke_failed "${scratch}" "the Gateway is not Programmed" "Programmed" || :
  rm -rf -- "${scratch}"
}

test_coredns_down() {
  log "scenario: coredns deployment unavailable"
  local scratch="" kc=""
  scratch="$(new_scenario coredns-down)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_COREDNS_RC=1"
  assert_smoke_failed "${scratch}" "the coredns deployment is unavailable" "coredns" || :
  rm -rf -- "${scratch}"
}

test_wrong_kube_dns_cluster_ip() {
  log "scenario: kube-dns Service clusterIP differs from 10.96.0.10"
  local scratch="" kc=""
  scratch="$(new_scenario wrong-kube-dns-ip)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_KUBEDNS_IP=10.96.0.11"
  assert_smoke_failed "${scratch}" "the kube-dns clusterIP is wrong" "kube-dns" "10.96.0.10" || :
  rm -rf -- "${scratch}"
}

test_cluster_dns_wrong_ip() {
  log "scenario: in-cluster FQDN resolves to the wrong IP"
  local scratch="" kc=""
  scratch="$(new_scenario cluster-dns-wrong-ip)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_GETENT_CLUSTER=10.96.0.99"
  assert_smoke_failed "${scratch}" "kubernetes.default does not resolve to 10.96.0.1" "expected 10.96.0.1" || :
  rm -rf -- "${scratch}"
}

test_negative_lookup_missing_nxdomain() {
  log "scenario: negative lookup does not return NXDOMAIN"
  local scratch="" kc=""
  scratch="$(new_scenario no-nxdomain)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness \
    "STUB_NSLOOKUP=Server:         10.96.0.10
Address:        10.96.0.10#53

** server can't find smoke-does-not-exist.cluster.local: SERVFAIL"
  assert_smoke_failed "${scratch}" "the negative lookup does not return NXDOMAIN" "NXDOMAIN" || :
  rm -rf -- "${scratch}"
}

test_external_forward_fails() {
  log "scenario: external forward does not resolve"
  local scratch="" kc=""
  scratch="$(new_scenario external-forward-fails)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness "STUB_GETENT_EXTERNAL="
  assert_smoke_failed "${scratch}" "the external forward fails" "example.com" || :
  rm -rf -- "${scratch}"
}

test_kubeconfig_arg() {
  log "scenario: smoke.sh consumes the kubeconfig the harness passes"
  local scratch="" kc="" out="" kclog=""
  scratch="$(new_scenario kubeconfig-arg)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" harness
  out="$(<"${scratch}/smoke.out")"
  kclog=""
  if [[ -f "${scratch}/kubectl.log" ]]; then
    kclog="$(<"${scratch}/kubectl.log")"
  fi
  if [[ "${SMOKE_RC}" -ne 0 ]]; then
    missing "smoke.sh exited ${SMOKE_RC}; expected 0 when invoked with the harness kubeconfig"
    printf 'smoke_test: output:\n%s\n' "${out}" >&2
  else
    ok "smoke.sh exited 0 when invoked with the harness kubeconfig"
  fi
  if grep -Fq -- "kubeconfig=${kc}" <<< "${kclog}"; then
    ok "the stub kubectl observed smoke.sh using the provided kubeconfig ${kc}"
  else
    missing "the stub kubectl never observed the provided kubeconfig ${kc}; kubectl log: ${kclog}"
  fi
  rm -rf -- "${scratch}"
}

test_kubeconfig_env_var() {
  log "scenario: smoke.sh works from the KUBECONFIG environment variable alone"
  local scratch="" kc="" out="" kclog=""
  scratch="$(new_scenario kubeconfig-env)"
  kc="$(make_kubeconfig "${scratch}")"
  run_smoke "${scratch}" "${kc}" env
  out="$(<"${scratch}/smoke.out")"
  kclog=""
  if [[ -f "${scratch}/kubectl.log" ]]; then
    kclog="$(<"${scratch}/kubectl.log")"
  fi
  if [[ "${SMOKE_RC}" -ne 0 ]]; then
    missing "smoke.sh exited ${SMOKE_RC}; expected 0 with only the KUBECONFIG environment variable set"
    printf 'smoke_test: output:\n%s\n' "${out}" >&2
  else
    ok "smoke.sh exited 0 with only the KUBECONFIG environment variable set"
  fi
  if grep -Fq -- "kubeconfig=${kc}" <<< "${kclog}"; then
    ok "the stub kubectl observed smoke.sh using KUBECONFIG=${kc}"
  else
    missing "the stub kubectl never observed KUBECONFIG=${kc}; kubectl log: ${kclog}"
  fi
  rm -rf -- "${scratch}"
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'smoke_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  # The smoke script under test is created by the implementation; until then
  # the red phase is this explicit, readable failure.
  if [[ ! -e "${SMOKE_SH}" ]]; then
    fail "workload smoke script not found: ${SMOKE_SH} (the implementation must provide it)" 1
  fi
  if [[ ! -f "${SMOKE_SH}" ]]; then
    fail "workload smoke script path is not a regular file: ${SMOKE_SH}" 1
  fi
  if [[ ! -x "${SMOKE_SH}" ]]; then
    fail "workload smoke script is not executable: ${SMOKE_SH}" 1
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

  test_all_healthy || :
  test_node_not_ready || :
  test_no_nodes || :
  test_kube_system_pod_not_running || :
  test_cilium_network_unavailable || :
  test_missing_gateway_class || :
  test_gateway_not_programmed || :
  test_coredns_down || :
  test_wrong_kube_dns_cluster_ip || :
  test_cluster_dns_wrong_ip || :
  test_negative_lookup_missing_nxdomain || :
  test_external_forward_fails || :
  test_kubeconfig_arg || :
  test_kubeconfig_env_var || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "smoke-check contract check failed: ${problems} problem(s)" 1
  fi
  log "smoke-check contract satisfied"
}

main "$@"
