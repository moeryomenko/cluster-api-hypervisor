#!/usr/bin/env bash
#
# k8netd_stub_test.sh — k8netd control-socket stub contract and the provider
# host-tool contract after the k8netd migration (spec VC-08).
#
# Two contracts are pinned here.
#
# Contract 1: a stubbed k8netd control socket. The provider talks to k8netd
# exclusively over a JSON-RPC 2.0 Unix control socket (default
# /run/user/1000/k8snet/control.sock, override HYPERVISOR_K8NETD_SOCKET;
# envelope per internal/k8netd/client.go: newline-delimited JSON-RPC 2.0,
# a "version" field carrying "1.0", typed error codes not_found,
# already_exists, invalid_params, conflict, internal). This test builds a
# fake JSON-RPC responder (embedded Go program, standard library only,
# compiled into the scratch directory) and drives it through a matching
# probe client, proving the stub socket path end to end:
#
#   1. the responder binds the requested socket path, replacing a stale
#      regular file left at the path (the k8netd restart contract requires
#      stale sockets to be unlinked before binding);
#   2. a CreateNetwork round-trip returns the JSON-RPC 2.0 envelope with the
#      echoed id and a null result for void methods;
#   3. GetNetwork returns the network object (CIDR/gateway/pool fields);
#   4. AllocateIP returns the allocated address as a JSON string;
#   5. an unknown port yields the typed error code not_found;
#   6. a version mismatch yields the typed error code invalid_params;
#   7. a malformed request line yields an error response with a null id and
#      does not take the responder down (the next valid call still works);
#   8. the method inventory wired in internal/k8netd/client.go is exactly the
#      ten contract methods (CreateNetwork, DeleteNetwork, CreatePort,
#      DeletePort, AttachPort, DetachPort, GetNetwork, GetPort, AllocateIP,
#      ReleaseIP).
#
# Contract 2: no host network tooling. After the migration the provider's
# host-tool contract must not reference bridge/dnsmasq/nftables binaries at
# all: the scenario scripts (run.sh, delete-cluster.sh, smoke.sh, scale.sh)
# contain no dnsmasq, nft, k8sbr0, "ip link", or "inet k8slab" references,
# and the harness README carries no stale references to the removed
# host-tool leftover checks or the stub ip/pgrep/nft tooling. Nothing here
# requires root, netlink, nftables, dnsmasq, a cluster, or a VM.
#
# The only non-shellbuild prerequisite is the go tool (standard library
# only; the build runs with GOPROXY=off and GOWORK=off so no module or
# network access is possible).
#
# Exit codes of this test:
#   0  both contracts hold
#   1  contract violation (including internal/k8netd/client.go being absent)
#   2  prerequisite problem (missing go tool, unexpected arguments)
#
# Usage:
#   test/e2e/k8netd_stub_test.sh

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"
readonly REPO_ROOT

# The Go client whose method inventory and envelope the stub mirrors.
readonly CLIENT_GO="${REPO_ROOT}/internal/k8netd/client.go"

# The scenario scripts whose host-tool surface must be free of the removed
# bridge/dnsmasq/nftables stack.
# shellcheck disable=SC2086
readonly SCENARIO_SCRIPTS="run.sh
delete-cluster.sh
smoke.sh
scale.sh"

readonly HARNESS_README="${SCRIPT_DIR}/README.md"

# The ten contract methods (k8netd contract spec REQ-001..REQ-004).
readonly CONTRACT_METHODS="AllocateIP
AttachPort
CreateNetwork
CreatePort
DeleteNetwork
DeletePort
DetachPort
GetNetwork
GetPort
ReleaseIP"

# Timeout for the single go build of the stub programs (seconds). The build
# is standard-library-only; anything slower means the toolchain is broken.
readonly BUILD_TIMEOUT=120
# Timeout waiting for the responder to bind its socket (seconds).
readonly BIND_TIMEOUT=10

problems=0
SCRATCH=""
SERVER_PID=""

log() { printf 'k8netd_stub_test: %s\n' "$*" >&2; }

ok() { printf 'k8netd_stub_test: ok: %s\n' "$*" >&2; }

missing() {
  printf 'k8netd_stub_test: missing: %s\n' "$*" >&2
  problems=$((problems + 1))
}

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'k8netd_stub_test: %s\n' "$message" >&2
  exit "$code"
}

# --- stub fixture -----------------------------------------------------------

# write_stub_src <dir> — emit the fake k8netd JSON-RPC responder and probe
# client as one standard-library-only Go program. serve subcommand: bind the
# socket path (unlinking any stale file first, per the restart contract),
# answer newline-delimited JSON-RPC 2.0 requests per connection, and append
# every accepted request's method and version to the log file. call
# subcommand: send one request and print the response line. rawsend
# subcommand: send an arbitrary line and print the response line (used for
# the malformed-request edge).
write_stub_src() {
  local dir="$1"
  cat > "${dir}/k8netdstub.go" <<'STUB'
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

type rpcReq struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Version string            `json:"version"`
	Method  string            `json:"method"`
	Params  map[string]string `json:"params"`
}

type rpcErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

const contractVersion = "1.0"

var voidMethods = map[string]bool{
	"CreateNetwork": true,
	"DeleteNetwork": true,
	"CreatePort":    true,
	"DeletePort":    true,
	"AttachPort":    true,
	"DetachPort":    true,
	"ReleaseIP":     true,
}

func respond(id json.RawMessage, result json.RawMessage, rpcError *rpcErr) []byte {
	b, err := json.Marshal(rpcResp{JSONRPC: "2.0", ID: id, Result: result, Error: rpcError})
	if err != nil {
		panic(err)
	}
	return b
}

func handle(line []byte, logf *os.File) []byte {
	var req rpcReq
	if err := json.Unmarshal(line, &req); err != nil {
		return respond(json.RawMessage("null"), nil,
			&rpcErr{Code: "invalid_params", Message: "malformed request"})
	}
	fmt.Fprintf(logf, "%s %s\n", req.Method, req.Version)
	if req.Version != contractVersion {
		return respond(req.ID, nil,
			&rpcErr{Code: "invalid_params", Message: "version mismatch"})
	}
	switch {
	case req.Method == "AllocateIP":
		return respond(req.ID, json.RawMessage(`"192.168.124.20"`), nil)
	case req.Method == "GetNetwork":
		name := ""
		if req.Params != nil {
			name = req.Params["name"]
		}
		network := map[string]string{
			"name":      name,
			"cidr":      "192.168.124.0/24",
			"gateway":   "192.168.124.1",
			"poolStart": "192.168.124.20",
			"poolEnd":   "192.168.124.250",
		}
		res, _ := json.Marshal(network)
		return respond(req.ID, res, nil)
	case req.Method == "GetPort":
		return respond(req.ID, nil, &rpcErr{Code: "not_found", Message: "port not found"})
	case voidMethods[req.Method]:
		return respond(req.ID, json.RawMessage("null"), nil)
	default:
		return respond(req.ID, nil, &rpcErr{Code: "not_found", Message: "unknown method"})
	}
}

func serve(sockPath, logPath string) {
	_ = os.Remove(sockPath) // stale sockets are unlinked before binding
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub serve: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = l.Close() }()
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub serve: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logf.Close() }()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			w := bufio.NewWriter(c)
			for sc.Scan() {
				if _, err := w.Write(handle(sc.Bytes(), logf)); err != nil {
					return
				}
				if err := w.WriteByte('\n'); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			}
		}(conn)
	}
}

func roundtrip(sockPath string, payload []byte) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub call: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "stub call: %v\n", err)
		os.Exit(1)
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		fmt.Fprintf(os.Stderr, "stub call: no response\n")
		os.Exit(1)
	}
	fmt.Println(sc.Text())
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: k8netdstub serve <sock> <log> | call <sock> <payload> | rawsend <sock> <line>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: k8netdstub serve <sock> <log>")
			os.Exit(2)
		}
		serve(os.Args[2], os.Args[3])
	case "call":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: k8netdstub call <sock> <payload>")
			os.Exit(2)
		}
		roundtrip(os.Args[2], []byte(os.Args[3]))
	case "rawsend":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: k8netdstub rawsend <sock> <line>")
			os.Exit(2)
		}
		roundtrip(os.Args[2], []byte(os.Args[3]))
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		os.Exit(2)
	}
}
STUB
}

# build_stub <dir> — compile the stub program into <dir>/k8netdstub with an
# isolated, offline Go environment (scratch HOME/cache, GOPROXY=off,
# GOWORK=off, GOTOOLCHAIN=local). Prints the binary path.
build_stub() {
  local dir="$1"
  local gocache="${dir}/gocache" gopath="${dir}/gopath"
  mkdir -p "${gocache}" "${gopath}" "${dir}/home"
  timeout "${BUILD_TIMEOUT}" env \
    HOME="${dir}/home" \
    GOCACHE="${gocache}" \
    GOPATH="${gopath}" \
    GOPROXY=off \
    GOWORK=off \
    GOTOOLCHAIN=local \
    go build -o "${dir}/k8netdstub" "${dir}/k8netdstub.go" >&2 \
    || fail "go build of the k8netd stub failed (see output above)" 2
  printf '%s' "${dir}/k8netdstub"
}

