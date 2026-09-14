#!/usr/bin/env bash
# Generate persistent, local-only OpenVPN PKI and a KDE/NetworkManager profile.
#
# This script deliberately has no Docker, NetworkManager, or privileged-host
# operations. It only writes the selected local secrets tree and tracked-template
# derivatives. The subscription directory is created but never read or written.
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd -P)
PATH_GUARD="$SCRIPT_DIR/vpnkit-local-path-guard.sh"
[[ -r "$PATH_GUARD" ]] || { echo 'missing local secret path guard' >&2; exit 1; }
# shellcheck source=/dev/null
. "$PATH_GUARD"

BASE=${VPNKIT_LOCAL_SECRETS_DIR:-secrets/vpnkit-local}
ENDPOINT=${VPNKIT_LOCAL_ENDPOINT:-${VPNKIT_LOCAL_HOST:-127.0.0.1}}
PORT=${VPNKIT_LOCAL_PORT:-${VPNKIT_LOCAL_OPENVPN_PORT:-1194}}
PUSH_DNS=${VPNKIT_LOCAL_OPENVPN_PUSH_DNS:-${VPNKIT_OPENVPN_PUSH_DNS:-8.8.8.8}}
CERT_DAYS=${VPNKIT_LOCAL_CERT_DAYS:-825}

