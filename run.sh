#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
. scripts/vpnkit/vpnkit-local-dependencies.sh
vpnkit_dependency_path
vpnkit_docker_group_session "$PWD/run.sh" "$@"
[[ -x .build/local-vpn-kde.bin ]] || { echo 'Сначала выполните ./install.sh.' >&2; exit 10; }
exec .build/local-vpn-kde.bin ui "$@"
