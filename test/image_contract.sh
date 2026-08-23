#!/usr/bin/env bash
#
# image_contract.sh — verify the provider container image content contract.
#
# Contract (install contract, prose):
#   The provider image produced by `make image` must contain:
#     - the provider binary, installed and executable at the pinned
#       entrypoint path /usr/local/bin/cluster-api-hypervisor, and the
#       image entrypoint must be set to that provider binary;
#     - the pinned tool binaries discoverable on PATH inside the container:
#       cloud-hypervisor, qemu-img, mksquashfs. DNS/DHCP come from the
#       k8netd daemon quadlet and ship in no image.
#
# Image reference resolution (first match wins):
#   1. the IMAGE environment variable;
#   2. the IMAGE or IMG variable declared in the repository Makefile (the
#      value must be a literal tag; make functions are not expanded);
#   3. the default local tag cluster-api-hypervisor:dev.
#
# PATH lookup mechanism: each binary is resolved inside the container with
# `which` when the image ships it, falling back to a POSIX `command -v`
# lookup through the container shell. The pinned base image (fedora) does
# not install /usr/bin/which, so the fallback is the common path. The
# contract being asserted is "the binary resolves on PATH inside the
# container", not the lookup tool itself.
#
# Exit codes:
#   0  the image satisfies the content contract
#   1  contract violation: a required binary is missing or the entrypoint
#      does not point at the provider binary
#   2  prerequisite problem: podman unavailable, unexpected arguments, or
#      a podman invocation failed (check the podman daemon)
#   3  the image is not built; run `make image` first (or point IMAGE at a
#      built image)
#
# Usage:
#   test/image_contract.sh             # default / Makefile / make image
#   IMAGE=<reference> test/image_contract.sh

set -Eeuo pipefail

readonly DEFAULT_IMAGE="cluster-api-hypervisor:dev"
readonly ENTRYPOINT_BIN="/usr/local/bin/cluster-api-hypervisor"
readonly REQUIRED_TOOLS="cloud-hypervisor qemu-img mksquashfs"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly REPO_ROOT

log() { printf 'image_contract: %s\n' "$*" >&2; }

fail() {
  local message="$1"
  local code="${2:-1}"
  printf 'image_contract: %s\n' "$message" >&2
  exit "$code"
}

# read_makefile_image <makefile> — print the first IMAGE or IMG assignment,
# stripped of quotes, trailing comments, and whitespace. Prints nothing when
# the file is absent or the value is computed with make functions.
read_makefile_image() {
  local makefile="$1"
  local line=""
  [ -f "$makefile" ] || return 0
  line="$(sed -n -E \
    '/^[[:space:]]*(IMAGE|IMG)[[:space:]]*[:?+]*=[[:space:]]*/ {
       s/^[[:space:]]*(IMAGE|IMG)[[:space:]]*[:?+]*=[[:space:]]*//
       s/[[:space:]]*#.*$//
       s/[[:space:]]*$//
       p
       q
     }' "$makefile")"
  case "$line" in
    *'$'*) return 0 ;; # computed via make functions; the contract needs a literal tag
  esac
  line="${line%\"}"
  line="${line#\"}"
  line="${line%\'}"
  line="${line#\'}"
  printf '%s\n' "$line"
}

resolve_image() {
  local image=""
  if [ -n "${IMAGE:-}" ]; then
    printf '%s\n' "$IMAGE"
    return 0
  fi
  image="$(read_makefile_image "${PWD}/Makefile")"
  if [ -n "$image" ]; then
    printf '%s\n' "$image"
    return 0
  fi
  image="$(read_makefile_image "${REPO_ROOT}/Makefile")"
  if [ -n "$image" ]; then
    printf '%s\n' "$image"
    return 0
  fi
  printf '%s\n' "$DEFAULT_IMAGE"
}

require_podman() {
  if ! command -v podman >/dev/null 2>&1; then
    fail "podman not found in PATH; the image content contract check requires podman" 2
  fi
}

require_image_built() {
  local image="$1"
  if podman image exists "$image" >/dev/null 2>&1; then
    return 0
  fi
  fail "image '${image}' is not built; run 'make image' first (or set IMAGE=<reference> to check a built image)" 3
}

check_entrypoint_binary() {
  local image="$1"
  if podman run --rm --entrypoint test "$image" -x "${ENTRYPOINT_BIN}" >/dev/null 2>&1; then
    log "ok: provider binary is present and executable at ${ENTRYPOINT_BIN}"
    return 0
  fi
  log "missing: provider binary not found or not executable at ${ENTRYPOINT_BIN}"
  return 1
}

check_entrypoint_config() {
  local image="$1"
  local entrypoint=""
  entrypoint="$(podman image inspect --format '{{json .Config.Entrypoint}}' "$image" 2>/dev/null)" || {
    log "missing: cannot inspect the entrypoint of image '${image}'"
    return 1
  }
  case "$entrypoint" in
    *cluster-api-hypervisor*)
      log "ok: image entrypoint is the provider binary (${entrypoint})"
      return 0
      ;;
    *)
      log "missing: image entrypoint '${entrypoint}' is not the provider binary (expected '${ENTRYPOINT_BIN}')"
      return 1
      ;;
  esac
}

check_tool() {
  # check_tool <image> <tool> — assert <tool> resolves on PATH inside <image>.
  local image="$1"
  local tool="$2"
  local output="" rc=0
  output="$(podman run --rm --entrypoint which "$image" "$tool" 2>/dev/null)" || rc=$?
  case "$rc" in
    0)
      log "ok: ${tool} on PATH (${output})"
      return 0
      ;;
    125)
      fail "podman run failed (exit 125) while checking '${tool}' in '${image}'; is the podman daemon running?" 2
      ;;
    126 | 127) ;; # `which` missing or not executable in the image: retry below
    *)
      log "missing: ${tool} not found on PATH in image '${image}'"
      return 1
      ;;
  esac
  rc=0 # the first attempt may have left a stale status; retry with a fresh one
  output="$(podman run --rm --entrypoint sh "$image" -c "command -v ${tool}" 2>/dev/null)" || rc=$?
  case "$rc" in
    0)
      log "ok: ${tool} on PATH (${output})"
      return 0
      ;;
    125)
      fail "podman run failed (exit 125) while checking '${tool}' in '${image}'; is the podman daemon running?" 2
      ;;
    127)
      log "error: image '${image}' ships neither 'which' nor a shell; cannot perform a PATH lookup"
      return 1
      ;;
    *)
      log "missing: ${tool} not found on PATH in image '${image}'"
      return 1
      ;;
  esac
}

main() {
  local image=""
  local tool=""
  local problems=0
  if [ "$#" -ne 0 ]; then
    printf 'image_contract: usage: IMAGE=<image> %s\n' "$0" >&2
    exit 2
  fi
  require_podman
  image="$(resolve_image)"
  log "checking image '${image}'"
  require_image_built "$image"
  check_entrypoint_binary "$image" || problems=$((problems + 1))
  check_entrypoint_config "$image" || problems=$((problems + 1))
  # The tool list is a fixed contract constant; word-splitting is intentional.
  # shellcheck disable=SC2086
  for tool in $REQUIRED_TOOLS; do
    check_tool "$image" "$tool" || problems=$((problems + 1))
  done
  if [ "$problems" -gt 0 ]; then
    fail "contract check failed: ${problems} problem(s) in image '${image}'" 1
  fi
  log "image '${image}' satisfies the content contract"
}

main "$@"
