#!/usr/bin/env bash
#
# clusterctl_test.sh — verify the release-components contract of the
# `make components OUT_DIR=<dir>` target (the artifacts clusterctl consumes).
#
# Contract (prose; the make target must provide all of the following):
#
#   1. Running `make components OUT_DIR=<dir>` from the repository root writes
#      a provider release layout under <dir>:
#
#          <dir>/infrastructure-hypervisor/v0.1.0/
#            infrastructure-components.yaml
#            metadata.yaml
#            cluster-template.yaml
#          <dir>/bootstrap-hypervisor/v0.1.0/
#            bootstrap-components.yaml
#            metadata.yaml
#            cluster-template.yaml
#          <dir>/control-plane-hypervisor/v0.1.0/
#            control-plane-components.yaml
#            metadata.yaml
#            cluster-template.yaml
#
#   2. The three *-components.yaml files are byte-identical: the provider
#      release ships one shared object set in all three provider folders.
#
#   3. Each components file is a well-formed multi-document YAML stream whose
#      object inventory is exactly: the five CRDs (hypervisorclusters,
#      hypervisormachines, and hypervisormachinetemplates under
#      infrastructure.cluster.x-k8s.io; hypervisorconfigs under
#      bootstrap.cluster.x-k8s.io; hypervisorcontrolplanes under
#      controlplane.cluster.x-k8s.io), the Namespace hypervisor-system, one
#      ServiceAccount, one ClusterRole named manager-role (its rules covering
#      bootstrap.cluster.x-k8s.io, cluster.x-k8s.io,
#      controlplane.cluster.x-k8s.io, infrastructure.cluster.x-k8s.io, and the
#      core "" group's secrets), one ClusterRoleBinding, one
#      MutatingWebhookConfiguration and one ValidatingWebhookConfiguration
#      with five webhooks each, and nothing else — in particular no Deployment
#      and no Service.
#
#   4. Every webhook clientConfig addresses the provider webhook server
#      directly: url: https://127.0.0.1:9443/<path> for each of the ten paths
#      from config/webhook/manifests.yaml, and no clientConfig carries a
#      service: reference.
#
#   5. The objects carry the clusterctl labels
#      cluster.x-k8s.io/provider: infrastructure-hypervisor and
#      clusterctl.cluster.x-k8s.io: "".
#
# The test runs without a live cluster: it invokes the make target against a
# scratch OUT_DIR and asserts against the emitted files directly. No cluster,
# VM, or quadlet is started and no network is touched.
#
# Exit codes:
#   0  the release-components artifacts satisfy the contract
#   1  contract violation (make components missing or failing, missing files,
#      wrong object inventory, wrong webhook clientConfig shape, ...)
#   2  prerequisite problem (missing tool, unexpected arguments)
#
# Usage:
#   test/e2e/clusterctl_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"
readonly REPO_ROOT

# The version directory under each provider folder, pinned by the release
# contract.
readonly VERSION_DIR="v0.1.0"

# Side files that must sit next to the components file in every provider
# folder. The word-splitting into per-file loops is intentional (fixed
# contract lists); the list is newline-separated because IFS drops spaces.
# shellcheck disable=SC2086
readonly SIDE_FILES="
metadata.yaml
cluster-template.yaml
"

# The five CRDs the components files must define, one per document.
# shellcheck disable=SC2086
readonly REQUIRED_CRDS="
hypervisorclusters.infrastructure.cluster.x-k8s.io
hypervisormachines.infrastructure.cluster.x-k8s.io
hypervisormachinetemplates.infrastructure.cluster.x-k8s.io
hypervisorconfigs.bootstrap.cluster.x-k8s.io
hypervisorcontrolplanes.controlplane.cluster.x-k8s.io
"

# The ten webhook paths from config/webhook/manifests.yaml that the emitted
# clientConfigs must expose at url: https://127.0.0.1:9443/<path>.
# shellcheck disable=SC2086
readonly WEBHOOK_PATHS="
/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster
/mutate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig
/mutate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane
/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine
/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachinetemplate
/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster
/validate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig
/validate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane
/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine
/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachinetemplate
"

# The direct webhook server address every clientConfig must use.
readonly WEBHOOK_SERVER="https://127.0.0.1:9443"

# The object inventory of every components file: eleven documents in a
# multi-document stream, so at least ten ^--- separators.
readonly MIN_DOCUMENTS=11

problems=0
SCRATCH=""
OUT_DIR=""
INFRA_DIR=""
BOOTSTRAP_DIR=""
CONTROLPLANE_DIR=""
INFRA_COMPONENTS=""
BOOTSTRAP_COMPONENTS=""
CONTROLPLANE_COMPONENTS=""

