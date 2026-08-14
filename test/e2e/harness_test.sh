#!/usr/bin/env bash
#
# harness_test.sh — verify the full-lab e2e run harness environment contract.
#
# Contract (prose; the harness script test/e2e/run.sh must provide all of the
# following). The harness consumes a management plane plus host paths, so its
# environment is the contract this test pins. Every variable below must be
# validated before the harness does any heavy work (no cluster, no VM, no
# quadlet is started in any of these tests).
#
#   1. MANAGEMENT_KUBECONFIG — the management-cluster kubeconfig. Two paths
#      are allowed:
#
#        a. the variable is set and names an existing, readable, non-empty
#           file; the harness uses it directly, or
#        b. the variable is unset or empty; the harness falls back to the
#           committed management-plane bootstrap (test/e2e/mgmt). The
#           bootstrap is driven by its state directory (MGMT_STATE_DIR,
#           default /var/lib/k8slab/mgmt, the mgmt module's default state
#           prefix); the state must be provisioned with the admin kubeconfig
#           at <state>/kubeconfigs/admin.conf. The harness may then consume
#           that kubeconfig or bring the plane up through the bootstrap
#           scripts.
#
#      When neither path yields a kubeconfig, the harness exits 1 with a
#      clear error message naming MANAGEMENT_KUBECONFIG.
#
#   2. IMAGE — the provider image reference. Unset or empty is accepted and
#      defaults to the Makefile tag cluster-api-hypervisor:dev. A set value
#      must be a syntactically plausible container reference (no whitespace);
#      otherwise the harness exits 1 with an error naming IMAGE.
#
#   3. BASE_IMAGE — the k8labs base image path (the provider environment
#      contract default is build/k8labs-base.qcow2; the relative default
#      resolves against the working directory the harness is invoked from).
#      The resolved path must be an existing, readable, regular file;
#      otherwise the harness exits 1 with an error naming BASE_IMAGE.
#
#   4. FIRMWARE — the CLOUDHV.fd path (default build/CLOUDHV.fd). The
#      resolved path must be an existing, readable, regular file; otherwise
#      the harness exits 1 with an error naming FIRMWARE.
#
#   5. STATE_DIR — the provider state directory (the provider environment
#      contract default is /var/lib/k8slab). The resolved path must be an
#      existing, writable directory and must not be a regular file;
#      otherwise the harness exits 1 with an error naming STATE_DIR.
#
#   6. OUT_DIR — the provider release layout directory (the default is
#      <repo>/out; an explicit value is honored as-is). The resolved path
#      must be an existing directory containing the three provider release
#      directories infrastructure-hypervisor/v0.1.0,
#      bootstrap-hypervisor/v0.1.0, and control-plane-hypervisor/v0.1.0;
#      otherwise the harness exits 1 with an error naming OUT_DIR.
#
#   7. The harness requires the go tool (require_cmd go) before any heavy
#      work; a missing go is an environment validation failure naming go.
#
#   8. apply_templates — the workload Cluster is generated with
#      `go tool clusterctl generate cluster k8labs --namespace default
#      --infrastructure hypervisor --kubernetes-version v1.32.13
#      --control-plane-machine-count 1 --worker-machine-count 3` piped into
#      `kubectl apply --kubeconfig=<admin> -f -`. The test drives the flow
#      through a stub go and a stub kubectl on PATH and asserts the exact
#      generate invocation and that kubectl apply reads the manifest from
#      stdin; no real cluster is contacted.
#
#   9. --help and -h — exit 0 and document every variable above, its default,
#      and the management-plane bootstrap fallback (test/e2e/mgmt).
#
# Exit codes of the harness under test:
#   0  --help / -h
#   1  environment validation failure (before any heavy work)
#
# This test never starts a real cluster or a VM: it exercises the
# environment-validation entry path, plus the apply_templates generation pipe
# through stubbed go and kubectl binaries on a narrowed PATH. Each scenario
# keeps every variable valid except the one under test, so the error must
# name exactly that variable. The harness is run with a controlled
# environment (env -i) and a 30s timeout so a harness that skips validation
# and attempts heavy work is caught instead of hanging the test.
#
# Exit codes of this test:
#   0  the harness satisfies the environment contract
#   1  contract violation (including the harness script being absent)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/harness_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"
readonly REPO_ROOT
RUN_SH="${SCRIPT_DIR}/run.sh"
readonly RUN_SH

