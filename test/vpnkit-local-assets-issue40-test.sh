#!/usr/bin/env bash
# Fixture/static coverage for issue #40's local-only PKI/profile asset slice.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
script="$repo_root/scripts/vpnkit/vpnkit-local-assets.sh"
path_guard="$repo_root/scripts/vpnkit/vpnkit-local-path-guard.sh"
server_template="$repo_root/config/openvpn/local-server.tpl"
client_template="$repo_root/config/openvpn/local-client.ovpn.template"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-assets-issue40.XXXXXX")
unsafe_tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-assets-unmarked.XXXXXX")
trap 'rm -rf -- "$tmp" "$unsafe_tmp"' EXIT

fail() {
  echo "vpnkit-local-assets-test: $*" >&2
  exit 1
}
require_file() { [[ -f "$1" ]] || fail "missing file: $1"; }
require_mode() {
  local expected=$1 path=$2 actual
  actual=$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path")
  [[ "$actual" == "$expected" ]] || fail "unexpected mode $actual for $path (expected $expected)"
}
require_contains() { grep -Fq -- "$1" "$2" || fail "missing expected fixture text in $2"; }
require_absent() { ! grep -Fq -- "$1" "$2" || fail "unexpected text in $2"; }

[[ -x "$script" ]] || fail "asset generator is not executable"
[[ -r "$path_guard" ]] || fail "shared local path guard is missing"
bash -n "$script" "$path_guard" || fail "asset/path-guard shell syntax is invalid"
require_contains 'vpnkit_local_path_guard_validate_secret_root' "$script"
require_contains 'vpnkit_local_path_guard_validate_secret_tree' "$script"
require_file "$server_template"
require_file "$client_template"

# Static safety: the renderer must not grow live host/container mutation paths.
if grep -Eq '(^|[^[:alnum:]_])(docker|nmcli|sudo)([^[:alnum:]_]|$)' "$script"; then
  fail "asset generator contains a live Docker/NetworkManager/privileged command"
fi
require_contains 'tun-mtu 1400' "$server_template"
require_contains 'mssfix 1360' "$server_template"
require_contains 'push "redirect-gateway def1 bypass-dhcp"' "$server_template"
require_contains 'push "dhcp-option DNS {{OPENVPN_PUSH_DNS}}"' "$server_template"
require_contains 'tun-mtu 1400' "$client_template"
require_contains 'mssfix 1360' "$client_template"
require_absent 'redirect-gateway def1 bypass-dhcp' "$client_template"
require_contains 'block-ipv6' "$client_template"
require_absent '1.1.1.1' "$server_template"
require_absent '1.1.1.1' "$client_template"

base="$tmp/secrets/vpnkit-local"
output=$(VPNKIT_LOCAL_SECRETS_DIR="$base" \
  VPNKIT_LOCAL_ENDPOINT=127.0.0.1 \
  VPNKIT_LOCAL_OPENVPN_PORT=21194 \
  VPNKIT_LOCAL_OPENVPN_PUSH_DNS=8.8.8.8 \
  "$script")

# Metadata is allowed; generated secret/profile material is not.
require_contains 'vpnkit_local_assets=ok' <(printf '%s\n' "$output")
require_contains 'secret_material=not_printed' <(printf '%s\n' "$output")
require_contains 'subscription_input=not_read' <(printf '%s\n' "$output")
require_absent 'BEGIN ' <(printf '%s\n' "$output")
require_absent 'PRIVATE KEY' <(printf '%s\n' "$output")

for dir in \
  "$base" \
  "$base/openvpn" \
  "$base/openvpn/pki" \
  "$base/openvpn/server" \
  "$base/openvpn/client" \
  "$base/rendered" \
  "$base/rendered/openvpn" \
  "$base/rendered/openvpn/pki" \
  "$base/vibe-vpn"; do
  require_mode 700 "$dir"
done

for file in \
  ca.crt ca.key server.crt server.key client.crt client.key ta.key; do
  require_file "$base/openvpn/pki/$file"
  require_mode 600 "$base/openvpn/pki/$file"
done
for file in \
  "$base/openvpn/server/server.conf" \
  "$base/rendered/openvpn/server.conf" \
  "$base/openvpn/client/vpnkit-local.ovpn"; do
  require_file "$file"
  require_mode 600 "$file"
done

for file in ca.crt server.crt server.key ta.key; do
  require_file "$base/rendered/openvpn/pki/$file"
  require_mode 600 "$base/rendered/openvpn/pki/$file"
done

# The generated profile is inline and importable without reading external secret files.
profile="$base/openvpn/client/vpnkit-local.ovpn"
server_conf="$base/openvpn/server/server.conf"
require_contains 'remote 127.0.0.1 21194' "$profile"
require_contains 'proto udp' "$profile"
require_contains 'tun-mtu 1400' "$profile"
require_contains 'mssfix 1360' "$profile"
require_absent 'redirect-gateway def1 bypass-dhcp' "$profile"
require_contains 'block-ipv6' "$profile"
require_contains '<ca>' "$profile"
require_contains '<cert>' "$profile"
require_contains '<key>' "$profile"
require_contains '<tls-auth>' "$profile"
require_absent '{{' "$profile"
require_absent '}}' "$profile"
require_contains 'server 10.89.0.0 255.255.255.0' "$server_conf"
require_contains 'tun-mtu 1400' "$server_conf"
require_contains 'mssfix 1360' "$server_conf"
require_contains 'push "redirect-gateway def1 bypass-dhcp"' "$server_conf"
require_contains 'push "dhcp-option DNS 8.8.8.8"' "$server_conf"
require_absent '1.1.1.1' "$server_conf"
require_absent '1.1.1.1' "$profile"

