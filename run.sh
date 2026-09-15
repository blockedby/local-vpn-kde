#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
[[ -x .build/local-vpn-kde.bin ]] || { echo 'Сначала выполните ./install.sh.' >&2; exit 10; }
exec .build/local-vpn-kde.bin ui "$@"
