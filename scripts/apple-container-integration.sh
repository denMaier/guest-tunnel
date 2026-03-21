#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="${GT_CONTAINER_WORKDIR:-${TMPDIR:-/tmp}/guest-tunnel-container}"
NETWORK_NAME="guest-tunnel-test"
TEST_IMAGE="guest-tunnel-test-env:latest"
VPS_CONTAINER="guest-tunnel-vps"
HOME_CONTAINER="guest-tunnel-home"
CLIENT_CONTAINER="guest-tunnel-client"

export GT_WORKDIR="${WORKDIR}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

prepare_files() {
  need_cmd ssh-keygen

  mkdir -p "${WORKDIR}/keys" "${WORKDIR}/logs"

  if [[ ! -f "${WORKDIR}/keys/client_ed25519" ]]; then
    ssh-keygen -q -t ed25519 -N "" -f "${WORKDIR}/keys/client_ed25519" -C "guest-tunnel-client-test" >/dev/null
  fi

  if [[ ! -f "${WORKDIR}/keys/reverse_ed25519" ]]; then
    ssh-keygen -q -t ed25519 -N "" -f "${WORKDIR}/keys/reverse_ed25519" -C "guest-tunnel-reverse-test" >/dev/null
  fi
}

write_runtime_configs() {
  local vps_host="$1"

  cat >"${WORKDIR}/config-good.yml" <<EOF
vps_host: ${vps_host}
vps_user: jumpuser
vps_port: 22
home_user: tunneluser
tunnel_port: 2222
socks_port: 1080
socks_bind: 127.0.0.1
EOF

  cat >"${WORKDIR}/config-wrong-port.yml" <<EOF
vps_host: ${vps_host}
vps_user: jumpuser
vps_port: 22
home_user: tunneluser
tunnel_port: 2299
socks_port: 1080
socks_bind: 127.0.0.1
EOF

  cat >"${WORKDIR}/config-wrong-user.yml" <<EOF
vps_host: ${vps_host}
vps_user: wronguser
vps_port: 22
home_user: tunneluser
tunnel_port: 2222
socks_port: 1080
socks_bind: 127.0.0.1
EOF
}

container_cli() {
  container "$@"
}

container_exists() {
  local inspect_output
  inspect_output="$(container_cli inspect "$1" 2>/dev/null | tr -d '[:space:]')"
  [[ "${inspect_output}" != "[]" && -n "${inspect_output}" ]]
}

network_exists() {
  local inspect_output
  inspect_output="$(container_cli network inspect "$1" 2>/dev/null | tr -d '[:space:]')"
  [[ "${inspect_output}" != "[]" && -n "${inspect_output}" ]]
}

delete_container_if_present() {
  if container_exists "$1"; then
    container_cli delete --force "$1" >/dev/null 2>&1 || true
  fi
}

ensure_system_started() {
  container_cli system start >/dev/null
}

build_images() {
  container_cli build -t "${TEST_IMAGE}" -f "${ROOT_DIR}/test/docker/Dockerfile" "${ROOT_DIR}"
}

ensure_network() {
  if ! network_exists "${NETWORK_NAME}"; then
    container_cli network create "${NETWORK_NAME}" >/dev/null
  fi
}

wait_for_exec() {
  local container_name="$1"
  local command="$2"
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if container_cli exec "${container_name}" bash -lc "${command}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for ${container_name}: ${command}" >&2
  return 1
}

get_container_ip() {
  container_cli inspect "$1" \
    | sed -En 's/.*"ipv4Address"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' \
    | tr -d '\\' \
    | head -n 1 \
    | cut -d/ -f1
}

start_vps() {
  delete_container_if_present "${VPS_CONTAINER}"
  container_cli run -d \
    --name "${VPS_CONTAINER}" \
    --network "${NETWORK_NAME}" \
    --env GT_ROLE=vps \
    --env GT_WORKDIR=/work \
    --env GT_VPS_HOST="${VPS_CONTAINER}" \
    --volume "${WORKDIR}:/work" \
    --entrypoint /opt/guest-tunnel-test/entrypoint.sh \
    "${TEST_IMAGE}" >/dev/null
  wait_for_exec "${VPS_CONTAINER}" "nc -z 127.0.0.1 22"
}

