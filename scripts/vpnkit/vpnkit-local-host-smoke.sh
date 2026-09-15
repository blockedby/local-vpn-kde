#!/usr/bin/env bash
# Compatibility entrypoint for the native, read-only connection checks.
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ -x "$root/.build/local-vpn-kde.bin" ]] || { echo 'Сначала выполните ./install.sh.' >&2; exit 10; }
exec "$root/.build/local-vpn-kde.bin" host-smoke "$@"
