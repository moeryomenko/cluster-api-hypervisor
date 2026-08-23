#!/usr/bin/env bash
#
# mgmt_test.sh — verify the self-bootstrapped management plane contract.
#
# Contract (prose; the management plane bootstrap in this directory must
# provide all four pieces below):
#
#   1. pki.sh — generates the management PKI and kubeconfigs into a state
#      directory. The state directory layout produced is:
#
#          <state>/pki/
#            ca.pem, ca-key.pem               management CA
#            apiserver.pem, apiserver-key.pem apiserver serving certificate
#            service-account.pem              service-account public key
#            service-account-key.pem          service-account private key
#          <state>/kubeconfigs/
#            admin.conf                       operator/admin access
#            etcd.conf                        apiserver -> etcd client
#            kube-apiserver.conf              apiserver loopback client
#            cluster-api-core.conf            CAPI core controller
#            cluster-api-hypervisor.conf      the provider (install contract)
#
#      pki.sh takes the state directory as its first (required) positional
#      argument, creates it when absent, and writes every file above. Every
#      kubeconfig must parse and must be wired to the management endpoint
#      (server URL), the CA, a client certificate/key pair, and a current
#      context.
#
#   2. units/*.quadlet — podman quadlet unit templates for etcd,
#      kube-apiserver, the CAPI core controller, and the provider. Each file
#      is a quadlet [Container] unit carrying the key runtime directives:
#      the etcd data directory, the apiserver client-CA and serving-cert
#      flags, the core controller kubeconfig, and the provider image and
#      entrypoint flags from the install contract (Image, Network, Privileged,
#      AddCapability, Exec, Environment).
#
#   3. core/ — the CAPI core controller manifests pinned to the CAPI 1.13
#      release series: the core CRDs (the cluster.x-k8s.io kinds plus the
#      addons ClusterResourceSet pair), the controller RBAC and manager
#      Deployment, and a metadata.yaml release-series marker naming the 1.13
#      series. The controller image tag must also carry the 1.13 series.
#
#   4. apply.sh / down.sh — lifecycle scripts. apply.sh installs the quadlet
#      units and applies the core manifests idempotently, and validates its
#      environment before acting (a missing management state directory is a
#      clear error). apply.sh also creates every quadlet bind-mount source a
#      fresh state lacks (at minimum <state>/etcd, /tmp/ch-capi,
#      /etc/cluster-api-hypervisor/webhook-certs, /var/lib/k8slab/build)
#      before starting the plane services and resets failed units
#      (systemctl reset-failed) so a crashed service can be retried. down.sh
#      stops the quadlets. Both are executable.
#
# The test runs without a live management plane: pki.sh is executed against a
# scratch state directory and every other assertion checks the committed
# files directly.
#
# Exit codes:
#   0  the bootstrap satisfies the contract
#   1  contract violation (missing file, missing directive, unparsable
#      kubeconfig, ...)
#   2  prerequisite problem (no kubeconfig parser available, unexpected
#      arguments)
#
# Usage:
#   test/e2e/mgmt/mgmt_test.sh

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly SCRIPT_DIR
MGMT_DIR="${SCRIPT_DIR}"
readonly MGMT_DIR

readonly PKI_SH="${MGMT_DIR}/pki.sh"
readonly APPLY_SH="${MGMT_DIR}/apply.sh"
readonly DOWN_SH="${MGMT_DIR}/down.sh"
readonly UNITS_DIR="${MGMT_DIR}/units"
readonly CORE_DIR="${MGMT_DIR}/core"

# Repository root (three levels up from test/e2e/mgmt/).
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
readonly REPO_ROOT
# The committed clusterctl configuration template that apply.sh renders into
# <state>/clusterctl/cluster-api/clusterctl.yaml with OUT_DIR substituted
# (clusterctl's own config file name; the same file the test/clusterctl
# contract pins).
CLUSTERCTL_TEMPLATE="${REPO_ROOT}/clusterctl.yaml"
readonly CLUSTERCTL_TEMPLATE

# Management endpoint advertised in every generated kubeconfig (the bare
# apiserver quadlet listens on the host loopback).
readonly MGMT_SERVER_URL="https://127.0.0.1:6443"

