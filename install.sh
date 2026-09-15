#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
for arg in "$@"; do
  case "$arg" in
    -h|--help|--dry-run|--plan) exec bash scripts/vpnkit/vpnkit-local-install.sh "$@" ;;
    --demo) ;;
    *) echo 'Неизвестный параметр установки.' >&2; exit 2 ;;
  esac
done
(( EUID != 0 )) || { echo 'Run ./install.sh as your desktop user, without sudo.' >&2; exit 3; }
command -v go >/dev/null || { echo 'Go is required to build the application.' >&2; exit 10; }
command -v bun >/dev/null || { echo 'Bun is required.' >&2; exit 10; }
command -v docker >/dev/null || { echo 'Docker with Compose is required.' >&2; exit 10; }
[[ -t 0 && -t 1 ]] || { echo 'Запусти ./install.sh в обычном терминале.' >&2; exit 2; }
printf 'Подготавливаем установщик…\n'
bootstrap_log=$(mktemp "${TMPDIR:-/tmp}/local-vpn-kde-setup.XXXXXX")
trap 'rm -f -- "$bootstrap_log"' EXIT
if ! { go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde && (cd scripts/vpnkit/tui && bun install --frozen-lockfile); } >"$bootstrap_log" 2>&1; then
  printf 'Не удалось подготовить установщик. Приватный журнал: %s\n' "$bootstrap_log" >&2
  trap - EXIT
  exit 10
fi
rm -f -- "$bootstrap_log"
trap - EXIT
exec bun scripts/vpnkit/tui/setup-index.ts "$@"
