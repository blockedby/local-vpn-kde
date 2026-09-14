#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
for arg in "$@"; do
  case "$arg" in -h|--help|--dry-run|--plan) exec bash scripts/vpnkit/vpnkit-local-install.sh "$@" ;; esac
done
(( EUID != 0 )) || { echo 'Run ./install.sh as your desktop user, without sudo.' >&2; exit 3; }
command -v bun >/dev/null || { echo 'Bun is required.' >&2; exit 10; }
command -v docker >/dev/null || { echo 'Docker with Compose is required.' >&2; exit 10; }
(cd scripts/vpnkit/tui && bun install --frozen-lockfile)
docker compose -p vpnkit-local -f docker-compose.yml -f compose.local.yaml build vpnkit
exec bash scripts/vpnkit/vpnkit-local-install.sh "$@"