# Expected artifacts under the state directory after pki.sh runs.
# The word-splitting into per-file loops is intentional (fixed contract lists).
# shellcheck disable=SC2086
readonly PKI_CERTS="ca.pem apiserver.pem"
# shellcheck disable=SC2086
readonly PKI_KEYS="ca-key.pem apiserver-key.pem service-account-key.pem"
# shellcheck disable=SC2086
readonly KUBECONFIGS="admin.conf etcd.conf kube-apiserver.conf cluster-api-core.conf cluster-api-hypervisor.conf"

# shellcheck disable=SC2086
readonly QUADLET_UNITS="etcd.quadlet kube-apiserver.quadlet cluster-api-core.quadlet cluster-api-hypervisor.quadlet"

# CAPI core CRDs required in core/: the cluster.x-k8s.io kinds and the
# ClusterResourceSet pair from the addons group (the ClusterResourceSet
# binding is the API the core controllers rely on for resource delivery).
# shellcheck disable=SC2086
readonly CORE_CRDS="
clusters.cluster.x-k8s.io
machines.cluster.x-k8s.io
machinesets.cluster.x-k8s.io
machinedeployments.cluster.x-k8s.io
machinehealthchecks.cluster.x-k8s.io
machinedrainrules.cluster.x-k8s.io
machinepools.cluster.x-k8s.io
clusterclasses.cluster.x-k8s.io
clusterresourcesets.addons.cluster.x-k8s.io
clusterresourcesetbindings.addons.cluster.x-k8s.io
"

# CAPI core release-series pin: the controller image must carry the 1.13
# series tag (patch bumps within the series are accepted).
readonly CORE_CONTROLLER_IMAGE_TAG="cluster-api-controller:v1.13."

problems=0
SCRATCH=""
KUBECTL=""
PYTHON3=""

log() { printf 'mgmt_test: %s\n' "$*" >&2; }

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'mgmt_test: %s\n' "$message" >&2
  exit "$code"
}