log() { printf 'clusterctl_test: %s\n' "$*" >&2; }

ok() { printf 'clusterctl_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'clusterctl_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'clusterctl_test: %s\n' "$message" >&2
  exit "$code"
}

# count_regex <file> <pattern> — the number of lines matching the BRE pattern.
# grep -c exits 1 when nothing matches; the count itself is all the caller
# needs.
count_regex() {
  local file="$1" pattern="$2"
  grep -c -- "${pattern}" "$file" || true
}

# check_file <path> — the path must be a non-empty regular file.
check_file() {
  local path="$1"
  if [[ ! -e "$path" ]]; then
    missing "expected file does not exist: ${path}"
    return 1
  fi
  if [[ ! -f "$path" ]]; then
    missing "expected a regular file: ${path}"
    return 1
  fi
  if [[ ! -s "$path" ]]; then
    missing "expected a non-empty file: ${path}"
    return 1
  fi
  return 0
}

# check_contains <file> <pattern> — the file must contain the literal pattern.
check_contains() {
  local file="$1"
  local pattern="$2"
  if grep -Fq -- "$pattern" "$file"; then
    return 0
  fi
  missing "file ${file} does not contain: ${pattern}"
  return 1
}

# extract_doc <file> <kind> — print the YAML document in <file> whose top-level
# kind equals <kind>. The stream is split on ^--- separators; output is empty
# when no such document exists.
extract_doc() {
  local file="$1" kind="$2"
  awk -v target="${kind}" '
    /^---/ { if (match_kind) exit; match_kind = 0; next }
    /^kind:[[:space:]]*/ {
      k = $0
      sub(/^kind:[[:space:]]*/, "", k)
      sub(/[[:space:]]+$/, "", k)
      if (k == target) { match_kind = 1; print; next }
    }
    match_kind { print }
  ' "$file"
}

# check_object_count <file> <kind> <expected> — the file must contain exactly
# <expected> documents whose top-level kind equals <kind>.
check_object_count() {
  local file="$1" kind="$2" expected="$3"
  local count=""
  count="$(count_regex "${file}" "^kind: ${kind}$")"
  if [[ "${count}" -ne "${expected}" ]]; then
    missing "${file} defines ${count} 'kind: ${kind}' objects; expected exactly ${expected}"
    return 1
  fi
  return 0
}

# check_multidoc <file> — the file is a well-formed multi-document YAML stream:
# at least ten ^--- separators (eleven objects) and no empty document between
# separators. A trailing separator is tolerated.
check_multidoc() {
  local file="$1"
  local seps=""
  seps="$(count_regex "$file" '^---')"
  if [[ "${seps}" -lt $((MIN_DOCUMENTS - 1)) ]]; then
    missing "components file ${file} is not a multi-document YAML stream (${seps} '^---' separators; expected at least $((MIN_DOCUMENTS - 1)) for ${MIN_DOCUMENTS} objects)"
    return 1
  fi
  if ! awk '
    /^---/ { if (segment_empty) bad = 1; segment_empty = 1; next }
    /^[[:space:]]*$/ { next }
    /^#/ { next }
    { segment_empty = 0 }
    END { exit (bad ? 1 : 0) }
  ' "$file"; then
    missing "components file ${file} contains an empty document between separators"
    return 1
  fi
  return 0
}

# check_clusterrole <file> — the ClusterRole document is named manager-role and
# its rules cover the four provider/CAPI API groups plus the core "" group's
# secrets. The apiGroup markers are scoped to the ClusterRole document because
# the webhook rule blocks repeat the same group names.
check_clusterrole() {
  local file="$1"
  local doc="" group="" ok=1
  doc="$(extract_doc "${file}" ClusterRole)"
  if [[ -z "${doc}" ]]; then
    missing "no ClusterRole document found in ${file}"
    return 1
  fi
  grep -Fq -- "name: manager-role" <<< "${doc}" \
    || { missing "ClusterRole is not named manager-role in ${file}"; ok=0; }
  for group in \
    '- bootstrap.cluster.x-k8s.io' \
    '- cluster.x-k8s.io' \
    '- controlplane.cluster.x-k8s.io' \
    '- infrastructure.cluster.x-k8s.io'; do
    grep -Fq -- "${group}" <<< "${doc}" \
      || { missing "ClusterRole rules do not cover the apiGroup ${group#- }: ${file}"; ok=0; }
  done
  grep -Fq -- '- ""' <<< "${doc}" \
    || { missing "ClusterRole rules do not cover the core (\"\") apiGroup: ${file}"; ok=0; }
  grep -Fq -- '- secrets' <<< "${doc}" \
    || { missing "ClusterRole rules do not grant access to secrets: ${file}"; ok=0; }
  [[ "${ok}" -eq 1 ]]
}

