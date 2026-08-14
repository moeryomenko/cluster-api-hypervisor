#!/usr/bin/env bash
#
# smoke.sh — workload-cluster smoke checks for the cluster-api-hypervisor
# full-lab e2e. Mirrors the k8labs `make smoke-test` acceptance gate: nodes
# Ready, kube-system pods Running, Cilium health (NetworkUnavailable=False plus
# a WARN-only cilium status exec), the GatewayClass cilium present with the
# Gateway Programmed, the coredns Deployment Available with the kube-dns
# Service on 10.96.0.10, and in-cluster DNS regressions (FQDN -> 10.96.0.1,
# NXDOMAIN negative lookup, external forward).
#
# The harness (test/e2e/run.sh) passes the workload kubeconfig both as the
# KUBECONFIG environment variable and as the first positional argument;
# either invocation shape is accepted, a positional argument wins.
#
# Usage:
#   KUBECONFIG=<workload-kubeconfig> bash test/e2e/smoke.sh [<workload-kubeconfig>]
#
# Exit codes:
#   0  every smoke check passed
#   1  one or more smoke checks failed (each failing check names itself as
#      "FAIL: <check>")

set -Eeuo pipefail
IFS=$'\n\t'

# The kubeconfig contract: a positional argument overrides the environment so
# both harness shapes (KUBECONFIG env alone, or env plus the first argument)
# work.
if [[ -n "${1:-}" ]]; then
  KUBECONFIG="${1}"
fi
: "${KUBECONFIG:?workload kubeconfig is required: pass it as the first argument or set KUBECONFIG}"
export KUBECONFIG

command -v kubectl >/dev/null 2>&1 \
  || { printf 'ERROR: kubectl is required on PATH\n' >&2; exit 1; }

fail=0

printf '%s\n' "=== smoke-check: validating workload cluster health ==="

printf '%s\n' "--- check 1: nodes Ready ---"
NODES="$(kubectl get nodes --no-headers 2>/dev/null || true)"
if [[ -z "${NODES}" ]]; then
  printf '%s\n' "  FAIL: no nodes found"
  fail=1
else
  NOT_READY="$(printf '%s\n' "${NODES}" | awk '{if($2!="Ready"){print $1}}' || true)"
  if [[ -n "${NOT_READY}" ]]; then
    printf '%s\n' "  FAIL: nodes not Ready: ${NOT_READY}"
    fail=1
  else
    printf '%s\n' "  PASS: all nodes Ready"
  fi
fi

printf '%s\n' "--- check 2: kube-system pods Running ---"
NOT_RUNNING="$(kubectl get pods -n kube-system --no-headers 2>/dev/null \
  | awk '{if($3!="Running"&&$3!="Completed"){print $1":"$3}}' || true)"
if [[ -n "${NOT_RUNNING}" ]]; then
  printf '%s\n' "  FAIL: some kube-system pods not Running: ${NOT_RUNNING}"
  fail=1
else
  printf '%s\n' "  PASS: kube-system pods Running"
fi

printf '%s\n' "--- check 3: Cilium health (NetworkUnavailable=False) ---"
NET_AVAIL="$(kubectl get nodes -o jsonpath='{.items[*].status.conditions[?(@.type=="NetworkUnavailable")].status}' 2>/dev/null || true)"
if [[ -n "${NET_AVAIL}" ]]; then
  net_vals=()
  # The values are space-separated; split with an explicit space IFS so the
  # script's restrictive default IFS cannot collapse them into one word.
  IFS=' ' read -r -a net_vals <<< "${NET_AVAIL}" || true
  all_false=1
  for s in "${net_vals[@]}"; do
    if [[ "${s}" != "False" ]]; then
      all_false=0
      break
    fi
  done
  if [[ "${all_false}" -eq 1 ]]; then
    printf '%s\n' "  PASS: Cilium healthy on all nodes (NetworkUnavailable=False)"
  else
    printf '%s\n' "  FAIL: some nodes have network unavailable"
    fail=1
  fi
else
  printf '%s\n' "  FAIL: no NetworkUnavailable node condition found"
  fail=1
fi

printf '%s\n' "--- check 3b: Cilium status via pod exec (WARN only) ---"
CILIUM_POD="$(kubectl -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [[ -n "${CILIUM_POD}" ]]; then
  if kubectl -n kube-system exec "${CILIUM_POD}" -c cilium-agent -- cilium status --brief >/dev/null 2>&1; then
    printf '%s\n' "  PASS: Cilium status check ok"
  else
    printf '%s\n' "  WARN: Cilium exec failed (RBAC may need system:kube-apiserver-proxy binding)"
  fi
else
  printf '%s\n' "  SKIP: no Cilium pod found"
fi