missing() {
  printf 'mgmt_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

ok() { printf 'mgmt_test: ok: %s\n' "$*" >&2; }

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

# check_not_contains <file> <pattern> — the file must not contain the literal
# pattern (used to pin that the provider unit carries no dnsmasq/nftables
# host-tool contract after the k8netd migration).
check_not_contains() {
  local file="$1"
  local pattern="$2"
  if ! grep -Fq -- "$pattern" "$file"; then
    return 0
  fi
  missing "file ${file} must not contain: ${pattern}"
  return 1
}

# check_pem_cert <file> — a PEM certificate that parses as X.509 when openssl
# is available.
check_pem_cert() {
  local file="$1"
  if ! grep -q "BEGIN CERTIFICATE" "$file"; then
    missing "${file} is not a PEM certificate (BEGIN CERTIFICATE missing)"
    return 1
  fi
  if command -v openssl >/dev/null 2>&1; then
    if ! openssl x509 -in "$file" -noout >/dev/null 2>&1; then
      missing "${file} does not parse as an X.509 certificate (openssl)"
      return 1
    fi
  fi
  return 0
}

# check_pem_key <file> — a PEM private key that openssl can read when openssl
# is available.
check_pem_key() {
  local file="$1"
  if ! grep -Eq "BEGIN (RSA |EC |)PRIVATE KEY" "$file"; then
    missing "${file} is not a PEM private key (BEGIN * PRIVATE KEY missing)"
    return 1
  fi
  if command -v openssl >/dev/null 2>&1; then
    if ! openssl pkey -in "$file" -noout >/dev/null 2>&1; then
      missing "${file} does not parse as a private key (openssl)"
      return 1
    fi
  fi
  return 0
}

# check_pem_public_key <file> — a PEM public key (the service-account key pair
# publishes only the public part).
check_pem_public_key() {
  local file="$1"
  if ! grep -q "BEGIN PUBLIC KEY" "$file"; then
    missing "${file} is not a PEM public key (BEGIN PUBLIC KEY missing)"
    return 1
  fi
  if command -v openssl >/dev/null 2>&1; then
    if ! openssl pkey -pubin -in "$file" -noout >/dev/null 2>&1; then
      missing "${file} does not parse as a public key (openssl)"
      return 1
    fi
  fi
  return 0
}

# check_kubeconfig_parses <file> — the kubeconfig parses as YAML and as a
# kubectl config. Prefers kubectl, falls back to python3 + PyYAML; the
# prereq check in main guarantees at least one parser exists.
check_kubeconfig_parses() {
  local file="$1"
  if [[ -n "${KUBECTL}" ]]; then
    if "${KUBECTL}" config view --kubeconfig="${file}" >/dev/null 2>&1; then
      return 0
    fi
    missing "kubeconfig does not parse: ${file} (kubectl config view failed)"
    return 1
  fi
  if "${PYTHON3}" -c 'import sys, yaml; yaml.safe_load(open(sys.argv[1]))' "${file}" >/dev/null 2>&1; then
    return 0
  fi
  missing "kubeconfig does not parse: ${file} (yaml load failed)"
  return 1
}

# check_kubeconfig_wiring <file> — the kubeconfig points at the management
# endpoint, trusts the management CA, carries a client identity, and selects
# a current context.
check_kubeconfig_wiring() {
  local file="$1"
  check_contains "${file}" "server: ${MGMT_SERVER_URL}" || return 1
  if ! grep -Eq "certificate-authority(-data)?:" "${file}"; then
    missing "kubeconfig ${file} does not reference the CA (certificate-authority / certificate-authority-data)"
    return 1
  fi
  if ! grep -Eq "client-certificate(-data)?:" "${file}" || ! grep -Eq "client-key(-data)?:" "${file}"; then
    missing "kubeconfig ${file} does not carry a client certificate and key"
    return 1
  fi
  if ! grep -q "current-context:" "${file}"; then
    missing "kubeconfig ${file} does not set a current context"
    return 1
  fi
  return 0
}

# check_core_marker <pattern> — the pattern appears in some YAML manifest
# under core/.
check_core_marker() {
  local pattern="$1"
  if grep -rFq --include='*.yaml' -- "${pattern}" "${CORE_DIR}"; then
    return 0
  fi
  missing "core manifests do not contain: ${pattern}"
  return 1
}

# first_line <file> <pattern> — the 1-based line number of the first
# non-comment line containing the literal pattern, or empty when the pattern
# is absent. Comment lines (first non-blank character '#') are skipped so
# prose about a command never counts as the command itself.
first_line() {
  local file="$1"
  local pattern="$2"
  awk -v pat="${pattern}" '!/^[[:space:]]*#/ && index($0, pat) { print NR; exit }' "${file}"
}

# last_line <file> <pattern> — the 1-based line number of the last non-comment
# line containing the literal pattern, or empty when the pattern is absent.
last_line() {
  local file="$1"
  local pattern="$2"
  awk -v pat="${pattern}" '!/^[[:space:]]*#/ && index($0, pat) { line = NR } END { if (line) print line }' "${file}"
}

# command_count <file> <pattern> — the number of non-comment lines containing
# the literal pattern.
command_count() {
  local file="$1"
  local pattern="$2"
  awk -v pat="${pattern}" '!/^[[:space:]]*#/ && index($0, pat) { n++ } END { print n + 0 }' "${file}"
}

# has_command <file> <pattern> — true when a non-comment line contains the
# literal pattern.
has_command() {
  local file="$1"
  local pattern="$2"
  awk -v pat="${pattern}" '!/^[[:space:]]*#/ && index($0, pat) { found = 1 } END { exit !found }' "${file}"
}

# first_line_with <file> <pattern1> <pattern2> — the 1-based line number of
# the first non-comment line containing both literal patterns, or empty when
# no line matches. Comment lines (first non-blank character '#') are skipped.
first_line_with() {
  local file="$1"
  local pattern1="$2"
  local pattern2="$3"
  awk -v pat1="${pattern1}" -v pat2="${pattern2}" \
    '!/^[[:space:]]*#/ && index($0, pat1) && index($0, pat2) { print NR; exit }' "${file}"
}

# has_line_with <file> <pattern1> <pattern2> — true when a non-comment line
# contains both literal patterns.
has_line_with() {
  local file="$1"
  local pattern1="$2"
  local pattern2="$3"
  awk -v pat1="${pattern1}" -v pat2="${pattern2}" \
    '!/^[[:space:]]*#/ && index($0, pat1) && index($0, pat2) { found = 1 } END { exit !found }' "${file}"
}

# --- test groups ------------------------------------------------------------

test_pki_generation() {
  log "test: pki generation (pki.sh)"
  local state="" rc=0 out=""

  if ! check_file "${PKI_SH}"; then
    return 1
  fi
  if [[ ! -x "${PKI_SH}" ]]; then
    missing "${PKI_SH} is not executable"
    return 1
  fi

  # Edge: the state directory argument is required.
  rc=0
  out="$(bash "${PKI_SH}" 2>&1)" || rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    missing "pki.sh accepts no state-directory argument; expected a usage error"
  else
    ok "pki.sh rejects a missing state-directory argument"
  fi

  # Edge: pki.sh creates the state directory when given a nonexistent path.
  state="${SCRATCH}/state"
  rc=0
  out="$(bash "${PKI_SH}" "${state}" 2>&1)" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    missing "pki.sh failed: ${out}"
    return 1
  fi
  ok "pki.sh created the state directory and generated the PKI"

  # Edge: every PKI file exists with the expected kind of content.
  local f=""
  for f in ${PKI_CERTS}; do
    check_file "${state}/pki/${f}" || continue
    check_pem_cert "${state}/pki/${f}" || :
  done
  for f in ${PKI_KEYS}; do
    check_file "${state}/pki/${f}" || continue
    check_pem_key "${state}/pki/${f}" || :
  done
  check_file "${state}/pki/service-account.pem" || :
  check_pem_public_key "${state}/pki/service-account.pem" || :
  # Edge (deep, openssl only): the apiserver serving certificate is signed by
  # the management CA.
  if command -v openssl >/dev/null 2>&1; then
    if openssl verify -CAfile "${state}/pki/ca.pem" "${state}/pki/apiserver.pem" >/dev/null 2>&1; then
      ok "apiserver certificate verifies against the management CA"
    else
      missing "apiserver certificate does not verify against the management CA"
    fi
  fi

  # Edge: every kubeconfig exists, parses, and is wired.
  local kc=""
  for kc in ${KUBECONFIGS}; do
    check_file "${state}/kubeconfigs/${kc}" || continue
    check_kubeconfig_parses "${state}/kubeconfigs/${kc}" || :
    check_kubeconfig_wiring "${state}/kubeconfigs/${kc}" || :
  done
}

test_quadlet_units() {
  log "test: quadlet units (units/)"
  local unit="" path=""
  for unit in ${QUADLET_UNITS}; do
    path="${UNITS_DIR}/${unit}"
    check_file "${path}" || continue
    check_contains "${path}" "[Container]" || :
    case "${unit}" in
      etcd.quadlet)
        # Edge: the etcd unit must pin its data directory.
        check_contains "${path}" "Image=" || :
        check_contains "${path}" "--data-dir=" || :
        ;;
      kube-apiserver.quadlet)
        # Edge: the apiserver unit must wire client-CA, serving cert, and
        # the etcd endpoint.
        check_contains "${path}" "Image=" || :
        check_contains "${path}" "--client-ca-file=" || :
        check_contains "${path}" "--tls-cert-file=" || :
        check_contains "${path}" "--tls-private-key-file=" || :
        check_contains "${path}" "--etcd-servers=" || :
        ;;
      cluster-api-core.quadlet)
        # Edge: the core controller image is pinned to the 1.13 series and
        # the unit passes the management kubeconfig.
        check_contains "${path}" "--kubeconfig=" || :
        check_contains "${path}" "cluster-api-controller:v1.13." || :
        ;;
      cluster-api-hypervisor.quadlet)
        # Edge: the provider unit carries the install-contract image and the
        # key runtime directives (host network, privileges, capabilities,
        # kubeconfig, provider environment).
        check_contains "${path}" "Image=localhost/cluster-api-hypervisor:dev" || :
        check_contains "${path}" "Network=host" || :
        check_contains "${path}" "PodmanArgs=--privileged" || :
        check_contains "${path}" "AddCapability=NET_ADMIN" || :
        check_contains "${path}" "--kubeconfig=" || :
        check_contains "${path}" "Environment=HYPERVISOR_" || :
        # Edge: after the k8netd migration the unit wires the k8netd control
        # socket and carries no dnsmasq/nftables host-tool contract.
        check_contains "${path}" "Environment=HYPERVISOR_K8NETD_SOCKET=" || :
        check_not_contains "${path}" "DNSMASQ" || :
        check_not_contains "${path}" "dnsmasq" || :
        check_not_contains "${path}" "nftables" || :
        ;;
    esac
  done
}