# check_components_structure <file> — the components file holds exactly the
# eleven-object inventory of the provider release contract.
check_components_structure() {
  local file="$1"
  local crd="" total=0

  check_multidoc "${file}" || :
  check_contains "${file}" "name: hypervisor-system" || :

  # Exactly the five required CRDs, one per document.
  # shellcheck disable=SC2086
  for crd in ${REQUIRED_CRDS}; do
    check_contains "${file}" "name: ${crd}" || :
  done
  check_object_count "${file}" CustomResourceDefinition 5 || :

  # The exact object inventory: Namespace, ServiceAccount, ClusterRole,
  # ClusterRoleBinding, and the two webhook configurations, each once.
  check_object_count "${file}" Namespace 1 || :
  check_object_count "${file}" ServiceAccount 1 || :
  check_object_count "${file}" ClusterRole 1 || :
  check_object_count "${file}" ClusterRoleBinding 1 || :
  check_object_count "${file}" MutatingWebhookConfiguration 1 || :
  check_object_count "${file}" ValidatingWebhookConfiguration 1 || :

  # No Deployment and no Service are part of the release artifacts.
  check_object_count "${file}" Deployment 0 || :
  check_object_count "${file}" Service 0 || :

  # The total top-level kind count must match the eleven-object inventory
  # exactly.
  total="$(count_regex "${file}" '^kind:')"
  if [[ "${total}" -ne "${MIN_DOCUMENTS}" ]]; then
    missing "${file} contains ${total} top-level objects; expected exactly ${MIN_DOCUMENTS}"
  fi

  check_clusterrole "${file}" || :
}

# check_webhooks <file> — the mutating and validating webhook configurations
# each carry exactly five webhooks, every clientConfig uses the direct
# url: https://127.0.0.1:9443/<path> form, and no clientConfig references a
# service.
check_webhooks() {
  local file="$1"
  local mdoc="" vdoc="" count="" path="" ok=1
  mdoc="$(extract_doc "${file}" MutatingWebhookConfiguration)"
  vdoc="$(extract_doc "${file}" ValidatingWebhookConfiguration)"
  if [[ -z "${mdoc}" || -z "${vdoc}" ]]; then
    missing "webhook configurations missing from ${file}"
    return 1
  fi

  # Five mutating and five validating webhooks, identified by their pinned
  # mhypervisor*/vhypervisor* name prefixes.
  count="$(grep -c '^  name: mhypervisor' <<< "${mdoc}" || true)"
  if [[ "${count}" -ne 5 ]]; then
    missing "${file} mutating webhooks: found ${count} 'mhypervisor*' name entries; expected exactly 5"
    ok=0
  fi
  count="$(grep -c '^  name: vhypervisor' <<< "${vdoc}" || true)"
  if [[ "${count}" -ne 5 ]]; then
    missing "${file} validating webhooks: found ${count} 'vhypervisor*' name entries; expected exactly 5"
    ok=0
  fi

  # Every clientConfig must address the webhook server directly.
  count="$(grep -cF -- "url: ${WEBHOOK_SERVER}/" <<< "${mdoc}" || true)"
  if [[ "${count}" -ne 5 ]]; then
    missing "${file} mutating clientConfigs: found ${count} 'url: ${WEBHOOK_SERVER}/' entries; expected exactly 5"
    ok=0
  fi
  count="$(grep -cF -- "url: ${WEBHOOK_SERVER}/" <<< "${vdoc}" || true)"
  if [[ "${count}" -ne 5 ]]; then
    missing "${file} validating clientConfigs: found ${count} 'url: ${WEBHOOK_SERVER}/' entries; expected exactly 5"
    ok=0
  fi

  # No clientConfig may fall back to a Service reference.
  count="$(grep -cF -- 'service:' <<< "${mdoc}" || true)"
  if [[ "${count}" -ne 0 ]]; then
    missing "${file} mutating clientConfigs carry a 'service:' key (${count} occurrence(s)); expected url-only clientConfigs"
    ok=0
  fi
  count="$(grep -cF -- 'service:' <<< "${vdoc}" || true)"
  if [[ "${count}" -ne 0 ]]; then
    missing "${file} validating clientConfigs carry a 'service:' key (${count} occurrence(s)); expected url-only clientConfigs"
    ok=0
  fi

  # The ten expected paths from config/webhook/manifests.yaml.
  # shellcheck disable=SC2086
  for path in ${WEBHOOK_PATHS}; do
    if ! grep -Fq -- "url: ${WEBHOOK_SERVER}${path}" "${file}"; then
      missing "${file} webhook clientConfig does not expose ${WEBHOOK_SERVER}${path}"
      ok=0
    fi
  done

  [[ "${ok}" -eq 1 ]]
}