openssl verify -CAfile "$base/openvpn/pki/ca.crt" \
  "$base/openvpn/pki/server.crt" "$base/openvpn/pki/client.crt" >/dev/null 2>&1 ||
  fail "generated certificates did not verify against the local CA"

ca_hash_before=$(sha256sum "$base/openvpn/pki/ca.key" | cut -d' ' -f1)
server_hash_before=$(sha256sum "$base/openvpn/pki/server.key" | cut -d' ' -f1)
VPNKIT_LOCAL_SECRETS_DIR="$base" \
  VPNKIT_LOCAL_ENDPOINT=127.0.0.1 \
  VPNKIT_LOCAL_OPENVPN_PORT=21194 \
  VPNKIT_LOCAL_OPENVPN_PUSH_DNS=8.8.8.8 \
  "$script" >/dev/null
[[ "$ca_hash_before" == "$(sha256sum "$base/openvpn/pki/ca.key" | cut -d' ' -f1)" ]] ||
  fail "rerender rotated the persistent CA key"
[[ "$server_hash_before" == "$(sha256sum "$base/openvpn/pki/server.key" | cut -d' ' -f1)" ]] ||
  fail "rerender rotated the persistent server key"

# Configuration is local-only and bounded to the Google DNS policy.
if VPNKIT_LOCAL_SECRETS_DIR="$tmp/rejected" VPNKIT_LOCAL_ENDPOINT=203.0.113.7 "$script" >/dev/null 2>&1; then
  fail "non-local endpoint was accepted"
fi
if VPNKIT_LOCAL_SECRETS_DIR="$tmp/rejected-dns" VPNKIT_LOCAL_OPENVPN_PUSH_DNS=1.1.1.1 "$script" >/dev/null 2>&1; then
  fail "non-Google pushed DNS was accepted"
fi

# Direct helper root-boundary checks are disposable and must fail before the
# first mkdir/chmod/write. Unprefixed temporary roots require the explicit
# fixture capability; a named vpnkit-local-* root remains lifecycle-compatible.
unsafe_base="$unsafe_tmp/existing-secret-root"
mkdir -p "$unsafe_base"
chmod 755 "$unsafe_base"
unsafe_before=$(find "$unsafe_tmp" -mindepth 1 -maxdepth 2 -printf '%P:%m:%y\n' | sort)
if env -u VPNKIT_LOCAL_TEST_FIXTURE VPNKIT_LOCAL_SECRETS_DIR="$unsafe_base" "$script" >/dev/null 2>&1; then
  fail "arbitrary temporary secret root was accepted without test capability"
fi
unsafe_after=$(find "$unsafe_tmp" -mindepth 1 -maxdepth 2 -printf '%P:%m:%y\n' | sort)
[[ "$unsafe_before" == "$unsafe_after" ]] || fail "arbitrary-root rejection mutated its parent"

marked_base="$unsafe_tmp/marked/vpnkit-local"
VPNKIT_LOCAL_TEST_FIXTURE=1 VPNKIT_LOCAL_SECRETS_DIR="$marked_base" \
  VPNKIT_LOCAL_ENDPOINT=127.0.0.1 VPNKIT_LOCAL_OPENVPN_PORT=21196 \
  VPNKIT_LOCAL_OPENVPN_PUSH_DNS=8.8.8.8 "$script" >/dev/null
require_file "$marked_base/openvpn/client/vpnkit-local.ovpn"

production_base="$tmp/vpnkit-local-production"
if VPNKIT_LOCAL_SECRETS_DIR="$production_base" "$script" >/dev/null 2>&1; then
  fail "production-like secret root was accepted"
fi
[[ ! -e "$production_base" ]] || fail "production-like rejection created a root"

# A symlinked root component is rejected even when the symlink target would
# otherwise be an allowed disposable directory. Check both contents and mode
# so the test proves the helper did not follow it for mkdir/chmod/write.
root_link_target="$tmp/root-link-target"
root_link="$tmp/root-link"
mkdir -p "$root_link_target"
ln -s "$root_link_target" "$root_link"
root_target_before=$(stat -c '%a %F' "$root_link_target")
if VPNKIT_LOCAL_SECRETS_DIR="$root_link/vpnkit-local" "$script" >/dev/null 2>&1; then
  fail "symlinked secret root was accepted"
fi
[[ "$(stat -c '%a %F' "$root_link_target")" == "$root_target_before" ]] ||
  fail "symlink-root rejection mutated the target"
[[ ! -e "$root_link_target/openvpn" ]] || fail "symlink-root rejection created target content"

child_root="$tmp/child-link-root"
child_link_target="$tmp/child-link-target"
mkdir -p "$child_root" "$child_link_target"
chmod 755 "$child_link_target"
ln -s "$child_link_target" "$child_root/openvpn"
child_target_before=$(stat -c '%a %F' "$child_link_target")
if VPNKIT_LOCAL_SECRETS_DIR="$child_root" "$script" >/dev/null 2>&1; then
  fail "symlinked secret subtree was accepted"
fi
[[ "$(stat -c '%a %F' "$child_link_target")" == "$child_target_before" ]] ||
  fail "symlink-subtree rejection chmodded its target"
[[ ! -e "$child_link_target/pki" ]] || fail "symlink-subtree rejection created target content"

printf 'vpnkit-local-assets issue-40 fixture/static tests passed\n'