test_core_manifests() {
  log "test: core manifests (core/)"
  if [[ ! -d "${CORE_DIR}" ]]; then
    missing "core manifests directory does not exist: ${CORE_DIR}"
    return 1
  fi

  # Edge: the release-series marker names the 1.13 series.
  if check_file "${CORE_DIR}/metadata.yaml"; then
    check_contains "${CORE_DIR}/metadata.yaml" "releaseSeries" || :
    check_contains "${CORE_DIR}/metadata.yaml" "major: 1" || :
    check_contains "${CORE_DIR}/metadata.yaml" "minor: 13" || :
  fi

  # Edge: every core CRD is present in some manifest under core/.
  local crd="" missing_crd=0
  for crd in ${CORE_CRDS}; do
    if ! grep -rFq --include='*.yaml' -- "${crd}" "${CORE_DIR}"; then
      missing "core manifests do not define the CRD: ${crd}"
      missing_crd=1
    fi
  done
  if [[ "${missing_crd}" -eq 0 ]]; then
    ok "core CRDs present for all required kinds"
  fi

  # Edge: the manifests are real CRDs, not placeholders.
  check_core_marker "kind: CustomResourceDefinition" || :
  check_core_marker "apiextensions.k8s.io/v1" || :
  # Edge: the controller RBAC and manager Deployment are present.
  check_core_marker "capi-system" || :
  check_core_marker "capi-manager-role" || :
  check_core_marker "capi-controller-manager" || :
  # Edge: the controller image carries the 1.13 series tag (drift guard).
  check_core_marker "${CORE_CONTROLLER_IMAGE_TAG}" || :
}

