#!/usr/bin/env bash
#
# apply.sh — bring the management plane up (idempotent).
#
# Reads MGMT_STATE_DIR (the state directory produced by pki.sh), validates the
# environment, applies the CAPI core manifests (test/e2e/mgmt/core/) to the
# bare apiserver, renders the clusterctl configuration and the offline core
# override into the state directory, initializes the Cluster API providers
# via `go tool clusterctl init`, patches the admission webhook CA bundles,
# installs the quadlet units (test/e2e/mgmt/units/) with the state directory
# rendered in, and starts the management-plane services via systemd.
#
# Environment:
#   MGMT_STATE_DIR   state directory with pki/ and kubeconfigs/ (required)
#   OUT_DIR          provider release layout directory (default <repo>/out);
#                    must hold the three v0.1.0 provider directories
#
# The clusterctl configuration is rendered from the committed clusterctl.yaml
# template (repo root) into <state>/clusterctl/cluster-api/clusterctl.yaml,
# the location clusterctl resolves when XDG_CONFIG_HOME points at
# <state>/clusterctl; the committed placeholder base paths are substituted
# with OUT_DIR and the state overrides directory. The core Cluster API
# override is assembled offline from the committed core manifests into
# <state>/clusterctl/overrides/cluster-api/v1.13.5/, so clusterctl init never
# fetches the core components from the network.
#
# The script is idempotent: kubectl apply is declarative, clusterctl init
# skips providers of the same name, type, and version already installed,
# kubectl patch of an identical caBundle is a no-op, installing the same
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

# Repository root, resolved from the script location
# (test/e2e/mgmt is three levels below the root).
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../../.." && pwd -P)"
readonly REPO_ROOT

# Committed clusterctl configuration template (repo root) that apply.sh
# renders into the state directory.
readonly CLUSTERCTL_TEMPLATE="${REPO_ROOT}/clusterctl.yaml"

# Provider release layout directory; OUT_DIR overrides the default.
readonly DEFAULT_OUT_DIR="${REPO_ROOT}/out"
readonly OUT_PROVIDER_DIRS=(
  "infrastructure-hypervisor"
  "bootstrap-hypervisor"
  "control-plane-hypervisor"
)
readonly OUT_PROVIDER_VERSION="v0.1.0"

# Default base paths committed in the clusterctl template; substituted with
# the real OUT_DIR and the state overrides directory when rendering.
readonly CLUSTERCTL_OUT_PREFIX="/var/lib/k8slab/out"
readonly CLUSTERCTL_OVERRIDES_PREFIX="/var/lib/k8slab/overrides"

# Core Cluster API version pinned by the offline override layout.
readonly CORE_CAPI_OVERRIDE_VERSION="v1.13.5"

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

# validate_out_dir <dir> — the provider release layout (the three v0.1.0
# provider directories) must exist under <dir> before clusterctl init.
validate_out_dir() {
  local dir="$1"
  local provider_dir=""
  [[ -d "${dir}" ]] \
    || die "OUT_DIR does not exist: ${dir} (run 'make components' first)"
  for provider_dir in "${OUT_PROVIDER_DIRS[@]}"; do
    if [[ ! -d "${dir}/${provider_dir}/${OUT_PROVIDER_VERSION}" ]]; then
      die "OUT_DIR incomplete: missing ${dir}/${provider_dir}/${OUT_PROVIDER_VERSION} (run 'make components' first)"
    fi
  done
}