usage() {
  cat <<'USAGE'
Usage: scripts/vpnkit/vpnkit-local-assets.sh [options]

Generate persistent local-only OpenVPN CA/server/client material and an inline
profile for KDE/NetworkManager import. The default output is
secrets/vpnkit-local/; use a temporary directory for tests or experiments.

Options:
  --secrets-dir <dir>  Output tree (default: secrets/vpnkit-local)
  --endpoint <host>    Local profile endpoint: 127.0.0.1 or localhost
  --port <port>        Published UDP port (default: 1194)
  --push-dns <address> Google DNS: 8.8.8.8 or 8.8.4.4 (default: 8.8.8.8)
  --help               Show this help

Environment aliases:
  VPNKIT_LOCAL_SECRETS_DIR, VPNKIT_LOCAL_ENDPOINT, VPNKIT_LOCAL_HOST,
  VPNKIT_LOCAL_PORT, VPNKIT_LOCAL_OPENVPN_PORT,
  VPNKIT_LOCAL_OPENVPN_PUSH_DNS, VPNKIT_OPENVPN_PUSH_DNS,
  VPNKIT_LOCAL_CERT_DAYS

Existing local identities are never rotated. A partially populated PKI tree is
rejected rather than repaired. The command prints metadata only; key, profile,
and subscription contents are never printed or consumed.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --secrets-dir)
      [[ $# -ge 2 ]] || { echo "--secrets-dir requires a value" >&2; exit 2; }
      BASE=$2
      shift 2
      ;;
    --endpoint)
      [[ $# -ge 2 ]] || { echo "--endpoint requires a value" >&2; exit 2; }
      ENDPOINT=$2
      shift 2
      ;;
    --port)
      [[ $# -ge 2 ]] || { echo "--port requires a value" >&2; exit 2; }
      PORT=$2
      shift 2
      ;;
    --push-dns)
      [[ $# -ge 2 ]] || { echo "--push-dns requires a value" >&2; exit 2; }
      PUSH_DNS=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$BASE" in /*) ;; *) BASE="$REPO_ROOT/$BASE" ;; esac

# Direct invocation must enforce the same root boundary as the lifecycle
# adapter before any output directory is created or any existing path is
# chmodded. The shared guard is read-only and canonicalizes BASE for all later
# path construction.
vpnkit_local_path_guard_validate_secret_root "$BASE" "$REPO_ROOT" || exit 20
BASE=$VPNKIT_LOCAL_PATH_GUARD_BASE
vpnkit_local_path_guard_validate_secret_tree "$BASE" || exit 20

case "$ENDPOINT" in
  127.0.0.1|localhost) ;;
  *)
    echo "endpoint must be localhost or 127.0.0.1" >&2
    exit 2
    ;;
esac

if ! [[ "$PORT" =~ ^[0-9]+$ ]] || (( 10#$PORT < 1 || 10#$PORT > 65535 )); then
  echo "port must be a UDP port in the range 1-65535" >&2
  exit 2
fi

case "$PUSH_DNS" in
  8.8.8.8|8.8.4.4) ;;
  *)
    echo "push DNS must be Google DNS 8.8.8.8 or 8.8.4.4" >&2
    exit 2
    ;;
esac

if ! [[ "$CERT_DAYS" =~ ^[0-9]+$ ]] || (( 10#$CERT_DAYS < 1 )); then
  echo "VPNKIT_LOCAL_CERT_DAYS must be a positive integer" >&2
  exit 2
fi

command -v openssl >/dev/null 2>&1 || { echo "missing required command: openssl" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "missing required command: python3" >&2; exit 1; }

PKI="$BASE/openvpn/pki"
SERVER_DIR="$BASE/openvpn/server"
CLIENT_DIR="$BASE/openvpn/client"
RENDERED_OPENVPN="$BASE/rendered/openvpn"
RENDERED_PKI="$RENDERED_OPENVPN/pki"
SUBSCRIPTION_DIR="$BASE/vibe-vpn"
SERVER_CONFIG="$SERVER_DIR/server.conf"
RENDERED_SERVER_CONFIG="$RENDERED_OPENVPN/server.conf"
PROFILE="$CLIENT_DIR/vpnkit-local.ovpn"

# Recheck the complete secret tree immediately before mkdir and again before
# chmod. This rejects an existing symlinked child such as openvpn/ without
# touching its target.
vpnkit_local_path_guard_validate_secret_tree "$BASE" || exit 20
mkdir -p "$PKI" "$SERVER_DIR" "$CLIENT_DIR" "$RENDERED_PKI" "$SUBSCRIPTION_DIR"
vpnkit_local_path_guard_validate_secret_tree "$BASE" || exit 20
chmod 700 "$BASE" "$BASE/openvpn" "$PKI" "$SERVER_DIR" "$CLIENT_DIR" \
  "$BASE/rendered" "$RENDERED_OPENVPN" "$RENDERED_PKI" "$SUBSCRIPTION_DIR"

PKI_FILES=(
  "$PKI/ca.crt"
  "$PKI/ca.key"
  "$PKI/server.crt"
  "$PKI/server.key"
  "$PKI/client.crt"
  "$PKI/client.key"
  "$PKI/ta.key"
)

present=0
missing=0
for path in "${PKI_FILES[@]}"; do
  if [[ -e "$path" ]]; then
    present=1
  else
    missing=1
  fi
done
if (( present && missing )); then
  echo "local OpenVPN PKI is incomplete; refusing to rotate or repair existing identities" >&2
  exit 3
fi

WORK=$(mktemp -d "$BASE/.vpnkit-local-assets.XXXXXX")
chmod 700 "$WORK"
cleanup() {
  rm -rf -- "$WORK"
}
trap cleanup EXIT HUP INT TERM

install_private() {
  # Validate the destination tree for every private-file write, not only at
  # process startup, so a redirected destination fails closed before install.
  vpnkit_local_path_guard_validate_secret_tree "$BASE" || return 1
  install -m 0600 -- "$1" "$2"
}

make_ca() {
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 \
    -days "$CERT_DAYS" \
    -subj '/CN=vpnkit-local-ca' \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:1' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$WORK/ca.key" -out "$WORK/ca.crt" >/dev/null 2>&1
}

make_cert() {
  local name=$1 common_name=$2 extended_usage=$3
  local extfile="$WORK/$name.ext"
  printf '%s\n' \
    'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyEncipherment' \
    "extendedKeyUsage=critical,$extended_usage" >"$extfile"
  chmod 600 "$extfile"
  openssl req -newkey rsa:2048 -nodes \
    -subj "/CN=$common_name" \
    -keyout "$WORK/$name.key" -out "$WORK/$name.csr" >/dev/null 2>&1
  openssl x509 -req -sha256 -days "$CERT_DAYS" \
    -in "$WORK/$name.csr" \
    -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" \
    -CAserial "$WORK/ca.srl" -CAcreateserial \
    -extfile "$extfile" -out "$WORK/$name.crt" >/dev/null 2>&1
}

make_tls_auth_key() {
  if command -v openvpn >/dev/null 2>&1; then
    if openvpn --genkey secret "$WORK/ta.key" >/dev/null 2>&1; then
      return 0
    fi
    if openvpn --genkey --secret "$WORK/ta.key" >/dev/null 2>&1; then
      return 0
    fi
  fi
  {
    printf '%s\n' '#' '# 2048 bit OpenVPN static key' '#'
    printf '%s\n' '-----BEGIN OpenVPN Static key V1-----'
    openssl rand -hex 256 | fold -w 32
    printf '%s\n' '-----END OpenVPN Static key V1-----'
  } >"$WORK/ta.key"
  chmod 600 "$WORK/ta.key"
}

if (( ! present )); then
  make_ca
  make_cert server vpnkit-local-server serverAuth
  make_cert client vpnkit-local clientAuth
  make_tls_auth_key
  for name in ca.crt ca.key server.crt server.key client.crt client.key ta.key; do
    install_private "$WORK/$name" "$PKI/$name"
  done
fi
vpnkit_local_path_guard_validate_secret_tree "$BASE" || exit 20
chmod 600 "${PKI_FILES[@]}"

python3 - "$REPO_ROOT/config/openvpn/local-server.tpl" "$WORK/server.conf" "$PUSH_DNS" <<'PY'
import pathlib
import sys

template = pathlib.Path(sys.argv[1])
output = pathlib.Path(sys.argv[2])
dns = sys.argv[3]
text = template.read_text(encoding="utf-8")
text = text.replace("{{OPENVPN_PUSH_DNS}}", dns)
if "{{OPENVPN_PUSH_DNS}}" in text:
    raise SystemExit("local server template contains an unresolved DNS placeholder")
output.write_text(text, encoding="utf-8")
PY
install_private "$WORK/server.conf" "$SERVER_CONFIG"
install_private "$WORK/server.conf" "$RENDERED_SERVER_CONFIG"

python3 - "$REPO_ROOT/config/openvpn/local-client.ovpn.template" "$WORK/vpnkit-local.ovpn" "$ENDPOINT" "$PORT" "$PKI" <<'PY'
import pathlib
import sys

template_path, output_path, endpoint, port, pki_path = sys.argv[1:]
pki = pathlib.Path(pki_path)
values = {
    "{{LOCAL_REMOTE}}": f"[{endpoint}]" if ":" in endpoint else endpoint,
    "{{LOCAL_PORT}}": port,
    "{{CA_CRT}}": (pki / "ca.crt").read_text(encoding="utf-8").strip(),
    "{{CLIENT_CRT}}": (pki / "client.crt").read_text(encoding="utf-8").strip(),
    "{{CLIENT_KEY}}": (pki / "client.key").read_text(encoding="utf-8").strip(),
    "{{TA_KEY}}": (pki / "ta.key").read_text(encoding="utf-8").strip(),
}
text = pathlib.Path(template_path).read_text(encoding="utf-8")
for marker, value in values.items():
    text = text.replace(marker, value)
if "{{" in text or "}}" in text:
    raise SystemExit("local client template contains an unresolved placeholder")
pathlib.Path(output_path).write_text(text.rstrip() + "\n", encoding="utf-8")
PY
install_private "$WORK/vpnkit-local.ovpn" "$PROFILE"

for name in ca.crt server.crt server.key ta.key; do
  install_private "$PKI/$name" "$RENDERED_PKI/$name"
done

printf 'vpnkit_local_assets=ok\n'
printf 'secrets_dir=%s\n' "$BASE"
printf 'server_config=%s\n' "$SERVER_CONFIG"
printf 'rendered_server_config=%s\n' "$RENDERED_SERVER_CONFIG"
printf 'client_profile=%s\n' "$PROFILE"
printf 'endpoint=%s\n' "$ENDPOINT"
printf 'port=%s\n' "$PORT"
printf 'permissions=directories_700_files_600\n'
printf 'secret_material=not_printed\n'
printf 'subscription_input=not_read\n'
printf 'networkmanager_import=not_run\n'
