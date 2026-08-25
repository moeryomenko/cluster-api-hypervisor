#!/usr/bin/env bash
#
# fake-cloud-hypervisor.sh — test double for the cloud-hypervisor binary.
#
# The subprocess manager under test spawns this script with
#   --api-socket path=<sock>
# and treats it as the cloud-hypervisor process. The script:
#
#   1. Appends one FAKE_INVOCATION block (each argv entry on its own line) to
#      the file named by FAKE_CH_RECORD, so tests can assert the exact argv
#      the manager passed and count how many times it respawned the process.
#   2. Honors a set of FAKE_CH_* knobs:
#        FAKE_CH_EXIT          exit code for a simulated startup failure
#        FAKE_CH_EXIT_MSG      stderr line written before that exit
#        FAKE_CH_EXIT_DELAY    seconds to stay up before that exit
#        FAKE_CH_SOCKET_DELAY  seconds to wait before binding the API socket
#        FAKE_CH_NO_SOCKET     "1" to stay up forever without creating a socket
#        FAKE_CH_IGNORE_TERM   "1" to ignore SIGTERM (forces the SIGKILL path)
#        FAKE_CH_SIGNAL_FILE   file to append "SIGTERM" to when SIGTERM is
#                              received and handled by the socket server
#   3. With no knobs, binds a real listening unix socket at <sock> (via a tiny
#      embedded python3 server — the only tool reliably available here that
#      can expose a unix listener from a shell script) and serves it until
#      terminated. On SIGTERM it records the signal and exits 0. Like the
#      real cloud-hypervisor, the bind fails when the socket pathname already
#      exists (AddrInUse); stale-socket tolerance is the Manager's job, not
#      the child's.
#
# The script is the exact process the manager spawns: the bash layer only
# records arguments and performs startup delays/exits, then execs the python
# server so the manager's PID stays the single long-lived process (no orphaned
# grandchildren on the SIGKILL path).
set -Eeuo pipefail

record="${FAKE_CH_RECORD:-}"
if [[ -n "$record" ]]; then
    {
        printf 'FAKE_INVOCATION\n'
        for arg in "$@"; do
            printf '%s\n' "$arg"
        done
    } >> "$record"
fi

# Simulated startup failure: exit immediately (or after FAKE_CH_EXIT_DELAY
# seconds) with a stderr message the manager must capture.
if [[ -n "${FAKE_CH_EXIT:-}" ]]; then
    if [[ -n "${FAKE_CH_EXIT_DELAY:-}" ]]; then
        sleep "$FAKE_CH_EXIT_DELAY"
    fi
    printf '%s\n' "${FAKE_CH_EXIT_MSG:-fake-cloud-hypervisor: simulated startup failure}" >&2
    exit "$FAKE_CH_EXIT"
fi

# Stay up forever without binding a socket: exercises the WaitReady timeout
# path. SIGTERM exits cleanly so Stop can reap the process.
if [[ "${FAKE_CH_NO_SOCKET:-}" == "1" ]]; then
    trap 'exit 0' SIGTERM SIGINT
    while :; do
        sleep 1
    done
fi

if [[ -n "${FAKE_CH_SOCKET_DELAY:-}" ]]; then
    sleep "$FAKE_CH_SOCKET_DELAY"
fi

exec python3 - "$@" <<'PY'
import os
import signal
import socket
import sys

sock_path = None
prev = ""
for arg in sys.argv[1:]:
    if prev == "--api-socket" and arg.startswith("path="):
        sock_path = arg[len("path="):]
    prev = arg

signal_file = os.environ.get("FAKE_CH_SIGNAL_FILE", "")
ignore_term = os.environ.get("FAKE_CH_IGNORE_TERM", "") == "1"

got_term = False

def on_term(signum, frame):
    global got_term
    got_term = True

if ignore_term:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
else:
    signal.signal(signal.SIGTERM, on_term)

if sock_path is None:
    sys.stderr.write("fake-cloud-hypervisor: no --api-socket path given\n")
    sys.exit(1)

os.makedirs(os.path.dirname(sock_path), exist_ok=True)

server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(sock_path)
server.listen(16)
server.settimeout(1.0)

while True:
    try:
        conn, _ = server.accept()
        conn.close()
    except socket.timeout:
        if got_term:
            break
    except OSError:
        break

if got_term and signal_file:
    with open(signal_file, "a") as f:
        f.write("SIGTERM\n")

sys.exit(0)
PY