# Defaults pinned by the contract (the Makefile tag and the provider
# environment contract values from docs/install-contract.md).
readonly IMAGE_DEFAULT="cluster-api-hypervisor:dev"
readonly BASE_IMAGE_DEFAULT="build/k8labs-base.qcow2"
readonly FIRMWARE_DEFAULT="build/CLOUDHV.fd"
readonly STATE_DIR_DEFAULT="/var/lib/k8slab"
readonly MGMT_STATE_DIR_DEFAULT="/var/lib/k8slab/mgmt"
readonly MGMT_BOOTSTRAP_DIR="test/e2e/mgmt"

# OUT_DIR contract: the default provider release layout directory and the
# three provider release directories (the layout `make components OUT_DIR=`
# emits; see test/e2e/clusterctl_test.sh). The version directory is pinned
# by the release contract.
readonly OUT_DIR_DEFAULT="${REPO_ROOT}/out"
readonly OUT_VERSION_DIR="v0.1.0"
# shellcheck disable=SC2034
readonly OUT_PROVIDERS=$'infrastructure-hypervisor\nbootstrap-hypervisor\ncontrol-plane-hypervisor'

# Timeout for a single harness invocation (seconds). Environment validation
# must return in milliseconds; a harness that proceeds to heavy work trips
# this instead of hanging the test.
readonly HARNESS_TIMEOUT=30

problems=0
SCRATCH=""

log() { printf 'harness_test: %s\n' "$*" >&2; }

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'harness_test: %s\n' "$message" >&2
  exit "$code"
}

missing() {
  printf 'harness_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

ok() { printf 'harness_test: ok: %s\n' "$*" >&2; }

# run_harness [VAR=value ...] [ARG ...] — execute the harness under test in a
# controlled environment. Arguments matching VAR=value become environment
# assignments; every other argument is passed to the harness. The harness
# receives only PATH and HOME plus the explicit assignments, so ambient
# variables on the test host cannot satisfy (or confuse) validation. stdout
# and stderr are captured together; the exit status is printed to stderr by
# the caller via the return code.
run_harness() {
  local -a harness_env=()
  local -a harness_args=()
  local arg=""
  for arg in "$@"; do
    if [[ "${arg}" == *=* ]]; then
      harness_env+=("${arg}")
    else
      harness_args+=("${arg}")
    fi
  done
  local rc=0 out=""
  out="$(timeout "${HARNESS_TIMEOUT}" env -i "${harness_env[@]}" PATH="${PATH}" \
    HOME="${HOME}" bash "${RUN_SH}" "${harness_args[@]}" 2>&1)" || rc=$?
  printf '%s' "${out}"
  return "${rc}"
}

# contains_var_ref <output> <var> — the output mentions the exact variable
# name as a standalone token. BASE_IMAGE contains IMAGE as a substring, so
# the bare IMAGE check must not match inside BASE_IMAGE; the token boundary
# excludes alphanumerics and underscores. The two-letter go name gets the
# same treatment so a stray substring cannot satisfy the requirement.
contains_var_ref() {
  local out="$1"
  local var="$2"
  case "${var}" in
    IMAGE|go)
      grep -qE "(^|[^[:alnum:]_])${var}([^[:alnum:]_]|$)" <<< "${out}"
      ;;
    *)
      grep -Fq -- "${var}" <<< "${out}"
      ;;
  esac
}

