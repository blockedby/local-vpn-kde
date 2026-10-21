#!/usr/bin/env bash
# User-session adapter. Privileged networking belongs only to fixed system units.
set -euo pipefail
repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)"
binary="$repo/.build/local-vpn-kde.bin"
base="${VPNKIT_LOCAL_SECRETS_DIR:-$repo/secrets/vpnkit-local}"
if [[ $repo == /usr/share/local-vpn-kde ]]; then
  binary=/usr/libexec/local-vpn-kde/local-vpn-kde.bin
  base="${VPNKIT_LOCAL_SECRETS_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/local-vpn-kde}"
fi
case "${1:-}" in
  servers|diagnostics)
    exec "$binary" native-operation-client "$@" ;;
  toggle)
    [[ $# -eq 2 && "$2" == mode ]] || exit 2
    action=toggle-mode ;;

  status)
    [[ $# -eq 1 || ( $# -eq 2 && "$2" == --json ) ]] || exit 2
    action=status ;;
  backend)
    [[ $# -eq 2 && "$2" == start ]] || exit 2
    action=backend-start ;;
  start|disconnect|stop)
    [[ $# -eq 1 ]] || exit 2
    action="$1" ;;
  *)
    printf '%s\n' 'native runtime: unsupported action' >&2
    exit 2 ;;
esac
exec "$binary" native-desktop --repo "$repo" --secrets-dir "$base" --action "$action" --yes
