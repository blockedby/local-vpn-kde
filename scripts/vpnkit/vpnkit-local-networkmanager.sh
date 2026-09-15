#!/usr/bin/env bash
# Compatibility entrypoint; ownership, readiness and rollback live in Go.
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ -x "$root/.build/local-vpn-kde.bin" ]] || { echo 'Сначала выполните ./install.sh.' >&2; exit 10; }
action=${1:-plan}
if (( $# )); then shift; fi
case "$action" in help|-h|--help) exec "$root/.build/local-vpn-kde.bin" networkmanager --help ;; esac
exec "$root/.build/local-vpn-kde.bin" networkmanager --repo "$root" --action "$action" "$@"
