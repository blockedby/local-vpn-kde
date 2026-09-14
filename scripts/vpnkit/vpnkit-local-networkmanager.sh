#!/usr/bin/env bash
# Bounded NetworkManager adapter for the local vpnkit OpenVPN profile.
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd -P)
PATH_GUARD="$SCRIPT_DIR/vpnkit-local-path-guard.sh"
[[ -r "$PATH_GUARD" ]] || { echo 'missing local secret path guard' >&2; exit 1; }
# shellcheck source=/dev/null
. "$PATH_GUARD"

BASE=${VPNKIT_LOCAL_SECRETS_DIR:-$REPO_ROOT/secrets/vpnkit-local}
vpnkit_local_path_guard_validate_secret_root "$BASE" "$REPO_ROOT" || exit 20
BASE=$VPNKIT_LOCAL_PATH_GUARD_BASE
vpnkit_local_path_guard_validate_secret_tree "$BASE" || exit 20

EXPECTED_PROFILE="$BASE/openvpn/client/vpnkit-local.ovpn"
PROFILE=${VPNKIT_LOCAL_PROFILE:-$EXPECTED_PROFILE}
case "$PROFILE" in /*) ;; *) PROFILE="$REPO_ROOT/$PROFILE" ;; esac
PROFILE=$(realpath -m -- "$PROFILE") || { echo 'local NetworkManager profile path could not be canonicalized' >&2; exit 20; }
[[ "$PROFILE" == "$EXPECTED_PROFILE" ]] || { echo 'VPNKIT_LOCAL_PROFILE must remain the guarded vpnkit-local profile' >&2; exit 20; }
CONNECTION=${VPNKIT_LOCAL_NM_CONNECTION:-vpnkit-local}
STATE_DIR="$BASE/state"
# The single state file is the ownership capability.  The two legacy files are
# retained as a compatibility mirror for older local installations, but are
# never used as independent capabilities once this file exists.
STATE_FILE="$STATE_DIR/networkmanager-state"
OWNED_UUID_FILE="$STATE_DIR/networkmanager-uuid"
PROFILE_FINGERPRINT_FILE="$STATE_DIR/networkmanager-profile-fingerprint"
OPENVPN_SERVICE_TYPE=org.freedesktop.NetworkManager.openvpn
UUID_PATTERN='^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'
# Linux interface names are at most IFNAMSIZ-1 bytes.  Restrict the value
# to a tunnel-shaped, shell-safe name while allowing names such as tun-local
# or tun0.100 used by some NetworkManager setups.
DEVICE_PATTERN='^tun[A-Za-z0-9_.-]{0,14}$'
# NetworkManager can report a VPN's base/transport device while the VPN
# address is still being installed.  Keep activation readiness bounded by the
# helper rather than inheriting nmcli's long default wait.
NM_CONNECT_TIMEOUT_SECONDS=${VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS:-30}
YES=0
ACTION=${1:-plan}
[[ $# -gt 0 ]] && shift
while [[ $# -gt 0 ]]; do
  case "$1" in --yes) YES=1; shift ;; -h|--help) ACTION=help; shift ;; *) echo 'unknown option' >&2; exit 2 ;; esac
done

case "$CONNECTION" in vpnkit-local) ;; *) echo 'connection name must be vpnkit-local' >&2; exit 2 ;; esac
need_nm() { command -v nmcli >/dev/null 2>&1 || { echo 'nmcli is unavailable' >&2; exit 10; }; }
confirm() { (( YES == 1 )) || { echo 'mutation requires --yes' >&2; exit 3; }; }

# The state is an ownership capability.  A same-named NetworkManager profile is
# never an ownership proof; only this UUID, persisted below the ignored BASE,
# may authorize a mutation.
STATE_UUID=
STATE_FINGERPRINT=
STATE_SOURCE=
STATE_ERROR=
OWNERSHIP=
NAMED_UUIDS=()
NM_PROFILE_NAME=
NM_PROFILE_UUID=
NM_PROFILE_TYPE=
NM_PROFILE_SERVICE=
NM_PROFILE_DATA=
ACTIVE_DEVICE=

uuid_is_valid() { [[ "$1" =~ $UUID_PATTERN ]]; }
normalize_uuid() { printf '%s\n' "${1,,}"; }

# State capabilities are never read from or replaced through links.  A
# hardlink would let another directory entry continue to expose or mutate the
# capability, while a symlink could redirect it outside the private state
# directory.  Fail closed when the platform cannot report a link count.
single_link_regular_file() {
  local path=$1 links
  [[ -f "$path" && ! -L "$path" ]] || return 1
  links=$(stat -c '%h' -- "$path" 2>/dev/null) || return 1
  [[ "$links" == 1 ]]
}

state_path_is_safe_or_absent() {
  local path=$1
  if [[ -e "$path" || -L "$path" ]]; then
    single_link_regular_file "$path"
  else
    return 0
  fi
}

state_dir_is_safe() {
  [[ -d "$BASE" && ! -L "$BASE" ]] || return 1
  [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || return 1
}

state_dir_for_write() {
  [[ -d "$BASE" && ! -L "$BASE" ]] || return 1
  if [[ -e "$STATE_DIR" || -L "$STATE_DIR" ]]; then
    [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || return 1
  else
    mkdir -- "$STATE_DIR" || return 1
  fi
  [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || return 1
  chmod 700 "$STATE_DIR" || return 1
}

sync_state_path() {
  local path=$1
  # Atomic rename gives the state a single visible version; fsync the file and
  # containing directory as well so a successful commit is not merely a
  # userspace write.  The fallback is for minimal test/lab images.
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

path = sys.argv[1]
flags = os.O_RDONLY
if hasattr(os, "O_DIRECTORY") and os.path.isdir(path):
    flags |= os.O_DIRECTORY
fd = os.open(path, flags)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
  elif command -v sync >/dev/null 2>&1; then
    sync -d -- "$path" 2>/dev/null || sync
  else
    return 1
  fi
}

parse_state_payload() {
  local payload=$1 uuid fingerprint extra
  # A state payload is deliberately one line: UUID followed by the profile
  # fingerprint.  No shell evaluation or profile data is involved.
  [[ "$payload" != *$'\n'* && "$payload" != *$'\r'* ]] || return 1
  read -r uuid fingerprint extra <<<"$payload"
  [[ -n "$uuid" && -n "$fingerprint" && -z "$extra" ]] || return 1
  uuid_is_valid "$uuid" || return 1
  [[ "$fingerprint" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
  STATE_UUID=${uuid,,}
  STATE_FINGERPRINT=${fingerprint,,}
}

read_state() {
  local state_present=0 uuid_present=0 fingerprint_present=0 payload uuid fingerprint
  STATE_UUID=
  STATE_FINGERPRINT=
  STATE_SOURCE=
  STATE_ERROR=
  [[ -e "$STATE_FILE" || -L "$STATE_FILE" ]] && state_present=1
  [[ -e "$OWNED_UUID_FILE" || -L "$OWNED_UUID_FILE" ]] && uuid_present=1
  [[ -e "$PROFILE_FINGERPRINT_FILE" || -L "$PROFILE_FINGERPRINT_FILE" ]] && fingerprint_present=1

  # Do not follow a redirected state directory while inspecting a capability.
  # A missing state directory is the normal unconfigured case; an existing
  # one must be a real directory even when it currently has no state files.
  if [[ -e "$STATE_DIR" || -L "$STATE_DIR" ]]; then
    state_dir_is_safe || return 2
  fi

  if (( state_present )); then
    single_link_regular_file "$STATE_FILE" || return 2
    payload=$(<"$STATE_FILE") || return 2
    parse_state_payload "$payload" || return 2
    STATE_SOURCE=atomic
    return 0
  fi

  # Read the pre-transaction pair only when the atomic capability has not been
  # created yet.  Migration is deliberately deferred until the UUID has been
  # proved to own a local NM profile and the fingerprint has been checked.
  if (( ! uuid_present && ! fingerprint_present )); then
    return 0
  fi
  (( uuid_present && fingerprint_present )) || return 2
  single_link_regular_file "$OWNED_UUID_FILE" || return 2
  single_link_regular_file "$PROFILE_FINGERPRINT_FILE" || return 2
  uuid=$(<"$OWNED_UUID_FILE") || return 2
  fingerprint=$(<"$PROFILE_FINGERPRINT_FILE") || return 2
  uuid=${uuid//$'\r'/}
  fingerprint=${fingerprint//$'\r'/}
  uuid_is_valid "$uuid" || return 2
  [[ "$fingerprint" =~ ^[0-9A-Fa-f]{64}$ ]] || return 2
  STATE_UUID=${uuid,,}
  STATE_FINGERPRINT=${fingerprint,,}
  STATE_SOURCE=legacy
}

rollback_migrated_state() {
  local expected=$1 payload
  single_link_regular_file "$STATE_FILE" || return 1
  payload=$(<"$STATE_FILE") || return 1
  [[ "$payload" == "$expected" ]] || return 1
  rm -f -- "$STATE_FILE" || return 1
  sync_state_path "$STATE_DIR" >/dev/null 2>&1 || return 1
}

migrate_legacy_state() {
  local uuid=${1,,} fingerprint=${2,,} state_tmp= payload expected
  uuid_is_valid "$uuid" || return 1
  [[ "$fingerprint" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
  state_dir_for_write || return 1
  single_link_regular_file "$OWNED_UUID_FILE" || return 1
  single_link_regular_file "$PROFILE_FINGERPRINT_FILE" || return 1
  [[ ! -e "$STATE_FILE" && ! -L "$STATE_FILE" ]] || return 1

  # Only the new authoritative file is changed during this migration.  The
  # legacy pair is left byte-for-byte intact so every failure path can retry
  # without losing the previous ownership capability.
  state_tmp=$(mktemp "$STATE_DIR/.networkmanager-state.XXXXXX") || return 1
  chmod 600 "$state_tmp" || { rm -f -- "$state_tmp"; return 1; }
  printf '%s %s\n' "$uuid" "$fingerprint" >"$state_tmp" || {
    rm -f -- "$state_tmp"
    return 1
  }
  single_link_regular_file "$state_tmp" || {
    rm -f -- "$state_tmp"
    return 1
  }
  sync_state_path "$state_tmp" >/dev/null 2>&1 || {
    rm -f -- "$state_tmp"
    return 1
  }

  # The precondition prevents an ordinary collision.  rename/mv itself is the
  # atomic visibility boundary; the post-commit checks fail closed if a path
  # was redirected or replaced while the helper was running.
  [[ ! -e "$STATE_FILE" && ! -L "$STATE_FILE" ]] || {
    rm -f -- "$state_tmp"
    return 1
  }
  if ! mv -fT -- "$state_tmp" "$STATE_FILE"; then
    rm -f -- "$state_tmp"
    return 1
  fi
  state_tmp=
  expected="$uuid $fingerprint"
  if ! single_link_regular_file "$STATE_FILE"; then
    return 1
  fi
  payload=$(<"$STATE_FILE") || return 1
  if [[ "$payload" != "$expected" ]]; then
    rollback_migrated_state "$expected" >/dev/null 2>&1 || true
    return 1
  fi
  if ! sync_state_path "$STATE_DIR" >/dev/null 2>&1; then
    rollback_migrated_state "$expected" >/dev/null 2>&1 || true
    return 1
  fi
  STATE_UUID=$uuid
  STATE_FINGERPRINT=$fingerprint
  STATE_SOURCE=atomic
}

restore_state_file() {
  local path=$1 present=$2 content=$3 tmp
  state_path_is_safe_or_absent "$path" || return 1
  if [[ "$present" == yes ]]; then
    tmp=$(mktemp "$STATE_DIR/.networkmanager-restore.XXXXXX") || return 1
    chmod 600 "$tmp" || { rm -f -- "$tmp"; return 1; }
    printf '%s\n' "$content" >"$tmp" || { rm -f -- "$tmp"; return 1; }
    sync_state_path "$tmp" >/dev/null 2>&1 || { rm -f -- "$tmp"; return 1; }
    mv -fT -- "$tmp" "$path" || { rm -f -- "$tmp"; return 1; }
  elif [[ -e "$path" || -L "$path" ]]; then
    rm -f -- "$path" || return 1
  fi
}

write_state() {
  local uuid=${1,,} fingerprint=${2,,}
  local state_present=no uuid_present=no fingerprint_present=no
  local old_state= old_uuid= old_fingerprint=
  local state_tmp uuid_tmp fingerprint_tmp
  uuid_is_valid "$uuid" || return 1
  [[ "$fingerprint" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
  state_dir_for_write || return 1
  state_path_is_safe_or_absent "$STATE_FILE" || return 1
  state_path_is_safe_or_absent "$OWNED_UUID_FILE" || return 1
  state_path_is_safe_or_absent "$PROFILE_FINGERPRINT_FILE" || return 1

  if [[ -e "$STATE_FILE" ]]; then
    state_present=yes
    old_state=$(<"$STATE_FILE") || return 1
  fi
  if [[ -e "$OWNED_UUID_FILE" ]]; then
    uuid_present=yes
    old_uuid=$(<"$OWNED_UUID_FILE") || return 1
  fi
  if [[ -e "$PROFILE_FINGERPRINT_FILE" ]]; then
    fingerprint_present=yes
    old_fingerprint=$(<"$PROFILE_FINGERPRINT_FILE") || return 1
  fi

  # Stage every representation before changing any visible state.  The
  # atomic file is authoritative; the legacy pair is only a compatibility
  # mirror for installations upgraded from the original helper.
  state_tmp=$(mktemp "$STATE_DIR/.networkmanager-state.XXXXXX") || return 1
  uuid_tmp=$(mktemp "$STATE_DIR/.networkmanager-uuid.XXXXXX") || {
    rm -f -- "$state_tmp"
    return 1
  }
  fingerprint_tmp=$(mktemp "$STATE_DIR/.networkmanager-fingerprint.XXXXXX") || {
    rm -f -- "$state_tmp" "$uuid_tmp"
    return 1
  }
  chmod 600 "$state_tmp" "$uuid_tmp" "$fingerprint_tmp" || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  printf '%s %s\n' "$uuid" "$fingerprint" >"$state_tmp" || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  printf '%s\n' "$uuid" >"$uuid_tmp" || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  printf '%s\n' "$fingerprint" >"$fingerprint_tmp" || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  sync_state_path "$state_tmp" >/dev/null 2>&1 || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  sync_state_path "$uuid_tmp" >/dev/null 2>&1 || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }
  sync_state_path "$fingerprint_tmp" >/dev/null 2>&1 || {
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  }

  if ! mv -fT -- "$state_tmp" "$STATE_FILE"; then
    rm -f -- "$state_tmp" "$uuid_tmp" "$fingerprint_tmp"
    return 1
  fi
  if ! mv -fT -- "$uuid_tmp" "$OWNED_UUID_FILE" || ! mv -fT -- "$fingerprint_tmp" "$PROFILE_FINGERPRINT_FILE"; then
    rm -f -- "$uuid_tmp" "$fingerprint_tmp"
    restore_state_file "$STATE_FILE" "$state_present" "$old_state" || true
    restore_state_file "$OWNED_UUID_FILE" "$uuid_present" "$old_uuid" || true
    restore_state_file "$PROFILE_FINGERPRINT_FILE" "$fingerprint_present" "$old_fingerprint" || true
    sync_state_path "$STATE_DIR" >/dev/null 2>&1 || true
    return 1
  fi
  sync_state_path "$STATE_DIR" >/dev/null 2>&1 || {
    restore_state_file "$STATE_FILE" "$state_present" "$old_state" || true
    restore_state_file "$OWNED_UUID_FILE" "$uuid_present" "$old_uuid" || true
    restore_state_file "$PROFILE_FINGERPRINT_FILE" "$fingerprint_present" "$old_fingerprint" || true
    sync_state_path "$STATE_DIR" >/dev/null 2>&1 || true
    return 1
  }
  # Keep the shell variable state aligned with the committed capability.
  STATE_UUID=$uuid
  STATE_FINGERPRINT=$fingerprint
  STATE_SOURCE=atomic
}

clear_state() {
  if [[ ! -e "$STATE_DIR" && ! -L "$STATE_DIR" ]]; then
    return 0
  fi
  state_dir_is_safe || return 1
  # Validate every target before removing any of them.  The capability must
  # not be redirected through a symlink or shared through a hardlink.
  state_path_is_safe_or_absent "$STATE_FILE" || return 1
  state_path_is_safe_or_absent "$OWNED_UUID_FILE" || return 1
  state_path_is_safe_or_absent "$PROFILE_FINGERPRINT_FILE" || return 1
  # Remove the mirrors first.  If the final atomic unlink fails, the
  # capability remains readable and can be retried rather than disappearing
  # partially.
  rm -f -- "$OWNED_UUID_FILE" "$PROFILE_FINGERPRINT_FILE" || return 1
  rm -f -- "$STATE_FILE" || return 1
  sync_state_path "$STATE_DIR" >/dev/null 2>&1 || return 1
}

profile_source_is_local() {
  [[ -f "$PROFILE" && ! -L "$PROFILE" && -s "$PROFILE" ]] || return 1
  # Inspect only structural OpenVPN directives.  Inline certificates and keys
  # are never printed or copied by this adapter.
  awk '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    $1 == "client" && NF == 1 { client = 1; next }
    $1 == "dev" && ($2 == "tun" || $2 == "tun0") { dev = 1; next }
    $1 == "proto" && ($2 == "udp" || $2 == "udp4") { proto = 1; next }
    $1 == "remote" {
      remotes++
      host = $2
      gsub(/^\[/, "", host)
      gsub(/\]$/, "", host)
      if (host != "127.0.0.1" && host != "localhost") { bad = 1 }
      if (NF < 2 || NF > 3) { bad = 1 }
      if (NF == 3 && ($3 !~ /^[0-9]+$/ || $3 < 1 || $3 > 65535)) { bad = 1 }
      next
    }
    $1 == "remote-random" { bad = 1; next }
    END { exit !(client && dev && proto && remotes == 1 && !bad) }
  ' "$PROFILE" >/dev/null 2>&1
}

profile_fingerprint() {
  local digest
  if command -v sha256sum >/dev/null 2>&1; then
    digest=$(sha256sum -- "$PROFILE" | awk 'NR == 1 { print $1 }') || return 1
  elif command -v shasum >/dev/null 2>&1; then
    digest=$(shasum -a 256 -- "$PROFILE" | awk 'NR == 1 { print $1 }') || return 1
  else
    return 1
  fi
  [[ "$digest" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
  printf '%s\n' "${digest,,}"
}

connection_rows() {
  LC_ALL=C nmcli -t -f NAME,UUID,TYPE connection show 2>/dev/null
}
active_connection_rows() {
  # DEVICE is retained as diagnostic context only.  For a VPN it may be the
  # base/transport interface rather than the tunnel created by the VPN
  # plugin, so it is never used as the owned tunnel proof.
  LC_ALL=C nmcli -t -f NAME,UUID,TYPE,DEVICE connection show --active 2>/dev/null
}

# nmcli terse output is NAME:UUID:TYPE.  The canonical name contains no
# escaped delimiters, while the UUID is independently validated before use.
load_named_uuids() {
  local rows line name uuid type rest
  rows=$(connection_rows) || return 1
  NAMED_UUIDS=()
  while IFS= read -r line; do
    line=${line%$'\r'}
    [[ -n "$line" ]] || continue
    IFS=: read -r name uuid type rest <<<"$line"
    [[ "$name" == "$CONNECTION" ]] || continue
    if uuid_is_valid "$uuid"; then
      NAMED_UUIDS+=("${uuid,,}")
    else
      NAMED_UUIDS+=("invalid")
    fi
  done <<<"$rows"
}

safe_device_name() {
  local device=$1
  [[ "$device" =~ $DEVICE_PATTERN ]] || return 1
  (( ${#device} <= 15 )) || return 1
  [[ "$device" != ppp0 && "$device" != vpn0 ]]
}

# Prove that the exact persisted UUID is active without trusting its reported
# DEVICE.  A work VPN or another profile may use the same transport device,
# but it cannot satisfy the UUID match below.
active_connection_for_uuid() {
  local wanted=${1,,} rows line name uuid type device rest found=0
  uuid_is_valid "$wanted" || return 2
  rows=$(active_connection_rows) || return 2
  while IFS= read -r line; do
    line=${line%$'\r'}
    [[ -n "$line" ]] || continue
    IFS=: read -r name uuid type device rest <<<"$line"
    uuid_is_valid "$uuid" || continue
    [[ "${uuid,,}" == "$wanted" ]] || continue
    [[ "$type" == vpn ]] || return 2
    (( found == 0 )) || return 2
    found=1
  done <<<"$rows"
  (( found == 1 )) || return 1
}

ipv4_address_is_valid() {
  local value=$1 old_ifs=$IFS octet
  local -a octets=()
  IFS=.
  read -r -a octets <<<"$value"
  IFS=$old_ifs
  [[ ${#octets[@]} -eq 4 ]] || return 1
  for octet in "${octets[@]}"; do
    [[ "$octet" =~ ^[0-9]{1,3}$ ]] || return 1
    (( 10#$octet <= 255 )) || return 1
  done
}

ipv4_cidr_is_valid() {
  local value=$1 address prefix
  [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$ ]] || return 1
  address=${value%/*}
  prefix=${value##*/}
  ipv4_address_is_valid "$address" || return 1
  (( 10#$prefix <= 32 ))
}

parse_active_ip4_values() {
  local output=$1 line value found=0
  ACTIVE_IPV4=
  while IFS= read -r line; do
    line=${line%$'\r'}
    [[ -n "$line" ]] || continue
    if [[ "$line" == *:* ]]; then
      value=${line##*:}
    else
      value=$line
    fi
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    [[ -z "$value" || "$value" == -- ]] && continue
    ipv4_cidr_is_valid "$value" || return 2
    (( found == 0 )) || return 2
    ACTIVE_IPV4=$value
    found=1
  done <<<"$output"
  (( found == 1 )) || return 3
}

# Query only the active IP data for the exact owned UUID. An active VPN may
# briefly have no address while it is activating; return 3 for that bounded
# readiness state. Any malformed or duplicate address is unsafe. NetworkManager
# exposes active IP4 data through `connection show uuid` on current releases;
# the device-details fallback still binds the address to the exact UUID rather
# than trusting the transport DEVICE field from an active VPN row.
active_ip4_for_uuid() {
  local wanted=${1,,} output fallback_output rc
  ACTIVE_IPV4=
  uuid_is_valid "$wanted" || return 2
  output=$(LC_ALL=C nmcli -t -f IP4.ADDRESS connection show uuid "$wanted" 2>/dev/null) || return 2
  if parse_active_ip4_values "$output"; then
    return 0
  else
    rc=$?
    if (( rc == 2 )); then
      return 2
    fi
  fi

  if ! fallback_output=$(LC_ALL=C nmcli -t -f GENERAL.CON-UUID,IP4.ADDRESS device show 2>/dev/null | \
    awk -F: -v wanted="$wanted" '
      tolower($1) == "general.con-uuid" { matched=(tolower($2) == tolower(wanted)); next }
      matched && $1 ~ /^IP4\.ADDRESS/ { print $2 }
    '); then
    # A device-details field set is not available on every nmcli frontend;
    # treat that as the same bounded activation race as an empty IP field.
    return 3
  fi
  parse_active_ip4_values "$fallback_output"
}

# The kernel address table is the second half of the proof.  Match the exact
# address/CIDR reported for the owned active UUID and require one, and only
# one, safe tun-shaped interface.  A missing or non-tunnel match is not
# accepted as a hint to try an arbitrary tun/ppp/vpn device.
kernel_device_for_ipv4() {
  local wanted=$1 wanted_address=${1%/*} output line index device family address rest found=0
  ACTIVE_DEVICE=
  command -v ip >/dev/null 2>&1 || return 2
  output=$(ip -o -4 addr 2>/dev/null) || return 2
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    read -r index device family address rest <<<"$line"
    # Point-to-point tun addresses may be printed without a local prefix
    # (followed by `peer`), while subnet-style tun addresses include /CIDR.
    # The exact IPv4 address remains the UUID-bound identity; any duplicate
    # kernel occurrence is rejected below.
    address=${address%%/*}
    [[ "$family" == inet && "$address" == "$wanted_address" ]] || continue
    safe_device_name "$device" || return 2
    (( found == 0 )) || return 2
    ACTIVE_DEVICE=$device
    found=1
  done <<<"$output"
  (( found == 1 )) || return 2
}

# Return 3 when the UUID is active but its runtime address is not ready yet;
# return 2 for malformed, mismatched, or ambiguous proof.
active_device_for_uuid() {
  local wanted=${1,,} rc
  ACTIVE_DEVICE=
  uuid_is_valid "$wanted" || return 2
  active_connection_for_uuid "$wanted" || return $?
  active_ip4_for_uuid "$wanted" || {
    rc=$?
    (( rc == 3 )) && return 3
    return 2
  }
  kernel_device_for_ipv4 "$ACTIVE_IPV4" || return 2
}

active_has_uuid() {
  active_connection_for_uuid "$1" >/dev/null 2>&1
}

load_nm_profile() {
  local uuid=$1 details first line_count
  uuid_is_valid "$uuid" || return 1
  details=$(LC_ALL=C nmcli -t -f connection.id,connection.uuid,connection.type,vpn.service-type,vpn.data connection show uuid "$uuid" 2>/dev/null) || return 1
  details=${details%$'\n'}
  [[ -n "$details" ]] || return 1
  first=${details%%$'\n'*}
  # nmcli's terse form is one colon-delimited row.  Accept the equivalent
  # one-value-per-line shape as well, which is useful with versioned nmcli
  # frontends and keeps tests close to real output.
  if [[ "$first" != *:* ]]; then
    line_count=$(printf '%s\n' "$details" | awk 'END { print NR }')
    if (( line_count >= 5 )); then
      mapfile -t _nm_values <<<"$details"
      NM_PROFILE_NAME=${_nm_values[0]}
      NM_PROFILE_UUID=${_nm_values[1]}
      NM_PROFILE_TYPE=${_nm_values[2]}
      NM_PROFILE_SERVICE=${_nm_values[3]}
      NM_PROFILE_DATA=${_nm_values[4]}
    else
      return 1
    fi
  elif [[ "$first" == connection.id:* || "$first" == connection.uuid:* || "$first" == connection.type:* ]]; then
    # Some nmcli wrappers retain field labels even in terse mode.
    while IFS= read -r line; do
      case "$line" in
        connection.id:*) NM_PROFILE_NAME=${line#connection.id:} ;;
        connection.uuid:*) NM_PROFILE_UUID=${line#connection.uuid:} ;;
        connection.type:*) NM_PROFILE_TYPE=${line#connection.type:} ;;
        vpn.service-type:*) NM_PROFILE_SERVICE=${line#vpn.service-type:} ;;
        vpn.data:*) NM_PROFILE_DATA=${line#vpn.data:} ;;
      esac
    done <<<"$details"
  else
    first=${first%$'\r'}
    IFS=: read -r NM_PROFILE_NAME NM_PROFILE_UUID NM_PROFILE_TYPE NM_PROFILE_SERVICE NM_PROFILE_DATA <<<"$first"
  fi
  [[ -n "$NM_PROFILE_NAME" && -n "$NM_PROFILE_UUID" && -n "$NM_PROFILE_TYPE" && -n "$NM_PROFILE_SERVICE" ]] || return 1
}

vpn_data_is_local() {
  local data=$1 entry key value host remote_count=0
  # nmcli terse mode escapes delimiters inside values (for example the
  # remote host/port colon).  Restore only that structural escape before
  # inspecting the redacted VPN data.
  data=${data//\\:/:}
  local -a entries=()
  IFS=',' read -r -a entries <<<"$data"
  for entry in "${entries[@]}"; do
    entry="${entry#"${entry%%[![:space:]]*}"}"
    entry="${entry%"${entry##*[![:space:]]}"}"
    [[ -n "$entry" ]] || continue
    [[ "$entry" == *=* ]] || continue
    key=${entry%%=*}
    value=${entry#*=}
    key="${key#"${key%%[![:space:]]*}"}"
    key="${key%"${key##*[![:space:]]}"}"
    key=${key//[[:space:]]/}
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    case "$key" in
      remote)
        remote_count=$((remote_count + 1))
        host=${value%%:*}
        host=${host#[}
        host=${host%]}
        case "$host" in 127.0.0.1|localhost) ;; *) return 1 ;; esac
        ;;
      remote-random) return 1 ;;
    esac
  done
  (( remote_count == 1 ))
}

nm_profile_is_local() {
  local uuid=$1 expected_name=${2:-}
  load_nm_profile "$uuid" || return 1
  [[ "${NM_PROFILE_UUID,,}" == "${uuid,,}" ]] || return 1
  [[ "$NM_PROFILE_TYPE" == vpn ]] || return 1
  [[ "$NM_PROFILE_SERVICE" == "$OPENVPN_SERVICE_TYPE" ]] || return 1
  [[ -z "$expected_name" || "$NM_PROFILE_NAME" == "$expected_name" ]] || return 1
  vpn_data_is_local "$NM_PROFILE_DATA"
}

assess_ownership() {
  local check_profile=${1:-no} named current_fingerprint
  OWNERSHIP=
  STATE_ERROR=
  if read_state; then
    :
  else
    OWNERSHIP=state-invalid
    STATE_ERROR=read
    return 2
  fi
  load_named_uuids || return 1
  if [[ -n "$STATE_UUID" ]]; then
    if ! load_nm_profile "$STATE_UUID"; then
      if (( ${#NAMED_UUIDS[@]} == 0 )); then OWNERSHIP=stale; else OWNERSHIP=foreign-collision; fi
      [[ "$STATE_SOURCE" == legacy ]] && return 2
      return 0
    fi
    # A persisted UUID is an owned capability only while the exact
    # NetworkManager profile still has the helper's canonical name.  A
    # foreign-name profile may otherwise look like a valid localhost OpenVPN
    # profile and reach legacy migration or an owned operation.
    if ! nm_profile_is_local "$STATE_UUID" "$CONNECTION"; then
      OWNERSHIP=invalid
      [[ "$STATE_SOURCE" == legacy ]] && return 2
      return 0
    fi
    for named in "${NAMED_UUIDS[@]}"; do
      if [[ "$named" != "$STATE_UUID" ]]; then
        OWNERSHIP=foreign-collision
        [[ "$STATE_SOURCE" == legacy ]] && return 2
        return 0
      fi
    done

    if [[ "$STATE_SOURCE" == legacy ]]; then
      # A syntactically valid legacy pair is not enough to become a new
      # capability.  It must still describe this exact local profile.  In
      # particular, a drifted legacy fingerprint must not silently enter the
      # refresh path and mutate NetworkManager.
      if ! profile_source_is_local; then
        OWNERSHIP=legacy-source-invalid
        STATE_ERROR=legacy-source
        return 2
      fi
      current_fingerprint=$(profile_fingerprint) || {
        OWNERSHIP=legacy-source-invalid
        STATE_ERROR=legacy-source
        return 2
      }
      if [[ "$current_fingerprint" != "$STATE_FINGERPRINT" ]]; then
        OWNERSHIP=legacy-drift
        STATE_ERROR=legacy-fingerprint
        return 2
      fi
      if ! migrate_legacy_state "$STATE_UUID" "$STATE_FINGERPRINT"; then
        OWNERSHIP=state-migration-failed
        STATE_ERROR=migration
        return 2
      fi
    fi

    OWNERSHIP=owned
    if [[ "$check_profile" == yes ]]; then
      if ! profile_source_is_local; then
        OWNERSHIP=source-invalid
      else
        current_fingerprint=$(profile_fingerprint) || OWNERSHIP=source-invalid
        if [[ "$OWNERSHIP" == owned && "$current_fingerprint" != "$STATE_FINGERPRINT" ]]; then
          OWNERSHIP=drift
        fi
      fi
    fi
    return 0
  fi
  case "${#NAMED_UUIDS[@]}" in
    0) OWNERSHIP=missing ;;
    1) OWNERSHIP=foreign ;;
    *) OWNERSHIP=duplicate ;;
  esac
}

ownership_error() {
  case "$OWNERSHIP" in
    foreign|foreign-collision) echo 'refusing foreign same-named NetworkManager profile' >&2 ;;
    duplicate) echo 'refusing duplicate NetworkManager profiles named vpnkit-local' >&2 ;;
    state-invalid) echo 'NetworkManager ownership state is incomplete or invalid' >&2 ;;
    state-migration-failed) echo 'NetworkManager ownership migration failed; the valid legacy pair was preserved' >&2 ;;
    legacy-drift) echo 'legacy NetworkManager ownership fingerprint does not match the local OpenVPN profile' >&2 ;;
    legacy-source-invalid) echo 'local OpenVPN profile is missing or is not a localhost UDP profile; refusing legacy migration' >&2 ;;
    stale) echo 'owned NetworkManager UUID is stale; refusing same-name adoption' >&2 ;;
    invalid) echo 'owned NetworkManager profile is not a local OpenVPN profile' >&2 ;;
    source-invalid) echo 'local OpenVPN profile is missing or is not a localhost UDP profile' >&2 ;;
    drift) echo 'local OpenVPN profile changed; import must refresh the owned NetworkManager profile' >&2 ;;
    missing) echo 'vpnkit-local NetworkManager profile is not imported' >&2 ;;
    *) echo 'NetworkManager ownership could not be established' >&2 ;;
  esac
}

assessment_error() {
  if [[ -n "$OWNERSHIP" ]]; then
    ownership_error
  else
    echo 'NetworkManager inventory query failed' >&2
  fi
}

harden_uuid() {
  local uuid=$1 name=$2
  nmcli connection modify uuid "$uuid" connection.id "$name" connection.autoconnect no ipv4.route-metric 50 ipv6.method disabled >/dev/null 2>&1
}

delete_uuid() {
  local uuid=$1
  uuid_is_valid "$uuid" || return 1
  nmcli connection delete uuid "$uuid" >/dev/null 2>&1
}

extract_import_uuid() {
  local output=$1 matches line
  matches=$(printf '%s\n' "$output" | grep -Eio '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | tr 'A-F' 'a-f' | sort -u || true)
  IMPORT_UUIDS=()
  while IFS= read -r line; do
    [[ -n "$line" ]] && IMPORT_UUIDS+=("$line")
  done <<<"$matches"
}

IMPORT_PRE_UUIDS=()
ALL_UUIDS=()
ROLLBACK_STATE_UUID=
ROLLBACK_STATE_FINGERPRINT=

load_all_uuids() {
  local rows line name uuid type rest
  rows=$(connection_rows) || return 1
  ALL_UUIDS=()
  while IFS= read -r line; do
    line=${line%$'\r'}
    [[ -n "$line" ]] || continue
    IFS=: read -r name uuid type rest <<<"$line"
    uuid_is_valid "$uuid" && ALL_UUIDS+=("${uuid,,}")
  done <<<"$rows"
}

uuid_was_preexisting() {
  local wanted=$1 value
  for value in "${IMPORT_PRE_UUIDS[@]}"; do
    [[ "$value" == "$wanted" ]] && return 0
  done
  return 1
}

find_post_import_uuid() {
  local named
  local -a candidates=()
  load_all_uuids || return 2
  for named in "${ALL_UUIDS[@]}"; do
    uuid_is_valid "$named" || return 2
    if ! uuid_was_preexisting "$named"; then
      candidates+=("$named")
    fi
  done
  (( ${#candidates[@]} == 1 )) || return 1
  printf '%s\n' "${candidates[0]}"
}

check_named_allowlist() {
  local old_uuid=${1:-} new_uuid=${2:-} named
  load_named_uuids || return 1
  for named in "${NAMED_UUIDS[@]}"; do
    [[ "$named" == "$old_uuid" || "$named" == "$new_uuid" ]] || return 1
  done
}

rollback_imported_profile() {
  local old_uuid=${1:-} old_fingerprint=${2:-} new_uuid=${3:-} temp_name=${4:-}
  local restore_uuid=${5:-${ROLLBACK_STATE_UUID:-$old_uuid}}
  local restore_fingerprint=${6:-${ROLLBACK_STATE_FINGERPRINT:-$old_fingerprint}}
  local failed=0
  # Keep the old canonical name available while restoring the old capability.
  # If the new profile was already renamed to vpnkit-local, move it back to a
  # private staging name before compensating.  A failed cleanup may leave that
  # staging profile behind, but it cannot shadow the owned old profile.
  if [[ -n "$new_uuid" ]]; then
    harden_uuid "$new_uuid" "$temp_name" >/dev/null 2>&1 || true
    if load_nm_profile "$new_uuid" >/dev/null 2>&1; then
      delete_uuid "$new_uuid" >/dev/null 2>&1 || failed=1
    fi
  fi
  if [[ -n "$restore_uuid" ]]; then
    write_state "$restore_uuid" "$restore_fingerprint" >/dev/null 2>&1 || failed=1
  else
    clear_state >/dev/null 2>&1 || failed=1
  fi
  return "$failed"
}

refresh_failure() {
  local message=$1 rollback_status=$2
  if (( rollback_status == 0 )); then
    echo "$message; previous NetworkManager profile and ownership were restored" >&2
  else
    echo "$message; NetworkManager rollback was incomplete" >&2
  fi
  return 20
}

import_and_swap() {
  local old_uuid=${1:-} fingerprint=$2 import_output new_uuid temp_name
  local old_fingerprint=${STATE_FINGERPRINT:-} rollback_status candidate
  local prior_state_uuid=${STATE_UUID:-} prior_state_fingerprint=${STATE_FINGERPRINT:-}
  ROLLBACK_STATE_UUID=$prior_state_uuid
  ROLLBACK_STATE_FINGERPRINT=$prior_state_fingerprint
  load_all_uuids || {
    echo 'NetworkManager inventory query failed before import' >&2
    return 20
  }
  IMPORT_PRE_UUIDS=("${ALL_UUIDS[@]}")
  if ! import_output=$(LC_ALL=C nmcli connection import type openvpn file "$PROFILE" 2>&1); then
    # nmcli can return non-zero after creating a profile (for example when a
    # frontend fails while printing its result).  If its bounded output names
    # exactly one new UUID, compensate it without touching the old capability.
    extract_import_uuid "$import_output"
    if (( ${#IMPORT_UUIDS[@]} == 1 )); then
      new_uuid=${IMPORT_UUIDS[0]}
    else
      # If the frontend failed before printing its UUID, identify one and only
      # one connection newly present after the import.  Never infer cleanup
      # from a same-named profile that predated this transaction.
      candidate=$(find_post_import_uuid 2>/dev/null || true)
      new_uuid=$candidate
    fi
    if [[ -n "$new_uuid" ]]; then
      if uuid_was_preexisting "$new_uuid"; then
        echo 'NetworkManager import failed and identified a pre-existing UUID; refusing cleanup' >&2
        return 20
      fi
      if ! nm_profile_is_local "$new_uuid"; then
        echo 'NetworkManager import failed and its new profile was not local; refusing cleanup' >&2
        return 20
      fi
      temp_name="vpnkit-local-import-$new_uuid"
      rollback_status=0
      rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
      refresh_failure 'NetworkManager import failed after creating a profile' "$rollback_status" || return 20
    fi
    echo 'NetworkManager import failed' >&2
    return 20
  fi
  extract_import_uuid "$import_output"
  if (( ${#IMPORT_UUIDS[@]} != 1 )); then
    echo 'NetworkManager import result could not be associated with one profile UUID' >&2
    return 20
  fi
  new_uuid=${IMPORT_UUIDS[0]}
  [[ -z "$old_uuid" || "$new_uuid" != "$old_uuid" ]] || {
    echo 'NetworkManager import returned the owned UUID unexpectedly' >&2
    return 20
  }
  uuid_was_preexisting "$new_uuid" && {
    echo 'NetworkManager import returned a pre-existing UUID unexpectedly' >&2
    return 20
  }
  temp_name="vpnkit-local-import-$new_uuid"
  if ! nm_profile_is_local "$new_uuid"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager imported profile is not a localhost OpenVPN profile' "$rollback_status" || return 20
  fi

  # The new UUID is validated and staged under a non-canonical name first.
  # The old profile is intentionally untouched through both rename checks.
  if ! harden_uuid "$new_uuid" "$temp_name" || ! nm_profile_is_local "$new_uuid"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager imported profile hardening failed' "$rollback_status" || return 20
  fi
  if ! check_named_allowlist "$old_uuid" "$new_uuid"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager import encountered a foreign or duplicate profile' "$rollback_status" || return 20
  fi
  if ! harden_uuid "$new_uuid" "$CONNECTION" || ! nm_profile_is_local "$new_uuid" "$CONNECTION"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager profile naming or hardening failed' "$rollback_status" || return 20
  fi
  if ! check_named_allowlist "$old_uuid" "$new_uuid"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager import created a foreign or duplicate profile' "$rollback_status" || return 20
  fi

  # Commit the new capability before deleting the old profile.  If the atomic
  # state write fails, write_state's own compensation preserves the old state;
  # the surrounding rollback then removes only this imported UUID.
  if ! write_state "$new_uuid" "$fingerprint"; then
    rollback_status=0
    rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
    refresh_failure 'NetworkManager ownership state could not be persisted' "$rollback_status" || return 20
  fi

  # The old UUID remains recoverable until the capability above is committed.
  # A failed cleanup compensates back to the old state and stages/deletes only
  # the new UUID, never a same-named foreign profile.
  if [[ -n "$old_uuid" ]]; then
    if ! delete_uuid "$old_uuid" || load_nm_profile "$old_uuid" >/dev/null 2>&1; then
      rollback_status=0
      rollback_imported_profile "$old_uuid" "$old_fingerprint" "$new_uuid" "$temp_name" || rollback_status=$?
      refresh_failure 'owned NetworkManager profile could not be safely replaced' "$rollback_status" || return 20
    fi
  fi
  printf 'networkmanager_import=ok\nconnection=vpnkit-local\n'
}

read_owned_active_state() {
  local rc
  ACTIVE_DEVICE=none
  if active_device_for_uuid "$1" >/dev/null 2>&1; then
    rc=0
  else
    rc=$?
  fi
  case "$rc" in
    0) return 0 ;;
    1|3) ACTIVE_DEVICE=none; return 1 ;;
    *) ACTIVE_DEVICE=invalid; return 2 ;;
  esac
}

nm_connect_timeout() {
  [[ "$NM_CONNECT_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] &&
    (( 10#$NM_CONNECT_TIMEOUT_SECONDS >= 1 && 10#$NM_CONNECT_TIMEOUT_SECONDS <= 120 )) || {
    echo 'VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS must be in 1..120' >&2
    return 1
  }
  printf '%s\n' "$NM_CONNECT_TIMEOUT_SECONDS"
}

route4_uses_device() {
  local target=$1 expected_device=$2 output primary trailer= route_target route_device
  safe_device_name "$expected_device" || return 2
  output=$(ip -4 route get "$target" 2>/dev/null) || return 1
  [[ -n "$output" ]] || return 1
  primary=${output%%$'\n'*}
  if [[ "$output" == *$'\n'* ]]; then
    trailer=${output#*$'\n'}
    [[ "$trailer" != *$'\n'* && "${trailer//[[:space:]]/}" == cache ]] || return 2
  fi
  [[ ! "$primary" =~ (^|[[:space:]])(unreachable|prohibit|blackhole|throw)([[:space:]]|$) ]] || return 1
  route_target=$(awk '{ print $1; exit }' <<<"$primary")
  [[ "$route_target" == "$target" ]] || return 2
  route_device=$(awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "dev") {
          if (seen || i == NF) bad=1
          else { seen=1; device=$(i + 1) }
        }
      }
    }
    END {
      if (bad || !seen || device == "") exit 1
      print device
    }
  ' <<<"$primary") || return 2
  [[ "$route_device" == "$expected_device" ]] || return 1
}

owned_full_tunnel_routes_ready() {
  local device=$1 rc
  # OpenVPN redirect-gateway def1 covers these opposite IPv4 halves.  Requiring
  # both prevents one early/specific route from satisfying connect readiness.
  route4_uses_device 1.1.1.1 "$device" || { rc=$?; return "$rc"; }
  route4_uses_device 8.8.8.8 "$device"
}

wait_for_owned_active_device() {
  local uuid=$1 timeout deadline rc
  timeout=$(nm_connect_timeout) || return 2
  deadline=$((SECONDS + timeout))
  while :; do
    if active_device_for_uuid "$uuid" >/dev/null 2>&1; then
      if owned_full_tunnel_routes_ready "$ACTIVE_DEVICE"; then
        return 0
      else
        rc=$?
        # A well-formed route on a different device is the expected short
        # NetworkManager publication race. Malformed/ambiguous output is not.
        (( rc == 1 )) || return 2
      fi
    else
      rc=$?
      # No active row or no assigned IPv4 is a normal activation race.  A
      # malformed/duplicate/mismatched address is unsafe and must fail closed.
      case "$rc" in
        1|3) ;;
        *) return 2 ;;
      esac
    fi
    (( SECONDS >= deadline )) && return 1
    sleep 1
  done
}

plan() {
  need_nm
  local profile_state=missing active_state=no configured=no device=none rc
  if profile_source_is_local; then
    profile_state=ready
  elif [[ -s "$PROFILE" && ! -L "$PROFILE" ]]; then
    profile_state=invalid
  fi
  assess_ownership yes || { assessment_error; exit 20; }
  [[ "$OWNERSHIP" == owned ]] && configured=yes
  if [[ "$OWNERSHIP" == owned ]]; then
    if read_owned_active_state "$STATE_UUID"; then
      active_state=yes
      device=$ACTIVE_DEVICE
    else
      rc=$?
      (( rc == 1 )) || { echo 'active owned NetworkManager device is unavailable or unsafe' >&2; exit 20; }
    fi
  fi
  printf 'command=plan\nmutation=none\nconnection=vpnkit-local\nprofile=%s\n' "$profile_state"
  printf 'existing_connection=%s\nactive=%s\nownership=%s\ndevice=%s\nprivate_values=not_printed\n' "$configured" "$active_state" "$OWNERSHIP" "$device"
}

status() {
  need_nm
  local profile_state=missing active_state=no configured=no device=none rc
  if profile_source_is_local; then
    profile_state=ready
  elif [[ -s "$PROFILE" && ! -L "$PROFILE" ]]; then
    profile_state=invalid
  fi
  assess_ownership yes || { assessment_error; exit 20; }
  [[ "$OWNERSHIP" == owned ]] && configured=yes
  if [[ "$OWNERSHIP" == owned ]]; then
    if read_owned_active_state "$STATE_UUID"; then
      active_state=yes
      device=$ACTIVE_DEVICE
    else
      rc=$?
      (( rc == 1 )) || { echo 'active owned NetworkManager device is unavailable or unsafe' >&2; exit 20; }
    fi
  fi
  printf 'command=status\nmutation=none\nconnection=vpnkit-local\nconfigured=%s\nactive=%s\nownership=%s\ndevice=%s\nprofile=%s\nprivate_values=not_printed\n' "$configured" "$active_state" "$OWNERSHIP" "$device" "$profile_state"
}

import_profile() {
  local current_fingerprint
  confirm
  need_nm
  profile_source_is_local || { echo 'local OpenVPN profile is missing or is not a localhost UDP profile' >&2; exit 11; }
  current_fingerprint=$(profile_fingerprint) || { echo 'local OpenVPN profile fingerprint is unavailable' >&2; exit 11; }
  assess_ownership || { assessment_error; exit 20; }
  case "$OWNERSHIP" in
    owned)
      if [[ "$STATE_FINGERPRINT" == "$current_fingerprint" ]]; then
        printf 'networkmanager_import=already-configured\nconnection=vpnkit-local\n'
        return
      fi
      import_and_swap "$STATE_UUID" "$current_fingerprint" || exit $?
      ;;
    missing|stale)
      # A stale UUID may be replaced only when no same-named record exists.
      (( ${#NAMED_UUIDS[@]} == 0 )) || { ownership_error; exit 20; }
      import_and_swap '' "$current_fingerprint" || exit $?
      ;;
    *)
      ownership_error
      exit 20
      ;;
  esac
}

require_owned() {
  assess_ownership yes || { assessment_error; exit 20; }
  [[ "$OWNERSHIP" == owned ]] || { ownership_error; exit 11; }
}

verify_active_mapping() {
  need_nm
  require_owned
  read_owned_active_state "$STATE_UUID" || {
    echo 'owned NetworkManager UUID to IPv4 to tun mapping is unavailable' >&2
    exit 20
  }
  # An assigned IPv4 on the exact owned UUID and a matching kernel tun device
  # are the NetworkManager/OpenVPN handshake proof. Do not print the UUID or
  # address: callers only need the bounded proof markers.
  printf 'networkmanager_mapping=pass\n'
  printf 'owned_uuid_ip4_tun=pass\n'
  printf 'openvpn_handshake=pass\n'
  printf 'device=%s\n' "$ACTIVE_DEVICE"
}

connect_profile() {
  local rc
  confirm
  need_nm
  require_owned
  # Keep nmcli itself non-blocking and own the bounded readiness window.  A
  # successful `connection up` can still leave the VPN active with no IP while
  # the plugin finishes creating/configuring its tunnel.
  nm_connect_timeout >/dev/null || exit 2
  command -v ip >/dev/null 2>&1 || { echo 'ip is unavailable for NetworkManager connect' >&2; exit 10; }
  nmcli --wait 0 connection up uuid "$STATE_UUID" >/dev/null 2>&1 || { echo 'NetworkManager connect failed' >&2; exit 20; }
  if wait_for_owned_active_device "$STATE_UUID"; then
    :
  else
    rc=$?
    if (( rc == 1 )); then
      echo 'NetworkManager connect timed out before the owned tunnel and full-tunnel routes became ready' >&2
    else
      echo 'NetworkManager did not report a safe device for the owned profile' >&2
    fi
    exit 20
  fi
  printf 'networkmanager_connect=ok\nconnection=vpnkit-local\ndevice=%s\n' "$ACTIVE_DEVICE"
}

disconnect_profile() {
  local rc
  confirm
  need_nm
  assess_ownership yes || { assessment_error; exit 20; }
  if [[ "$OWNERSHIP" == missing ]]; then
    printf 'networkmanager_disconnect=not-configured\nconnection=vpnkit-local\n'
    return
  fi
  [[ "$OWNERSHIP" == owned ]] || { ownership_error; exit 20; }
  # Stopping is authorized by the exact owned UUID, not by the tunnel mapping.
  # The mapping can disappear first during activation/rollback, while nmcli
  # can still safely deactivate this UUID without touching a work VPN.
  if active_connection_for_uuid "$STATE_UUID"; then
    nmcli connection down uuid "$STATE_UUID" >/dev/null 2>&1 || { echo 'NetworkManager disconnect failed' >&2; exit 20; }
  else
    rc=$?
    (( rc == 1 )) || { echo 'active owned NetworkManager connection is unavailable or unsafe' >&2; exit 20; }
  fi
  printf 'networkmanager_disconnect=ok\nconnection=vpnkit-local\n'
}

remove_profile() {
  local rc
  confirm
  need_nm
  assess_ownership yes || { assessment_error; exit 20; }
  if [[ "$OWNERSHIP" == missing ]]; then
    printf 'networkmanager_remove=not-configured\nconnection=vpnkit-local\n'
    return
  fi
  [[ "$OWNERSHIP" == owned ]] || { ownership_error; exit 20; }
  # As with disconnect, do not require a safe interface mapping before the
  # exact owned UUID can be brought down and removed.
  if active_connection_for_uuid "$STATE_UUID"; then
    nmcli connection down uuid "$STATE_UUID" >/dev/null 2>&1 || { echo 'NetworkManager disconnect failed' >&2; exit 20; }
  else
    rc=$?
    (( rc == 1 )) || { echo 'active owned NetworkManager connection is unavailable or unsafe' >&2; exit 20; }
  fi
  delete_uuid "$STATE_UUID" || { echo 'NetworkManager remove failed' >&2; exit 20; }
  clear_state || { echo 'NetworkManager ownership state could not be cleared' >&2; exit 20; }
  printf 'networkmanager_remove=ok\nconnection=vpnkit-local\n'
}

usage() { printf 'Usage: %s plan|status|verify|import|connect|disconnect|remove [--yes]\n' "$0"; }
case "$ACTION" in plan) plan ;; status) status ;; verify) [[ $# -eq 0 ]] || { usage >&2; exit 2; }; verify_active_mapping ;; import) import_profile ;; connect) connect_profile ;; disconnect) disconnect_profile ;; remove) remove_profile ;; help|-h|--help) usage ;; *) usage >&2; exit 2 ;; esac
