#!/usr/bin/env bash
#
# pki.sh — generate the management-plane PKI and kubeconfigs.
#
# Generates the self-signed management CA, the apiserver serving certificate,
# the service-account key pair, per-identity client certificates, and one
# kubeconfig per management-plane component into a state directory.
#
# State directory layout produced:
#
#   <state>/pki/
#     ca.pem, ca-key.pem                 management CA (self-signed)
#     apiserver.pem, apiserver-key.pem   apiserver serving certificate (SANs:
#                                        localhost, 127.0.0.1)
#     service-account.pem                service-account public key
#     service-account-key.pem            service-account private key
#     clients/
#       admin.pem, admin-key.pem              operator/admin identity
#       etcd-client.pem, etcd-client-key.pem  apiserver -> etcd client
#       etcd-server.pem, etcd-server-key.pem  etcd server (client/peer TLS)
#       kube-apiserver.pem, kube-apiserver-key.pem  apiserver loopback client
#       cluster-api-core.pem, cluster-api-core-key.pem       CAPI core controller
#       cluster-api-hypervisor.pem, cluster-api-hypervisor-key.pem  the provider
#   <state>/kubeconfigs/
#     admin.conf                       operator/admin access
#     etcd.conf                        apiserver -> etcd client
#     kube-apiserver.conf              apiserver loopback client
#     cluster-api-core.conf            CAPI core controller
#     cluster-api-hypervisor.conf      the provider (install contract)
#
# Every kubeconfig points at https://127.0.0.1:6443, embeds the management CA
# and its identity's client certificate/key, and selects a current context.
#
# Usage:
#   pki.sh <state-directory>
#
# The state directory is created when absent. Existing artifacts are reused so
# re-running the script does not rotate certificates of a live management
# plane.

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

MGMT_SERVER_URL="https://127.0.0.1:6443"
MGMT_CLUSTER_NAME="mgmt"
CERT_DAYS="3650"
CA_SUBJECT="/CN=kubernetes"
APISERVER_SAN="subjectAltName=DNS:localhost,IP:127.0.0.1"
# Resolved from the state-directory argument in main(); read by the cert and
# kubeconfig helpers (bash dynamic scoping makes it visible to callees).
PKI_DIR=""

log() { printf 'pki: %s\n' "$*" >&2; }

die() {
  printf 'pki: error: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf 'usage: %s <state-directory>\n' "${0##*/}" >&2
  exit 1
}

# gen_key <file> — a fresh RSA-2048 PKCS#8 private key.
gen_key() {
  local file="$1"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$file"
  chmod 600 "$file"
}

# gen_ca — self-signed management CA with the CA basic constraint.
gen_ca() {
  local pki_dir="$1"
  if [[ ! -f "${pki_dir}/ca.pem" || ! -f "${pki_dir}/ca-key.pem" ]]; then
    log "generating management CA"
    gen_key "${pki_dir}/ca-key.pem"
    openssl req -x509 -new -key "${pki_dir}/ca-key.pem" -subj "${CA_SUBJECT}" \
      -days "${CERT_DAYS}" \
      -addext "basicConstraints=critical,CA:TRUE" \
      -addext "keyUsage=critical,keyCertSign,cRLSign" \
      -out "${pki_dir}/ca.pem"
  fi
}

# sign_cert <key-file> <csr-file> <cert-file> <subject> [extra-ext...] —
# sign a CSR with the management CA, copying the requested extensions.
sign_cert() {
  local key_file="$1"
  local csr_file="$2"
  local cert_file="$3"
  local subject="$4"
  shift 4
  local ext=("$@")
  local req_args=(-new -key "${key_file}" -subj "${subject}")
  local ext_arg
  for ext_arg in "${ext[@]}"; do
    req_args+=(-addext "${ext_arg}")
  done
  openssl req "${req_args[@]}" -out "${csr_file}"
  openssl x509 -req -in "${csr_file}" -CA "${PKI_DIR}/ca.pem" \
    -CAkey "${PKI_DIR}/ca-key.pem" -CAcreateserial \
    -days "${CERT_DAYS}" -copy_extensions copy -out "${cert_file}"
  rm -f "${csr_file}"
}

# gen_identity <pki-dir> <name> <cn> [extra-ext...] — key + CA-signed cert for
# one management-plane identity in pki/clients/.
gen_identity() {
  local dir="$1"
  local name="$2"
  local cn="$3"
  shift 3
  local ext=("$@")
  local key_file="${dir}/clients/${name}-key.pem"
  local cert_file="${dir}/clients/${name}.pem"
  if [[ -f "${key_file}" && -f "${cert_file}" ]]; then
    return 0
  fi
  log "generating client identity ${name} (${cn})"
  gen_key "${key_file}"
  sign_cert "${key_file}" "${dir}/clients/${name}.csr" "${cert_file}" \
    "/CN=${cn}" "${ext[@]}"
}

# gen_apiserver — apiserver serving certificate signed by the management CA.
gen_apiserver() {
  local pki_dir="$1"
  if [[ -f "${pki_dir}/apiserver.pem" && -f "${pki_dir}/apiserver-key.pem" ]]; then
    return 0
  fi
  log "generating apiserver serving certificate"
  gen_key "${pki_dir}/apiserver-key.pem"
  sign_cert "${pki_dir}/apiserver-key.pem" "${pki_dir}/apiserver.csr" \
    "${pki_dir}/apiserver.pem" "/CN=kube-apiserver" \
    "${APISERVER_SAN}" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=serverAuth"
}

