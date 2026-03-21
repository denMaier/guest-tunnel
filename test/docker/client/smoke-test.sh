#!/usr/bin/env bash
set -euo pipefail

TEST_CASE="${1:-happy}"
WORKDIR="/work"
PASS=0
FAIL=0

log_pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
log_fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# ── Happy path ──────────────────────────────────────────────────────────────

run_happy() {
  echo "--- happy path ---"
  local outfile
  outfile="$(mktemp)"

  guest-tunnel \
    --config "${WORKDIR}/config-good.yml" \
    --identity /root/.ssh/id_ed25519 \
    --no-reconnect \
    >"${outfile}" 2>&1 &
  local tunnel_pid=$!

  # Wait for "tunnel is up" in output (max 60s)
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if ! kill -0 "${tunnel_pid}" 2>/dev/null; then
      # Process exited — tunnel failed
      echo "  guest-tunnel output:"
      sed 's/^/    /' "${outfile}"
      log_fail "guest-tunnel exited before tunnel was established"
      rm -f "${outfile}"
      return
    fi
    if grep -q "tunnel is up" "${outfile}" 2>/dev/null; then
      break
    fi
    sleep 1
  done

  if ! kill -0 "${tunnel_pid}" 2>/dev/null; then
    echo "  guest-tunnel output:"
    sed 's/^/    /' "${outfile}"
    log_fail "guest-tunnel exited before tunnel was established"
    rm -f "${outfile}"
    return
  fi

  if ! grep -q "tunnel is up" "${outfile}" 2>/dev/null; then
    kill "${tunnel_pid}" 2>/dev/null || true
    wait "${tunnel_pid}" 2>/dev/null || true
    echo "  guest-tunnel output:"
    sed 's/^/    /' "${outfile}"
    log_fail "\"tunnel is up\" not found in output after timeout"
    rm -f "${outfile}"
    return
  fi

  log_pass "tunnel is up"

  # Curl through SOCKS proxy to reach homeserver HTTP on port 8080
  local response
  response="$(curl --socks5-hostname 127.0.0.1:1080 -m 15 -s http://localhost:8080/ 2>/dev/null || true)"

  if echo "${response}" | grep -q "homeserver"; then
    log_pass "SOCKS proxy reaches homeserver HTTP (port 8080)"
  else
    log_fail "could not reach homeserver HTTP through SOCKS proxy"
    echo "  response: ${response}"
  fi

  kill "${tunnel_pid}" 2>/dev/null || true
  wait "${tunnel_pid}" 2>/dev/null || true
  rm -f "${outfile}"
}

# ── Failure test helper ─────────────────────────────────────────────────────

run_failure() {
  local label="$1"
  local config="$2"
  local timeout="${3:-30}"
  local socks_bind="${4:-127.0.0.1}"
  local socks_port="${5:-1080}"
  local pre_cmd="${6:-}"

  echo "--- ${label} ---"
  local outfile
  outfile="$(mktemp)"

  # Run optional pre-command (e.g., bind port for conflict test)
  if [[ -n "${pre_cmd}" ]]; then
    eval "${pre_cmd}"
    # Verify the port holder is actually binding the port before proceeding.
    # Try to bind the same port — if it fails, something is already listening (good).
    if [[ -n "${socks_port}" ]]; then
      local verify_deadline=$((SECONDS + 5))
      local port_bound=0
      while (( SECONDS < verify_deadline )); do
        if python3 -c "
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
try:
    s.bind(('${socks_bind}', ${socks_port}))
    s.close()
    sys.exit(1)  # port is free — pre_cmd not holding it yet
except OSError:
    sys.exit(0)  # port in use — good
" 2>/dev/null; then
          port_bound=1
          break
        fi
        sleep 0.1
      done
      if (( port_bound == 0 )); then
        log_fail "pre_cmd did not bind port ${socks_bind}:${socks_port}"
        rm -f "${outfile}"
        return
      fi
    fi
  fi

  local exit_code=0
  timeout "${timeout}" guest-tunnel \
    --config "${config}" \
    --identity /root/.ssh/id_ed25519 \
    --no-reconnect \
    >"${outfile}" 2>&1 || exit_code=$?

  # Kill anything we started in pre_cmd
  if [[ -n "${pre_cmd}" ]]; then
    # Kill background nc listeners on the SOCKS port
    fuser -k "${socks_port}/tcp" 2>/dev/null || true
    sleep 0.5
  fi

  if (( exit_code != 0 )); then
    log_pass "exited with non-zero status (${exit_code})"
  else
    log_fail "expected non-zero exit, got 0"
  fi

  if grep -q "tunnel is up" "${outfile}" 2>/dev/null; then
    log_fail "\"tunnel is up\" appeared in output (should have failed)"
  else
    log_pass "\"tunnel is up\" not in output"
  fi

  rm -f "${outfile}"
}

# ── Main ────────────────────────────────────────────────────────────────────

case "${TEST_CASE}" in
  happy)
    run_happy
    ;;
  all)
    run_happy
    echo
    run_failure "wrong tunnel port" "${WORKDIR}/config-wrong-port.yml" 45
    echo
    run_failure "wrong VPS user" "${WORKDIR}/config-wrong-user.yml" 30
    echo
    run_failure "SOCKS port conflict" "${WORKDIR}/config-good.yml" 15 \
      "127.0.0.1" "1080" \
      "nc -l 127.0.0.1 1080 &"
    ;;
  *)
    echo "Usage: smoke-test.sh <happy|all>" >&2
    exit 1
    ;;
esac

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  exit 1
fi
exit 0