start_home() {
  local vps_host="$1"
  delete_container_if_present "${HOME_CONTAINER}"
  container_cli run -d \
    --name "${HOME_CONTAINER}" \
    --network "${NETWORK_NAME}" \
    --env GT_ROLE=home \
    --env GT_WORKDIR=/work \
    --env GT_VPS_HOST="${vps_host}" \
    --volume "${WORKDIR}:/work" \
    --entrypoint /opt/guest-tunnel-test/entrypoint.sh \
    "${TEST_IMAGE}" >/dev/null
  wait_for_exec "${HOME_CONTAINER}" "nc -z 127.0.0.1 22"
}

start_client() {
  delete_container_if_present "${CLIENT_CONTAINER}"
  container_cli run -d \
    --name "${CLIENT_CONTAINER}" \
    --network "${NETWORK_NAME}" \
    --env GT_ROLE=client \
    --env GT_WORKDIR=/work \
    --volume "${WORKDIR}:/work" \
    --entrypoint /opt/guest-tunnel-test/entrypoint.sh \
    "${TEST_IMAGE}" >/dev/null
  wait_for_exec "${CLIENT_CONTAINER}" "true"
}

wait_for_stack() {
  wait_for_exec "${VPS_CONTAINER}" "nc -z 127.0.0.1 22"
  wait_for_exec "${HOME_CONTAINER}" "nc -z 127.0.0.1 22"
  wait_for_exec "${VPS_CONTAINER}" "nc -z 127.0.0.1 2222"
}

down_stack() {
  delete_container_if_present "${CLIENT_CONTAINER}"
  delete_container_if_present "${HOME_CONTAINER}"
  delete_container_if_present "${VPS_CONTAINER}"
  if network_exists "${NETWORK_NAME}"; then
    container_cli network delete "${NETWORK_NAME}" >/dev/null 2>&1 || true
  fi
}

up_stack() {
  prepare_files
  ensure_system_started
  down_stack
  build_images
  ensure_network
  start_vps
  local vps_ip
  vps_ip="$(get_container_ip "${VPS_CONTAINER}")"
  if [[ -z "${vps_ip}" ]]; then
    echo "could not determine ${VPS_CONTAINER} IP address" >&2
    exit 1
  fi
  write_runtime_configs "${vps_ip}"
  start_home "${vps_ip}"
  start_client
  wait_for_stack
}

print_logs() {
  local service="${1:-all}"
  local target=""
  case "${service}" in
    vps) target="${VPS_CONTAINER}" ;;
    home) target="${HOME_CONTAINER}" ;;
    client) target="${CLIENT_CONTAINER}" ;;
    all)
      for target in "${VPS_CONTAINER}" "${HOME_CONTAINER}" "${CLIENT_CONTAINER}"; do
        if container_exists "${target}"; then
          echo "===== ${target} ====="
          container_cli logs -n 100 "${target}" || true
        fi
      done
      return 0
      ;;
    *)
      echo "unknown service: ${service}" >&2
      exit 1
      ;;
  esac

  container_cli logs -f "${target}"
}

cmd="${1:-help}"
service="${2:-all}"

case "${cmd}" in
  prepare)
    prepare_files
    echo "Prepared test assets in ${WORKDIR}"
    ;;
  up)
    need_cmd container
    up_stack
    echo "Apple Container stack is ready"
    ;;
  down)
    need_cmd container
    down_stack
    ;;
  logs)
    need_cmd container
    print_logs "${service}"
    ;;
  smoke)
    need_cmd container
    up_stack
    container_cli exec "${CLIENT_CONTAINER}" /opt/guest-tunnel-test/client/smoke-test.sh happy
    ;;
  test)
    need_cmd container
    cleanup() {
      if [[ "${GT_KEEP_STACK:-0}" != "1" ]]; then
        down_stack >/dev/null 2>&1 || true
      fi
    }
    trap cleanup EXIT
    up_stack
    container_cli exec "${CLIENT_CONTAINER}" /opt/guest-tunnel-test/client/smoke-test.sh all
    ;;
  shell-client)
    need_cmd container
    up_stack
    container_cli exec -it "${CLIENT_CONTAINER}" bash
    ;;
  help|*)
    cat <<EOF
Usage: scripts/apple-container-integration.sh <command> [service]

Commands:
  prepare       Generate test keys under ${WORKDIR}
  up            Build images and start the Apple Container test stack
  down          Stop and remove the stack
  logs [name]   Follow logs for vps, home, client, or print snapshots for all
  smoke         Run the happy-path smoke test
  test          Run happy-path and negative-path checks
  shell-client  Open a shell in the client container

Environment:
  GT_CONTAINER_WORKDIR   Override the host workdir mounted into containers
  GT_KEEP_STACK=1        Keep the stack running after \`test\`
EOF
    ;;
esac