printf '%s\n' "--- check 4: GatewayClass and Gateway Programmed ---"
if kubectl get gatewayclass cilium >/dev/null 2>&1; then
  printf '%s\n' "  PASS: GatewayClass cilium exists"
else
  printf '%s\n' "  FAIL: GatewayClass cilium not found"
  fail=1
fi
GW_STATUS="$(kubectl get gateway -n default cilium-gw -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
if [[ "${GW_STATUS}" == "True" ]]; then
  printf '%s\n' "  PASS: Gateway cilium-gw is Programmed"
else
  printf '%s\n' "  FAIL: Gateway cilium-gw not Programmed (status=${GW_STATUS:-unknown})"
  fail=1
fi

printf '%s\n' "--- check 5: CoreDNS deployment and kube-dns Service ---"
if kubectl -n kube-system rollout status deployment/coredns --timeout=60s >/dev/null 2>&1; then
  printf '%s\n' "  PASS: coredns deployment Available"
else
  printf '%s\n' "  FAIL: coredns deployment not Available"
  fail=1
fi
DNS_IP="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
if [[ "${DNS_IP}" == "10.96.0.10" ]]; then
  printf '%s\n' "  PASS: kube-dns Service clusterIP is 10.96.0.10"
else
  printf '%s\n' "  FAIL: kube-dns Service clusterIP is ${DNS_IP:-unknown}, expected 10.96.0.10"
  fail=1
fi

printf '%s\n' "--- check 6: in-cluster DNS regressions ---"
DNS_NS="dns-check-$(date +%s)"
kubectl create namespace "${DNS_NS}" >/dev/null 2>&1 || true
probe_ready=0
if kubectl -n "${DNS_NS}" run dns-probe --image=nginx --restart=Never -- sleep 3600 >/dev/null 2>&1 \
  && kubectl -n "${DNS_NS}" run dns-neg --image=busybox:1.36 --restart=Never -- sleep 3600 >/dev/null 2>&1; then
  for ((i = 0; i < 30; i++)); do
    p1="$(kubectl -n "${DNS_NS}" get pod dns-probe -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    p2="$(kubectl -n "${DNS_NS}" get pod dns-neg -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${p1}" == "Running" && "${p2}" == "Running" ]]; then
      probe_ready=1
      break
    fi
    sleep 2
  done
fi

if [[ "${probe_ready}" -eq 1 ]]; then
  # The getent pipelines must stay intact (kubectl | awk | head); a
  # `kubectl ... || true | awk` grouping would bypass awk entirely.
  RESOLVED="$(kubectl -n "${DNS_NS}" exec dns-probe -- getent hosts kubernetes.default.svc.cluster.local 2>/dev/null \
    | awk '{print $1}' | head -1 || true)"
  if [[ "${RESOLVED}" == "10.96.0.1" ]]; then
    printf '%s\n' "  PASS: kubernetes.default.svc.cluster.local -> ${RESOLVED}"
  else
    printf '%s\n' "  FAIL: kubernetes.default.svc.cluster.local resolved to ${RESOLVED:-<none>}, expected 10.96.0.1"
    fail=1
  fi

  NEG_NAME="does-not-exist-$(date +%s).cluster.local"
  NEG_OUT="$(kubectl -n "${DNS_NS}" exec dns-neg -- nslookup "${NEG_NAME}" 2>&1 || true)"
  if grep -q NXDOMAIN <<< "${NEG_OUT}" && grep -q '10.96.0.10' <<< "${NEG_OUT}"; then
    printf '%s\n' "  PASS: ${NEG_NAME} -> NXDOMAIN (server 10.96.0.10)"
  else
    printf '%s\n' "  FAIL: negative lookup did not return NXDOMAIN from 10.96.0.10"
    fail=1
  fi

  EXT_IP="$(kubectl -n "${DNS_NS}" exec dns-probe -- getent hosts example.com 2>/dev/null \
    | awk '{print $1}' | head -1 || true)"
  if [[ -n "${EXT_IP}" ]]; then
    printf '%s\n' "  PASS: example.com resolved via CoreDNS forward -> ${EXT_IP}"
  else
    printf '%s\n' "  FAIL: example.com did not resolve via CoreDNS forward"
    fail=1
  fi
else
  printf '%s\n' "  FAIL: DNS probe pods did not reach Running (dns-probe=${p1:-unknown} dns-neg=${p2:-unknown})"
  fail=1
fi
kubectl delete namespace "${DNS_NS}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true

printf '%s\n' "=== smoke-check complete ==="
if [[ "${fail}" -eq 0 ]]; then
  printf '%s\n' "PASS: all checks passed"
  exit 0
fi
printf '%s\n' "FAIL: one or more checks failed"
exit 1
