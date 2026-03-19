#!/usr/bin/env bash
set -euo pipefail

cmd="${1:-}"
shift || true

is_reverse_active() {
  if [[ -f /tmp/reverse-tunnel.pid ]]; then
    pid="$(cat /tmp/reverse-tunnel.pid 2>/dev/null || true)"
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
  daemon-reload|enable|disable|mask|unmask|start|stop|restart)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
