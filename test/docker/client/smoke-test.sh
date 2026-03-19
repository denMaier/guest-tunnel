#!/usr/bin/env bash
set -euo pipefail

scenario="${1:-all}"
workdir="${GT_WORKDIR:-/work}"
key="${workdir}/keys/client_ed25519"
log_dir="${workdir}/logs"

mkdir -p "${log_dir}"
gt_pid=""
blocker_pid=""

cleanup_all() {
  cleanup_guest_tunnel
  if [[ -n "${blocker_pid}" ]] && kill -0 "${blocker_pid}" >/dev/null 2>&1; then
    kill "${blocker_pid}" >/dev/null 2>&1 || true
    wait "${blocker_pid}" >/dev/null 2>&1 || true
  fi
}

trap cleanup_all EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

wait_for_port() {
  local port="$1"
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if nc -z 127.0.0.1 "${port}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_log_text() {
  local text="$1"
  local log_file="$2"
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if grep -q "${text}" "${log_file}" 2>/dev/null; then
      return 0
    fi
    if [[ -n "${gt_pid}" ]] && ! kill -0 "${gt_pid}" >/dev/null 2>&1; then
      return 1
    fi
    sleep 1
  done
  return 1
}

assert_no_success_banner() {
  local log_file="$1"
  if grep -q "Both gates cleared" "${log_file}"; then
    fail "unexpected success banner in ${log_file}"
  fi
}

run_guest_tunnel() {
  local config_path="$1"
  local log_file="$2"
  guest-tunnel --mode=client --config "${config_path}" --identity "${key}" --no-reconnect >"${log_file}" 2>&1 &
  gt_pid=$!
}

cleanup_guest_tunnel() {
  if [[ -n "${gt_pid:-}" ]] && kill -0 "${gt_pid}" >/dev/null 2>&1; then
    kill "${gt_pid}" >/dev/null 2>&1 || true
    wait "${gt_pid}" >/dev/null 2>&1 || true
  fi
  gt_pid=""
}

run_happy_path() {
  local log_file="${log_dir}/happy.log"
  rm -f "${log_file}"

  if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    fail "home loopback service should not be reachable without the tunnel"
  fi

  run_guest_tunnel "${workdir}/config-good.yml" "${log_file}"

  wait_for_port 1080 || fail "guest-tunnel never opened the SOCKS port"
  wait_for_log_text "Both gates cleared" "${log_file}" || fail "expected success banner in ${log_file}"

  local body
  body="$(curl -fsS --socks5-hostname 127.0.0.1:1080 http://127.0.0.1:8080/)"
  [[ "${body}" == *"guest-tunnel-home-ok"* ]] || fail "unexpected tunneled response: ${body}"

  cleanup_guest_tunnel
  echo "happy: ok"
}

run_failure_case() {
  local name="$1"
  local config_path="$2"
  local expected_text="${3:-}"
  local log_file="${log_dir}/${name}.log"
  rm -f "${log_file}"

  run_guest_tunnel "${config_path}" "${log_file}"
  if wait "${gt_pid}"; then
    fail "${name} unexpectedly succeeded"
  fi
  gt_pid=""

  assert_no_success_banner "${log_file}"
  if [[ -n "${expected_text}" ]]; then
    grep -q "${expected_text}" "${log_file}" || fail "expected ${expected_text} in ${log_file}"
  fi
  echo "${name}: ok"
}

run_port_conflict() {
  local log_file="${log_dir}/port-conflict.log"
  rm -f "${log_file}"

  nc -l 127.0.0.1 1080 >/tmp/guest-tunnel-port-blocker.log 2>&1 &
  blocker_pid=$!
  sleep 1

  run_guest_tunnel "${workdir}/config-good.yml" "${log_file}"
  if wait "${gt_pid}"; then
    fail "port-conflict unexpectedly succeeded"
  fi
  gt_pid=""

  assert_no_success_banner "${log_file}"
  grep -q "already in use" "${log_file}" || fail "expected port-in-use error in ${log_file}"

  kill "${blocker_pid}" >/dev/null 2>&1 || true
  wait "${blocker_pid}" >/dev/null 2>&1 || true
  blocker_pid=""
  echo "port-conflict: ok"
}

case "${scenario}" in
  happy)
    run_happy_path
    ;;
  wrong-port)
    run_failure_case "wrong-port" "${workdir}/config-wrong-port.yml" "Failed to establish tunnel"
    ;;
  wrong-user)
    run_failure_case "wrong-user" "${workdir}/config-wrong-user.yml" "Failed to establish tunnel"
    ;;
  port-conflict)
    run_port_conflict
    ;;
  all)
    run_happy_path
    run_failure_case "wrong-port" "${workdir}/config-wrong-port.yml" "Failed to establish tunnel"
    run_failure_case "wrong-user" "${workdir}/config-wrong-user.yml" "Failed to establish tunnel"
    run_port_conflict
    ;;
  *)
    fail "unknown scenario: ${scenario}"
    ;;
esac