test_apply_scripts() {
  log "test: lifecycle scripts (apply.sh, down.sh)"
  if check_file "${APPLY_SH}"; then
    if [[ ! -x "${APPLY_SH}" ]]; then
      missing "${APPLY_SH} is not executable"
    fi
    # Edge: apply.sh validates its environment and refuses to act when the
    # management state directory is missing.
    local rc=0 out=""
    rc=0
    out="$(MGMT_STATE_DIR="${SCRATCH}/does-not-exist" bash "${APPLY_SH}" 2>&1)" || rc=$?
    if [[ "${rc}" -eq 0 ]]; then
      missing "apply.sh ran successfully with a missing state directory; expected a clear error"
    elif ! grep -qi 'state' <<<"${out}"; then
      missing "apply.sh failed without naming the state directory: ${out}"
    else
      ok "apply.sh rejects a missing state directory with a clear error"
    fi
    # Edge: apply.sh is idempotent by construction: it applies manifests
    # declaratively (kubectl apply) and reloads systemd before starting the
    # quadlets (daemon-reload), so reinstalling units does not duplicate
    # state. Full apply-twice parity requires a live plane and is covered by
    # the manual walkthrough.
    check_contains "${APPLY_SH}" "kubectl apply" || :
    check_contains "${APPLY_SH}" "systemctl daemon-reload" || :
  fi
  if check_file "${DOWN_SH}"; then
    if [[ ! -x "${DOWN_SH}" ]]; then
      missing "${DOWN_SH} is not executable"
    fi
    # Edge: down.sh stops the quadlet services through systemd.
    if grep -Eq "systemctl (stop|disable)" "${DOWN_SH}"; then
      ok "down.sh stops/disables the quadlet services via systemctl"
    else
      missing "down.sh does not stop/disable the quadlet services (no 'systemctl stop|disable')"
    fi
  fi
}

