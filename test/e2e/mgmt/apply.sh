#!/usr/bin/env bash
#
# apply.sh — bring the management plane up (idempotent).
#
# Reads MGMT_STATE_DIR (the state directory produced by pki.sh), validates the
# environment, applies the CAPI core manifests (test/e2e/mgmt/core/) to the
# bare apiserver, installs the quadlet units (test/e2e/mgmt/units/) with the
# state directory rendered in, and starts the management-plane services via
# systemd.
#
# Environment:
#   MGMT_STATE_DIR   state directory with pki/ and kubeconfigs/ (required)
#
# The script is idempotent: kubectl apply is declarative, installing the same
# quadlet units over themselves is a no-op after systemctl daemon-reload, and
# systemctl start on an already-running service succeeds.

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
readonly UNITS_DIR="${SCRIPT_DIR}/units"
readonly CORE_DIR="${SCRIPT_DIR}/core"
readonly QUADLET_DIR="/etc/containers/systemd"

# Default state prefix rendered into the committed quadlet templates.
readonly DEFAULT_STATE_PREFIX="/var/lib/k8slab/mgmt"

# Quadlet service names (installed unit file name minus .container).
readonly MGMT_SERVICES=(
  "mgmt-etcd"
  "mgmt-kube-apiserver"
  "mgmt-cluster-api-core"
  "mgmt-cluster-api-hypervisor"
)

log() { printf 'apply: %s\n' "$*" >&2; }

die() {
  printf 'apply: error: %s\n' "$*" >&2
  exit 1
}

# require_cmd <name> — fail with a clear message when a tool is missing.
require_cmd() {
  local name="$1"
  command -v "${name}" >/dev/null 2>&1 \
    || die "required tool not found: ${name} (install it or fix PATH)"
}

# require_file <path> <what> — fail when a state artifact is missing.
require_file() {
  local path="$1"
  local what="$2"
  [[ -f "${path}" ]] || die "state directory incomplete: missing ${what} at ${path}"
}

main() {
  : "${MGMT_STATE_DIR:?MGMT_STATE_DIR must be set to the management state directory}"
  if [[ ! -d "${MGMT_STATE_DIR}" ]]; then
    die "management state directory does not exist: ${MGMT_STATE_DIR} (run pki.sh first)"
  fi

  local pki_dir="${MGMT_STATE_DIR}/pki"
  local kubeconfig_dir="${MGMT_STATE_DIR}/kubeconfigs"
  local admin_kubeconfig="${kubeconfig_dir}/admin.conf"

  # Validate the state directory before acting.
  require_file "${pki_dir}/ca.pem" "management CA"
  require_file "${pki_dir}/apiserver.pem" "apiserver certificate"
  require_file "${admin_kubeconfig}" "admin kubeconfig"

  # Validate the environment before acting.
  require_cmd kubectl
  require_cmd systemctl
  require_cmd podman

  [[ -d "${UNITS_DIR}" ]] || die "quadlet units directory missing: ${UNITS_DIR}"
  [[ -d "${CORE_DIR}" ]] || die "core manifests directory missing: ${CORE_DIR}"

  # 1. Apply the CAPI core manifests to the management apiserver
  #    (declarative: re-running converges, never duplicates state).
  log "applying core manifests from ${CORE_DIR}"
  kubectl apply --kubeconfig="${admin_kubeconfig}" \
    -f "${CORE_DIR}/crds" \
    -f "${CORE_DIR}/rbac.yaml" \
    -f "${CORE_DIR}/manager.yaml"

  # 2. Install the quadlet units with the actual state directory rendered in.
  #    Podman quadlet generates one systemd service per .container file.
  log "installing quadlet units into ${QUADLET_DIR}"
  install -d -m 0755 "${QUADLET_DIR}"
  local unit="" installed="" state_escaped=""
  state_escaped=$(printf '%s' "${MGMT_STATE_DIR}" | sed 's/[&/\\]/\\&/g')
  for unit in "${UNITS_DIR}"/*.quadlet; do
    local base
    base="$(basename "${unit}" .quadlet)"
    installed="${QUADLET_DIR}/mgmt-${base}.container"
    sed "s|${DEFAULT_STATE_PREFIX}|${state_escaped}|g" "${unit}" > "${installed}"
    chmod 0644 "${installed}"
    log "installed ${installed}"
  done

  # 3. Reload systemd so the (re)installed quadlet units take effect, then
  #    start the plane. systemctl start is a no-op for already-running
  #    services, keeping the whole script idempotent.
  log "reloading systemd unit definitions"
  systemctl daemon-reload

  local svc=""
  for svc in "${MGMT_SERVICES[@]}"; do
    if systemctl start "${svc}" >/dev/null 2>&1; then
      log "started ${svc}"
    else
      die "failed to start ${svc}; check 'systemctl status ${svc}'"
    fi
  done

  log "management plane is up (state: ${MGMT_STATE_DIR})"
}

main "$@"
