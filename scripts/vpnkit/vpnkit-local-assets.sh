#!/usr/bin/env bash
# Compatibility entrypoint; private files are prepared by the native backend.
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ -x "$root/.build/local-vpn-kde.bin" ]] || { echo 'Сначала выполните ./install.sh.' >&2; exit 10; }
exec "$root/.build/local-vpn-kde.bin" assets --repo "$root" "$@"