# test_apply_rewire — pin the clusterctl rewire contract that apply.sh does
# not implement yet (these assertions are red until the flow lands):
#
#   1. validation additionally requires `go` (require_cmd go) so clusterctl
#      can be driven through `go tool clusterctl`;
#   2. apply.sh renders <state>/clusterctl/cluster-api/clusterctl.yaml from
#      the committed template with OUT_DIR substituted (OUT_DIR env, default
#      <repo>/out); the template pins the rendered-file contract: three
#      provider entries (infrastructure-, bootstrap-, control-plane-hypervisor)
#      and a top-level overridesFolder key (clusterctl resolves overridesFolder
#      with the flat viper key, so it must not live under variables:);
#   3. apply.sh assembles the CAPI core override at
#      <state>/clusterctl/overrides/cluster-api/v1.13.5/core-components.yaml
#      from the committed core/ sources and copies core/metadata.yaml
#      alongside;
#   4. apply.sh invokes `go tool clusterctl init` with XDG_CONFIG_HOME
#      pointing at <state>/clusterctl and exactly the hypervisor provider
#      flag set;
#   5. apply.sh patches the management CA bundle (ca.pem) into the admission
#      webhook configurations via kubectl patch;
#   6. the quadlet install/start steps stay unchanged (the existing
#      kubectl apply / systemctl daemon-reload checks keep guarding them);
#      the new steps are idempotent by construction (clusterctl init
#      converges, kubectl patch of an identical caBundle is a no-op).
#
# Everything is asserted statically against apply.sh and the committed
# template: no live cluster, no VM, and no quadlet is started.
test_apply_rewire() {
  log "test: apply.sh clusterctl rewire contract (pending)"
  if [[ ! -f "${APPLY_SH}" ]]; then
    return 1
  fi

  # Edge: validation requires `go` alongside kubectl/systemctl/podman.
  check_contains "${APPLY_SH}" "require_cmd go" || :

  # Edge: apply.sh renders the clusterctl config with OUT_DIR substituted.
  check_contains "${APPLY_SH}" "clusterctl.yaml" || :
  check_contains "${APPLY_SH}" "OUT_DIR" || :

  # Edge: the committed template carries the three provider entries and a
  # top-level overridesFolder key (flat key for clusterctl's viper lookup).
  if check_file "${CLUSTERCTL_TEMPLATE}"; then
    check_contains "${CLUSTERCTL_TEMPLATE}" "infrastructure-hypervisor" || :
    check_contains "${CLUSTERCTL_TEMPLATE}" "bootstrap-hypervisor" || :
    check_contains "${CLUSTERCTL_TEMPLATE}" "control-plane-hypervisor" || :
    check_contains "${CLUSTERCTL_TEMPLATE}" "overridesFolder" || :
    if grep -Eq '^overridesFolder:' "${CLUSTERCTL_TEMPLATE}"; then
      ok "clusterctl template declares a top-level overridesFolder key"
    else
      missing "clusterctl template lacks a top-level overridesFolder key (flat key, not under variables:)"
    fi
  fi

  # Edge: apply.sh assembles the CAPI core override and copies the metadata
  # marker alongside.
  check_contains "${APPLY_SH}" "overrides/cluster-api" || :
  check_contains "${APPLY_SH}" "v1.13.5" || :
  check_contains "${APPLY_SH}" "core-components.yaml" || :
  check_contains "${APPLY_SH}" "metadata.yaml" || :

  # Edge: clusterctl init runs with XDG_CONFIG_HOME wired to the state dir
  # and exactly the hypervisor provider flag set.
  check_contains "${APPLY_SH}" "XDG_CONFIG_HOME" || :
  check_contains "${APPLY_SH}" "go tool clusterctl init" || :
  check_contains "${APPLY_SH}" "--core cluster-api" || :
  check_contains "${APPLY_SH}" "--infrastructure hypervisor" || :
  check_contains "${APPLY_SH}" "--bootstrap hypervisor" || :
  check_contains "${APPLY_SH}" "--control-plane hypervisor" || :
  check_contains "${APPLY_SH}" "--skip-cert-manager" || :

  # Edge: the admission webhook configurations receive the management CA
  # bundle via kubectl patch (both configuration names).
  check_contains "${APPLY_SH}" "kubectl patch" || :
  check_contains "${APPLY_SH}" "mutating-webhook-configuration" || :
  check_contains "${APPLY_SH}" "validating-webhook-configuration" || :
  check_contains "${APPLY_SH}" "caBundle" || :
}