# expect_early_validation_failure <output> <rc> <var> [<path>] — assert that
# the harness exited 1 (not 0, not the timeout code) before any heavy work
# and that its error names the exact variable, and when given, the path.
expect_early_validation_failure() {
  local out="$1"
  local rc="$2"
  local var="$3"
  local path="${4:-}"
  local ok=1

  if [[ "${rc}" -eq 124 ]]; then
    missing "harness did not exit early (timeout after ${HARNESS_TIMEOUT}s); expected an environment validation error naming ${var}"
    ok=0
  elif [[ "${rc}" -eq 0 ]]; then
    missing "harness accepted an invalid environment; expected a validation error naming ${var}"
    ok=0
  elif [[ "${rc}" -ne 1 ]]; then
    missing "harness exited ${rc}; expected exit 1 for an environment validation error"
    ok=0
  fi

  if ! contains_var_ref "${out}" "${var}"; then
    missing "harness error does not name ${var}: ${out}"
    ok=0
  fi
  if [[ -n "${path}" ]] && ! grep -Fq -- "${path}" <<< "${out}"; then
    missing "harness error does not name the offending path ${path}: ${out}"
    ok=0
  fi

  [[ "${ok}" -eq 1 ]]
}

# setup_valid_base — create a scratch fixture where every environment
# variable is valid except the one a test intentionally breaks. The fake
# kubeconfig, base image, and firmware are non-empty regular files; the state
# directory exists and is writable; the OUT_DIR provider release layout
# exists. Prints the fixture path.
setup_valid_base() {
  local base="${SCRATCH}/valid-base"
  local provider=""
  mkdir -p "${base}/state"
  printf 'fake management kubeconfig\n' > "${base}/kubeconfig"
  printf 'fake base image\n' > "${base}/base.qcow2"
  printf 'fake firmware\n' > "${base}/firmware.fd"
  # shellcheck disable=SC2086
  for provider in ${OUT_PROVIDERS}; do
    mkdir -p "${base}/out/${provider}/${OUT_VERSION_DIR}"
  done
  printf '%s' "${base}"
}

# out_dir_valid <dir> — <dir> is an existing directory containing the three
# provider release directories the OUT_DIR contract requires.
out_dir_valid() {
  local dir="$1"
  local provider=""
  [[ -d "${dir}" ]] || return 1
  # shellcheck disable=SC2086
  for provider in ${OUT_PROVIDERS}; do
    [[ -d "${dir}/${provider}/${OUT_VERSION_DIR}" ]] || return 1
  done
  return 0
}

# --- test groups ------------------------------------------------------------

test_missing_kubeconfig() {
  log "test: MANAGEMENT_KUBECONFIG validation"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Explicit kubeconfig path that does not exist.
  local missing_kc="${base}/missing-admin.conf"
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${missing_kc}" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "MANAGEMENT_KUBECONFIG" "${missing_kc}" || :

  # No explicit kubeconfig and the bootstrap fallback state is not
  # provisioned (an empty scratch directory with no kubeconfigs/admin.conf).
  local empty_state="${SCRATCH}/empty-mgmt-state"
  mkdir -p "${empty_state}"
  rc=0
  out="$(run_harness \
    "MGMT_STATE_DIR=${empty_state}" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "MANAGEMENT_KUBECONFIG" || :

  # Neither variable set: the default bootstrap state is not provisioned.
  # Skip when a host has already provisioned the default state (the test
  # would then legitimately proceed past validation).
  local default_admin="${MGMT_STATE_DIR_DEFAULT}/kubeconfigs/admin.conf"
  if [[ -f "${default_admin}" ]]; then
    ok "skipping unset/unset kubeconfig case: default bootstrap state exists at ${default_admin}"
  else
    rc=0
    out="$(run_harness \
      "IMAGE=${IMAGE_DEFAULT}" \
      "BASE_IMAGE=${base}/base.qcow2" \
      "FIRMWARE=${base}/firmware.fd" \
      "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
    expect_early_validation_failure "${out}" "${rc}" "MANAGEMENT_KUBECONFIG" || :
  fi
}