# start_server <bin> <sock> <log> — launch the responder in the background
# and wait until the socket path exists as a socket. Aborts the test when
# binding does not happen within BIND_TIMEOUT.
start_server() {
  local bin="$1" sock="$2" reqlog="$3" i=0
  "${bin}" serve "${sock}" "${reqlog}" &
  SERVER_PID="$!"
  while [[ ! -S "${sock}" ]]; do
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
      fail "the stub responder exited before binding ${sock}" 1
    fi
    if (( i >= BIND_TIMEOUT * 10 )); then
      fail "the stub responder did not bind ${sock} within ${BIND_TIMEOUT}s" 1
    fi
    sleep 0.1
    i=$((i + 1))
  done
}

stop_server() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || :
    wait "${SERVER_PID}" 2>/dev/null || :
  fi
  SERVER_PID=""
}

# rpc_call <bin> <sock> <version> <method> <params-json> — one JSON-RPC
# round-trip; prints the response line.
rpc_call() {
  local bin="$1" sock="$2" version="$3" method="$4" params="$5"
  "${bin}" call "${sock}" \
    "{\"jsonrpc\":\"2.0\",\"id\":1,\"version\":\"${version}\",\"method\":\"${method}\",\"params\":${params}}"
}

# --- test groups ------------------------------------------------------------

test_socket_roundtrip() {
  log "test: stub k8netd socket binds, serves the envelope, and answers the contract methods"
  local dir="" bin="" sock="" reqlog="" out=""

  dir="$(mktemp -d "${SCRATCH}/roundtrip.XXXXXX")"
  write_stub_src "${dir}"
  bin="$(build_stub "${dir}")"
  sock="${dir}/k8snet/control.sock"
  reqlog="${dir}/requests.log"
  mkdir -p "${dir}/k8snet"

  # Edge: a stale regular file at the socket path is replaced by the bound
  # socket (the restart contract unlinks stale sockets before binding).
  printf 'stale\n' > "${sock}"
  start_server "${bin}" "${sock}" "${reqlog}"
  if [[ -S "${sock}" ]]; then
    ok "the stub responder bound ${sock} (stale regular file replaced)"
  else
    missing "${sock} is not a socket after the responder started"
  fi

  # Void-method round-trip: JSON-RPC 2.0 envelope, echoed id, null result.
  out="$(rpc_call "${bin}" "${sock}" "1.0" CreateNetwork \
    '{"name":"k8labs","cidr":"192.168.124.0/24","gateway":"192.168.124.1","poolStart":"192.168.124.20","poolEnd":"192.168.124.250"}')"
  if grep -Fq '"jsonrpc":"2.0"' <<< "${out}" \
    && grep -Fq '"id":1' <<< "${out}" \
    && grep -Fq '"result":null' <<< "${out}"; then
    ok "CreateNetwork round-trip returned the JSON-RPC 2.0 envelope with the echoed id"
  else
    missing "CreateNetwork response does not carry the expected envelope: ${out}"
  fi

  # GetNetwork returns the network object fields.
  out="$(rpc_call "${bin}" "${sock}" "1.0" GetNetwork '{"name":"k8labs"}')"
  if grep -Fq '"cidr":"192.168.124.0/24"' <<< "${out}" \
    && grep -Fq '"gateway":"192.168.124.1"' <<< "${out}"; then
    ok "GetNetwork returned the network object (cidr, gateway)"
  else
    missing "GetNetwork response does not carry the network object: ${out}"
  fi

  # AllocateIP returns the allocated address as a JSON string.
  out="$(rpc_call "${bin}" "${sock}" "1.0" AllocateIP \
    '{"network":"k8labs","mac":"c6:e5:50:1c:ec:01"}')"
  if grep -Fq '"result":"192.168.124.20"' <<< "${out}"; then
    ok "AllocateIP returned the allocated address"
  else
    missing "AllocateIP response does not carry the allocated address: ${out}"
  fi

  # Every request reached the stub log with the contract version.
  if grep -Fq 'CreateNetwork 1.0' <<< "$(cat "${reqlog}")" \
    && grep -Fq 'AllocateIP 1.0' <<< "$(cat "${reqlog}")"; then
    ok "the stub request log recorded the contract methods and version"
  else
    missing "the stub request log is incomplete: $(tr '\n' '|' < "${reqlog}")"
  fi

  stop_server
  rm -rf -- "${dir}"
}