# test_apply_order — pin the first-time bring-up ordering contract of apply.sh
# (a live apiserver is not listening until the plane quadlets start, so the
# core manifests cannot be applied first):
#
#   1. the management-plane services (mgmt-etcd, mgmt-kube-apiserver) must be
#      started before the core manifests are kubectl-applied — the first
#      `systemctl start` line must precede the first `kubectl apply` line;
#   2. apply.sh must wait for the management apiserver (poll /readyz via
#      kubectl, mirroring run.sh's wait_for_apiserver_ready) with a timeout
#      budget before relying on it;
#   3. the controller services (mgmt-cluster-api-core,
#      mgmt-cluster-api-hypervisor) must be started AFTER clusterctl init (the
#      core CRDs and the provider CRDs/webhooks exist only after init), so the
#      plane start and the controller start must be separate steps — at least
#      two `systemctl start` invocations, with the last one following the
#      `go tool clusterctl init` line.
#
# Everything is asserted statically against apply.sh: no live cluster, no VM,
# and no quadlet is started.
test_apply_order() {
  log "test: apply.sh first-time bring-up ordering"
  if [[ ! -f "${APPLY_SH}" ]]; then
    return 1
  fi

  local first_start="" first_apply="" last_start="" init_line="" start_count=0
  first_start="$(first_line "${APPLY_SH}" "systemctl start")"
  first_apply="$(first_line "${APPLY_SH}" "kubectl apply")"
  last_start="$(last_line "${APPLY_SH}" "systemctl start")"
  init_line="$(first_line "${APPLY_SH}" "go tool clusterctl init")"

  # Edge: plane start precedes the core apply (first-time bring-up has no
  # apiserver yet, so applying first fails with a connection refused).
  if [[ -z "${first_start}" ]]; then
    missing "apply.sh has no 'systemctl start' of the management services"
  elif [[ -z "${first_apply}" ]]; then
    missing "apply.sh has no 'kubectl apply' of the core manifests"
  elif (( first_start > first_apply )); then
    missing "systemctl start must precede kubectl apply (systemctl start at line ${first_start}, kubectl apply at line ${first_apply})"
  else
    ok "management-plane services are started before the core manifests are applied (systemctl start at line ${first_start}, kubectl apply at line ${first_apply})"
  fi

  # Edge: the management apiserver readiness wait with a timeout budget
  # (mirrors run.sh's wait_for_apiserver_ready: --raw='/readyz' polled until a
  # deadline).
  if has_command "${APPLY_SH}" "--raw='/readyz'" || has_command "${APPLY_SH}" "/readyz"; then
    if grep -Eiq 'deadline|timeout' "${APPLY_SH}"; then
      ok "apply.sh waits for the management apiserver with a timeout budget"
    else
      missing "apply.sh polls /readyz but lacks a timeout budget (deadline/TIMEOUT marker)"
    fi
  else
    missing "missing readyz wait: apply.sh does not wait for the management apiserver (/readyz marker absent)"
  fi

  # Edge: the controller services start separately, after clusterctl init has
  # created the core CRDs and the provider CRDs/webhooks.
  start_count="$(command_count "${APPLY_SH}" "systemctl start")"
  if (( start_count < 2 )); then
    missing "core/provider start must follow clusterctl init (expected separate plane and controller start steps, found ${start_count} 'systemctl start' invocation(s))"
  elif [[ -z "${init_line}" ]]; then
    missing "core/provider start must follow clusterctl init (no 'go tool clusterctl init' marker in apply.sh)"
  elif (( last_start < init_line )); then
    missing "core/provider start must follow clusterctl init (last 'systemctl start' at line ${last_start} is before 'go tool clusterctl init' at line ${init_line})"
  else
    ok "controller services are started after clusterctl init (last systemctl start at line ${last_start}, clusterctl init at line ${init_line})"
  fi
}