test_invalid_image() {
  log "test: IMAGE validation"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # A set IMAGE must be a plausible reference; whitespace is rejected.
  local bad_image="not a valid image reference"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${bad_image}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "IMAGE" || :

  # IMAGE unset is accepted (defaults to the Makefile tag), so with the
  # kubeconfig valid and the base image missing, the harness must fail on
  # BASE_IMAGE, not on IMAGE.
  local missing_base="${base}/missing-base.qcow2"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "BASE_IMAGE=${missing_base}" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "BASE_IMAGE" "${missing_base}" || :
}

test_missing_base_image() {
  log "test: BASE_IMAGE validation"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Explicit path that does not exist.
  local missing_base="${base}/missing-base.qcow2"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${missing_base}" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "BASE_IMAGE" "${missing_base}" || :

  # Unset: the default build/k8labs-base.qcow2 must still resolve to an
  # existing file. Skip when a host has baked the default image already.
  if [[ -f "${REPO_ROOT}/${BASE_IMAGE_DEFAULT}" ]]; then
    ok "skipping unset BASE_IMAGE case: default image exists at ${REPO_ROOT}/${BASE_IMAGE_DEFAULT}"
  else
    rc=0
    out="$(run_harness \
      "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
      "IMAGE=${IMAGE_DEFAULT}" \
      "FIRMWARE=${base}/firmware.fd" \
      "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
    expect_early_validation_failure "${out}" "${rc}" "BASE_IMAGE" || :
  fi
}

test_missing_firmware() {
  log "test: FIRMWARE validation"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Explicit path that does not exist.
  local missing_fw="${base}/missing-firmware.fd"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${missing_fw}" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "FIRMWARE" "${missing_fw}" || :

  # Unset: the default build/CLOUDHV.fd must still resolve to an existing
  # file. Skip when a host has provisioned the default firmware already.
  if [[ -f "${REPO_ROOT}/${FIRMWARE_DEFAULT}" ]]; then
    ok "skipping unset FIRMWARE case: default firmware exists at ${REPO_ROOT}/${FIRMWARE_DEFAULT}"
  else
    rc=0
    out="$(run_harness \
      "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
      "IMAGE=${IMAGE_DEFAULT}" \
      "BASE_IMAGE=${base}/base.qcow2" \
      "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
    expect_early_validation_failure "${out}" "${rc}" "FIRMWARE" || :
  fi
}

test_invalid_state_dir() {
  log "test: STATE_DIR validation"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Explicit path that does not exist.
  local missing_state="${base}/does-not-exist"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${missing_state}" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "STATE_DIR" "${missing_state}" || :

  # The state directory must be a directory; a regular file is invalid.
  local file_state="${base}/state-file"
  printf 'not a directory\n' > "${file_state}"
  rc=0
  out="$(run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${file_state}" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "STATE_DIR" "${file_state}" || :

  # The state directory must be writable. Root bypasses permission checks,
  # so an unwritable directory cannot be simulated as root.
  if [[ "${EUID}" -eq 0 ]]; then
    ok "skipping the not-writable STATE_DIR case: running as root bypasses permission checks"
  else
    local ro_state="${SCRATCH}/readonly-state"
    mkdir -p "${ro_state}"
    chmod 0555 "${ro_state}"
    rc=0
    out="$(run_harness \
      "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
      "IMAGE=${IMAGE_DEFAULT}" \
      "BASE_IMAGE=${base}/base.qcow2" \
      "FIRMWARE=${base}/firmware.fd" \
      "STATE_DIR=${ro_state}" \
      "OUT_DIR=${base}/out")" || rc=$?
    expect_early_validation_failure "${out}" "${rc}" "STATE_DIR" "${ro_state}" || :
  fi
}

