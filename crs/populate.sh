#!/usr/bin/env bash
#
# Copyright 2026 The cluster-api-hypervisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# populate.sh turns a directory of manifest YAML files into a core v1
# ConfigMap YAML stream on stdout. The ClusterResourceSet controller fetches
# each emitted ConfigMap and applies every string value of its data map as a
# YAML document, so each manifest file becomes one data entry keyed by its
# file name, and the ConfigMap name is derived from the file name.
#
# The source manifest directory is the first positional argument; the
# CRS_MANIFEST_DIR environment variable is the alternative interface:
#
#   populate.sh <manifest-dir>
#   CRS_MANIFEST_DIR=<manifest-dir> populate.sh
#
# stdout carries the ConfigMap stream only, so the output can be piped
# straight into `kubectl apply -f -`; diagnostics go to stderr.

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

# usage prints the invocation contract to stderr.
usage() {
    cat >&2 <<'EOF'
usage: populate.sh <manifest-dir>
       CRS_MANIFEST_DIR=<manifest-dir> populate.sh

Turns a directory of manifest YAML files into a core v1 ConfigMap YAML stream
on stdout. Each manifest file becomes one data entry keyed by its file name.
EOF
}

# configmap_name derives a DNS-1123 subdomain ConfigMap name from a manifest
# file name: the extension is dropped, the remainder is lowercased, and every
# run of non-alphanumeric characters becomes a single dash.
configmap_name() {
    local -r file_name="$1"
    printf '%s' "${file_name%.*}" \
        | tr '[:upper:]' '[:lower:]' \
        | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

# emit_configmap writes one ConfigMap document for a manifest file, preceded
# by a document separator unless it is the first document of the stream.
emit_configmap() {
    local -r first="$1"
    local -r file="$2"
    local -r file_name="$(basename -- "${file}")"
    local -r cm_name="$(configmap_name "${file_name}")"

    if [[ -z "${cm_name}" ]]; then
        printf 'populate.sh: %s: cannot derive a ConfigMap name from the file name\n' "${file}" >&2
        return 1
    fi

    if [[ "${first}" -eq 0 ]]; then
        printf -- '---\n'
    fi
    printf -- 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  %s: |\n' "${cm_name}" "${file_name}"
    # Indent the manifest content into the literal block scalar. awk keeps the
    # content literal and terminates every line, so the block scalar value
    # round-trips to the original manifest text.
    awk '{ print "    " $0 }' "${file}"
}

main() {
    local -r source_dir="${1:-${CRS_MANIFEST_DIR:-}}"

    if [[ -z "${source_dir}" ]]; then
        usage
        return 1
    fi
    if [[ ! -d "${source_dir}" ]]; then
        printf 'populate.sh: %s: not a directory\n' "${source_dir}" >&2
        return 1
    fi

    # Resolve to an absolute path so relative invocations (the contract test
    # runs the script from this package directory) resolve to the same input.
    local -r resolved_dir="$(cd -- "${source_dir}" && pwd -P)"

    local first=1
    local file
    # Top-level *.yaml and *.yml files only, in sorted order for a
    # deterministic stream. An empty directory yields an empty stream.
    while IFS= read -r -d '' file; do
        emit_configmap "${first}" "${file}"
        first=0
    done < <(find "${resolved_dir}" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' \) -print0 | sort -z)

    return 0
}

main "$@"
