#!/usr/bin/env bash
# Render local CachyOS/KDE sing-box and vibe-vpn configs without printing secrets.
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd -P)
PATH_GUARD="$SCRIPT_DIR/vpnkit-local-path-guard.sh"
SECURE_WRITER="$SCRIPT_DIR/vpnkit-render-local-kde-secure.py"
[[ -r "$PATH_GUARD" ]] || { echo 'missing local secret path guard' >&2; exit 1; }
[[ -r "$SECURE_WRITER" ]] || { echo 'missing local renderer writer' >&2; exit 1; }
# shellcheck source=/dev/null
. "$PATH_GUARD"

BASE=${VPNKIT_LOCAL_SECRETS_DIR:-secrets/vpnkit-local}
POLICY=${VPNKIT_LOCAL_POLICY:-strict}
RULESET_MODE=${VPNKIT_RULESET_SOURCE_MODE:-remote}
SELECTED_OUTBOUND_MODE=${VPNKIT_SELECTED_OUTBOUND_MODE:-subscription}
ALLOW_MISSING_SUBSCRIPTION=${VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION:-false}

# This read-only preflight preserves the local/isolated/test-fixture boundary
# (including the "VPNKIT_LOCAL_SECRETS_DIR must stay" rejection). It is not
# used as a write safety primitive: the secure writer reopens the
# destination from / component-by-component and holds every output directory
# as an O_DIRECTORY|O_NOFOLLOW fd before any output operation.
vpnkit_local_path_guard_validate_secret_root "$BASE" "$REPO_ROOT" || exit 20
BASE=$VPNKIT_LOCAL_PATH_GUARD_BASE

renderer_output_symlink_allowed() {
  case "$1" in
    "$BASE/rendered/sing-box/config.json"|\
    "$BASE/rendered/sing-box/rule-sets/vpnkit-adblock.json"|\
    "$BASE/rendered/sing-box/rule-sets/vpnkit-dev-direct.json"|\
    "$BASE/rendered/sing-box/rule-sets/geoip-ru.json"|\
    "$BASE/rendered/sing-box/rule-sets/geosite-category-ru.json"|\
    "$BASE/rendered/vibe-vpn/config.yaml"|\
    "$BASE/rendered/vibe-vpn/sub_url") return 0 ;;
    *) return 1 ;;
  esac
}

validate_existing_secret_tree() {
  local symlink symlinks hardlink
  [[ -d "$BASE" ]] || return 0

  if ! symlinks=$(find -P -- "$BASE" -type l -print 2>/dev/null); then
    echo 'could not inspect the local secret tree' >&2
    return 20
  fi
  while IFS= read -r symlink; do
    [[ -z "$symlink" ]] && continue
    if ! renderer_output_symlink_allowed "$symlink"; then
      echo 'local secret tree must not contain symlink entries' >&2
      return 20
    fi
  done <<<"$symlinks"

  if ! hardlink=$(find -P -- "$BASE" -type f -links +1 -print -quit 2>/dev/null); then
    echo 'could not inspect local secret file ownership' >&2
    return 20
  fi
  if [[ -n "$hardlink" ]]; then
    echo 'local secret tree files must not be hard-linked' >&2
    return 20
  fi
}

# Existing input/tree links are rejected before the writer is entered. A link
# installed after this check is handled by the held directory descriptors and
# descriptor-relative renameat in the writer.
validate_existing_secret_tree || exit 20
case "$POLICY" in strict|smart) ;; *) echo 'VPNKIT_LOCAL_POLICY must be strict or smart' >&2; exit 2 ;; esac
case "$RULESET_MODE" in remote|local-fixture) ;; *) echo 'VPNKIT_RULESET_SOURCE_MODE must be remote or local-fixture' >&2; exit 2 ;; esac
case "$SELECTED_OUTBOUND_MODE" in
  subscription) ;;
  direct-fixture)
    [[ "$RULESET_MODE" == local-fixture && "${VPNKIT_LOCAL_TEST_FIXTURE:-}" == 1 ]] || {
      echo 'direct-fixture outbound is restricted to explicit local test fixtures' >&2
      exit 2
    }
    ;;
  *) echo 'VPNKIT_SELECTED_OUTBOUND_MODE must be subscription or direct-fixture' >&2; exit 2 ;;
esac

# All mkdir/chmod/copy/redirection work is intentionally below this boundary
# and is implemented by the descriptor-relative writer. It stages each
# regular file with O_CREAT|O_EXCL in its held destination directory, fchmods,
# fsyncs, checks st_nlink == 1, then uses os.replace/renameat with both
# directory fds to replace the final entry without following it.
python3 "$SECURE_WRITER" \
  "$REPO_ROOT/config/sing-box/config.tun.json.template" \
  "$BASE" \
  "$POLICY" \
  "$RULESET_MODE" \
  "$SELECTED_OUTBOUND_MODE" \
  "$ALLOW_MISSING_SUBSCRIPTION"

printf 'vpnkit_local_render=ok\n'
printf 'policy=%s\n' "$POLICY"
printf 'ruleset_mode=%s\n' "$RULESET_MODE"
printf 'dns_failover=compose_local_watchdog\n'
printf 'secret_material=not_printed\n'