test_help() {
  log "test: --help and -h document the environment contract"
  local out="" rc=0

  # --help must exit 0 from a completely empty environment: documentation
  # never depends on validation passing.
  rc=0
  out="$(run_harness --help)" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    missing "harness --help exited ${rc}; expected 0"
  fi

  local var=""
  for var in MANAGEMENT_KUBECONFIG IMAGE BASE_IMAGE FIRMWARE STATE_DIR; do
    contains_var_ref "${out}" "${var}" \
      || missing "harness --help does not document ${var}"
  done

  grep -Fq -- "${IMAGE_DEFAULT}" <<< "${out}" \
    || missing "harness --help does not document the IMAGE default ${IMAGE_DEFAULT}"
  grep -Fq -- "${BASE_IMAGE_DEFAULT}" <<< "${out}" \
    || missing "harness --help does not document the BASE_IMAGE default ${BASE_IMAGE_DEFAULT}"
  grep -Fq -- "${FIRMWARE_DEFAULT}" <<< "${out}" \
    || missing "harness --help does not document the FIRMWARE default ${FIRMWARE_DEFAULT}"
  grep -Fq -- "${STATE_DIR_DEFAULT}" <<< "${out}" \
    || missing "harness --help does not document the STATE_DIR default ${STATE_DIR_DEFAULT}"
  grep -Fq -- "${MGMT_BOOTSTRAP_DIR}" <<< "${out}" \
    || missing "harness --help does not document the management-plane bootstrap fallback (${MGMT_BOOTSTRAP_DIR})"

  # -h behaves identically.
  rc=0
  out="$(run_harness -h)" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    missing "harness -h exited ${rc}; expected 0"
  fi
}

test_invalid_out_dir() {
  log "test: OUT_DIR validation"
  local base="" provider="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # The OUT_DIR scenarios run against a narrowed PATH that lacks kubectl:
  # today's harness (which does not validate OUT_DIR) must fail fast at the
  # first required tool instead of hanging on the fake kubeconfig; the
  # future harness fails at OUT_DIR during validation, before any tool use.
  # go is present so a future harness that checks go first still reaches the
  # OUT_DIR check.
  local outbin="${SCRATCH}/out-dir-bin"
  mkdir -p "${outbin}"
  ln -sf "$(command -v timeout)" "${outbin}/timeout"
  ln -sf "$(command -v env)" "${outbin}/env"
  ln -sf "$(command -v bash)" "${outbin}/bash"
  ln -sf "$(command -v dirname)" "${outbin}/dirname"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${outbin}/go"
  chmod +x "${outbin}/go"

  # Explicit OUT_DIR that does not exist.
  local missing_out="${base}/missing-out"
  rc=0
  out="$(PATH="${outbin}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${missing_out}")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "OUT_DIR" "${missing_out}" || :

  # OUT_DIR must be a directory; a regular file is invalid.
  local file_out="${base}/out-file"
  printf 'not a directory\n' > "${file_out}"
  rc=0
  out="$(PATH="${outbin}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${file_out}")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "OUT_DIR" "${file_out}" || :

  # Existing directory without the three provider release directories.
  local empty_out="${base}/empty-out"
  mkdir -p "${empty_out}"
  rc=0
  out="$(PATH="${outbin}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${empty_out}")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "OUT_DIR" || :

  # The provider release paths must be directories; regular files do not
  # satisfy the layout.
  local file_provider_out="${base}/file-provider-out"
  # shellcheck disable=SC2086
  for provider in ${OUT_PROVIDERS}; do
    mkdir -p "${file_provider_out}/${provider}"
    printf 'not a directory\n' > "${file_provider_out}/${provider}/${OUT_VERSION_DIR}"
  done
  rc=0
  out="$(PATH="${outbin}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${file_provider_out}")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "OUT_DIR" || :

  # Unset OUT_DIR: the default <repo>/out must hold the provider release
  # directories. Skip when a host has already built the default layout.
  if out_dir_valid "${OUT_DIR_DEFAULT}"; then
    ok "skipping unset OUT_DIR case: default release layout exists at ${OUT_DIR_DEFAULT}"
  else
    rc=0
    out="$(PATH="${outbin}" run_harness \
      "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
      "IMAGE=${IMAGE_DEFAULT}" \
      "BASE_IMAGE=${base}/base.qcow2" \
      "FIRMWARE=${base}/firmware.fd" \
      "STATE_DIR=${base}/state")" || rc=$?
    expect_early_validation_failure "${out}" "${rc}" "OUT_DIR" || :
  fi
}

