#!/usr/bin/env bash
#
# apply.sh — bring the management plane up (idempotent).
#
# Reads MGMT_STATE_DIR (the state directory produced by pki.sh), validates the
# environment, prepares every quadlet bind-mount source a fresh state lacks,
# installs the quadlet units (test/e2e/mgmt/units/) with the state directory
# rendered in, starts the management-plane services (etcd + apiserver) via
# systemd, waits for the apiserver to answer /readyz, applies the CAPI core
# manifests (test/e2e/mgmt/core/) to the bare apiserver, renders the clusterctl
# configuration and the offline core override into the state directory,
# initializes the Cluster API providers via `go tool clusterctl init`, patches
# the admission webhook CA bundles, and finally starts the controller services
# (CAPI core + hypervisor provider).
#
# Ordering: on a first-time bring-up the apiserver at https://127.0.0.1:6443 is
# not listening until the plane quadlets (mgmt-etcd, mgmt-kube-apiserver) have
# started, so the core manifests are applied only after the plane is up and
# /readyz answers ok; the controller services start last, after clusterctl init
# has created the core CRDs and the provider CRDs/webhooks.
#
# Mount sources and self-heal: a fresh management state (pki.sh) contains only
# pki/ and kubeconfigs/. apply.sh creates the non-sensitive etcd and webhook
# mount sources before services start. pki.sh generates the HostAgent mTLS
# client identity and trust root at <state>/agent-client and
# <state>/agent-ca/ca.crt; this script fails closed when they are absent and
# never creates credentials. It resets failed units (systemctl reset-failed)
# before each start so a previous crash-loop (start-limit-hit) does not block a
# retry.
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
# The script is idempotent: systemctl start on an already-running service
# succeeds, the apiserver wait passes immediately when the plane is already up,
# kubectl apply is declarative, clusterctl init skips providers of the same
# name, type, and version already installed, kubectl patch of an identical
# caBundle is a no-op, and installing the same quadlet units over themselves is
# a no-op after systemctl daemon-reload; mkdir -p of existing mount sources and
# systemctl reset-failed of healthy units are silent no-ops.

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

# Budget (seconds) allowed for the management apiserver to answer /readyz
# after the plane quadlets start.
readonly APISERVER_READY_TIMEOUT=300
readonly AGENT_READY_TIMEOUT=60