test_typed_errors() {
  log "test: typed error codes, version mismatch, and malformed-request resilience"
  local dir="" bin="" sock="" reqlog="" out=""

  dir="$(mktemp -d "${SCRATCH}/errors.XXXXXX")"
  write_stub_src "${dir}"
  bin="$(build_stub "${dir}")"
  sock="${dir}/control.sock"
  reqlog="${dir}/requests.log"

  start_server "${bin}" "${sock}" "${reqlog}"

  # Typed error: an unknown port maps to not_found.
  out="$(rpc_call "${bin}" "${sock}" "1.0" GetPort '{"name":"missing-port"}')"
  if grep -Fq '"code":"not_found"' <<< "${out}"; then
    ok "an unknown port yields the typed error code not_found"
  else
    missing "GetPort for an unknown port did not yield not_found: ${out}"
  fi

  # Version mismatch maps to invalid_params (contract REQ-001).
  out="$(rpc_call "${bin}" "${sock}" "9.9" CreateNetwork '{"name":"k8labs"}')"
  if grep -Fq '"code":"invalid_params"' <<< "${out}"; then
    ok "a version mismatch yields the typed error code invalid_params"
  else
    missing "a version-mismatched request did not yield invalid_params: ${out}"
  fi

  # Edge: a malformed request line yields an error response with a null id
  # and does not take the responder down.
  out="$("${bin}" rawsend "${sock}" 'this is not json')"
  if grep -Fq '"code":"invalid_params"' <<< "${out}" \
    && grep -Fq '"id":null' <<< "${out}"; then
    ok "a malformed request yielded an error response with a null id"
  else
    missing "a malformed request did not yield the expected error response: ${out}"
  fi
  out="$(rpc_call "${bin}" "${sock}" "1.0" DeleteNetwork '{"name":"k8labs"}')"
  if grep -Fq '"result":null' <<< "${out}"; then
    ok "the responder survived the malformed request (next call succeeded)"
  else
    missing "the responder did not survive the malformed request: ${out}"
  fi

  stop_server
  rm -rf -- "${dir}"
}

test_method_inventory() {
  log "test: the client method inventory is exactly the ten contract methods"
  local actual=""
  if [[ ! -f "${CLIENT_GO}" ]]; then
    fail "k8netd client source not found: ${CLIENT_GO}" 1
  fi
  actual="$(grep -oE 'c\.call\(ctx, "[A-Za-z]+"' "${CLIENT_GO}" \
    | sed -E 's/.*"([A-Za-z]+)"$/\1/' | sort -u)"
  if diff <(printf '%s\n' "${CONTRACT_METHODS}") <(printf '%s\n' "${actual}") >/dev/null; then
    ok "internal/k8netd/client.go wires exactly the ten contract methods"
  else
    missing "the client method inventory differs from the ten contract methods: $(printf '%s ' "${actual}")"
  fi
}

test_scenario_scripts_free_of_host_tools() {
  log "test: scenario scripts carry no bridge/dnsmasq/nftables host-tool references"
  local f="" hits=""
  for f in ${SCENARIO_SCRIPTS}; do
    local path="${SCRIPT_DIR}/${f}"
    if [[ ! -f "${path}" ]]; then
      missing "scenario script not found: ${path}"
      continue
    fi
    hits="$(grep -nE '\bdnsmasq\b|\bnft\b|k8sbr0|\bip link\b|inet k8slab' "${path}" || true)"
    if [[ -z "${hits}" ]]; then
      ok "${f} carries no bridge/dnsmasq/nftables host-tool references"
    else
      missing "${f} still references the removed host-tool stack: $(printf '%s | ' "${hits}")"
    fi
  done
}

test_readme_fresh() {
  log "test: the harness README carries no stale host-tool references"
  local hits=""
  hits="$(grep -nE 'k8sbr0|dnsmasq|\bnft\b|pgrep' "${HARNESS_README}" || true)"
  if [[ -z "${hits}" ]]; then
    ok "the harness README carries no stale host-tool references"
  else
    missing "the harness README still references the removed host-tool stack: $(printf '%s | ' "${hits}")"
  fi
}

# --- entry point ------------------------------------------------------------

main() {
  if [[ "$#" -ne 0 ]]; then
    printf 'k8netd_stub_test: usage: %s\n' "$0" >&2
    exit 2
  fi

  command -v go >/dev/null 2>&1 \
    || fail "the go tool is required to build the stub k8netd responder" 2
  command -v mktemp >/dev/null 2>&1 \
    || fail "mktemp (coreutils) is required for the scratch fixtures" 2
  command -v timeout >/dev/null 2>&1 \
    || fail "timeout (coreutils) is required for the build guard" 2

  SCRATCH="$(mktemp -d)"
  trap 'stop_server; rm -rf -- "${SCRATCH}"' EXIT
  log "scratch root ${SCRATCH}"

  test_socket_roundtrip || :
  test_typed_errors || :
  test_method_inventory || :
  test_scenario_scripts_free_of_host_tools || :
  test_readme_fresh || :

  if [[ "${problems}" -gt 0 ]]; then
    fail "k8netd stub / host-tool contract check failed: ${problems} problem(s)" 1
  fi
  log "k8netd stub and host-tool contracts satisfied"
}

main "$@"
