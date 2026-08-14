#!/usr/bin/env bash
#
# down.sh — stop the management plane (idempotent).
#
# Stops and disables the management-plane quadlet services in reverse
# dependency order (provider and core controllers first, then apiserver, then
# etcd) and reloads systemd so the unit state is consistent. Running the
# script when the plane is already down is a no-op.

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

readonly QUADLET_DIR="/etc/containers/systemd"

# Reverse dependency order: the provider and core controllers depend on the
# apiserver, which depends on etcd.
readonly MGMT_SERVICES=(
  "mgmt-cluster-api-hypervisor"
  "mgmt-cluster-api-core"
  "mgmt-kube-apiserver"
  "mgmt-etcd"
)

log() { printf 'down: %s\n' "$*" >&2; }

command -v systemctl >/dev/null 2>&1 \
  || { printf 'down: error: required tool not found: systemctl\n' >&2; exit 1; }

for svc in "${MGMT_SERVICES[@]}"; do
  if systemctl stop "${svc}" >/dev/null 2>&1; then
    log "stopped ${svc}"
  else
    log "${svc} not running or not installed (continuing)"
  fi
  systemctl disable "${svc}" >/dev/null 2>&1 || true
done

# Remove the installed quadlet units and reload so the service definitions do
# not linger after the plane is down.
rm -f -- "${QUADLET_DIR}"/mgmt-*.container
systemctl daemon-reload

log "management plane is down"