test_requires_go() {
  log "test: the harness requires the go tool before any heavy work"
  local base="" tool="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Narrow PATH to a stub bin that provides every tool require_cmd needs
  # except go: kubectl, base64, mktemp (so a future harness that checks go
  # after those still fails naming go), plus the tools the test runner
  # itself resolves (timeout, env, bash). The real go on the host must not
  # leak into the harness environment.
  local gobin="${SCRATCH}/go-required-bin"
  mkdir -p "${gobin}"
  ln -sf "$(command -v timeout)" "${gobin}/timeout"
  ln -sf "$(command -v env)" "${gobin}/env"
  ln -sf "$(command -v bash)" "${gobin}/bash"
  ln -sf "$(command -v dirname)" "${gobin}/dirname"
  for tool in kubectl base64 mktemp; do
    printf '#!/usr/bin/env bash\nexit 0\n' > "${gobin}/${tool}"
    chmod +x "${gobin}/${tool}"
  done

  rc=0
  out="$(PATH="${gobin}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out")" || rc=$?
  expect_early_validation_failure "${out}" "${rc}" "go" || :
}

test_apply_templates_flow() {
  log "test: apply_templates generates the Cluster via go tool clusterctl and pipes it into kubectl apply"
  local base="" out="" rc=0
  base="$(setup_valid_base)" || return 1

  # Stub bin prepended to PATH: a go that records every invocation (and
  # emits a fake manifest only for the clusterctl generate cluster dispatch)
  # and a kubectl that satisfies the full-lab flow without a cluster and
  # captures stdin when an apply reads from -f -.
  local stubbin="${SCRATCH}/apply-stub-bin"
  local calls="${SCRATCH}/apply-go-calls.txt"
  local kube_calls="${SCRATCH}/apply-kubectl-calls.txt"
  local stdin_rec="${SCRATCH}/apply-kubectl-stdin.txt"
  mkdir -p "${stubbin}"
  : > "${calls}"
  : > "${kube_calls}"
  : > "${stdin_rec}"

  cat > "${stubbin}/go" <<'STUB'
#!/usr/bin/env bash
# go stub: record every invocation; only the clusterctl generate cluster
# dispatch emits the fake manifest the harness pipes into kubectl apply.
printf 'go %s\n' "$*" >> "${STUB_GO_CALLS:-/dev/null}"
if [[ "${1:-}" == "tool" && "${2:-}" == "clusterctl" \
  && "${3:-}" == "generate" && "${4:-}" == "cluster" ]]; then
  printf 'apiVersion: cluster.x-k8s.io/v1beta1\nkind: Cluster\nmetadata:\n  name: k8labs\n'
fi
exit 0
STUB
  chmod +x "${stubbin}/go"

  cat > "${stubbin}/kubectl" <<'STUB'
#!/usr/bin/env bash
# kubectl stub: satisfy the full-lab flow without a cluster. The readyz poll
# succeeds, apply/delete succeed, get machine reports one Ready machine, and
# an apply reading stdin (-f -) captures the piped manifest for the test.
printf 'kubectl %s\n' "$*" >> "${STUB_KUBECTL_CALLS:-/dev/null}"
case "$*" in
  *--raw=*readyz*) exit 0 ;;
  apply*)
    if [[ " $* " == *" -f - "* ]]; then
      cat > "${STUB_KUBECTL_STDIN:-/dev/null}"
    fi
    exit 0 ;;
  delete*) exit 0 ;;
  get*)
    if [[ "$*" == *"metadata.name"* ]]; then
      printf 'vm-1\n'
    else
      printf 'True\n'
    fi
    exit 0 ;;
  *)
    printf 'kubectl stub: unexpected invocation: %s\n' "$*" >&2
    exit 1 ;;