main() {
  : "${MGMT_STATE_DIR:?MGMT_STATE_DIR must be set to the management state directory}"
  if [[ ! -d "${MGMT_STATE_DIR}" ]]; then
    die "management state directory does not exist: ${MGMT_STATE_DIR} (run pki.sh first)"
  fi

  local pki_dir="${MGMT_STATE_DIR}/pki"
  local kubeconfig_dir="${MGMT_STATE_DIR}/kubeconfigs"
  local admin_kubeconfig="${kubeconfig_dir}/admin.conf"
  local out_dir="${OUT_DIR:-${DEFAULT_OUT_DIR}}"

  # Validate the state directory before acting.
  require_file "${pki_dir}/ca.pem" "management CA"
  require_file "${pki_dir}/apiserver.pem" "apiserver certificate"
  require_file "${admin_kubeconfig}" "admin kubeconfig"

  # Validate the environment before acting.
  require_cmd kubectl
  require_cmd systemctl
  require_cmd podman
  require_cmd go
  validate_out_dir "${out_dir}"

  [[ -d "${UNITS_DIR}" ]] || die "quadlet units directory missing: ${UNITS_DIR}"
  [[ -d "${CORE_DIR}" ]] || die "core manifests directory missing: ${CORE_DIR}"

  # 1. Apply the CAPI core manifests to the management apiserver
  #    (declarative: re-running converges, never duplicates state).
  log "applying core manifests from ${CORE_DIR}"
  kubectl apply --kubeconfig="${admin_kubeconfig}" \
    -f "${CORE_DIR}/crds" \
    -f "${CORE_DIR}/rbac.yaml" \
    -f "${CORE_DIR}/manager.yaml"

  # 2. Render the clusterctl configuration from the committed template.
  #    clusterctl reads <config-home>/cluster-api/clusterctl.yaml, where the
  #    config home is $XDG_CONFIG_HOME; the state clusterctl/ subtree is used
  #    as that home so the configuration stays hermetic. The placeholder base
  #    paths are substituted with the real OUT_DIR and the state overrides
  #    directory (same sed-escape technique as the quadlet rendering below).
  local clusterctl_dir="${MGMT_STATE_DIR}/clusterctl"
  local xdg_config_dir="${clusterctl_dir}/cluster-api"
  local rendered_config="${xdg_config_dir}/clusterctl.yaml"
  local overrides_dir="${clusterctl_dir}/overrides"
  local out_escaped="" overrides_escaped=""
  mkdir -p "${xdg_config_dir}"
  out_escaped=$(printf '%s' "${out_dir}" | sed 's/[&/\\]/\\&/g')
  overrides_escaped=$(printf '%s' "${overrides_dir}" | sed 's/[&/\\]/\\&/g')
  sed -e "s|${CLUSTERCTL_OUT_PREFIX}|${out_escaped}|g" \
      -e "s|${CLUSTERCTL_OVERRIDES_PREFIX}|${overrides_escaped}|g" \
      "${CLUSTERCTL_TEMPLATE}" > "${rendered_config}"
  log "rendered clusterctl configuration at ${rendered_config}"

  # 3. Assemble the offline core-CAPI override from the committed core
  #    manifests: the CRDs, RBAC, and manager deployment concatenated into a
  #    single multi-document YAML, with the metadata marker copied alongside.
  #    clusterctl init reads these instead of the upstream core components,
  #    keeping the bootstrap free of network access.
  local core_override_dir="${overrides_dir}/cluster-api/${CORE_CAPI_OVERRIDE_VERSION}"
  local core_components="${core_override_dir}/core-components.yaml"
  local src="" first=1
  mkdir -p "${core_override_dir}"
  : > "${core_components}"
  for src in "${CORE_DIR}"/crds/*.yaml "${CORE_DIR}/rbac.yaml" "${CORE_DIR}/manager.yaml"; do
    if [[ "${first}" -eq 1 ]]; then
      first=0
    else
      printf '\n---\n' >> "${core_components}"
    fi
    cat "${src}" >> "${core_components}"
  done
  cp "${CORE_DIR}/metadata.yaml" "${core_override_dir}/metadata.yaml"
  log "assembled core override at ${core_override_dir}"

  # 4. Initialize the Cluster API providers with clusterctl. The rendered
  #    configuration registers the three hypervisor providers as local
  #    repositories and the overrides folder supplies the offline core
  #    components. The core version is pinned to v1.13.5 so clusterctl
  #    resolves the core components from the local override instead of the
  #    upstream GitHub release, keeping the bootstrap free of network access.
  #    Re-running skips providers of the same name, type, and version already
  #    installed.
  log "initializing Cluster API providers via clusterctl"
  XDG_CONFIG_HOME="${clusterctl_dir}" \
    go tool clusterctl init \
    --kubeconfig "${admin_kubeconfig}" \
    --core cluster-api:v1.13.5 \
    --infrastructure hypervisor \
    --bootstrap hypervisor \
    --control-plane hypervisor \
    --skip-cert-manager

  # 5. Patch the management CA into the admission webhook configurations so
  #    the provider webhook endpoints (served over TLS with the management CA
  #    as trust root) are accepted on first admission. Every webhook entry of
  #    both configurations receives the same bundle; patching an identical
  #    value is a no-op.
  local ca_b64="" webhook_config="" count="" i="" op="" ops=""
  ca_b64=$(base64 -w0 "${pki_dir}/ca.pem")
  for webhook_config in mutating-webhook-configuration validating-webhook-configuration; do
    count=$(kubectl get "${webhook_config}" --kubeconfig="${admin_kubeconfig}" \
      -o jsonpath='{.webhooks[*].name}' | wc -w) \
      || die "failed to read webhooks of ${webhook_config} (clusterctl init must have installed it)"
    ops=""
    for ((i = 0; i < count; i++)); do
      op=$(printf '{"op":"replace","path":"/webhooks/%d/clientConfig/caBundle","value":"%s"}' \
        "${i}" "${ca_b64}")
      if [[ -n "${ops}" ]]; then ops+=","; fi
      ops+="${op}"
    done
    kubectl patch "${webhook_config}" --kubeconfig="${admin_kubeconfig}" --type=json \
      -p "[${ops}]" >/dev/null
    log "patched caBundle into ${webhook_config}"
  done

  # 6. Install the quadlet units with the actual state directory rendered in.
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

  # 7. Reload systemd so the (re)installed quadlet units take effect, then
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