# test_apply_mount_sources — pin the mount-source preparation and self-heal
# contract of apply.sh. A fresh management state (pki.sh) has only pki/ and
# kubeconfigs/, but the quadlet units bind-mount more sources than pki.sh
# produces:
#
#   etcd.quadlet                    <state>/pki (exists), <state>/etcd (missing)
#   kube-apiserver.quadlet          <state>/pki (exists)
#   cluster-api-core.quadlet        <state>/kubeconfigs (exists)
#   cluster-api-hypervisor.quadlet  /var/lib/k8slab/build (missing),
#                                   /var/lib/k8slab (parent, exists),
#                                   /tmp/ch-capi (missing),
#                                   /etc/cluster-api-hypervisor/webhook-certs (missing),
#                                   <state>/kubeconfigs/cluster-api-hypervisor.conf (exists)
#
# A missing source directory makes podman fail (statfs: no such file or
# directory) and the unit crash-loops into systemd's start-limit-hit, so
# apply.sh must:
#
#   1. create the missing bind-mount sources before starting the plane
#      services (at minimum <state>/etcd, /tmp/ch-capi,
#      /etc/cluster-api-hypervisor/webhook-certs, /var/lib/k8slab/build via
#      mkdir -p) — the etcd data dir creation must precede the first
#      `systemctl start`;
#   2. reset failed systemd units (systemctl reset-failed) so a previous
#      crash-loop (start-limit-hit) does not block a retry.
#
# Everything is asserted statically against apply.sh: no live cluster, no VM,
# and no quadlet is started.
test_apply_mount_sources() {
  log "test: apply.sh mount-source preparation and self-heal"
  if [[ ! -f "${APPLY_SH}" ]]; then
    return 1
  fi

  # Edge: the etcd data directory under the state dir must exist before the
  # plane services start, so its mkdir -p line must precede the first
  # 'systemctl start'.
  local etcd_mkdir="" first_start=""
  etcd_mkdir="$(first_line_with "${APPLY_SH}" "mkdir -p" "etcd")"
  first_start="$(first_line "${APPLY_SH}" "systemctl start")"
  if [[ -z "${etcd_mkdir}" ]]; then
    missing "etcd data dir must be created before systemctl start (no non-comment 'mkdir -p' line referencing etcd)"
  elif [[ -z "${first_start}" ]]; then
    missing "etcd data dir must be created before systemctl start (apply.sh has no 'systemctl start' of the management services)"
  elif (( etcd_mkdir > first_start )); then
    missing "etcd data dir must be created before systemctl start (mkdir -p at line ${etcd_mkdir} is after systemctl start at line ${first_start})"
  else
    ok "etcd data dir is created before the management services start (mkdir -p at line ${etcd_mkdir}, systemctl start at line ${first_start})"
  fi

  # Edge: the other quadlet bind-mount sources that pki.sh does not produce
  # are created with mkdir -p (cloud-hypervisor control sockets, the webhook
  # serving certificates, and the provider build directory).
  if has_line_with "${APPLY_SH}" "mkdir -p" "/tmp/ch-capi"; then
    ok "apply.sh creates the /tmp/ch-capi mount source"
  else
    missing "missing mkdir -p /tmp/ch-capi"
  fi
  if has_line_with "${APPLY_SH}" "mkdir -p" "/etc/cluster-api-hypervisor/webhook-certs"; then
    ok "apply.sh creates the webhook-certs mount source"
  else
    missing "missing mkdir -p webhook-certs"
  fi
  if has_line_with "${APPLY_SH}" "mkdir -p" "/var/lib/k8slab/build"; then
    ok "apply.sh creates the /var/lib/k8slab/build mount source"
  else
    missing "missing mkdir -p /var/lib/k8slab/build"
  fi

  # Edge: a previously crashed unit stays blocked by systemd's
  # start-limit-hit until the failure is reset, so apply.sh must reset failed
  # services around the start loop to allow the retry.
  if has_command "${APPLY_SH}" "systemctl reset-failed"; then
    ok "apply.sh resets failed systemd services before retrying them"
  else
    missing "missing systemctl reset-failed"
  fi
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'mgmt_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  # Kubeconfig parsing needs kubectl or python3 with PyYAML. No live cluster
  # is required; both tools only parse the generated files.
  if command -v kubectl >/dev/null 2>&1; then
    KUBECTL="$(command -v kubectl)"
  elif command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    PYTHON3="$(command -v python3)"
  else
    fail "kubeconfig parsing needs kubectl or python3 with PyYAML; install one of them" 2
  fi

  SCRATCH="$(mktemp -d)"
  trap 'rm -rf -- "${SCRATCH}"' EXIT
  log "scratch state root ${SCRATCH}"

  test_pki_generation || :
  test_quadlet_units || :
  test_core_manifests || :
  test_apply_scripts || :
  test_apply_rewire || :
  test_apply_order || :
  test_apply_mount_sources || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "contract check failed: ${problems} problem(s)" 1
  fi
  log "management plane bootstrap satisfies the contract"
}

main "$@"