esac
STUB
  chmod +x "${stubbin}/kubectl"

  rc=0
  out="$(PATH="${stubbin}:${PATH}" run_harness \
    "MANAGEMENT_KUBECONFIG=${base}/kubeconfig" \
    "IMAGE=${IMAGE_DEFAULT}" \
    "BASE_IMAGE=${base}/base.qcow2" \
    "FIRMWARE=${base}/firmware.fd" \
    "STATE_DIR=${base}/state" \
    "OUT_DIR=${base}/out" \
    "SMOKE=0" \
    "STUB_GO_CALLS=${calls}" \
    "STUB_KUBECTL_CALLS=${kube_calls}" \
    "STUB_KUBECTL_STDIN=${stdin_rec}")" || rc=$?

  if [[ "${rc}" -ne 0 ]]; then
    missing "harness did not complete the full-lab flow (exit ${rc}); expected 0 with stubbed go and kubectl: ${out}"
  fi

  # The apply_templates contract: generate via `go tool clusterctl` with the
  # pinned cluster identity and flags, piped into `kubectl apply -f -`.
  local expected="go tool clusterctl generate cluster k8labs --namespace default --infrastructure hypervisor --kubernetes-version v1.32.13 --control-plane-machine-count 1 --worker-machine-count 3"
  if [[ ! -s "${calls}" ]]; then
    missing "go tool clusterctl generate cluster was never invoked (no recorded go calls in ${calls})"
  elif grep -Fq -- "${expected}" "${calls}"; then
    ok "go tool clusterctl generate cluster invoked with the pinned cluster identity and flags"
  else
    missing "go tool clusterctl generate cluster invocation does not match the pinned flags; recorded: $(tr '\n' '|' < "${calls}")"
  fi

  if [[ -s "${stdin_rec}" ]]; then
    if grep -Fq -- "apiVersion: cluster.x-k8s.io/v1beta1" "${stdin_rec}"; then
      ok "kubectl apply received the generated manifest on stdin"
    else
      missing "kubectl apply stdin does not carry the generated manifest: $(tr '\n' '|' < "${stdin_rec}")"
    fi
  else
    missing "kubectl apply never read a manifest from stdin (-f -): ${stdin_rec} is empty"
  fi
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'harness_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  # The harness under test is created by the implementation task; until then
  # the red phase is this explicit, readable failure.
  if [[ ! -e "${RUN_SH}" ]]; then
    fail "run harness script not found: ${RUN_SH} (the harness implementation must provide it)" 1
  fi
  if [[ ! -f "${RUN_SH}" ]]; then
    fail "run harness path is not a regular file: ${RUN_SH}" 1
  fi
  if [[ ! -x "${RUN_SH}" ]]; then
    fail "run harness script is not executable: ${RUN_SH}" 1
  fi

  command -v timeout >/dev/null 2>&1 \
    || fail "timeout (coreutils) is required for the early-exit guard" 2
  command -v env >/dev/null 2>&1 \
    || fail "env (coreutils) is required for environment isolation" 2

  SCRATCH="$(mktemp -d)"
  trap 'rm -rf -- "${SCRATCH}"' EXIT
  log "scratch root ${SCRATCH}"

  # The harness resolves the relative defaults (build/...) against the
  # working directory it is invoked from; run every scenario from the
  # repository root so that resolution is deterministic.
  cd "${REPO_ROOT}"

  test_missing_kubeconfig || :
  test_invalid_image || :
  test_missing_base_image || :
  test_missing_firmware || :
  test_invalid_state_dir || :
  test_invalid_out_dir || :
  test_requires_go || :
  test_apply_templates_flow || :
  test_help || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "harness environment contract check failed: ${problems} problem(s)" 1
  fi
  log "harness environment contract satisfied"
}

main "$@"
