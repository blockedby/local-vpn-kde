#!/usr/bin/env bash
# Install a locally built/downloaded native bundle. No build toolchain needed.
set -Eeuo pipefail
umask 077
export PATH=/usr/bin:/usr/sbin:/bin:/sbin
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf 'Usage: sudo bash ./install-native.sh [/path/to/vpnkit-portable]\nArch Linux x86_64. Installs dependencies/package, enrolls your desktop user,\nand prepares its profile when configuration and a desktop session exist.\nDoes not start or connect a VPN. Default artifact: vpnkit-portable beside this script.\n'
  exit 0
fi
[[ $# -le 1 ]] || { echo 'Expected at most one local portable artifact.' >&2; exit 2; }
(( EUID == 0 )) || { echo 'Run with sudo as your desktop user.' >&2; exit 2; }
[[ ${SUDO_UID:-} =~ ^[1-9][0-9]*$ && -n ${SUDO_USER:-} && ${SUDO_USER} != root ]] || {
  echo 'Run sudo from the desktop user account; direct root installation cannot select a desktop user.' >&2; exit 2;
}
[[ $(id -u -- "$SUDO_USER") == "$SUDO_UID" ]] || { echo 'Sudo caller identity mismatch.' >&2; exit 2; }
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
artifact=${1:-$script_dir/vpnkit-portable}
[[ -f "$artifact" && ! -L "$artifact" && -x "$artifact" ]] || {
  echo 'Provide the trusted local executable vpnkit-portable artifact (and verify its supplied SHA-256 first).' >&2; exit 2;
}
artifact=$(realpath -- "$artifact")
# Do not propagate root's HOME, user XDG overrides or loader variables into setup.
exec /usr/bin/env -i PATH="$PATH" LANG=C.UTF-8 HOME=/root \
  SUDO_UID="$SUDO_UID" SUDO_USER="$SUDO_USER" SUDO_GID="${SUDO_GID:-}" \
  "$artifact" --install