# --- test groups ------------------------------------------------------------

test_make_components() {
  log "test: make components OUT_DIR=<scratch>"
  local rc=0 out=""
  rc=0
  out="$(make components OUT_DIR="${OUT_DIR}" 2>&1)" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    fail "make components failed (exit ${rc}): ${out} (the components target must emit the provider release layout under OUT_DIR)" 1
  fi
  ok "make components emitted the provider release layout"
}

test_out_layout() {
  log "test: provider release layout under OUT_DIR"
  local pair="" dir="" comp="" f=""
  # dir:components-file pairs for the three providers (each components file is
  # named <type>-components.yaml where <type> drops the -hypervisor suffix).
  for pair in \
    "${INFRA_DIR}:${INFRA_COMPONENTS}" \
    "${BOOTSTRAP_DIR}:${BOOTSTRAP_COMPONENTS}" \
    "${CONTROLPLANE_DIR}:${CONTROLPLANE_COMPONENTS}"; do
    dir="${pair%%:*}"
    comp="${pair#*:}"
    check_file "${comp}" || :
    # shellcheck disable=SC2086
    for f in ${SIDE_FILES}; do
      check_file "${dir}/${f}" || :
    done
  done
}

test_byte_identical() {
  log "test: the three components files are byte-identical"
  if cmp -s -- "${INFRA_COMPONENTS}" "${BOOTSTRAP_COMPONENTS}" \
    && cmp -s -- "${INFRA_COMPONENTS}" "${CONTROLPLANE_COMPONENTS}"; then
    ok "infrastructure/bootstrap/control-plane components files are byte-identical"
  else
    missing "the three components files are not byte-identical (cmp)"
  fi
}

test_components_structure() {
  log "test: components file object inventory"
  check_components_structure "${INFRA_COMPONENTS}" || :
  check_components_structure "${BOOTSTRAP_COMPONENTS}" || :
  check_components_structure "${CONTROLPLANE_COMPONENTS}" || :
}

test_webhook_clientconfig() {
  log "test: webhook clientConfig url shape"
  check_webhooks "${INFRA_COMPONENTS}" || :
  check_webhooks "${BOOTSTRAP_COMPONENTS}" || :
  check_webhooks "${CONTROLPLANE_COMPONENTS}" || :
}

test_provider_label() {
  log "test: clusterctl provider labels"
  local f=""
  for f in "${INFRA_COMPONENTS}" "${BOOTSTRAP_COMPONENTS}" "${CONTROLPLANE_COMPONENTS}"; do
    check_contains "${f}" "cluster.x-k8s.io/provider: infrastructure-hypervisor" || :
    check_contains "${f}" 'clusterctl.cluster.x-k8s.io: ""' || :
  done
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'clusterctl_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  command -v make >/dev/null 2>&1 \
    || fail "make is required to invoke the components target" 2
  command -v mktemp >/dev/null 2>&1 \
    || fail "mktemp (coreutils) is required for the scratch OUT_DIR" 2
  command -v grep >/dev/null 2>&1 \
    || fail "grep is required for the marker checks" 2
  command -v awk >/dev/null 2>&1 \
    || fail "awk is required for the document-splitting checks" 2
  command -v cmp >/dev/null 2>&1 \
    || fail "cmp (coreutils) is required for the byte-identity check" 2

  SCRATCH="$(mktemp -d)"
  trap 'rm -rf -- "${SCRATCH}"' EXIT
  log "scratch root ${SCRATCH}"
  OUT_DIR="${SCRATCH}/out"

  INFRA_DIR="${OUT_DIR}/infrastructure-hypervisor/${VERSION_DIR}"
  BOOTSTRAP_DIR="${OUT_DIR}/bootstrap-hypervisor/${VERSION_DIR}"
  CONTROLPLANE_DIR="${OUT_DIR}/control-plane-hypervisor/${VERSION_DIR}"
  INFRA_COMPONENTS="${INFRA_DIR}/infrastructure-components.yaml"
  BOOTSTRAP_COMPONENTS="${BOOTSTRAP_DIR}/bootstrap-components.yaml"
  CONTROLPLANE_COMPONENTS="${CONTROLPLANE_DIR}/control-plane-components.yaml"

  # The make invocation resolves relative paths against the working directory;
  # run it from the repository root so the layout is deterministic.
  cd "${REPO_ROOT}"

  test_make_components || :
  test_out_layout || :
  test_byte_identical || :
  test_components_structure || :
  test_webhook_clientconfig || :
  test_provider_label || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "release-components contract check failed: ${problems} problem(s)" 1
  fi
  log "release-components contract satisfied"
}

main "$@"
