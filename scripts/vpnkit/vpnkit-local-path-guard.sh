#!/usr/bin/env bash
# Read-only secret-root validation shared by directly invoked local helpers.
#
# This file is intentionally source-only: it performs no mkdir/chmod/write or
# host integration. A caller must invoke vpnkit_local_path_guard_validate_*
# before deriving or mutating paths below VPNKIT_LOCAL_SECRETS_DIR.

vpnkit_local_path_guard_error() {
  printf '%s\n' "$1" >&2
}

# Return success when any lexical path component is a symlink. Checking the
# caller's original path before realpath -m is deliberate: even a symlink that
# ultimately resolves into an allowed tree is not an accepted secret root.
vpnkit_local_path_guard_has_symlink_component() {
  local path=$1 current=/ part
  local -a parts=()

  [[ "$path" == /* ]] || return 2
  IFS=/ read -r -a parts <<<"${path#/}"
  for part in "${parts[@]}"; do
    case "$part" in
      ''|.) continue ;;
      ..)
        current=${current%/*}
        [[ -n "$current" ]] || current=/
        continue
        ;;
    esac
    if [[ "$current" == / ]]; then
      current="/$part"
    else
      current="$current/$part"
    fi
    [[ -L "$current" ]] && return 0
  done
  return 1
}

vpnkit_local_path_guard_require_no_symlink() {
  local path=$1 rc
  if vpnkit_local_path_guard_has_symlink_component "$path"; then
    vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR must not contain symlink components'
    return 20
  else
    rc=$?
  fi
  if (( rc != 1 )); then
    vpnkit_local_path_guard_error 'local secret path must be absolute'
    return 20
  fi
}

vpnkit_local_path_guard_is_production_like() {
  local path=$1 part lower
  local -a parts=()

  IFS=/ read -r -a parts <<<"${path#/}"
  for part in "${parts[@]}"; do
    [[ -n "$part" ]] || continue
    lower=${part,,}
    case "$lower" in
      *production*|*prod*|*live*) return 0 ;;
    esac
  done
  return 1
}

vpnkit_local_path_guard_existing_parent() {
  local existing=$1 next
  while [[ ! -e "$existing" && ! -L "$existing" && "$existing" != / ]]; do
    next=${existing%/*}
    [[ "$next" != "$existing" ]] || next=/
    existing=$next
  done
  [[ -d "$existing" && ! -L "$existing" ]]
}

vpnkit_local_path_guard_validate_secret_root() {
  local requested=${1:-} repo_root=${2:-${REPO_ROOT:-}}
  local local_root labs_root base

  if [[ -z "$requested" || -z "$repo_root" ]]; then
    vpnkit_local_path_guard_error 'local secret root validation requires a path and repository root'
    return 20
  fi
  case "$repo_root" in
    /*) ;;
    *)
      vpnkit_local_path_guard_error 'repository root must be absolute'
      return 20
      ;;
  esac
  case "$requested" in
    /*) ;;
    *) requested="$repo_root/$requested" ;;
  esac

  vpnkit_local_path_guard_require_no_symlink "$requested" || return 20

  if ! command -v realpath >/dev/null 2>&1; then
    vpnkit_local_path_guard_error 'realpath is required to validate the local secret root'
    return 20
  fi
  if ! base=$(realpath -m -- "$requested"); then
    vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR could not be canonicalized'
    return 20
  fi

  # Canonical validation is separate from lexical validation so a path using
  # .. cannot escape the intended lifecycle roots.
  vpnkit_local_path_guard_require_no_symlink "$base" || {
    vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR must not resolve through a symlink'
    return 20
  }
  if vpnkit_local_path_guard_is_production_like "$base"; then
    vpnkit_local_path_guard_error 'refusing production-like local secret root'
    return 20
  fi

  local_root="$repo_root/secrets/vpnkit-local"
  labs_root="$repo_root/secrets/vpnkit-labs"
  case "$base" in
    "$local_root"|"$local_root"/*) ;;
    "$labs_root"/*) ;;
    /tmp/vpnkit-local-*|/var/tmp/vpnkit-local-*) ;;
    /tmp/*|/var/tmp/*)
      if [[ "${VPNKIT_LOCAL_TEST_FIXTURE:-}" != 1 ]]; then
        vpnkit_local_path_guard_error 'unprefixed temporary secret roots require VPNKIT_LOCAL_TEST_FIXTURE=1'
        return 20
      fi
      ;;
    *)
      vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR must stay in the local, isolated lab, or test temporary tree'
      return 20
      ;;
  esac

  if [[ "$base" == /tmp || "$base" == /var/tmp || "$base" == "$labs_root" || \
      "$base" == /tmp/vpnkit-local- || "$base" == /var/tmp/vpnkit-local- ]]; then
    vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR is too broad'
    return 20
  fi

  if [[ -e "$base" || -L "$base" ]]; then
    if [[ ! -d "$base" || -L "$base" || ! -O "$base" ]]; then
      vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR must be an owned directory'
      return 20
    fi
  elif ! vpnkit_local_path_guard_existing_parent "$base"; then
    vpnkit_local_path_guard_error 'VPNKIT_LOCAL_SECRETS_DIR parent is not a directory'
    return 20
  fi

  VPNKIT_LOCAL_PATH_GUARD_BASE=$base
  VPNKIT_LOCAL_PATH_GUARD_REPO_ROOT=$repo_root
}

# Validate paths that a local helper may create or overwrite. Missing paths are
# allowed; existing directories/files must be real local objects, never
# symlinks. Existing regular files must also have one link so chmod/write
# cannot alter an unrelated inode through a hardlink.
vpnkit_local_path_guard_validate_secret_tree() {
  local base=${1:-} path suffix links
  local -a dirs=(
    state
    vibe-vpn
    openvpn
    openvpn/pki
    openvpn/server
    openvpn/client
    rendered
    rendered/openvpn
    rendered/openvpn/pki
    rendered/sing-box
    rendered/vibe-vpn
  )
  local -a files=(
    openvpn/pki/ca.crt
    openvpn/pki/ca.key
    openvpn/pki/server.crt
    openvpn/pki/server.key
    openvpn/pki/client.crt
    openvpn/pki/client.key
    openvpn/pki/ta.key
    openvpn/server/server.conf
    openvpn/client/vpnkit-local.ovpn
    rendered/openvpn/server.conf
    rendered/openvpn/pki/ca.crt
    rendered/openvpn/pki/server.crt
    rendered/openvpn/pki/server.key
    rendered/openvpn/pki/ta.key
    state/networkmanager-state
    state/networkmanager-uuid
    state/networkmanager-profile-fingerprint
  )

  if [[ -z "$base" || "$base" != /* ]]; then
    vpnkit_local_path_guard_error 'local secret tree validation requires an absolute root'
    return 20
  fi
  vpnkit_local_path_guard_require_no_symlink "$base" || return 20

  for suffix in "${dirs[@]}"; do
    path="$base/$suffix"
    vpnkit_local_path_guard_require_no_symlink "$path" || return 20
    if [[ -e "$path" || -L "$path" ]]; then
      if [[ ! -d "$path" || -L "$path" ]]; then
        vpnkit_local_path_guard_error 'local secret tree contains a non-directory path'
        return 20
      fi
    fi
  done

  for suffix in "${files[@]}"; do
    path="$base/$suffix"
    vpnkit_local_path_guard_require_no_symlink "$path" || return 20
    if [[ -e "$path" || -L "$path" ]]; then
      if [[ ! -f "$path" || -L "$path" ]]; then
        vpnkit_local_path_guard_error 'local secret tree contains a non-regular file'
        return 20
      fi
      if ! links=$(stat -c '%h' -- "$path" 2>/dev/null); then
        vpnkit_local_path_guard_error 'could not validate local secret file ownership'
        return 20
      fi
      if [[ "$links" != 1 ]]; then
        vpnkit_local_path_guard_error 'local secret file must not be hard-linked'
        return 20
      fi
    fi
  done
}

# A sourced guard must never become an executable mutation entrypoint by
# accident. Keep direct execution read-only and value-free.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  printf '%s\n' 'vpnkit-local-path-guard.sh is source-only and performs no mutation' >&2
  exit 2
fi
