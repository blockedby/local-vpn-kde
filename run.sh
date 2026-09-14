#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
exec bash scripts/vpnkit/vpnkit-local-tui.sh "$@"
