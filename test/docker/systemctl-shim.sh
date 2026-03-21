#!/usr/bin/env bash
set -euo pipefail

cmd="${1:-}"
shift || true

PID_FILE="/tmp/reverse-tunnel.pid"

is_reverse_active() {
  if [[ -f "${PID_FILE}" ]]; then
    pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
      return 0
    fi
  fi
  return 1
}

case "${cmd}" in
  is-active)
    if [[ "${1:-}" == "--quiet" ]]; then
      shift
    fi
    case "${1:-}" in
      ssh|sshd|openssh-server)
        exit 0
        ;;
      reverse-tunnel.service)
        if is_reverse_active; then
          exit 0
        fi
        exit 3
        ;;
      *)
        exit 3
        ;;
    esac
    ;;
  show)
    echo "ExecStart=/usr/sbin/sshd -D"
    exit 0
    ;;
  start)
    case "${1:-}" in
      reverse-tunnel.service)
        # Read the ExecStart line from the service file and run autossh
        svc_file="/etc/systemd/system/reverse-tunnel.service"
        if [[ -f "${svc_file}" ]]; then
          exec_line="$(grep '^ExecStart=' "${svc_file}" | head -1 | sed 's/^ExecStart=//')"
          if [[ -n "${exec_line}" ]]; then
            # Start autossh in background, redirect output to log
            nohup bash -c "${exec_line}" >>/tmp/reverse-tunnel.log 2>&1 &
            echo $! > "${PID_FILE}"
          fi
        fi
        ;;
    esac
    exit 0
    ;;
  stop)
    case "${1:-}" in
      reverse-tunnel.service)
        if [[ -f "${PID_FILE}" ]]; then
          pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
          if [[ -n "${pid}" ]]; then
            kill "${pid}" 2>/dev/null || true
            wait "${pid}" 2>/dev/null || true
          fi
          rm -f "${PID_FILE}"
        fi
        ;;
    esac
    exit 0
    ;;
  restart)
    case "${1:-}" in
      reverse-tunnel.service)
        # Stop then start
        "$0" stop reverse-tunnel.service
        "$0" start reverse-tunnel.service
        ;;
    esac
    exit 0
    ;;
  daemon-reload|enable|disable|mask|unmask)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