# gen_service_account — SA key pair; only the public part is published as
# service-account.pem (apiserver --service-account-key-file), the private part
# signs service account tokens.
gen_service_account() {
  local pki_dir="$1"
  if [[ ! -f "${pki_dir}/service-account-key.pem" ]]; then
    log "generating service-account key pair"
    gen_key "${pki_dir}/service-account-key.pem"
  fi
  if [[ ! -f "${pki_dir}/service-account.pem" ]]; then
    openssl pkey -in "${pki_dir}/service-account-key.pem" -pubout \
      -out "${pki_dir}/service-account.pem"
  fi
}

# write_kubeconfig <kubeconfig> <user> <cert> <key> — render one kubeconfig
# pointing at the management endpoint with an embedded CA and client identity.
# Uses kubectl when available; falls back to writing the equivalent kubeconfig
# YAML directly so the script works on hosts without kubectl.
write_kubeconfig() {
  local kubeconfig="$1"
  local user="$2"
  local cert="$3"
  local key="$4"
  local context="${user}@${MGMT_CLUSTER_NAME}"

  if command -v kubectl >/dev/null 2>&1; then
    kubectl config set-cluster "${MGMT_CLUSTER_NAME}" \
      --kubeconfig="${kubeconfig}" \
      --server="${MGMT_SERVER_URL}" \
      --certificate-authority="${PKI_DIR}/ca.pem" \
      --embed-certs=true >/dev/null
    kubectl config set-credentials "${user}" \
      --kubeconfig="${kubeconfig}" \
      --client-certificate="${cert}" \
      --client-key="${key}" \
      --embed-certs=true >/dev/null
    kubectl config set-context "${context}" \
      --kubeconfig="${kubeconfig}" \
      --cluster="${MGMT_CLUSTER_NAME}" --user="${user}" >/dev/null
    kubectl config use-context "${context}" --kubeconfig="${kubeconfig}" >/dev/null
    return 0
  fi

  local ca_b64 cert_b64 key_b64
  ca_b64=$(base64 -w0 "${PKI_DIR}/ca.pem")
  cert_b64=$(base64 -w0 "${cert}")
  key_b64=$(base64 -w0 "${key}")
  cat > "${kubeconfig}" <<EOF
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ${ca_b64}
    server: ${MGMT_SERVER_URL}
  name: ${MGMT_CLUSTER_NAME}
contexts:
- context:
    cluster: ${MGMT_CLUSTER_NAME}
    user: ${user}
  name: ${context}
current-context: ${context}
kind: Config
preferences: {}
users:
- name: ${user}
  user:
    client-certificate-data: ${cert_b64}
    client-key-data: ${key_b64}
EOF
}

main() {
  [[ "$#" -eq 1 ]] || usage
  local state_dir="$1"
  PKI_DIR="${state_dir}/pki"
  local kubeconfig_dir="${state_dir}/kubeconfigs"

  mkdir -p "${PKI_DIR}/clients" "${kubeconfig_dir}"

  gen_ca "${PKI_DIR}"
  gen_apiserver "${PKI_DIR}"
  gen_service_account "${PKI_DIR}"

  # Per-identity client certificates, all signed by the management CA.
  gen_identity "${PKI_DIR}" admin "admin" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=clientAuth"
  gen_identity "${PKI_DIR}" etcd-client "etcd-client" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=clientAuth"
  gen_identity "${PKI_DIR}" etcd-server "etcd-server" \
    "${APISERVER_SAN}" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=serverAuth,clientAuth"
  gen_identity "${PKI_DIR}" kube-apiserver "kube-apiserver" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=clientAuth"
  gen_identity "${PKI_DIR}" cluster-api-core "cluster-api-core" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=clientAuth"
  gen_identity "${PKI_DIR}" cluster-api-hypervisor "cluster-api-hypervisor" \
    "keyUsage=critical,digitalSignature,keyEncipherment" \
    "extendedKeyUsage=clientAuth"

  write_kubeconfig "${kubeconfig_dir}/admin.conf" "admin" \
    "${PKI_DIR}/clients/admin.pem" "${PKI_DIR}/clients/admin-key.pem"
  write_kubeconfig "${kubeconfig_dir}/etcd.conf" "etcd-client" \
    "${PKI_DIR}/clients/etcd-client.pem" "${PKI_DIR}/clients/etcd-client-key.pem"
  write_kubeconfig "${kubeconfig_dir}/kube-apiserver.conf" "kube-apiserver" \
    "${PKI_DIR}/clients/kube-apiserver.pem" "${PKI_DIR}/clients/kube-apiserver-key.pem"
  write_kubeconfig "${kubeconfig_dir}/cluster-api-core.conf" "cluster-api-core" \
    "${PKI_DIR}/clients/cluster-api-core.pem" "${PKI_DIR}/clients/cluster-api-core-key.pem"
  write_kubeconfig "${kubeconfig_dir}/cluster-api-hypervisor.conf" "cluster-api-hypervisor" \
    "${PKI_DIR}/clients/cluster-api-hypervisor.pem" "${PKI_DIR}/clients/cluster-api-hypervisor-key.pem"

  log "management PKI written to ${state_dir}"
}

main "$@"