# Quadlet service names (installed unit file name minus .container). The plane
# services must come up before the core manifests are applied. The HostAgent
# must pass an authorized mTLS health check before the provider manager starts.
readonly MGMT_PLANE_SERVICES=(
  "mgmt-etcd"
  "mgmt-kube-apiserver"
)
readonly MGMT_AGENT_SERVICE="mgmt-hypervisor-agent"
readonly MGMT_CONTROLLER_SERVICES=(
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

require_dir() {
  local path="$1"
  local what="$2"
  [[ -d "${path}" ]] || die "state directory incomplete: missing ${what} at ${path}"
}

# stage_state_file <source> <destination> <what> — add generated bootstrap
# state without replacing a different pre-existing file.
stage_state_file() {
  local source="$1"
  local destination="$2"
  local what="$3"
  local temporary="${destination}.new.$$"

  [[ -f "${source}" && ! -L "${source}" ]] || die "missing ${what} source: ${source}"
  if [[ -e "${destination}" ]]; then
    [[ -f "${destination}" && ! -L "${destination}" ]] \
      || die "existing ${what} is not a regular file: ${destination}"
    cmp -s "${source}" "${destination}" \
      || die "refusing to replace existing ${what}: ${destination}"
    return 0
  fi

  install -m 0644 -- "${source}" "${temporary}"
  if ! mv --no-clobber -- "${temporary}" "${destination}"; then
    die "refusing to replace concurrently created ${what}: ${destination}"
  fi
  cmp -s "${source}" "${destination}" \
    || die "staged ${what} differs from its rendered source: ${destination}"
}

# render_e2e_provider_repository <clusterctl-dir> <out-dir> — build the
# cert-manager-free E2E components and arrange the three local provider types.
render_e2e_provider_repository() {
  local clusterctl_dir="$1"
  local out_dir="$2"
  local repository_dir="${clusterctl_dir}/providers"
  local rendered="${clusterctl_dir}/.e2e-components.$$"
  local provider_dir="" component="" source_dir="" destination_dir=""

  if ! (cd "${REPO_ROOT}" && go tool kustomize build config/e2e > "${rendered}"); then
    rm -f -- "${rendered}"
    die "failed to render E2E provider components"
  fi
  [[ -s "${rendered}" ]] || die "E2E provider component render is empty"

  for provider_dir in "${OUT_PROVIDER_DIRS[@]}"; do
    component="${provider_dir%-hypervisor}-components.yaml"
    source_dir="${out_dir}/${provider_dir}/${OUT_PROVIDER_VERSION}"
    destination_dir="${repository_dir}/${provider_dir}/${OUT_PROVIDER_VERSION}"
    mkdir -p "${destination_dir}"
    stage_state_file "${rendered}" "${destination_dir}/${component}" "E2E ${provider_dir} components"
    stage_state_file "${source_dir}/metadata.yaml" "${destination_dir}/metadata.yaml" "${provider_dir} metadata"
    stage_state_file "${source_dir}/cluster-template.yaml" "${destination_dir}/cluster-template.yaml" "${provider_dir} cluster template"
  done

  rm -f -- "${rendered}"
  printf '%s\n' "${repository_dir}"
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

# wait_for_apiserver_ready <kubeconfig> — poll the management apiserver until
# /readyz answers ok (mirrors test/e2e/run.sh's wait_for_apiserver_ready: a
# first-time bring-up has no apiserver listening until the plane quadlets have
# started, so nothing may rely on the cluster before this passes).
wait_for_apiserver_ready() {
  local kubeconfig="$1"
  local deadline=$(( $(date +%s) + APISERVER_READY_TIMEOUT ))
  log "waiting for the management apiserver to be ready"
  until kubectl get --kubeconfig="${kubeconfig}" --raw='/readyz' >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      die "management apiserver did not become ready within ${APISERVER_READY_TIMEOUT}s (step: wait for apiserver)"
    fi
    sleep 2
  done
  log "management apiserver is ready"
}

wait_for_agent_ready() {
  local certificate_dir="$1"
  local ca_file="$2"
  local deadline=$(( $(date +%s) + AGENT_READY_TIMEOUT ))
  log "waiting for the HostAgent mTLS health check"
  until systemctl is-active --quiet "${MGMT_AGENT_SERVICE}" && \
    (cd "${REPO_ROOT}" && go run ./cmd/agent-health \
      --address=127.0.0.1:9444 \
      --server-name=hypervisor-agent \
      --client-cert-dir="${certificate_dir}" \
      --ca="${ca_file}") >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      die "HostAgent did not pass the authorized mTLS health check within ${AGENT_READY_TIMEOUT}s; check 'journalctl -u ${MGMT_AGENT_SERVICE}'"
    fi
    sleep 2
  done
  log "HostAgent mTLS health check passed"
}

main() {
  : "${MGMT_STATE_DIR:?MGMT_STATE_DIR must be set to the management state directory}"
  if [[ ! -d "${MGMT_STATE_DIR}" ]]; then
    die "management state directory does not exist: ${MGMT_STATE_DIR} (run pki.sh first)"
  fi

  local pki_dir="${MGMT_STATE_DIR}/pki"
  local kubeconfig_dir="${MGMT_STATE_DIR}/kubeconfigs"
  local admin_kubeconfig="${kubeconfig_dir}/admin.conf"
  local agent_client_dir="${MGMT_STATE_DIR}/agent-client"
  local agent_server_dir="${MGMT_STATE_DIR}/agent-server"
  local agent_ca="${MGMT_STATE_DIR}/agent-ca/ca.crt"
  local webhook_cert_dir="${MGMT_STATE_DIR}/webhook-certs"
  local agent_artifact_dir="${MGMT_STATE_DIR}/agent-artifacts"
  local agent_state_dir="${MGMT_STATE_DIR}/agent-state"
  local host_uid="${SUDO_UID:-}"
  local host_home=""
  local host_group=""
  local out_dir="${OUT_DIR:-${DEFAULT_OUT_DIR}}"

  # Validate the state directory before acting.
  require_file "${pki_dir}/ca.pem" "management CA"
  require_file "${pki_dir}/apiserver.pem" "apiserver certificate"
  require_file "${admin_kubeconfig}" "admin kubeconfig"
  require_dir "${agent_client_dir}" "HostAgent client certificate directory"
  require_file "${agent_client_dir}/tls.crt" "HostAgent client certificate"
  require_file "${agent_client_dir}/tls.key" "HostAgent client key"
  require_dir "${agent_server_dir}" "HostAgent server certificate directory"
  require_file "${agent_server_dir}/tls.crt" "HostAgent server certificate"
  require_file "${agent_server_dir}/tls.key" "HostAgent server key"
  require_file "${agent_ca}" "HostAgent CA certificate"
  require_dir "${webhook_cert_dir}" "manager webhook certificate directory"
  require_file "${webhook_cert_dir}/tls.crt" "manager webhook certificate"
  require_file "${webhook_cert_dir}/tls.key" "manager webhook key"
  require_dir "${agent_artifact_dir}" "HostAgent artifact directory"
  require_dir "${agent_artifact_dir}/images" "HostAgent image directory"
  require_dir "${agent_artifact_dir}/firmware" "HostAgent firmware directory"
  require_file "${agent_artifact_dir}/manifest.json" "HostAgent artifact manifest"

  # The Agent reaches the lab user's user D-Bus, k8netd and user systemd. A
  # privileged invocation must retain that identity through sudo.
  [[ "${host_uid}" =~ ^[1-9][0-9]*$ ]] \
    || die "apply.sh must run via sudo from the non-root lab user (SUDO_UID is required)"

  # Validate the environment before acting.
  require_cmd getent
  require_cmd id
  require_cmd kubectl
  require_cmd systemctl
  require_cmd podman
  require_cmd go
  require_cmd cmp
  require_cmd install
  require_cmd mv
  validate_out_dir "${out_dir}"

  host_home=$(getent passwd "${host_uid}" | cut -d: -f6)
  [[ -n "${host_home}" && -d "${host_home}" ]] \
    || die "cannot resolve the lab user's home directory for UID ${host_uid}"
  [[ -S "/run/user/${host_uid}/bus" ]] \
    || die "lab user D-Bus is unavailable: /run/user/${host_uid}/bus"
  [[ -S "/run/user/${host_uid}/k8snet/control.sock" ]] \
    || die "k8netd socket is unavailable: /run/user/${host_uid}/k8snet/control.sock"
  [[ -d "${host_home}/.config/systemd/user" ]] \
    || die "lab user systemd unit directory is unavailable: ${host_home}/.config/systemd/user"
  host_group=$(id -g "${host_uid}")

  [[ -d "${UNITS_DIR}" ]] || die "quadlet units directory missing: ${UNITS_DIR}"
  [[ -d "${CORE_DIR}" ]] || die "core manifests directory missing: ${CORE_DIR}"

  # 1. Prepare the non-sensitive etcd bind-mount source. HostAgent mTLS,
  #    webhook credentials, and immutable artifact sources were validated above
  #    and must be staged independently.
  mkdir -p "${MGMT_STATE_DIR}/etcd"
  if [[ ! -e "${agent_state_dir}" ]]; then
    install -d -m 0700 -o "${host_uid}" -g "${host_group}" "${agent_state_dir}"
  fi
  if [[ ! -d "${agent_state_dir}" ]]; then
    die "HostAgent state path is not a directory: ${agent_state_dir}"
  fi
  if [[ "$(stat -c %u "${agent_state_dir}")" != "${host_uid}" ]]; then
    die "HostAgent state directory is not owned by lab user ${host_uid}: ${agent_state_dir}"
  fi
  if [[ ! -e "${agent_state_dir}/vms" ]]; then
    install -d -m 0700 -o "${host_uid}" -g "${host_group}" "${agent_state_dir}/vms"
  fi
  if [[ ! -d "${agent_state_dir}/vms" ]]; then
    die "HostAgent VM artifact path is not a directory: ${agent_state_dir}/vms"
  fi
  if [[ "$(stat -c %u "${agent_state_dir}/vms")" != "${host_uid}" ]]; then
    die "HostAgent VM artifact directory is not owned by lab user ${host_uid}: ${agent_state_dir}/vms"
  fi

  # 2. Install the quadlet units with the actual state directory and lab user
  #    rendered in.
  #    Podman quadlet generates one systemd service per .container file.
  log "installing quadlet units into ${QUADLET_DIR}"
  install -d -m 0755 "${QUADLET_DIR}"
  local unit="" installed="" state_escaped="" home_escaped=""
  state_escaped=$(printf '%s' "${MGMT_STATE_DIR}" | sed 's/[&|/\\]/\\&/g')
  home_escaped=$(printf '%s' "${host_home}" | sed 's/[&|/\\]/\\&/g')
  for unit in "${UNITS_DIR}"/*.quadlet; do
    local base
    base="$(basename "${unit}" .quadlet)"
    installed="${QUADLET_DIR}/mgmt-${base}.container"
    sed -e "s|${DEFAULT_STATE_PREFIX}|${state_escaped}|g" \
        -e "s|__HOST_UID__|${host_uid}|g" \
        -e "s|__HOST_HOME__|${home_escaped}|g" \
        "${unit}" > "${installed}"
    chmod 0644 "${installed}"
    log "installed ${installed}"
  done

  # 3. Reload systemd so the (re)installed quadlet units take effect.
  log "reloading systemd unit definitions"
  systemctl daemon-reload

  # 4. Start the plane: etcd first, then the apiserver. systemctl start is a
  #    no-op for already-running services, keeping the script idempotent. A
  #    unit that previously crash-looped (start-limit-hit) blocks a retry
  #    until its failure state is reset, so reset-failed precedes each start.
  local svc=""
  for svc in "${MGMT_PLANE_SERVICES[@]}"; do
    systemctl reset-failed "${svc}" >/dev/null 2>&1 || true
    if systemctl start "${svc}" >/dev/null 2>&1; then
      log "started ${svc}"
    else
      die "failed to start ${svc}; check 'systemctl status ${svc}'"
    fi
  done

  # 5. Wait for the management apiserver: a first-time bring-up has no
  #    apiserver listening until the plane quadlets above are up, so the core
  #    manifests must not be applied before /readyz answers ok.
  wait_for_apiserver_ready "${admin_kubeconfig}"

  # 6. Apply the CAPI core manifests to the management apiserver
  #    (declarative: re-running converges, never duplicates state).
  log "applying core manifests from ${CORE_DIR}"
  kubectl apply --kubeconfig="${admin_kubeconfig}" \
    -f "${CORE_DIR}/crds" \
    -f "${CORE_DIR}/rbac.yaml" \
    -f "${CORE_DIR}/manager.yaml"

  # 7. Render the isolated E2E provider repository and clusterctl configuration.
  #    The repository contains only bare-plane-compatible CRDs, RBAC, and
  #    loopback admission configuration; production release components remain
  #    in OUT_DIR and are used only for metadata and templates.
  local clusterctl_dir="${MGMT_STATE_DIR}/clusterctl"
  local xdg_config_dir="${clusterctl_dir}/cluster-api"
  local rendered_config="${xdg_config_dir}/clusterctl.yaml"
  local config_temporary="${rendered_config}.new.$$"
  local overrides_dir="${clusterctl_dir}/overrides"
  local provider_repository_dir="" provider_escaped="" overrides_escaped=""
  mkdir -p "${xdg_config_dir}"
  provider_repository_dir="$(render_e2e_provider_repository "${clusterctl_dir}" "${out_dir}")"
  provider_escaped=$(printf '%s' "${provider_repository_dir}" | sed 's/[&/\\]/\\&/g')
  overrides_escaped=$(printf '%s' "${overrides_dir}" | sed 's/[&/\\]/\\&/g')
  sed -e "s|${CLUSTERCTL_OUT_PREFIX}|${provider_escaped}|g" \
      -e "s|${CLUSTERCTL_OVERRIDES_PREFIX}|${overrides_escaped}|g" \
      "${CLUSTERCTL_TEMPLATE}" > "${config_temporary}"
  stage_state_file "${config_temporary}" "${rendered_config}" "clusterctl configuration"
  rm -f -- "${config_temporary}"
  log "rendered clusterctl configuration at ${rendered_config}"

  # 8. Assemble the offline core-CAPI override from the committed core
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

  # 9. Initialize the Cluster API providers with clusterctl. The rendered
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

  # 10. Patch the management CA into the admission webhook configurations so
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
    [[ "${count}" -gt 0 ]] \
      || die "${webhook_config} has no webhook entries (E2E components are incomplete)"
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

  # 11. Start and authenticate the HostAgent before the provider manager can
  #     reconcile workload resources. A healthy TCP listener alone is not
  #     sufficient: the Health RPC must succeed with the manager identity.
  systemctl reset-failed "${MGMT_AGENT_SERVICE}" >/dev/null 2>&1 || true
  if systemctl start "${MGMT_AGENT_SERVICE}" >/dev/null 2>&1; then
    log "started ${MGMT_AGENT_SERVICE}"
  else
    die "failed to start ${MGMT_AGENT_SERVICE}; check 'systemctl status ${MGMT_AGENT_SERVICE}'"
  fi
  wait_for_agent_ready "${agent_client_dir}" "${agent_ca}"

  # 12. Start the controller services (CAPI core + hypervisor provider) now
  #     that clusterctl init has created the core CRDs and the provider
  #     CRDs/webhooks. The same reset-failed self-heal as the plane start
  #     clears a previous crash-loop before each start.
  for svc in "${MGMT_CONTROLLER_SERVICES[@]}"; do
    systemctl reset-failed "${svc}" >/dev/null 2>&1 || true
    if systemctl start "${svc}" >/dev/null 2>&1; then
      log "started ${svc}"
    else
      die "failed to start ${svc}; check 'systemctl status ${svc}'"
    fi
  done

  log "management plane is up (state: ${MGMT_STATE_DIR})"
}

main "$@"
