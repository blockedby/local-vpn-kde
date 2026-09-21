#!/usr/bin/env bash
# Non-privileged local vpnkit lifecycle adapter for the issue #40 TUI.
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd -P)
LOCAL_ENV=${VPNKIT_LOCAL_ENV_FILE:-$REPO_ROOT/config/vpnkit-local.local.env}
if [[ -r "$LOCAL_ENV" ]]; then
  set -a
  # Operator-owned, gitignored file. Subscription/auth values still belong in secrets/.
  # shellcheck disable=SC1090
  . "$LOCAL_ENV"
  set +a
fi

BASE_INPUT=${VPNKIT_LOCAL_SECRETS_DIR:-$REPO_ROOT/secrets/vpnkit-local}
case "$BASE_INPUT" in /*) BASE_REQUEST=$BASE_INPUT ;; *) BASE_REQUEST="$REPO_ROOT/$BASE_INPUT" ;; esac
PROJECT=${VPNKIT_LOCAL_COMPOSE_PROJECT:-vpnkit-local}
LOCAL_RESOURCE_OWNER=local-lifecycle

# This adapter is deliberately narrower than Compose itself.  A local command
# must never be able to turn an environment typo into a production project or
# an unrelated secret root.  Validation happens before COMPOSE is assembled or
# any helper is allowed to create files.
path_has_symlink() {
  local path=$1 current=/ part
  local -a parts=()
  IFS=/ read -r -a parts <<<"${path#/}"
  for part in "${parts[@]}"; do
    [[ -n "$part" && "$part" != . ]] || continue
    if [[ "$part" == .. ]]; then
      current=${current%/*}
      [[ -n "$current" ]] || current=/
      continue
    fi
    [[ "$current" == / ]] && current="/$part" || current="$current/$part"
    [[ -L "$current" ]] && return 0
  done
  return 1
}

validate_project_identity() {
  [[ "$PROJECT" =~ ^vpnkit-local(-[a-z0-9][a-z0-9_-]{0,31})?$ ]] || {
    echo 'VPNKIT_LOCAL_COMPOSE_PROJECT must be vpnkit-local or a vpnkit-local-* test project' >&2
    return 20
  }
  case "$PROJECT" in
    *-prod*|*-production*|*-live*)
      echo 'refusing production-like local Docker project identity' >&2
      return 20
      ;;
  esac
}

validate_secret_root_identity() {
  local local_root="$REPO_ROOT/secrets/vpnkit-local"
  local labs_root="$REPO_ROOT/secrets/vpnkit-labs"
  local existing

  path_has_symlink "$BASE_REQUEST" && {
    echo 'VPNKIT_LOCAL_SECRETS_DIR must not contain symlink components' >&2
    return 20
  }
  BASE=$(realpath -m -- "$BASE_REQUEST") || {
    echo 'VPNKIT_LOCAL_SECRETS_DIR could not be canonicalized' >&2
    return 20
  }
  case "$BASE" in
    "$local_root"|"$local_root"/*) ;;
    "$labs_root"/*) ;;
    /tmp/vpnkit-local-*|/var/tmp/vpnkit-local-*) ;;
    /tmp/*|/var/tmp/*)
      [[ "${VPNKIT_LOCAL_TEST_FIXTURE:-}" == 1 ]] || {
        echo 'unprefixed temporary secret roots require VPNKIT_LOCAL_TEST_FIXTURE=1' >&2
        return 20
      }
      ;;
    *)
      echo 'VPNKIT_LOCAL_SECRETS_DIR must stay in the local, isolated lab, or test temporary tree' >&2
      return 20
      ;;
  esac
  [[ "$BASE" != /tmp && "$BASE" != /var/tmp && "$BASE" != "$labs_root" && "$BASE" != /tmp/vpnkit-local- && "$BASE" != /var/tmp/vpnkit-local- ]] || {
    echo 'VPNKIT_LOCAL_SECRETS_DIR is too broad' >&2
    return 20
  }
  path_has_symlink "$BASE" && {
    echo 'VPNKIT_LOCAL_SECRETS_DIR must not resolve through a symlink' >&2
    return 20
  }

  if [[ -e "$BASE" || -L "$BASE" ]]; then
    [[ -d "$BASE" && ! -L "$BASE" && -O "$BASE" ]] || {
      echo 'VPNKIT_LOCAL_SECRETS_DIR must be an owned directory' >&2
      return 20
    }
  else
    existing=$BASE
    while [[ ! -e "$existing" && ! -L "$existing" && "$existing" != / ]]; do
      existing=${existing%/*}
      [[ -n "$existing" ]] || existing=/
    done
    [[ -d "$existing" && ! -L "$existing" ]] || {
      echo 'VPNKIT_LOCAL_SECRETS_DIR parent is not a directory' >&2
      return 20
    }
  fi
}

validate_local_subtree_identities() {
  local suffix
  for suffix in \
    state vibe-vpn openvpn openvpn/pki openvpn/server openvpn/client \
    rendered rendered/openvpn rendered/sing-box rendered/vibe-vpn \
    state/lifecycle-transactions; do
    path_has_symlink "$BASE/$suffix" && {
      echo 'local secret subtree must not contain symlink components' >&2
      return 20
    }
  done
  return 0
}

validate_project_identity || exit $?
validate_secret_root_identity || exit $?
validate_local_subtree_identities || exit $?

PORT=${VPNKIT_LOCAL_OPENVPN_PORT:-21194}
ENDPOINT=${VPNKIT_LOCAL_ENDPOINT:-127.0.0.1}
PUSH_DNS=${VPNKIT_LOCAL_OPENVPN_PUSH_DNS:-8.8.8.8}
POLICY_FILE="$BASE/state/routing-policy"
LIFECYCLE_STATE_DIR="$BASE/state"
# These paths are deliberately fixed below the already-canonical, user-owned
# local secret root.  The lock is a persistent descriptor target; the journal
# and transaction directory are private durable recovery state, not /tmp.
LIFECYCLE_LOCK_FILE="$LIFECYCLE_STATE_DIR/lifecycle.lock"
LIFECYCLE_JOURNAL="$LIFECYCLE_STATE_DIR/lifecycle.journal"
LIFECYCLE_TX_ROOT="$LIFECYCLE_STATE_DIR/lifecycle-transactions"
LIFECYCLE_LOCK_WAIT_SECONDS=${VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS:-5}
LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS=${VPNKIT_LOCAL_LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS:-30}
BOOT_TIMEOUT=${VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS:-1200}
RETEST_TIMEOUT=${VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS:-180}
MANAGE_NETWORKMANAGER=${VPNKIT_LOCAL_MANAGE_NETWORKMANAGER:-true}
UNDERLAY_HELPER="$REPO_ROOT/scripts/vpnkit/vpnkit-local-underlay-routing.sh"
NM_HELPER="$REPO_ROOT/scripts/vpnkit/vpnkit-local-networkmanager.sh"
HOST_SMOKE=${VPNKIT_LOCAL_HOST_SMOKE_SCRIPT:-$REPO_ROOT/scripts/vpnkit/vpnkit-local-host-smoke.sh}
SINGBOX_RESTART_FILE=${VPNKIT_LOCAL_SINGBOX_RESTART_FILE:-/run/vpnkit/restart-sing-box}
SINGBOX_GENERATION_FILE=${VPNKIT_LOCAL_SINGBOX_GENERATION_FILE:-/run/vpnkit/sing-box-generation}

# Tests may use a fake helper, but only as an explicitly marked fixture wholly
# inside the already-validated temporary secret root.  There is no production
# override for the NetworkManager or underlay helper identities.
validate_test_fixture_helpers() {
  local candidate
  if [[ -n "${VPNKIT_LOCAL_TEST_NM_HELPER:-}" ]]; then
    [[ "${VPNKIT_LOCAL_TEST_FIXTURE:-}" == 1 && ( "$BASE" == /tmp/* || "$BASE" == /var/tmp/* ) ]] || {
      echo 'VPNKIT_LOCAL_TEST_NM_HELPER requires a temporary fixture root' >&2
      return 20
    }
    candidate=$VPNKIT_LOCAL_TEST_NM_HELPER
    path_has_symlink "$candidate" && return 20
    candidate=$(realpath -m -- "$candidate") || return 20
    [[ "$candidate" == "$BASE/"* && -x "$candidate" && ! -L "$candidate" ]] || {
      echo 'test NetworkManager helper must be executable inside the local test root' >&2
      return 20
    }
    path_has_symlink "$candidate" && return 20
    NM_HELPER=$candidate
  fi
  if [[ -n "${VPNKIT_LOCAL_TEST_UNDERLAY_HELPER:-}" ]]; then
    [[ "${VPNKIT_LOCAL_TEST_FIXTURE:-}" == 1 && ( "$BASE" == /tmp/* || "$BASE" == /var/tmp/* ) ]] || {
      echo 'VPNKIT_LOCAL_TEST_UNDERLAY_HELPER requires a temporary fixture root' >&2
      return 20
    }
    candidate=$VPNKIT_LOCAL_TEST_UNDERLAY_HELPER
    path_has_symlink "$candidate" && return 20
    candidate=$(realpath -m -- "$candidate") || return 20
    [[ "$candidate" == "$BASE/"* && -x "$candidate" && ! -L "$candidate" ]] || {
      echo 'test underlay helper must be executable inside the local test root' >&2
      return 20
    }
    path_has_symlink "$candidate" && return 20
    UNDERLAY_HELPER=$candidate
  fi
}
validate_test_fixture_helpers || exit $?
COMPOSE=(docker compose -p "$PROJECT" -f "$REPO_ROOT/docker-compose.yml" -f "$REPO_ROOT/compose.local.yaml")

lifecycle_docker_command() {
  if [[ "${LIFECYCLE_RECOVERY_MODE:-0}" == 1 ]]; then
    lifecycle_bounded_external docker "$@"
  else
    docker "$@"
  fi
}

lifecycle_compose_command() {
  if [[ "${LIFECYCLE_RECOVERY_MODE:-0}" == 1 ]]; then
    lifecycle_bounded_external "${COMPOSE[@]}" "$@"
  else
    "${COMPOSE[@]}" "$@"
  fi
}

NM_CONFIGURED=unavailable
NM_ACTIVE=unavailable
NM_DEVICE=unavailable
NM_OWNERSHIP=unavailable
NM_OWNED_UUID=
NETWORKMANAGER_UUID_PATTERN='^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'

export VPNKIT_LOCAL_SECRETS_DIR="$BASE"
# The lifecycle adapter owns one immutable Compose resource marker.  Tests use
# a separate owner through their own runner; this script must never adopt that
# marker or an arbitrary inherited value.
export VPNKIT_LOCAL_RESOURCE_OWNER="$LOCAL_RESOURCE_OWNER"
# Do not allow an inherited profile override to redirect the fixed local NM
# helper outside this validated secret root.
export VPNKIT_LOCAL_PROFILE="$BASE/openvpn/client/vpnkit-local.ovpn"
export VPNKIT_LOCAL_NM_CONNECTION=vpnkit-local
export VPNKIT_OPENVPN_BIND_ADDRESS=127.0.0.1
export VPNKIT_OPENVPN_PORT="$PORT"
export VPNKIT_BOOTSTRAP_PICK_ON_START=${VPNKIT_BOOTSTRAP_PICK_ON_START:-false}

sync_local_path() {
  local path=$1
  # Python gives us O_NOFOLLOW and a real fsync for both regular files and the
  # containing directory.  Minimal lab images may not have Python; in that
  # case a policy commit remains atomic, but durability is best effort.
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" >/dev/null 2>&1 <<'PY'
import os
import sys

path = sys.argv[1]
flags = os.O_RDONLY
if hasattr(os, "O_NOFOLLOW"):
    flags |= os.O_NOFOLLOW
if os.path.isdir(path) and hasattr(os, "O_DIRECTORY"):
    flags |= os.O_DIRECTORY
fd = os.open(path, flags)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
    return $?
  fi
  return 0
}

atomic_policy_restore() {
  local source=$1 target=$2
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$source" "$target" >/dev/null 2>&1 <<'PY' && return 0
import os
import sys

source, target = sys.argv[1:]
if os.path.islink(source):
    raise SystemExit(1)
os.replace(source, target)
PY
  fi
  mv -f -- "$source" "$target"
}

unlink_policy_no_follow() {
  local path=$1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" >/dev/null 2>&1 <<'PY' && return 0
import os
import sys

path = sys.argv[1]
try:
    os.unlink(path)
except FileNotFoundError:
    pass
PY
  fi
  rm -f -- "$path"
}

policy_path_is_safe() {
  [[ ! -L "$BASE/state" ]] || {
    echo 'local routing policy directory must not be a symlink' >&2
    return 1
  }
  if [[ -e "$BASE/state" && ! -d "$BASE/state" ]]; then
    echo 'local routing policy directory must be a directory' >&2
    return 1
  fi
  [[ ! -L "$POLICY_FILE" ]] || {
    echo 'local routing policy must not be a symlink' >&2
    return 1
  }
  if [[ -e "$POLICY_FILE" && ! -f "$POLICY_FILE" ]]; then
    echo 'local routing policy must be a regular file' >&2
    return 1
  fi
}

safe_policy() {
  local value=${VPNKIT_LOCAL_POLICY:-}
  policy_path_is_safe || return 2
  if [[ -r "$POLICY_FILE" ]]; then value=$(<"$POLICY_FILE") || return 1; fi
  case "$value" in strict|smart) printf '%s\n' "$value" ;; '') printf 'strict\n' ;; *) echo 'invalid local routing policy state' >&2; return 2 ;; esac
}

policy_state_dir_for_write() {
  [[ ! -L "$BASE" ]] || return 1
  if [[ ! -e "$BASE" ]]; then
    mkdir -- "$BASE" 2>/dev/null || { [[ -d "$BASE" && ! -L "$BASE" && -O "$BASE" ]] || return 1; }
  fi
  [[ -d "$BASE" && ! -L "$BASE" && -O "$BASE" ]] || return 1
  chmod 700 "$BASE" || return 1

  [[ ! -L "$BASE/state" ]] || return 1
  if [[ ! -e "$BASE/state" ]]; then
    mkdir -- "$BASE/state" 2>/dev/null || { [[ -d "$BASE/state" && ! -L "$BASE/state" && -O "$BASE/state" ]] || return 1; }
  fi
  [[ -d "$BASE/state" && ! -L "$BASE/state" && -O "$BASE/state" ]] || return 1
  chmod 700 "$BASE/state" || return 1
}

remove_policy() {
  policy_path_is_safe || return 1
  [[ -e "$POLICY_FILE" ]] || return 0
  rm -f -- "$POLICY_FILE" || return 1
  sync_local_path "$BASE/state" || return 1
}

write_policy() {
  local value=$1 stage='' backup='' rollback_failed=0
  case "$value" in strict|smart) ;; *) return 2 ;; esac
  policy_state_dir_for_write || return 1
  policy_path_is_safe || return 1

  # Stage in the policy's directory so the final mv is a same-filesystem
  # rename.  The old inode is held by a hard link until the directory fsync
  # succeeds; this lets a post-rename durability failure restore exact prior
  # bytes and mode without following a policy symlink.
  stage=$(mktemp "$BASE/state/.routing-policy.XXXXXX") || return 1
  if ! [[ -f "$stage" && ! -L "$stage" ]]; then
    rm -f -- "$stage" || true
    return 1
  fi
  if ! printf '%s\n' "$value" >"$stage"; then
    rm -f -- "$stage" || true
    return 1
  fi
  # chmod is intentionally before rename.  A failure here leaves the old
  # policy inode untouched and the staged file is removed below.
  if ! chmod 600 "$stage" || ! sync_local_path "$stage"; then
    rm -f -- "$stage" || true
    return 1
  fi

  if [[ -e "$POLICY_FILE" ]]; then
    backup=$(mktemp "$BASE/state/.routing-policy-backup.XXXXXX") || {
      rm -f -- "$stage" || true
      return 1
    }
    if ! rm -f -- "$backup" || ! ln -- "$POLICY_FILE" "$backup"; then
      rm -f -- "$stage" "$backup" || true
      return 1
    fi
  fi
  policy_path_is_safe || {
    rm -f -- "$stage" "$backup" || true
    return 1
  }
  # Sync the directory before commit where possible.  This is not visible
  # state, and a failure here still guarantees the old policy remains.
  sync_local_path "$BASE/state" || {
    rm -f -- "$stage" "$backup" || true
    return 1
  }
  if ! mv -f -- "$stage" "$POLICY_FILE"; then
    rm -f -- "$stage" "$backup" || true
    return 1
  fi
  stage=''

  if ! sync_local_path "$BASE/state"; then
    # A directory fsync failure is a failed commit, not a reason to leave the
    # candidate policy visible.  The backup is an exact old inode; for an
    # initially absent policy remove the candidate instead.
    if [[ -n "$backup" && -e "$backup" ]]; then
      atomic_policy_restore "$backup" "$POLICY_FILE" || rollback_failed=1
      backup=''
    else
      unlink_policy_no_follow "$POLICY_FILE" || rollback_failed=1
    fi
    sync_local_path "$BASE/state" >/dev/null 2>&1 || true
    rm -f -- "$stage" "$backup" || true
    (( rollback_failed == 0 )) || return 1
    return 1
  fi
  rm -f -- "$backup" || true
}

server_browser_id_valid() {
  [[ "${1:-}" =~ ^srv_[A-Za-z0-9_-]{27}$ ]]
}

server_browser_batch_valid() {
  local value=${1:-} id
  local -a ids
  local -A seen=()
  [[ -n "$value" && "$value" != *, && "$value" != ,* && "$value" != *,,* ]] || return 1
  IFS=, read -r -a ids <<< "$value"
  (( ${#ids[@]} <= 1000 )) || return 1
  for id in "${ids[@]}"; do
    server_browser_id_valid "$id" || return 1
    [[ -z "${seen[$id]:-}" ]] || return 1
    seen[$id]=1
  done
}

lifecycle_mutating_invocation() {
  case "${1:-}:${2:-}:$#" in
    servers:check-batch:4) server_browser_batch_valid "${3:-}" ;;
    start::1|stop::1|disconnect::1|backend:start:2|toggle:mode:2|retest:select:2|servers:test-all:2|servers:refresh:2) return 0 ;;
    servers:availability:4|servers:speed:3|servers:ping:3|servers:select:3) server_browser_id_valid "${3:-}" ;;
    *) return 1 ;;
  esac
}

lifecycle_lock_wait_seconds() {
  [[ "$LIFECYCLE_LOCK_WAIT_SECONDS" =~ ^[0-9]+$ ]] &&
    (( 10#$LIFECYCLE_LOCK_WAIT_SECONDS <= 3600 )) || {
      echo 'VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS must be an integer in 0..3600' >&2
      return 2
    }
  printf '%s\n' "$LIFECYCLE_LOCK_WAIT_SECONDS"
}

lifecycle_compensation_timeout_seconds() {
  [[ "$LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] &&
    (( 10#$LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS >= 1 && 10#$LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS <= 3600 )) || {
      echo 'VPNKIT_LOCAL_LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS must be an integer in 1..3600' >&2
      return 2
    }
  printf '%s\n' "$LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS"
}

# The lifecycle lock is opened with O_NOFOLLOW and held on an inherited file
# descriptor for the entire mutating invocation. This is the descriptor form of
# `flock -x -w`; a pathname-only flock is not sufficient because it can follow
# a replaced symlink between validation and open.
# Lock order is lifecycle lock -> Compose/docker exec -> Go state transaction;
# no nested operation acquires this lock in reverse order.
acquire_lifecycle_lock() {
  local wait
  wait=$(lifecycle_lock_wait_seconds) || return
  if [[ "${VPNKIT_LOCAL_LIFECYCLE_LOCK_HELD:-}" == 1 ]]; then
    [[ "${VPNKIT_LOCAL_LIFECYCLE_LOCK_FD:-}" =~ ^[0-9]+$ ]] || {
      echo 'lifecycle lock descriptor marker is invalid' >&2
      return 75
    }
    python3 - "$VPNKIT_LOCAL_LIFECYCLE_LOCK_FD" "$LIFECYCLE_LOCK_FILE" <<'PY'
import errno
import fcntl
import os
import stat
import sys

fd = int(sys.argv[1])
path = sys.argv[2]
try:
    descriptor = os.fstat(fd)
    pathname = os.lstat(path)
    if not stat.S_ISREG(descriptor.st_mode) or not stat.S_ISREG(pathname.st_mode):
        raise OSError(errno.ELOOP, "lifecycle lock is not a regular file")
    if (descriptor.st_dev, descriptor.st_ino) != (pathname.st_dev, pathname.st_ino):
        raise OSError(errno.ELOOP, "lifecycle lock descriptor identity changed")
    if descriptor.st_uid != os.getuid() or pathname.st_uid != os.getuid():
        raise PermissionError(errno.EACCES, "lifecycle lock is not user-owned")
    if descriptor.st_nlink != 1 or pathname.st_nlink != 1:
        raise OSError(errno.ELOOP, "lifecycle lock must have one link")
    if stat.S_IMODE(descriptor.st_mode) != 0o600 or stat.S_IMODE(pathname.st_mode) != 0o600:
        raise PermissionError(errno.EACCES, "lifecycle lock mode is not private")
    # The launcher acquired LOCK_EX.  A non-blocking re-lock is a cheap proof
    # that an inherited descriptor was not merely injected through the env.
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
except (OSError, ValueError):
    print("lifecycle lock descriptor validation failed", file=sys.stderr)
    raise SystemExit(75)
PY
    return $?
  fi

  policy_state_dir_for_write || {
    echo 'cannot prepare the canonical lifecycle state directory' >&2
    return 20
  }
  command -v python3 >/dev/null 2>&1 || {
    echo 'python3 is required for the secure lifecycle lock descriptor' >&2
    return 20
  }
  local launcher_pid launcher_rc
  lifecycle_forward_launcher_signal() {
    local signal=$1 status=$2
    trap - INT TERM HUP
    # The launcher creates a private session for the locked child; signal the
    # whole group first so docker/Compose descendants cannot outlive the
    # compensation boundary, then fall back to the exact PID.
    kill -"$signal" -- "-$launcher_pid" 2>/dev/null || kill -"$signal" "$launcher_pid" 2>/dev/null || true
    local wait_seconds=${LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS:-30} deadline
    launcher_alive() {
      kill -0 "$launcher_pid" 2>/dev/null || return 1
      if [[ -r "/proc/$launcher_pid/stat" ]]; then
        [[ "$(awk '{print $3}' "/proc/$launcher_pid/stat" 2>/dev/null)" != Z ]]
      fi
    }
    [[ "$wait_seconds" =~ ^[0-9]+$ ]] && (( wait_seconds <= 3600 )) || wait_seconds=30
    deadline=$((SECONDS + wait_seconds))
    while launcher_alive && (( SECONDS < deadline )); do sleep 0.1; done
    if launcher_alive; then
      kill -KILL -- "-$launcher_pid" 2>/dev/null || kill -KILL "$launcher_pid" 2>/dev/null || true
    fi
    wait "$launcher_pid" 2>/dev/null || true
    exit "$status"
  }
  trap 'lifecycle_forward_launcher_signal INT 130' INT
  trap 'lifecycle_forward_launcher_signal TERM 143' TERM
  trap 'lifecycle_forward_launcher_signal HUP 129' HUP
  python3 - "$LIFECYCLE_LOCK_FILE" "$wait" "$SCRIPT_DIR/vpnkit-local.sh" "$@" <<'PY' &
import errno
import fcntl
import os
import stat
import sys
import time

path = sys.argv[1]
wait = int(sys.argv[2])
script = sys.argv[3]
argv = sys.argv[4:]
try:
    prior = os.lstat(path)
    if (not stat.S_ISREG(prior.st_mode) or prior.st_uid != os.getuid() or
            prior.st_nlink != 1 or stat.S_IMODE(prior.st_mode) != 0o600):
        print("lifecycle lock is not a private user-owned single-link file", file=sys.stderr)
        raise SystemExit(75)
    existed = True
except FileNotFoundError:
    existed = False
flags = os.O_CREAT | os.O_RDWR
if not hasattr(os, "O_NOFOLLOW"):
    print("secure lifecycle lock requires O_NOFOLLOW", file=sys.stderr)
    raise SystemExit(20)
flags |= os.O_NOFOLLOW
try:
    fd = os.open(path, flags, 0o600)
except OSError:
    print("could not securely open lifecycle lock", file=sys.stderr)
    raise SystemExit(20)
try:
    if not existed:
        os.fchmod(fd, 0o600)
    deadline = time.monotonic() + wait
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            break
        except BlockingIOError:
            if time.monotonic() >= deadline:
                print("lifecycle lock is busy; bounded wait expired", file=sys.stderr)
                raise SystemExit(75)
            time.sleep(0.05)
    info = os.fstat(fd)
    pathname = os.lstat(path)
    if (not stat.S_ISREG(info.st_mode) or not stat.S_ISREG(pathname.st_mode) or
            (info.st_dev, info.st_ino) != (pathname.st_dev, pathname.st_ino) or
            info.st_uid != os.getuid() or pathname.st_uid != os.getuid() or
            info.st_nlink != 1 or pathname.st_nlink != 1 or
            stat.S_IMODE(info.st_mode) != 0o600 or stat.S_IMODE(pathname.st_mode) != 0o600):
        print("lifecycle lock is not a private user-owned single-link file", file=sys.stderr)
        raise SystemExit(75)
    # Direct shell callers retain the private lock-launcher session and the
    # forwarding behavior above. The TUI already owns a dedicated supervised
    # process group; detaching this child again would make its TERM/KILL miss
    # compensation and descendants.
    if os.environ.get("VPNKIT_TUI_SUPERVISED") != "1":
        try:
            os.setsid()
        except OSError:
            # The fallback PID forwarding below still targets the exact locked
            # child if this process is already a session leader.
            pass
    os.set_inheritable(fd, True)
    env = os.environ.copy()
    env["VPNKIT_LOCAL_LIFECYCLE_LOCK_HELD"] = "1"
    env["VPNKIT_LOCAL_LIFECYCLE_LOCK_FD"] = str(fd)
    os.execvpe("bash", ["bash", script, *argv], env)
except PermissionError:
    print("lifecycle lock is not user-owned or writable", file=sys.stderr)
    raise SystemExit(75)
except OSError:
    print("lifecycle lock could not be acquired safely", file=sys.stderr)
    raise SystemExit(75)
PY
  launcher_pid=$!
  if wait "$launcher_pid"; then
    launcher_rc=0
  else
    launcher_rc=$?
  fi
  trap - INT TERM HUP
  # The launcher execs the locked child. The original shell must not dispatch
  # the same mutation a second time after that child exits.
  exit "$launcher_rc"
}

lifecycle_private_file_ok() {
  local path=$1 expected_mode=${2:-}
  [[ -f "$path" && ! -L "$path" && -O "$path" ]] || return 1
  local links mode
  links=$(stat -c '%h' -- "$path" 2>/dev/null) || return 1
  [[ "$links" == 1 ]] || return 1
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || return 1
  if [[ -n "$expected_mode" ]]; then
    [[ "$mode" == "$expected_mode" ]] || return 1
  else
    [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
  fi
}

lifecycle_private_dir_ok() {
  local path=$1
  [[ -d "$path" && ! -L "$path" && -O "$path" ]] || return 1
  local mode
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || return 1
  [[ "$mode" == 700 ]]
}

lifecycle_sync_durable() {
  local path=$1 parent
  sync_local_path "$path" || return 1
  parent=$(dirname -- "$path")
  sync_local_path "$parent" || return 1
}

# Copy a private snapshot without following a source or destination link. The
# transaction directory is the only durable copy of pre-state; no profile
# bytes are ever sent to stdout or an error message.
lifecycle_snapshot_file() {
  local source=$1 destination=$2 present_file=${3:-} mode_file=${4:-}
  local parent tmp mode
  if [[ -L "$source" || ( -e "$source" && ! -f "$source" ) ]]; then return 1; fi
  parent=$(dirname -- "$destination")
  lifecycle_private_dir_ok "$parent" || return 1
  if [[ -e "$source" ]]; then
    lifecycle_private_file_ok "$source" || return 1
    mode=$(stat -c '%a' -- "$source" 2>/dev/null) || return 1
    tmp=$(mktemp "$parent/.snapshot.XXXXXX") || return 1
    if ! chmod 600 "$tmp" || ! cat -- "$source" >"$tmp" || ! chmod 600 "$tmp" || ! lifecycle_sync_durable "$tmp"; then
      rm -f -- "$tmp"
      return 1
    fi
    if ! mv -fT -- "$tmp" "$destination"; then
      rm -f -- "$tmp"
      return 1
    fi
    lifecycle_sync_durable "$destination" || return 1
    if [[ -n "$present_file" ]]; then
      lifecycle_atomic_write_private "$present_file" yes || return 1
    fi
    if [[ -n "$mode_file" ]]; then
      lifecycle_atomic_write_private "$mode_file" "$mode" || return 1
    fi
  else
    [[ ! -e "$destination" && ! -L "$destination" ]] || return 1
    if [[ -n "$present_file" ]]; then lifecycle_atomic_write_private "$present_file" no || return 1; fi
    if [[ -n "$mode_file" ]]; then lifecycle_atomic_write_private "$mode_file" none || return 1; fi
  fi
}

lifecycle_atomic_write_private() {
  local target=$1 value=$2 parent tmp
  parent=$(dirname -- "$target")
  lifecycle_private_dir_ok "$parent" || return 1
  [[ ! -L "$target" ]] || return 1
  if [[ -e "$target" ]]; then lifecycle_private_file_ok "$target" || return 1; fi
  tmp=$(mktemp "$parent/.atomic.XXXXXX") || return 1
  if ! chmod 600 "$tmp" || ! printf '%s\n' "$value" >"$tmp" || ! lifecycle_sync_durable "$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  if ! mv -fT -- "$tmp" "$target"; then
    rm -f -- "$tmp"
    return 1
  fi
  lifecycle_sync_durable "$target"
}

lifecycle_transaction_shape_ok() {
  lifecycle_private_dir_ok "$LIFECYCLE_STATE_DIR" || return 1
  if [[ -e "$LIFECYCLE_TX_ROOT" || -L "$LIFECYCLE_TX_ROOT" ]]; then
    lifecycle_private_dir_ok "$LIFECYCLE_TX_ROOT" || return 1
  fi
  if [[ -e "$LIFECYCLE_JOURNAL" || -L "$LIFECYCLE_JOURNAL" ]]; then
    lifecycle_private_file_ok "$LIFECYCLE_JOURNAL" 600 || return 1
  fi
}

LIFECYCLE_TX_DIR=
LIFECYCLE_TX_ID=
LIFECYCLE_TX_OPERATION=
LIFECYCLE_TX_PHASE=
LIFECYCLE_TX_PREPARED=0
LIFECYCLE_TX_POST_READY=no
LIFECYCLE_TX_TRAPS=0
LIFECYCLE_TX_COMPENSATING=0
LIFECYCLE_TX_FINALIZED=0
LIFECYCLE_TX_SIGNAL_STATUS=
LIFECYCLE_POLICY_BEFORE=
LIFECYCLE_POLICY_PRESENT=no
LIFECYCLE_POLICY_MODE=none
LIFECYCLE_CONTAINER_PRESENT=no
LIFECYCLE_CONTAINER_ID=
LIFECYCLE_CONTAINER_NAME=
LIFECYCLE_CONTAINER_PROJECT=
LIFECYCLE_CONTAINER_OWNER=
LIFECYCLE_CONTAINER_WORKDIR=
LIFECYCLE_CONTAINER_STATE=absent
LIFECYCLE_NM_ENABLED=no
LIFECYCLE_NM_CONFIGURED=not-managed
LIFECYCLE_NM_ACTIVE=not-managed
LIFECYCLE_NM_DEVICE=not-managed
LIFECYCLE_NM_OWNERSHIP=not-managed
LIFECYCLE_NM_OWNED_UUID=
LIFECYCLE_RETEST_GENERATION=
LIFECYCLE_RETEST_REQUEST=absent

lifecycle_journal_write() {
  local phase=$1 tmp
  [[ -n "$LIFECYCLE_TX_DIR" && -d "$LIFECYCLE_TX_DIR" ]] || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_STATE_DIR" || return 1
  [[ ! -L "$LIFECYCLE_JOURNAL" ]] || return 1
  if [[ -e "$LIFECYCLE_JOURNAL" ]]; then lifecycle_private_file_ok "$LIFECYCLE_JOURNAL" 600 || return 1; fi
  tmp=$(mktemp "$LIFECYCLE_STATE_DIR/.lifecycle-journal.XXXXXX") || return 1
  if ! chmod 600 "$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  if ! {
    printf 'schema=1\n'
    printf 'operation=%s\n' "$LIFECYCLE_TX_OPERATION"
    printf 'phase=%s\n' "$phase"
    printf 'id=%s\n' "$LIFECYCLE_TX_ID"
    printf 'base=%s\n' "$BASE"
    printf 'project=%s\n' "$PROJECT"
    printf 'owner=%s\n' "$LOCAL_RESOURCE_OWNER"
    printf 'snapshot_dir=%s\n' "$LIFECYCLE_TX_DIR"
    printf 'post_ready=%s\n' "$LIFECYCLE_TX_POST_READY"
    printf 'policy_present=%s\n' "$LIFECYCLE_POLICY_PRESENT"
    printf 'policy_mode=%s\n' "$LIFECYCLE_POLICY_MODE"
    printf 'container_present=%s\n' "$LIFECYCLE_CONTAINER_PRESENT"
    printf 'container_id=%s\n' "$LIFECYCLE_CONTAINER_ID"
    printf 'container_name=%s\n' "$LIFECYCLE_CONTAINER_NAME"
    printf 'container_project=%s\n' "$LIFECYCLE_CONTAINER_PROJECT"
    printf 'container_owner=%s\n' "$LIFECYCLE_CONTAINER_OWNER"
    printf 'container_workdir=%s\n' "$LIFECYCLE_CONTAINER_WORKDIR"
    printf 'container_state=%s\n' "$LIFECYCLE_CONTAINER_STATE"
    printf 'networkmanager_enabled=%s\n' "$LIFECYCLE_NM_ENABLED"
    printf 'nm_configured=%s\n' "$LIFECYCLE_NM_CONFIGURED"
    printf 'nm_active=%s\n' "$LIFECYCLE_NM_ACTIVE"
    printf 'nm_device=%s\n' "$LIFECYCLE_NM_DEVICE"
    printf 'nm_ownership=%s\n' "$LIFECYCLE_NM_OWNERSHIP"
    printf 'nm_owned_uuid=%s\n' "$LIFECYCLE_NM_OWNED_UUID"
    printf 'retest_generation=%s\n' "$LIFECYCLE_RETEST_GENERATION"
    printf 'retest_request=%s\n' "$LIFECYCLE_RETEST_REQUEST"
  } >"$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  lifecycle_sync_durable "$tmp" || { rm -f -- "$tmp"; return 1; }
  mv -fT -- "$tmp" "$LIFECYCLE_JOURNAL" || { rm -f -- "$tmp"; return 1; }
  lifecycle_sync_durable "$LIFECYCLE_JOURNAL"
}

lifecycle_begin_transaction() {
  local operation=$1 policy mode
  lifecycle_transaction_shape_ok || return 1
  [[ ! -e "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]] || return 1
  if [[ ! -e "$LIFECYCLE_TX_ROOT" ]]; then
    mkdir -- "$LIFECYCLE_TX_ROOT" || return 1
    chmod 700 "$LIFECYCLE_TX_ROOT" || return 1
  fi
  lifecycle_private_dir_ok "$LIFECYCLE_TX_ROOT" || return 1
  LIFECYCLE_TX_DIR=$(mktemp -d "$LIFECYCLE_TX_ROOT/txn.XXXXXX") || return 1
  chmod 700 "$LIFECYCLE_TX_DIR" || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR" || return 1
  LIFECYCLE_TX_ID=${LIFECYCLE_TX_DIR##*/}
  LIFECYCLE_TX_OPERATION=$operation
  LIFECYCLE_TX_PHASE=preparing
  LIFECYCLE_TX_PREPARED=0
  LIFECYCLE_TX_POST_READY=no
  LIFECYCLE_TX_COMPENSATING=0
  LIFECYCLE_TX_FINALIZED=0
  policy=$(safe_policy) || return 1
  LIFECYCLE_POLICY_BEFORE=$policy
  if [[ -e "$POLICY_FILE" ]]; then
    lifecycle_private_file_ok "$POLICY_FILE" || return 1
    LIFECYCLE_POLICY_PRESENT=yes
    LIFECYCLE_POLICY_MODE=$(stat -c '%a' -- "$POLICY_FILE" 2>/dev/null) || return 1
    lifecycle_snapshot_file "$POLICY_FILE" "$LIFECYCLE_TX_DIR/policy" \
      "$LIFECYCLE_TX_DIR/policy.present" "$LIFECYCLE_TX_DIR/policy.mode" || return 1
  else
    LIFECYCLE_POLICY_PRESENT=no
    LIFECYCLE_POLICY_MODE=none
    lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/policy.present" no || return 1
    lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/policy.mode" none || return 1
  fi
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/policy.value" "$policy" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.present" no || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.id" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.name" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.project" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.owner" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.workdir" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.state" absent || return 1
  # Seed a complete private marker shape before the operation-specific exact
  # NM/Compose snapshot. No journal is published until that snapshot is
  # complete, so an interrupted read-only preparation can be discarded safely.
  mkdir "$LIFECYCLE_TX_DIR/nm" || return 1
  chmod 700 "$LIFECYCLE_TX_DIR/nm" || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR/nm" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/enabled" no || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/configured" not-managed || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/active" not-managed || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/device" not-managed || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/ownership" not-managed || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/owned-uuid" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.present" no || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.mode" none || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/retest.generation" '' || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/retest.request" absent || return 1
}

lifecycle_transaction_phase() {
  local phase=$1
  [[ "$LIFECYCLE_TX_PREPARED" == 1 || "$phase" == preparing ]] || return 1
  [[ "$LIFECYCLE_TX_PHASE" != committed || "$phase" == committed ]] || return 1
  if [[ "$phase" == committed ]]; then
    [[ "$LIFECYCLE_TX_POST_READY" == yes ]] || return 1
    lifecycle_private_file_ok "$LIFECYCLE_TX_DIR/post-state" 600 || return 1
  fi
  LIFECYCLE_TX_PHASE=$phase
  lifecycle_journal_write "$phase" || return 1
  if [[ "${VPNKIT_TUI_DIAGNOSTICS:-0}" == 1 && "${VPNKIT_TUI_SUPERVISED:-0}" == 1 ]]; then
    printf '[%ss] vpnkit_phase=%s\n' "$SECONDS" "$phase" >&2
  fi
  # The committed record is the terminal durable boundary.  The fixture
  # failpoint deliberately kills this process immediately after the journal
  # fsync so every command exercises next-invocation committed recovery.
  if [[ "${VPNKIT_LOCAL_TEST_FIXTURE:-}" == 1 && "${VPNKIT_LOCAL_LIFECYCLE_FAILPOINT:-}" == "$phase" ]]; then
    kill -KILL "$BASHPID"
  fi
}

lifecycle_scalar_ok() {
  local value=$1
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* && "$value" != *=* ]] || return 1
  [[ "$value" =~ ^[A-Za-z0-9_./:+,-]*$ ]]
}

lifecycle_record_container_snapshot() {
  local cid=$1 state=$2
  if [[ -z "$cid" ]]; then
    LIFECYCLE_CONTAINER_PRESENT=no
    LIFECYCLE_CONTAINER_ID=
    LIFECYCLE_CONTAINER_NAME=
    LIFECYCLE_CONTAINER_PROJECT=
    LIFECYCLE_CONTAINER_OWNER=
    LIFECYCLE_CONTAINER_WORKDIR=
    LIFECYCLE_CONTAINER_STATE=absent
  else
    local name project owner workdir
    local_compose_preflight_for_container "$cid" || return 1
    state=$(container_state "$cid")
    name=$(local_resource_name container "$cid") || return 1
    project=$(local_resource_label container "$cid" com.docker.compose.project) || return 1
    owner=$(local_resource_label container "$cid" com.vpnkit.local.owner) || return 1
    workdir=$(local_resource_label container "$cid" com.docker.compose.project.working_dir) || return 1
    [[ "$state" == healthy || "$state" == running || "$state" == stopped || "$state" == unknown ]] || return 1
    for value in "$cid" "$name" "$project" "$owner" "$workdir"; do
      lifecycle_scalar_ok "$value" || return 1
    done
    LIFECYCLE_CONTAINER_PRESENT=yes
    LIFECYCLE_CONTAINER_ID=$cid
    LIFECYCLE_CONTAINER_NAME=$name
    LIFECYCLE_CONTAINER_PROJECT=$project
    LIFECYCLE_CONTAINER_OWNER=$owner
    LIFECYCLE_CONTAINER_WORKDIR=$workdir
    LIFECYCLE_CONTAINER_STATE=$state
  fi
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.present" "$LIFECYCLE_CONTAINER_PRESENT" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.id" "$LIFECYCLE_CONTAINER_ID" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.name" "$LIFECYCLE_CONTAINER_NAME" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.project" "$LIFECYCLE_CONTAINER_PROJECT" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.owner" "$LIFECYCLE_CONTAINER_OWNER" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.workdir" "$LIFECYCLE_CONTAINER_WORKDIR" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/container.state" "$LIFECYCLE_CONTAINER_STATE" || return 1
}

lifecycle_record_networkmanager_snapshot() {
  local active_source=$1
  if networkmanager_enabled; then
    LIFECYCLE_NM_ENABLED=yes
    case "$NM_CONFIGURED:$NM_ACTIVE:$NM_OWNERSHIP" in
      yes:yes:owned|yes:no:owned|no:no:missing|no:no:stale|no:no:foreign|no:no:foreign-collision|no:no:duplicate|no:no:invalid|no:no:source-invalid|no:no:drift) ;;
      *) return 1 ;;
    esac
    lifecycle_scalar_ok "$NM_CONFIGURED" || return 1
    lifecycle_scalar_ok "$NM_ACTIVE" || return 1
    lifecycle_scalar_ok "$NM_DEVICE" || return 1
    lifecycle_scalar_ok "$NM_OWNERSHIP" || return 1
    [[ -z "$NM_OWNED_UUID" || "$NM_OWNED_UUID" =~ $NETWORKMANAGER_UUID_PATTERN ]] || return 1
    LIFECYCLE_NM_CONFIGURED=$NM_CONFIGURED
    LIFECYCLE_NM_ACTIVE=$NM_ACTIVE
    LIFECYCLE_NM_DEVICE=$NM_DEVICE
    LIFECYCLE_NM_OWNERSHIP=$NM_OWNERSHIP
    LIFECYCLE_NM_OWNED_UUID=${NM_OWNED_UUID,,}
    if [[ -e "$LIFECYCLE_TX_DIR/nm" || -L "$LIFECYCLE_TX_DIR/nm" ]]; then
      lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR/nm" || return 1
      rm -rf -- "$LIFECYCLE_TX_DIR/nm" || return 1
    fi
    mkdir "$LIFECYCLE_TX_DIR/nm" || return 1
    chmod 700 "$LIFECYCLE_TX_DIR/nm" || return 1
    lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR/nm" || return 1
    snapshot_owned_nm_capability "$LIFECYCLE_TX_DIR/nm" || return 1
    if [[ -n "$active_source" ]]; then
      lifecycle_snapshot_file "$active_source" "$LIFECYCLE_TX_DIR/nm/foreign-active" \
        "$LIFECYCLE_TX_DIR/nm/foreign-active.present" "$LIFECYCLE_TX_DIR/nm/foreign-active.mode" || return 1
    else
      lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.present" no || return 1
      lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.mode" none || return 1
    fi
  else
    LIFECYCLE_NM_ENABLED=no
    LIFECYCLE_NM_CONFIGURED=not-managed
    LIFECYCLE_NM_ACTIVE=not-managed
    LIFECYCLE_NM_DEVICE=not-managed
    LIFECYCLE_NM_OWNERSHIP=not-managed
    LIFECYCLE_NM_OWNED_UUID=
    if [[ -e "$LIFECYCLE_TX_DIR/nm" || -L "$LIFECYCLE_TX_DIR/nm" ]]; then
      lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR/nm" || return 1
      rm -rf -- "$LIFECYCLE_TX_DIR/nm" || return 1
    fi
    mkdir "$LIFECYCLE_TX_DIR/nm" || return 1
    chmod 700 "$LIFECYCLE_TX_DIR/nm" || return 1
    lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR/nm" || return 1
    lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.present" no || return 1
    lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/foreign-active.mode" none || return 1
  fi
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/enabled" "$LIFECYCLE_NM_ENABLED" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/configured" "$LIFECYCLE_NM_CONFIGURED" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/active" "$LIFECYCLE_NM_ACTIVE" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/device" "$LIFECYCLE_NM_DEVICE" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/ownership" "$LIFECYCLE_NM_OWNERSHIP" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/nm/owned-uuid" "$LIFECYCLE_NM_OWNED_UUID" || return 1
}

lifecycle_sync_transaction_snapshot() {
  local path
  while IFS= read -r -d '' path; do
    lifecycle_sync_durable "$path" || return 1
  done < <(find -P -- "$LIFECYCLE_TX_DIR" -type f -print0 2>/dev/null)
  while IFS= read -r -d '' path; do
    lifecycle_sync_durable "$path" || return 1
  done < <(find -P -- "$LIFECYCLE_TX_DIR" -depth -type d -print0 2>/dev/null)
}

lifecycle_record_prepared_snapshot() {
  local active_source=$1 cid=$2 state=$3 defer_phase=${4:-0}
  lifecycle_record_container_snapshot "$cid" "$state" || return 1
  lifecycle_record_networkmanager_snapshot "$active_source" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/retest.generation" "$LIFECYCLE_RETEST_GENERATION" || return 1
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/retest.request" "$LIFECYCLE_RETEST_REQUEST" || return 1
  lifecycle_sync_transaction_snapshot || return 1
  LIFECYCLE_TX_PREPARED=1
  if [[ "$defer_phase" == 1 ]]; then
    # Retest fills its generation snapshot from ownership-checked reads after
    # this durable container/NM snapshot. Do not publish a journal with an
    # incomplete retest scalar; no external mutation occurs before that read.
    return 0
  fi
  lifecycle_journal_write preparing || return 1
  # The journal is now durable; install the one-shot traps before publishing
  # the prepared phase or returning to the dispatcher.
  lifecycle_install_traps
  lifecycle_transaction_phase prepared
}

lifecycle_record_post_state() {
  local policy post_policy_present= post_policy_mode= post_cid= post_container_state=
  local post_container_name= post_container_project= post_container_owner= post_container_workdir=
  local post_nm_enabled= post_nm_configured= post_nm_active= post_nm_device= post_nm_ownership= post_nm_owned_uuid=
  local post_generation= post_request=absent post_tmp

  [[ "$LIFECYCLE_TX_PREPARED" == 1 ]] || return 1
  policy_path_is_safe || return 1
  policy=$(safe_policy) || return 1
  if [[ -e "$POLICY_FILE" ]]; then
    lifecycle_private_file_ok "$POLICY_FILE" || return 1
    post_policy_present=yes
    post_policy_mode=$(stat -c '%a' -- "$POLICY_FILE" 2>/dev/null) || return 1
  else
    post_policy_present=no
    post_policy_mode=none
  fi

  # The post-state is captured only after the command's own success checks.
  # It is a compact proof of the visible result, not a second pre-state
  # snapshot and never a source for compensation. Any ownership/read failure
  # is unverifiable and therefore retains the journal.
  local_compose_preflight || return 1
  post_cid=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ -n "$post_cid" ]]; then
    post_container_state=$(container_state "$post_cid")
    case "$post_container_state" in healthy|running|stopped|unknown) ;; *) return 1 ;; esac
    post_container_name=$(local_resource_name container "$post_cid") || return 1
    post_container_project=$(local_resource_label container "$post_cid" com.docker.compose.project) || return 1
    post_container_owner=$(local_resource_label container "$post_cid" com.vpnkit.local.owner) || return 1
    post_container_workdir=$(local_resource_label container "$post_cid" com.docker.compose.project.working_dir) || return 1
  else
    post_container_state=absent
  fi

  if networkmanager_enabled; then
    post_nm_enabled=yes
    read_networkmanager_state || return 1
    post_nm_configured=$NM_CONFIGURED
    post_nm_active=$NM_ACTIVE
    post_nm_device=$NM_DEVICE
    post_nm_ownership=$NM_OWNERSHIP
    post_nm_owned_uuid=${NM_OWNED_UUID,,}
  else
    post_nm_enabled=no
    post_nm_configured=not-managed
    post_nm_active=not-managed
    post_nm_device=not-managed
    post_nm_ownership=not-managed
    post_nm_owned_uuid=
  fi

  case "$LIFECYCLE_TX_OPERATION" in
    disconnect)
      [[ "$post_cid" == "$LIFECYCLE_CONTAINER_ID" && "$post_container_state" == "$LIFECYCLE_CONTAINER_STATE" ]] || return 1
      if [[ "$post_nm_enabled" == yes ]]; then [[ "$post_nm_active" == no ]] || return 1; fi
      [[ "$post_policy_present" == "$LIFECYCLE_POLICY_PRESENT" && "$post_policy_mode" == "$LIFECYCLE_POLICY_MODE" && "$policy" == "$LIFECYCLE_POLICY_BEFORE" ]] || return 1
      ;;
    start)
      [[ "$post_cid" != "" && "$post_container_state" == healthy ]] || return 1
      if [[ "$post_nm_enabled" == yes ]]; then [[ "$post_nm_active" == yes ]] || return 1; fi
      [[ "$post_policy_present" == "$LIFECYCLE_POLICY_PRESENT" &&
         "$post_policy_mode" == "$LIFECYCLE_POLICY_MODE" &&
         "$policy" == "$LIFECYCLE_POLICY_BEFORE" ]] || return 1
      ;;
    stop)
      [[ -z "$post_cid" && "$post_container_state" == absent ]] || return 1
      if [[ "$post_nm_enabled" == yes ]]; then [[ "$post_nm_active" == no ]] || return 1; fi
      [[ "$post_policy_present" == "$LIFECYCLE_POLICY_PRESENT" &&
         "$post_policy_mode" == "$LIFECYCLE_POLICY_MODE" &&
         "$policy" == "$LIFECYCLE_POLICY_BEFORE" ]] || return 1
      ;;
    toggle)
      [[ "$policy" == strict || "$policy" == smart ]] || return 1
      [[ "$policy" != "$LIFECYCLE_POLICY_BEFORE" ]] || return 1
      if [[ "$LIFECYCLE_CONTAINER_PRESENT" == yes && "$LIFECYCLE_CONTAINER_STATE" == healthy ]]; then
        [[ "$post_cid" != "" && "$post_container_state" == healthy ]] || return 1
      else
        [[ "$post_cid" == "$LIFECYCLE_CONTAINER_ID" &&
           "$post_container_state" == "$LIFECYCLE_CONTAINER_STATE" ]] || return 1
      fi
      if [[ "$post_nm_enabled" == yes ]]; then
        [[ "$post_nm_configured" == "$LIFECYCLE_NM_CONFIGURED" &&
           "$post_nm_active" == "$LIFECYCLE_NM_ACTIVE" &&
           "$post_nm_device" == "$LIFECYCLE_NM_DEVICE" &&
           "$post_nm_ownership" == "$LIFECYCLE_NM_OWNERSHIP" &&
           "$post_nm_owned_uuid" == "${LIFECYCLE_NM_OWNED_UUID,,}" ]] || return 1
      fi
      ;;
    retest)
      [[ "$post_cid" == "$LIFECYCLE_CONTAINER_ID" && "$post_container_state" == healthy ]] || return 1
      post_generation=$(container_runtime_generation "$post_cid") || return 1
      [[ "$post_generation" =~ ^[0-9]+$ && "$post_generation" != "$LIFECYCLE_RETEST_GENERATION" ]] || return 1
      container_request_absent "$post_cid" || return 1
      if [[ "$post_nm_enabled" == yes ]]; then
        [[ "$post_nm_configured" == "$LIFECYCLE_NM_CONFIGURED" &&
           "$post_nm_active" == "$LIFECYCLE_NM_ACTIVE" &&
           "$post_nm_device" == "$LIFECYCLE_NM_DEVICE" &&
           "$post_nm_ownership" == "$LIFECYCLE_NM_OWNERSHIP" &&
           "$post_nm_owned_uuid" == "${LIFECYCLE_NM_OWNED_UUID,,}" ]] || return 1
      fi
      ;;
    *) return 1 ;;
  esac

  for value in "$post_policy_present" "$post_policy_mode" "$policy" "$post_cid" "$post_container_state" \
      "$post_container_name" "$post_container_project" "$post_container_owner" "$post_container_workdir" \
      "$post_nm_enabled" "$post_nm_configured" "$post_nm_active" "$post_nm_device" "$post_nm_ownership" \
      "$post_nm_owned_uuid" "$post_generation" "$post_request"; do
    lifecycle_scalar_ok "$value" || return 1
  done

  post_tmp=$(mktemp "$LIFECYCLE_TX_DIR/.post-state.XXXXXX") || return 1
  if ! chmod 600 "$post_tmp"; then
    rm -f -- "$post_tmp"
    return 1
  fi
  if ! {
    printf 'schema=1\n'
    printf 'operation=%s\n' "$LIFECYCLE_TX_OPERATION"
    printf 'policy_present=%s\n' "$post_policy_present"
    printf 'policy_mode=%s\n' "$post_policy_mode"
    printf 'policy_value=%s\n' "$policy"
    printf 'container_present=%s\n' "$([[ -n "$post_cid" ]] && echo yes || echo no)"
    printf 'container_id=%s\n' "$post_cid"
    printf 'container_name=%s\n' "$post_container_name"
    printf 'container_project=%s\n' "$post_container_project"
    printf 'container_owner=%s\n' "$post_container_owner"
    printf 'container_workdir=%s\n' "$post_container_workdir"
    printf 'container_state=%s\n' "$post_container_state"
    printf 'networkmanager_enabled=%s\n' "$post_nm_enabled"
    printf 'nm_configured=%s\n' "$post_nm_configured"
    printf 'nm_active=%s\n' "$post_nm_active"
    printf 'nm_device=%s\n' "$post_nm_device"
    printf 'nm_ownership=%s\n' "$post_nm_ownership"
    printf 'nm_owned_uuid=%s\n' "$post_nm_owned_uuid"
    printf 'retest_generation=%s\n' "$post_generation"
    printf 'retest_request=%s\n' "$post_request"
  } >"$post_tmp"; then
    rm -f -- "$post_tmp"
    return 1
  fi
  lifecycle_sync_durable "$post_tmp" || { rm -f -- "$post_tmp"; return 1; }
  mv -fT -- "$post_tmp" "$LIFECYCLE_TX_DIR/post-state" || { rm -f -- "$post_tmp"; return 1; }
  lifecycle_sync_durable "$LIFECYCLE_TX_DIR/post-state" || return 1
  LIFECYCLE_TX_POST_READY=yes
}

validate_networkmanager_setting() {
  case "${MANAGE_NETWORKMANAGER,,}" in
    1|true|yes|on|0|false|no|off) ;;
    *) echo 'VPNKIT_LOCAL_MANAGE_NETWORKMANAGER must be true or false' >&2; return 2 ;;
  esac
}

networkmanager_enabled() {
  case "${MANAGE_NETWORKMANAGER,,}" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

container_id() { lifecycle_compose_command ps -q vpnkit 2>/dev/null || true; }
compose_selected_container() {
  local output
  command -v docker >/dev/null 2>&1 || return 1
  output=$(lifecycle_compose_command ps -q vpnkit 2>/dev/null) || return 1
  [[ -z "$output" ]] && return 2
  [[ "$output" != *$'\n'* ]] || return 1
  printf '%s\n' "$output"
}
container_state() {
  local cid=${1:-}
  [[ -n "$cid" ]] || cid=$(container_id)
  [[ -n "$cid" ]] || { printf 'absent\n'; return; }
  lifecycle_docker_command inspect "$cid" --format '{{if not .State.Running}}stopped{{else if .State.Health}}{{.State.Health.Status}}{{else}}running{{end}}' 2>/dev/null || printf 'unknown\n'
}

# Compose's project selector is not an ownership boundary: a stale resource
# can keep the requested name while carrying another project's labels (or no
# labels at all).  Every mutating lifecycle path calls this type-aware
# preflight immediately before Compose or container mutation.  Only the
# requested project and its exact expected resource names are considered;
# unrelated projects are deliberately not a global veto.
LOCAL_COMPOSE_PREFLIGHT_ERROR=
LOCAL_PREFLIGHT_CONTAINER_ID=

local_resource_label() {
  local kind=$1 id=$2 label=$3 value
  case "$kind" in
    container) value=$(lifecycle_docker_command inspect --format "{{ index .Config.Labels \"$label\" }}" "$id" 2>/dev/null) || return 1 ;;
    network|volume) value=$(lifecycle_docker_command inspect --format "{{ index .Labels \"$label\" }}" "$id" 2>/dev/null) || return 1 ;;
    *) return 2 ;;
  esac
  case "$value" in
    '<no value>'|'<nil>') value= ;;
  esac
  printf '%s\n' "$value"
}

local_resource_name() {
  local kind=$1 id=$2 name
  case "$kind" in container|network|volume) ;; *) return 2 ;; esac
  name=$(lifecycle_docker_command inspect --format '{{.Name}}' "$id" 2>/dev/null) || return 1
  # Docker reports container names with one leading slash; compare the
  # normalized name, never the display form, against an exact allow-list.
  name=${name#/}
  [[ -n "$name" ]] || return 1
  printf '%s\n' "$name"
}

local_compose_expected_names() {
  local kind=$1
  case "$kind" in
    # Compose v2 uses hyphens; the underscore forms are retained only for
    # collision discovery on hosts that still have a v1-created stack.
    container)
      printf '%s\n' \
        "${PROJECT}-vpnkit-1" "${PROJECT}_vpnkit_1" \
        "${PROJECT}-ovpn-client-test-1" "${PROJECT}_ovpn-client-test_1"
      ;;
    network)
      printf '%s\n' "${PROJECT}_vpnkit-local"
      ;;
    volume)
      printf '%s\n' \
        "${PROJECT}_vpnkit-local-vibe-vpn-state" \
        "${PROJECT}_vpnkit-local-sing-box-state" \
        "${PROJECT}_vpnkit-local-vpnkit-logs" \
        "${PROJECT}_vpnkit-local-vibe-vpn-logs"
      ;;
    *) return 2 ;;
  esac
}

local_compose_container_name_allowed() {
  local actual_name=$1 expected_name
  [[ -n "$actual_name" ]] || return 1
  while IFS= read -r expected_name; do
    [[ "$actual_name" == "$expected_name" ]] && return 0
  done < <(local_compose_expected_names container)
  return 1
}

local_compose_resource_ids() {
  local kind=$1 project_ids named_ids name
  case "$kind" in
    container)
      project_ids=$(lifecycle_docker_command container ls -aq --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null) || {
        LOCAL_COMPOSE_PREFLIGHT_ERROR='cannot list local Compose containers'
        return 1
      }
      ;;
    network)
      project_ids=$(lifecycle_docker_command network ls -q --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null) || {
        LOCAL_COMPOSE_PREFLIGHT_ERROR='cannot list local Compose networks'
        return 1
      }
      ;;
    volume)
      project_ids=$(lifecycle_docker_command volume ls -q --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null) || {
        LOCAL_COMPOSE_PREFLIGHT_ERROR='cannot list local Compose volumes'
        return 1
      }
      ;;
    *)
      LOCAL_COMPOSE_PREFLIGHT_ERROR="unsupported local Compose resource kind: $kind"
      return 1
      ;;
  esac

  # Project-label listing catches resources whose names are wrong.  Exact
  # name listing catches a foreign or label-less resource that would otherwise
  # collide with the requested Compose project before Compose gets to it.
  printf '%s\n' "$project_ids"
  while IFS= read -r name; do
    [[ -n "$name" ]] || continue
    case "$kind" in
      container)
        if ! named_ids=$(lifecycle_docker_command container ls -aq --filter "name=^/${name}$" 2>/dev/null); then
          LOCAL_COMPOSE_PREFLIGHT_ERROR="cannot inspect local Compose $kind names"
          return 1
        fi
        ;;
      network)
        if ! named_ids=$(lifecycle_docker_command network ls -q --filter "name=^${name}$" 2>/dev/null); then
          LOCAL_COMPOSE_PREFLIGHT_ERROR="cannot inspect local Compose $kind names"
          return 1
        fi
        ;;
      volume)
        if ! named_ids=$(lifecycle_docker_command volume ls -q --filter "name=^${name}$" 2>/dev/null); then
          LOCAL_COMPOSE_PREFLIGHT_ERROR="cannot inspect local Compose $kind names"
          return 1
        fi
        ;;
    esac
    printf '%s\n' "$named_ids"
  done < <(local_compose_expected_names "$kind")
}

local_compose_resource_owned() {
  local kind=$1 id=$2 project owner workdir compose_resource actual_name expected_name
  [[ -n "$id" ]] || {
    LOCAL_COMPOSE_PREFLIGHT_ERROR="$kind resource id is empty"
    return 1
  }
  project=$(local_resource_label "$kind" "$id" com.docker.compose.project || true)
  owner=$(local_resource_label "$kind" "$id" com.vpnkit.local.owner || true)
  case "$kind" in
    container)
      actual_name=$(local_resource_name container "$id" || true)
      if ! local_compose_container_name_allowed "$actual_name"; then
        LOCAL_COMPOSE_PREFLIGHT_ERROR="container $id has unexpected name=${actual_name:-missing} for project=$PROJECT"
        return 1
      fi
      workdir=$(local_resource_label container "$id" com.docker.compose.project.working_dir || true)
      if [[ "$project" != "$PROJECT" || "$owner" != "$LOCAL_RESOURCE_OWNER" || "$workdir" != "$REPO_ROOT" ]]; then
        LOCAL_COMPOSE_PREFLIGHT_ERROR="container $id requires project=$PROJECT owner=$LOCAL_RESOURCE_OWNER working_dir=$REPO_ROOT"
        return 1
      fi
      ;;
    network)
      compose_resource=$(local_resource_label network "$id" com.docker.compose.network || true)
      actual_name=$(local_resource_name network "$id" || true)
      case "$compose_resource" in
        vpnkit-local) expected_name="${PROJECT}_vpnkit-local" ;;
        *)
          LOCAL_COMPOSE_PREFLIGHT_ERROR="network $id has unexpected Compose network label: ${compose_resource:-missing}"
          return 1
          ;;
      esac
      if [[ "$project" != "$PROJECT" || "$owner" != "$LOCAL_RESOURCE_OWNER" || "$actual_name" != "$expected_name" ]]; then
        LOCAL_COMPOSE_PREFLIGHT_ERROR="network $id requires name=$expected_name project=$PROJECT owner=$LOCAL_RESOURCE_OWNER"
        return 1
      fi
      ;;
    volume)
      compose_resource=$(local_resource_label volume "$id" com.docker.compose.volume || true)
      actual_name=$(local_resource_name volume "$id" || true)
      case "$compose_resource" in
        vpnkit-local-vibe-vpn-state|vpnkit-local-sing-box-state|vpnkit-local-vpnkit-logs|vpnkit-local-vibe-vpn-logs)
          expected_name="${PROJECT}_${compose_resource}"
          ;;
        *)
          LOCAL_COMPOSE_PREFLIGHT_ERROR="volume $id has unexpected Compose volume label: ${compose_resource:-missing}"
          return 1
          ;;
      esac
      if [[ "$project" != "$PROJECT" || "$owner" != "$LOCAL_RESOURCE_OWNER" || "$actual_name" != "$expected_name" ]]; then
        LOCAL_COMPOSE_PREFLIGHT_ERROR="volume $id requires name=$expected_name project=$PROJECT owner=$LOCAL_RESOURCE_OWNER"
        return 1
      fi
      ;;
    *)
      LOCAL_COMPOSE_PREFLIGHT_ERROR="unsupported local Compose resource kind: $kind"
      return 1
      ;;
  esac
}

local_compose_preflight() {
  local kind ids id selected_id
  LOCAL_COMPOSE_PREFLIGHT_ERROR=
  LOCAL_PREFLIGHT_CONTAINER_ID=
  command -v docker >/dev/null 2>&1 || {
    LOCAL_COMPOSE_PREFLIGHT_ERROR='docker is unavailable'
    return 1
  }

  for kind in container network volume; do
    ids=$(local_compose_resource_ids "$kind") || return 1
    while IFS= read -r id; do
      [[ -n "$id" ]] || continue
      local_compose_resource_owned "$kind" "$id" || return 1
    done <<<"$(printf '%s\n' "$ids" | awk 'NF && !seen[$0]++')"
  done

  # Use the same Compose-selected vpnkit container for subsequent exec/state
  # operations, but validate it independently in case a mock/runtime exposes
  # it without a project-label listing.
  if ! selected_id=$(lifecycle_compose_command ps -q vpnkit 2>/dev/null); then
    LOCAL_COMPOSE_PREFLIGHT_ERROR='cannot select local Compose vpnkit container'
    return 1
  fi
  if [[ -n "$selected_id" ]]; then
    if [[ "$selected_id" == *$'\n'* ]]; then
      LOCAL_COMPOSE_PREFLIGHT_ERROR='multiple vpnkit containers selected by Compose'
      return 1
    fi
    local_compose_resource_owned container "$selected_id" || return 1
    LOCAL_PREFLIGHT_CONTAINER_ID=$selected_id
  fi
}

local_compose_preflight_for_container() {
  local cid=$1
  local_compose_preflight || return 1
  [[ -n "$cid" && "$LOCAL_PREFLIGHT_CONTAINER_ID" == "$cid" ]] || {
    LOCAL_COMPOSE_PREFLIGHT_ERROR='Compose selected a different local vpnkit container'
    return 1
  }
}

# `docker exec` bypasses Compose's service selector, so each read or mutation
# gets a fresh ownership proof and must use the exact container selected by
# that proof. Callers intentionally repeat the proof after external intervals
# before invoking a mutation such as `vibe-vpn pick`.
docker_exec_local() {
  local cid=$1
  shift
  local_compose_preflight_for_container "$cid" || return 1
  lifecycle_docker_command exec "$cid" "$@"
}

SERVER_BROWSER_SCHEMA=vibe-vpn.server-browser.v2
SERVER_BROWSER_SUPERVISOR=/usr/local/bin/vibe-vpn-server-op-supervisor
SERVER_BROWSER_VERIFY_OPEN=3
SERVER_BROWSER_CONTROL_TIMEOUT_SECONDS=5

server_browser_unavailable() {
  printf '{"schema":"%s","status":"unavailable","generation":0}\n' "$SERVER_BROWSER_SCHEMA"
}

server_browser_operation_token() {
  command -v python3 >/dev/null 2>&1 || return 1
  python3 - <<'PY'
import secrets
print("op_" + secrets.token_urlsafe(32))
PY
}

server_browser_helper_exec() {
  local cid=$1 command=$2 token=$3 operation=${4:-} server_id=${5:-} target_url=${6:-}
  local -a argv=("$SERVER_BROWSER_SUPERVISOR" "$command" "$token")
  case "$command:$operation:$server_id" in
    prepare:check-batch:*|run:check-batch:*)
      server_browser_batch_valid "$server_id" || return 2
      [[ "$target_url" == https://* && ${#target_url} -le 2048 ]] || return 2
      argv+=("$operation" "$server_id" "$target_url") ;;
    prepare:availability:*|run:availability:*)
      server_browser_id_valid "$server_id" || return 2
      [[ "$target_url" == https://* && ${#target_url} -le 2048 ]] || return 2
      argv+=("$operation" "$server_id" "$target_url") ;;

    prepare:refresh:|run:refresh:|prepare:list:|prepare:test-all:|prepare:current:|run:list:|run:test-all:|run:current:)
      argv+=("$operation")
      ;;
    prepare:speed:*|run:speed:*|prepare:ping:*|prepare:select:*|run:ping:*|run:select:*)
      server_browser_id_valid "$server_id" || return 2
      argv+=("$operation" "$server_id")
      ;;
    cancel::|verify::) ;;
    *) return 2 ;;
  esac
  local -a exec_options=()
  if [[ "$command" == run && "$operation" == speed && "${VPNKIT_SPEED_PROGRESS:-0}" == 1 ]]; then
    # Older supervisors ignore the opt-in environment and retain final-only JSON.
    exec_options=(-e VPNKIT_SPEED_PROGRESS=1)
  fi
  if [[ "$command" == run || "${LIFECYCLE_RECOVERY_MODE:-0}" == 1 ]]; then
    lifecycle_docker_command exec "${exec_options[@]}" "$cid" "${argv[@]}"
  else
    # A wedged client attachment is not process closure. Bound each fixed
    # prepare/cancel/verify attachment so the caller can re-prove ownership and
    # retry the token protocol rather than being trapped in one Docker CLI.
    command -v timeout >/dev/null 2>&1 || return 125
    timeout --foreground -k 1s "$SERVER_BROWSER_CONTROL_TIMEOUT_SECONDS" \
      docker exec "$cid" "${argv[@]}"
  fi
}

# Cancellation targets the captured container ID, not whichever container
# Compose may select later. The immutable local project/owner/workdir/name
# labels are re-proved immediately before every helper exec. If replacement
# stopped or removed the original container, Docker itself has already closed
# every process in it; an unproved running container is never adopted.
server_browser_original_container_closed() {
  local cid=$1 state ids
  state=$(container_state "$cid")
  [[ "$state" == stopped ]] && return 0
  [[ "$state" != unknown ]] && return 1
  # Distinguish a removed exact ID from a transient inspect failure. A failed
  # list is uncertainty, not absence; only a successful empty exact-ID query
  # proves that Docker can no longer host the operation.
  ids=$(lifecycle_docker_command container ls -aq --no-trunc --filter "id=$cid" 2>/dev/null) || return 1
  [[ -z "$ids" ]]
}

server_browser_original_container_owned_running() {
  local cid=$1 state
  local_compose_resource_owned container "$cid" >/dev/null 2>&1 || return 1
  state=$(container_state "$cid")
  [[ "$state" == healthy || "$state" == running || "$state" == starting || "$state" == unhealthy ]]
}

server_browser_cancel_and_verify() {
  local cid=$1 token=$2 verify_rc
  while true; do
    if server_browser_original_container_closed "$cid"; then
      return 0
    fi
    if ! server_browser_original_container_owned_running "$cid"; then
      # Ownership/runtime lookup uncertainty is fail-closed. Do not return
      # while an unproved container might still host the exact operation.
      sleep 0.05
      continue
    fi
    server_browser_helper_exec "$cid" cancel "$token" '' '' >/dev/null 2>&1 || true

    # Re-prove the exact original container again after cancellation's external
    # interval and immediately before verify. verify never signals and returns
    # 3 only while the recorded Go process group can still mutate.
    if server_browser_original_container_closed "$cid"; then
      return 0
    fi
    if ! server_browser_original_container_owned_running "$cid"; then
      sleep 0.05
      continue
    fi
    if server_browser_helper_exec "$cid" verify "$token" '' '' >/dev/null 2>&1; then
      return 0
    else
      verify_rc=$?
    fi
    case "$verify_rc" in
      "$SERVER_BROWSER_VERIFY_OPEN") sleep 0.05 ;;
      *) sleep 0.05 ;;
    esac
  done
}

server_browser_drain_exec_client() {
  local exec_pid=$1
  [[ "$exec_pid" =~ ^[1-9][0-9]*$ ]] || return 0
  if kill -0 "$exec_pid" 2>/dev/null; then
    kill -TERM "$exec_pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$exec_pid" 2>/dev/null || break
      sleep 0.02
    done
    kill -KILL "$exec_pid" 2>/dev/null || true
  fi
  wait "$exec_pid" 2>/dev/null || true
}

# The backend is useful before a host VPN exists. Starting it is a separate
# transaction, so a subsequent failed host connection restores that ready backend.
ensure_backend_ready() {
  local cid
  local_compose_preflight || return 1
  cid=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ -n "$cid" ]]; then
    [[ "$(container_state "$cid")" == healthy ]]
    return
  fi
  bash "$SCRIPT_DIR/vpnkit-local.sh" backend start >/dev/null || return 1
  local_compose_preflight || return 1
  cid=$LOCAL_PREFLIGHT_CONTAINER_ID
  [[ -n "$cid" && "$(container_state "$cid")" == healthy ]]
}

# This is deliberately not a generic container command bridge. The host first
# reserves one opaque token, then the in-container supervisor constructs the
# only permitted vibe-vpn argv. Killing the host Docker client is never treated
# as cancellation: every signal and unexpected client exit re-proves the exact
# original container, invokes token-scoped cancel, and waits for verify to prove
# in-container TERM/KILL/process-group closure before this shell can return.
server_browser_command() {
  local operation=${1:-} server_id=${2:-} target_url=${3:-} cid state rc token exec_pid=

  case "$operation:$#" in
    check-batch:3) server_browser_batch_valid "$server_id" || return 2 ;;
    availability:3) server_browser_id_valid "$server_id" || return 2
      [[ "$target_url" == https://* && ${#target_url} -le 2048 ]] || return 2 ;;

    refresh:1|list:1|test-all:1|current:1) ;;
    speed:2|ping:2|select:2) server_browser_id_valid "$server_id" || return 2 ;;
    *) return 2 ;;
  esac

  if ! ensure_backend_ready >/dev/null 2>&1; then
    server_browser_unavailable
    return 0
  fi
  cid=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ -z "$cid" ]]; then
    server_browser_unavailable
    return 0
  fi
  state=$(container_state "$cid")
  if [[ "$state" != healthy ]]; then
    server_browser_unavailable
    return 0
  fi
  token=$(server_browser_operation_token) || {
    server_browser_unavailable
    return 0
  }
  [[ "$token" =~ ^op_[A-Za-z0-9_-]{43}$ ]] || {
    server_browser_unavailable
    return 0
  }

  # Prepare is a synchronous no-mutation reservation. It closes cancellation
  # races before worker PID publication and makes an absent state after run a
  # proof of normal cleanup rather than an unobserved future start.
  if ! local_compose_preflight_for_container "$cid" >/dev/null 2>&1; then
    server_browser_unavailable
    return 0
  fi
  if server_browser_helper_exec "$cid" prepare "$token" "$operation" "$server_id" "$target_url" >/dev/null 2>&1; then
    rc=0
  else
    rc=$?
    # Older supervisors reject the new fixed verb before creating a token.
    if [[ "$operation" == check-batch && "$rc" == 2 ]]; then
      printf '{"schema":"%s","status":"backend-outdated","generation":0}\n' "$SERVER_BROWSER_SCHEMA"
    else
      server_browser_unavailable
    fi
    return 0
  fi
  if ! local_compose_preflight_for_container "$cid" >/dev/null 2>&1; then
    server_browser_cancel_and_verify "$cid" "$token"
    server_browser_unavailable
    return 0
  fi

  server_browser_forward_signal() {
    local status=$1
    trap - INT TERM HUP
    server_browser_cancel_and_verify "$cid" "$token"
    server_browser_drain_exec_client "$exec_pid"
    exit "$status"
  }
  trap 'server_browser_forward_signal 130' INT
  trap 'server_browser_forward_signal 143' TERM
  trap 'server_browser_forward_signal 129' HUP

  # Only the helper's bounded final response and validated check progress cross this boundary;
  # all helper/worker diagnostics are discarded. The Docker client is merely
  # an attachment and is not trusted as the in-container process owner.
  server_browser_helper_exec "$cid" run "$token" "$operation" "$server_id" "$target_url" 2>/dev/null &
  exec_pid=$!
  if wait "$exec_pid"; then rc=0; else rc=$?; fi
  trap - INT TERM HUP

  # A client/attachment can fail without killing a daemon-owned exec process.
  # Verify every return. If the token remains open, cancel and drain it before
  # preserving the original safe helper exit status.
  if server_browser_original_container_closed "$cid"; then
    return "$rc"
  fi
  if server_browser_original_container_owned_running "$cid"; then
    if ! server_browser_helper_exec "$cid" verify "$token" '' '' >/dev/null 2>&1; then
      server_browser_cancel_and_verify "$cid" "$token"
    fi
  else
    server_browser_cancel_and_verify "$cid" "$token"
  fi
  server_browser_drain_exec_client "$exec_pid"
  return "$rc"
}

render_all() {
  local policy=${1:-}
  [[ -n "$policy" ]] || policy=$(safe_policy)
  case "$policy" in strict|smart) ;; *) return 2 ;; esac
  if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
    lifecycle_bounded_external "$REPO_ROOT/scripts/vpnkit/vpnkit-local-assets.sh" --secrets-dir "$BASE" --endpoint "$ENDPOINT" --port "$PORT" --push-dns "$PUSH_DNS" >/dev/null || return $?
    lifecycle_bounded_external env VPNKIT_LOCAL_POLICY="$policy" "$REPO_ROOT/scripts/vpnkit/vpnkit-render-local-kde-configs.sh" >/dev/null
  else
    "$REPO_ROOT/scripts/vpnkit/vpnkit-local-assets.sh" --secrets-dir "$BASE" --endpoint "$ENDPOINT" --port "$PORT" --push-dns "$PUSH_DNS" >/dev/null || return $?
    VPNKIT_LOCAL_POLICY="$policy" "$REPO_ROOT/scripts/vpnkit/vpnkit-render-local-kde-configs.sh" >/dev/null
  fi
}

run_host_smoke() {
  local output rc line
  if output=$(VPNKIT_LOCAL_SMOKE_DEVICE="$NM_DEVICE" bash "$HOST_SMOKE" 2>&1); then
    return 0
  else
    rc=$?
  fi
  if [[ "${VPNKIT_TUI_DIAGNOSTICS:-0}" == 1 && "${VPNKIT_TUI_SUPERVISED:-0}" == 1 ]]; then
    # The OpenTUI bridge captures this stream in the private attempt log.
    printf '%s\n' "$output" >&2
  fi
  # Keep only the canonical smoke's finite diagnostic vocabulary. Never echo
  # arbitrary command output, endpoints or errors from an overridden helper.
  while IFS= read -r line; do
    case "$line" in
      host_smoke_failed_check=inputs|host_smoke_failed_check=route-policy|\
      host_smoke_failed_check=dns|host_smoke_failed_check=dns-route|\
      host_smoke_failed_check=literal-ip-route|host_smoke_failed_check=literal-ip-https|\
      host_smoke_failed_check=hostname-https|host_smoke_failed_check=ping-route|\
      host_smoke_failed_check=ping|host_smoke_failed_check=ipv6)
        printf '%s\n' "$line" >&2
        return "$rc"
        ;;
    esac
  done <<<"$output"
  printf 'host_smoke_failed_check=unknown\n' >&2
  return "$rc"
}

wait_healthy() {
  local cid state deadline
  [[ "$BOOT_TIMEOUT" =~ ^[0-9]+$ ]] && (( BOOT_TIMEOUT >= 1 && BOOT_TIMEOUT <= 7200 )) || { echo 'VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS must be in 1..7200' >&2; return 2; }
  cid=$(container_id)
  [[ -n "$cid" ]] || return 1
  deadline=$((SECONDS + BOOT_TIMEOUT))
  if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
    lifecycle_recovery_budget_ok || return 1
    (( deadline < LIFECYCLE_RECOVERY_DEADLINE )) || deadline=$LIFECYCLE_RECOVERY_DEADLINE
  fi
  while (( SECONDS < deadline )); do
    state=$(container_state "$cid")
    case "$state" in      healthy) return 0 ;;
      stopped|absent) return 1 ;;
    esac
    if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
      lifecycle_bounded_external sleep 1 || return 1
    else
      sleep 1
    fi
  done
  return 1
}

# Read only the fixed local NM helper status. Values are deliberately reduced
# to bounded state markers, so status and transaction diagnostics never expose
# profile data or the owned UUID.
safe_networkmanager_device() {
  local device=$1
  [[ "$device" =~ ^tun[A-Za-z0-9_.-]{0,14}$ ]] || return 1
  (( ${#device} <= 15 )) || return 1
  [[ "$device" != ppp0 && "$device" != vpn0 ]]
}

uuid_is_canonical() {
  [[ "$1" =~ $NETWORKMANAGER_UUID_PATTERN ]]
}

status_field() {
  local output=$1 field=$2 value
  value=$(awk -F= -v wanted="$field" '
    $1 == wanted { count++; value=$2 }
    END { if (count != 1) exit 1; print value }
  ' <<<"$output") || return 1
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  printf '%s\n' "$value"
}

optional_status_field() {
  local output=$1 field=$2 value
  value=$(awk -F= -v wanted="$field" '
    $1 == wanted { count++; value=$2 }
    END { if (count > 1) exit 1; if (count == 1) print value }
  ' <<<"$output") || return 1
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  printf '%s\n' "$value"
}

read_owned_uuid_file() {
  local path=$1 payload uuid fingerprint extra
  [[ -f "$path" && ! -L "$path" ]] || return 1
  payload=$(<"$path") || return 1
  [[ "$payload" != *$'\n'* && "$payload" != *$'\r'* ]] || return 1
  if [[ "$path" == */networkmanager-state ]]; then
    read -r uuid fingerprint extra <<<"$payload"
    [[ -n "$uuid" && -n "$fingerprint" && -z "$extra" ]] || return 1
    [[ "$fingerprint" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
  else
    read -r uuid extra <<<"$payload"
    [[ -n "$uuid" && -z "$extra" ]] || return 1
  fi
  uuid_is_canonical "$uuid" || return 1
  printf '%s\n' "${uuid,,}"
}

# The helper's status is the ownership proof. A configured/owned claim must
# agree with the private capability UUID; a malformed or ambiguous claim is
# never converted into an exclusion from the host VPN inventory.
read_current_owned_uuid() {
  local state_path="$BASE/state/networkmanager-state"
  local mirror_path="$BASE/state/networkmanager-uuid"
  local state_present=0 mirror_present=0 state_uuid mirror_uuid
  [[ -e "$state_path" || -L "$state_path" ]] && state_present=1
  [[ -e "$mirror_path" || -L "$mirror_path" ]] && mirror_present=1
  if (( state_present )); then
    state_uuid=$(read_owned_uuid_file "$state_path") || return 1
  fi
  if (( mirror_present )); then
    mirror_uuid=$(read_owned_uuid_file "$mirror_path") || return 1
  fi
  if (( state_present && mirror_present )); then
    [[ "$state_uuid" == "$mirror_uuid" ]] || return 1
    printf '%s\n' "$state_uuid"
  elif (( state_present )); then
    printf '%s\n' "$state_uuid"
  elif (( mirror_present )); then
    # A legacy UUID without its matching fingerprint is not an ownership
    # capability, even if the UUID itself is syntactically valid.
    local fingerprint_payload
    [[ -f "$BASE/state/networkmanager-profile-fingerprint" && ! -L "$BASE/state/networkmanager-profile-fingerprint" ]] || return 1
    fingerprint_payload=$(<"$BASE/state/networkmanager-profile-fingerprint") || return 1
    [[ "$fingerprint_payload" =~ ^[0-9A-Fa-f]{64}$ ]] || return 1
    printf '%s\n' "$mirror_uuid"
  else
    return 1
  fi
}

read_networkmanager_state() {
  NM_OWNED_UUID=
  NM_OWNERSHIP=unavailable
  if ! networkmanager_enabled; then
    NM_CONFIGURED=not-managed
    NM_ACTIVE=not-managed
    NM_DEVICE=not-managed
    NM_OWNERSHIP=not-managed
    return 0
  fi
  [[ -x "$NM_HELPER" ]] || return 1
  local output configured active device ownership status_uuid explicit_uuid state_uuid
  if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
    output=$(lifecycle_bounded_external "$NM_HELPER" status 2>/dev/null) || return 1
  else
    output=$("$NM_HELPER" status 2>/dev/null) || return 1
  fi
  configured=$(status_field "$output" configured) || return 1
  active=$(status_field "$output" active) || return 1
  device=$(status_field "$output" device) || return 1
  ownership=$(status_field "$output" ownership) || return 1
  status_uuid=$(optional_status_field "$output" owned_uuid) || return 1
  explicit_uuid=$(optional_status_field "$output" uuid) || return 1
  if [[ -n "$status_uuid" ]]; then
    uuid_is_canonical "$status_uuid" || return 1
    status_uuid=${status_uuid,,}
  fi
  if [[ -n "$explicit_uuid" ]]; then
    uuid_is_canonical "$explicit_uuid" || return 1
    explicit_uuid=${explicit_uuid,,}
    [[ -z "$status_uuid" || "$status_uuid" == "$explicit_uuid" ]] || return 1
    status_uuid=$explicit_uuid
  fi

  case "$configured:$active:$ownership" in
    yes:yes:owned)
      safe_networkmanager_device "$device" || return 1
      state_uuid=$(read_current_owned_uuid) || return 1
      [[ -z "$status_uuid" || "${status_uuid,,}" == "$state_uuid" ]] || return 1
      NM_OWNED_UUID=$state_uuid
      ;;
    yes:no:owned)
      [[ "$device" == none || -z "$device" ]] || return 1
      state_uuid=$(read_current_owned_uuid) || return 1
      [[ -z "$status_uuid" || "${status_uuid,,}" == "$state_uuid" ]] || return 1
      NM_OWNED_UUID=$state_uuid
      device=none
      ;;
    no:no:missing|no:no:stale|no:no:foreign|no:no:foreign-collision|no:no:duplicate|no:no:invalid|no:no:source-invalid|no:no:drift)
      [[ "$device" == none || -z "$device" ]] || return 1
      [[ -z "$status_uuid" ]] || return 1
      device=none
      ;;
    *)
      return 1
      ;;
  esac
  NM_CONFIGURED=$configured
  NM_ACTIVE=$active
  NM_DEVICE=$device
  NM_OWNERSHIP=$ownership
}

networkmanager_status_values() {
  if ! networkmanager_enabled; then
    printf 'not-managed not-managed\n'
    return 0
  fi
  if read_networkmanager_state; then
    printf '%s %s\n' "$NM_CONFIGURED" "$NM_ACTIVE"
  else
    printf 'unavailable unavailable\n'
  fi
}

# Snapshot active VPN identities before the first Docker call. UUID and TYPE
# are delimiter-safe fields; display names are intentionally never queried.
# The snapshot is local temporary state only; it is never printed.
capture_active_vpn_identities() {
  local destination=$1 owned_uuid=${2:-} output tmp line uuid type extra
  if [[ -n "$owned_uuid" ]]; then
    uuid_is_canonical "$owned_uuid" || return 1
    owned_uuid=${owned_uuid,,}
  fi
  command -v nmcli >/dev/null 2>&1 || return 1
  if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
    output=$(lifecycle_bounded_external nmcli -t --escape yes -f UUID,TYPE connection show --active 2>/dev/null) || return 1
  else
    output=$(nmcli -t --escape yes -f UUID,TYPE connection show --active 2>/dev/null) || return 1
  fi
  tmp=$(mktemp "${destination}.XXXXXX") || return 1
  chmod 600 "$tmp" || { rm -f -- "$tmp"; return 1; }
  while IFS= read -r line; do
    line=${line%$'\r'}
    [[ -n "$line" ]] || continue
    [[ "$line" != *:*:* ]] || { rm -f -- "$tmp"; return 1; }
    IFS=: read -r uuid type extra <<<"$line"
    [[ -n "$uuid" && -n "$type" && -z "$extra" ]] || { rm -f -- "$tmp"; return 1; }
    uuid_is_canonical "$uuid" || { rm -f -- "$tmp"; return 1; }
    [[ "$type" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || { rm -f -- "$tmp"; return 1; }
    case "$type" in
      vpn|wireguard|ipsec|openvpn|strongswan) ;;
      *) continue ;;
    esac
    uuid=${uuid,,}
    [[ -n "$owned_uuid" && "$uuid" == "${owned_uuid,,}" ]] || printf '%s\n' "$uuid" >>"$tmp"
  done <<<"$output"
  : >"$destination" || { rm -f -- "$tmp"; return 1; }
  chmod 600 "$destination" || { rm -f -- "$tmp" "$destination"; return 1; }
  if ! LC_ALL=C sort -u "$tmp" >"$destination"; then
    rm -f -- "$tmp" "$destination"
    return 1
  fi
  rm -f -- "$tmp"
}

filter_other_vpn_identities() {
  local source=$1
  LC_ALL=C sort -u "$source"
}

verify_other_active_vpn_set() {
  local before=$1 after=$2 before_other=$3 after_other=$4 owned_uuid=${5:-}
  capture_active_vpn_identities "$after" "$owned_uuid" || return 1
  filter_other_vpn_identities "$before" >"$before_other"
  filter_other_vpn_identities "$after" >"$after_other"
  cmp -s "$before_other" "$after_other"
}

snapshot_owned_nm_file() {
  local source=$1 destination=$2 parent tmp mode
  if [[ -L "$source" ]]; then return 1; fi
  parent=$(dirname -- "$destination")
  lifecycle_private_dir_ok "$parent" || return 1
  [[ ! -L "$destination" && ! -e "$destination" ]] || return 1
  if [[ -e "$source" ]]; then
    lifecycle_private_file_ok "$source" || return 1
    mode=$(stat -c '%a' -- "$source") || return 1
    tmp=$(mktemp "$parent/.nm-snapshot.XXXXXX") || return 1
    if ! chmod 600 "$tmp" || ! cat -- "$source" >"$tmp" || ! chmod 600 "$tmp" || ! lifecycle_sync_durable "$tmp"; then
      rm -f -- "$tmp"
      return 1
    fi
    mv -fT -- "$tmp" "$destination" || { rm -f -- "$tmp"; return 1; }
    lifecycle_sync_durable "$destination" || return 1
    lifecycle_atomic_write_private "$destination.mode" "$mode" || return 1
    lifecycle_atomic_write_private "$destination.present" yes || return 1
  else
    lifecycle_atomic_write_private "$destination.present" no || return 1
  fi
}

snapshot_owned_nm_capability() {
  local destination=$1 name
  mkdir -- "$destination/files" || return 1
  chmod 700 "$destination/files" || return 1
  for name in networkmanager-state networkmanager-uuid networkmanager-profile-fingerprint; do
    snapshot_owned_nm_file "$BASE/state/$name" "$destination/files/$name" || return 1
  done
  snapshot_owned_nm_file "$BASE/openvpn/client/vpnkit-local.ovpn" "$destination/files/profile" || return 1
}

snapshot_owned_uuid() {
  local destination=$1 name uuid fingerprint payload
  for name in networkmanager-state networkmanager-uuid; do
    [[ "$(<"$destination/files/$name.present")" == yes ]] || continue
    payload=$(<"$destination/files/$name") || return 1
    case "$name" in
      networkmanager-state) read -r uuid fingerprint <<<"$payload" || return 1 ;;
      networkmanager-uuid) read -r uuid <<<"$payload" || return 1 ;;
    esac
    [[ -n "$uuid" ]] && { printf '%s\n' "${uuid,,}"; return 0; }
  done
  return 1
}

restore_owned_nm_file() {
  local destination=$1 name=$2 target=$3 source="$destination/files/$2"
  local present mode tmp parent
  present=$(<"$source.present") || return 1
  parent=$(dirname -- "$target")
  [[ -d "$parent" && ! -L "$parent" ]] || {
    [[ "$present" == no ]] && return 0
    return 1
  }
  if [[ "$present" == yes ]]; then
    [[ -f "$source" && ! -L "$source" ]] || return 1
    [[ ! -L "$target" && (! -e "$target" || -f "$target") ]] || return 1
    mode=$(<"$source.mode") || return 1
    [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
    tmp=$(mktemp "$parent/.$name.restore.XXXXXX") || return 1
    if ! chmod 600 "$tmp" || ! cat -- "$source" >"$tmp" || ! chmod "$mode" "$tmp" || ! sync_local_path "$tmp"; then
      rm -f -- "$tmp" || true
      return 1
    fi
    if ! mv -f -- "$tmp" "$target"; then
      rm -f -- "$tmp" || true
      return 1
    fi
  else
    [[ ! -L "$target" && (! -e "$target" || -f "$target") ]] || return 1
    [[ ! -e "$target" ]] || rm -f -- "$target" || return 1
  fi
  sync_local_path "$parent"
}

restore_owned_nm_capability_files() {
  local destination=$1
  restore_owned_nm_file "$destination" networkmanager-state "$BASE/state/networkmanager-state" || return 1
  restore_owned_nm_file "$destination" networkmanager-uuid "$BASE/state/networkmanager-uuid" || return 1
  restore_owned_nm_file "$destination" networkmanager-profile-fingerprint "$BASE/state/networkmanager-profile-fingerprint" || return 1
  restore_owned_nm_file "$destination" profile "$BASE/openvpn/client/vpnkit-local.ovpn" || return 1
}

owned_nm_capability_matches_snapshot() {
  local destination=$1 name present target source mode target_mode
  for name in networkmanager-state networkmanager-uuid networkmanager-profile-fingerprint profile; do
    source="$destination/files/$name"
    target="$BASE/openvpn/client/vpnkit-local.ovpn"
    case "$name" in networkmanager-state|networkmanager-uuid|networkmanager-profile-fingerprint) target="$BASE/state/$name" ;; esac
    present=$(<"$source.present") || return 1
    if [[ "$present" == yes ]]; then
      [[ -f "$target" && ! -L "$target" ]] || return 1
      mode=$(<"$source.mode") || return 1
      target_mode=$(stat -c '%a' -- "$target" 2>/dev/null) || return 1
      [[ "$target_mode" == "$mode" ]] || return 1
      cmp -s -- "$source" "$target" || return 1
    else
      [[ ! -e "$target" && ! -L "$target" ]] || return 1
    fi
  done
}

lifecycle_nm_mutation() {
  if [[ "$LIFECYCLE_RECOVERY_MODE" == 1 ]]; then
    lifecycle_bounded_external "$NM_HELPER" "$@"
  else
    "$NM_HELPER" "$@"
  fi
}

restore_networkmanager_capability() {
  local destination=$1 configured_before=$2 active_before=$3 device_before=$4
  local failed=0 current_configured current_active current_device current_uuid before_uuid
  local status_readable=0 capability_cleanup=0
  networkmanager_enabled || return 0
  current_uuid=$(read_current_owned_uuid 2>/dev/null || true)
  before_uuid=$(snapshot_owned_uuid "$destination" 2>/dev/null || true)

  # Remove only a newly-created/replaced local capability.  A pre-existing
  # owned UUID is left in NetworkManager so rollback can preserve it exactly.
  # If status cannot prove a tunnel device (for example while the VPN address
  # is disappearing), still use the helper's exact-UUID disconnect/remove path
  # for a newly-created capability; do not let interface proof block cleanup.
  if read_networkmanager_state; then
    status_readable=1
    current_configured=$NM_CONFIGURED
    current_active=$NM_ACTIVE
    current_device=$NM_DEVICE
    current_uuid=$(read_current_owned_uuid 2>/dev/null || true)
    if [[ "$configured_before" == no && "$current_configured" == yes ]]; then
      capability_cleanup=1
      [[ "$current_active" == yes ]] && lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || true
      lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
    elif [[ "$configured_before" == yes && "$current_configured" == yes && -n "$before_uuid" && "$current_uuid" != "$before_uuid" ]]; then
      capability_cleanup=1
      [[ "$current_active" == yes ]] && lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || true
      lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
    fi
  else
    if [[ "$configured_before" == no && -n "$current_uuid" ]]; then
      capability_cleanup=1
      lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || failed=1
      lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
    elif [[ "$configured_before" == yes && -n "$before_uuid" && "$current_uuid" != "$before_uuid" && -n "$current_uuid" ]]; then
      capability_cleanup=1
      lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || failed=1
      lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
    fi
  fi

  # Restore the source profile and the helper's persisted ownership capability
  # before any compensating import.  These files are the exact UUID/fingerprint
  # identity used by the fixed helper; no arbitrary NM profile is touched.
  restore_owned_nm_capability_files "$destination" || failed=1

  if [[ "$configured_before" == yes ]]; then
    if [[ "$status_readable" == 0 && -n "$before_uuid" && ( "$current_uuid" == "$before_uuid" || "$capability_cleanup" == 1 ) ]]; then
      # The exact pre-existing UUID is still the capability being restored;
      # an unavailable device mapping is not evidence that it needs import.
      :
    elif ! read_networkmanager_state || [[ "$NM_CONFIGURED" != yes ]]; then
      lifecycle_nm_mutation import --yes >/dev/null 2>&1 || failed=1
    fi
  else
    if read_networkmanager_state && [[ "$NM_CONFIGURED" == yes ]]; then
      lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || true
      lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
    fi
  fi

  if read_networkmanager_state; then
    if [[ "$active_before" == yes && "$NM_ACTIVE" != yes ]]; then
      lifecycle_nm_mutation connect --yes >/dev/null 2>&1 || failed=1
    elif [[ "$active_before" != yes && "$NM_ACTIVE" == yes ]]; then
      lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || failed=1
    fi
  else
    failed=1
  fi
  if ! read_networkmanager_state; then
    failed=1
  else
    [[ "$NM_CONFIGURED" == "$configured_before" && "$NM_ACTIVE" == "$active_before" && "$NM_DEVICE" == "$device_before" ]] || failed=1
  fi
  owned_nm_capability_matches_snapshot "$destination" || failed=1
  return "$failed"
}

LIFECYCLE_J_OPERATION=
LIFECYCLE_J_PHASE=
LIFECYCLE_J_ID=
LIFECYCLE_J_BASE=
LIFECYCLE_J_PROJECT=
LIFECYCLE_J_OWNER=
LIFECYCLE_J_DIR=
LIFECYCLE_J_POST_READY=
LIFECYCLE_J_POST_POLICY_PRESENT=
LIFECYCLE_J_POST_POLICY_MODE=
LIFECYCLE_J_POST_POLICY_VALUE=
LIFECYCLE_J_POST_CONTAINER_PRESENT=
LIFECYCLE_J_POST_CONTAINER_ID=
LIFECYCLE_J_POST_CONTAINER_NAME=
LIFECYCLE_J_POST_CONTAINER_PROJECT=
LIFECYCLE_J_POST_CONTAINER_OWNER=
LIFECYCLE_J_POST_CONTAINER_WORKDIR=
LIFECYCLE_J_POST_CONTAINER_STATE=
LIFECYCLE_J_POST_NM_ENABLED=
LIFECYCLE_J_POST_NM_CONFIGURED=
LIFECYCLE_J_POST_NM_ACTIVE=
LIFECYCLE_J_POST_NM_DEVICE=
LIFECYCLE_J_POST_NM_OWNERSHIP=
LIFECYCLE_J_POST_NM_OWNED_UUID=
LIFECYCLE_J_POST_RETEST_GENERATION=
LIFECYCLE_J_POST_RETEST_REQUEST=
LIFECYCLE_J_POLICY_PRESENT=
LIFECYCLE_J_POLICY_MODE=
LIFECYCLE_J_POLICY_VALUE=
LIFECYCLE_J_CONTAINER_PRESENT=
LIFECYCLE_J_CONTAINER_ID=
LIFECYCLE_J_CONTAINER_NAME=
LIFECYCLE_J_CONTAINER_PROJECT=
LIFECYCLE_J_CONTAINER_OWNER=
LIFECYCLE_J_CONTAINER_WORKDIR=
LIFECYCLE_J_CONTAINER_STATE=
LIFECYCLE_J_NM_ENABLED=
LIFECYCLE_J_NM_CONFIGURED=
LIFECYCLE_J_NM_ACTIVE=
LIFECYCLE_J_NM_DEVICE=
LIFECYCLE_J_NM_OWNERSHIP=
LIFECYCLE_J_NM_OWNED_UUID=
LIFECYCLE_J_RETEST_GENERATION=
LIFECYCLE_J_RETEST_REQUEST=
LIFECYCLE_RECOVERY_DEADLINE=0
LIFECYCLE_RECOVERY_MODE=0

lifecycle_journal_keys_are_known() {
  awk -F= '
    BEGIN {
      split("schema operation phase id base project owner snapshot_dir post_ready policy_present policy_mode container_present container_id container_name container_project container_owner container_workdir container_state networkmanager_enabled nm_configured nm_active nm_device nm_ownership nm_owned_uuid retest_generation retest_request", keys, " ")
      for (i in keys) allowed[keys[i]] = 1
    }
    ($1 == "" || !allowed[$1]) { exit 1 }
  ' "$LIFECYCLE_JOURNAL"
}

lifecycle_journal_field() {
  local field=$1
  awk -F= -v wanted="$field" '
    $1 == wanted { count++; value=substr($0, index($0, "=") + 1) }
    END { if (count != 1) exit 1; print value }
  ' "$LIFECYCLE_JOURNAL"
}

lifecycle_post_state_keys_are_known() {
  local path=$1
  awk -F= '
    BEGIN {
      split("schema operation policy_present policy_mode policy_value container_present container_id container_name container_project container_owner container_workdir container_state networkmanager_enabled nm_configured nm_active nm_device nm_ownership nm_owned_uuid retest_generation retest_request", keys, " ")
      for (i in keys) allowed[keys[i]] = 1
    }
    ($1 == "" || !allowed[$1]) { exit 1 }
  ' "$path"
}

lifecycle_post_state_field() {
  local path=$1 field=$2
  awk -F= -v wanted="$field" '
    $1 == wanted { count++; value=substr($0, index($0, "=") + 1) }
    END { if (count != 1) exit 1; print value }
  ' "$path"
}

lifecycle_read_post_state() {
  local path="$LIFECYCLE_J_DIR/post-state" schema
  [[ -f "$path" && ! -L "$path" ]] || return 1
  lifecycle_private_file_ok "$path" 600 || return 1
  lifecycle_post_state_keys_are_known "$path" || return 1
  schema=$(lifecycle_post_state_field "$path" schema) || return 1
  [[ "$schema" == 1 ]] || return 1
  LIFECYCLE_J_POST_POLICY_PRESENT=$(lifecycle_post_state_field "$path" policy_present) || return 1
  LIFECYCLE_J_POST_POLICY_MODE=$(lifecycle_post_state_field "$path" policy_mode) || return 1
  LIFECYCLE_J_POST_POLICY_VALUE=$(lifecycle_post_state_field "$path" policy_value) || return 1
  LIFECYCLE_J_POST_CONTAINER_PRESENT=$(lifecycle_post_state_field "$path" container_present) || return 1
  LIFECYCLE_J_POST_CONTAINER_ID=$(lifecycle_post_state_field "$path" container_id) || return 1
  LIFECYCLE_J_POST_CONTAINER_NAME=$(lifecycle_post_state_field "$path" container_name) || return 1
  LIFECYCLE_J_POST_CONTAINER_PROJECT=$(lifecycle_post_state_field "$path" container_project) || return 1
  LIFECYCLE_J_POST_CONTAINER_OWNER=$(lifecycle_post_state_field "$path" container_owner) || return 1
  LIFECYCLE_J_POST_CONTAINER_WORKDIR=$(lifecycle_post_state_field "$path" container_workdir) || return 1
  LIFECYCLE_J_POST_CONTAINER_STATE=$(lifecycle_post_state_field "$path" container_state) || return 1
  LIFECYCLE_J_POST_NM_ENABLED=$(lifecycle_post_state_field "$path" networkmanager_enabled) || return 1
  LIFECYCLE_J_POST_NM_CONFIGURED=$(lifecycle_post_state_field "$path" nm_configured) || return 1
  LIFECYCLE_J_POST_NM_ACTIVE=$(lifecycle_post_state_field "$path" nm_active) || return 1
  LIFECYCLE_J_POST_NM_DEVICE=$(lifecycle_post_state_field "$path" nm_device) || return 1
  LIFECYCLE_J_POST_NM_OWNERSHIP=$(lifecycle_post_state_field "$path" nm_ownership) || return 1
  LIFECYCLE_J_POST_NM_OWNED_UUID=$(lifecycle_post_state_field "$path" nm_owned_uuid) || return 1
  LIFECYCLE_J_POST_RETEST_GENERATION=$(lifecycle_post_state_field "$path" retest_generation) || return 1
  LIFECYCLE_J_POST_RETEST_REQUEST=$(lifecycle_post_state_field "$path" retest_request) || return 1

  [[ "$(lifecycle_post_state_field "$path" operation)" == "$LIFECYCLE_J_OPERATION" ]] || return 1
  [[ "$LIFECYCLE_J_POST_POLICY_PRESENT" == yes || "$LIFECYCLE_J_POST_POLICY_PRESENT" == no ]] || return 1
  [[ "$LIFECYCLE_J_POST_POLICY_MODE" == none || "$LIFECYCLE_J_POST_POLICY_MODE" =~ ^[0-7]{3,4}$ ]] || return 1
  [[ "$LIFECYCLE_J_POST_POLICY_VALUE" == strict || "$LIFECYCLE_J_POST_POLICY_VALUE" == smart ]] || return 1
  [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == yes || "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == no ]] || return 1
  if [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == yes ]]; then
    [[ -n "$LIFECYCLE_J_POST_CONTAINER_ID" ]] || return 1
    case "$LIFECYCLE_J_POST_CONTAINER_STATE" in healthy|running|stopped|unknown) ;; *) return 1 ;; esac
    lifecycle_scalar_ok "$LIFECYCLE_J_POST_CONTAINER_ID" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_POST_CONTAINER_NAME" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_POST_CONTAINER_PROJECT" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_POST_CONTAINER_OWNER" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_POST_CONTAINER_WORKDIR" || return 1
  else
    [[ -z "$LIFECYCLE_J_POST_CONTAINER_ID" && -z "$LIFECYCLE_J_POST_CONTAINER_NAME" &&
       -z "$LIFECYCLE_J_POST_CONTAINER_PROJECT" && -z "$LIFECYCLE_J_POST_CONTAINER_OWNER" &&
       -z "$LIFECYCLE_J_POST_CONTAINER_WORKDIR" && "$LIFECYCLE_J_POST_CONTAINER_STATE" == absent ]] || return 1
  fi
  [[ "$LIFECYCLE_J_POST_NM_ENABLED" == yes || "$LIFECYCLE_J_POST_NM_ENABLED" == no ]] || return 1
  for value in "$LIFECYCLE_J_POST_NM_CONFIGURED" "$LIFECYCLE_J_POST_NM_ACTIVE" \
      "$LIFECYCLE_J_POST_NM_DEVICE" "$LIFECYCLE_J_POST_NM_OWNERSHIP"; do
    lifecycle_scalar_ok "$value" || return 1
  done
  [[ "$LIFECYCLE_J_POST_NM_OWNED_UUID" == '' || "$LIFECYCLE_J_POST_NM_OWNED_UUID" =~ $NETWORKMANAGER_UUID_PATTERN ]] || return 1
  [[ "$LIFECYCLE_J_POST_RETEST_REQUEST" == absent || "$LIFECYCLE_J_POST_RETEST_REQUEST" == present ]] || return 1
  [[ -z "$LIFECYCLE_J_POST_RETEST_GENERATION" || "$LIFECYCLE_J_POST_RETEST_GENERATION" =~ ^[0-9]+$ ]] || return 1
}

lifecycle_read_journal() {
  lifecycle_transaction_shape_ok || return 1
  [[ -f "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]] || return 1
  lifecycle_journal_keys_are_known || return 1
  local schema
  schema=$(lifecycle_journal_field schema) || return 1
  [[ "$schema" == 1 ]] || return 1
  LIFECYCLE_J_OPERATION=$(lifecycle_journal_field operation) || return 1
  LIFECYCLE_J_PHASE=$(lifecycle_journal_field phase) || return 1
  LIFECYCLE_J_ID=$(lifecycle_journal_field id) || return 1
  LIFECYCLE_J_BASE=$(lifecycle_journal_field base) || return 1
  LIFECYCLE_J_PROJECT=$(lifecycle_journal_field project) || return 1
  LIFECYCLE_J_OWNER=$(lifecycle_journal_field owner) || return 1
  LIFECYCLE_J_DIR=$(lifecycle_journal_field snapshot_dir) || return 1
  LIFECYCLE_J_POST_READY=$(lifecycle_journal_field post_ready) || return 1
  LIFECYCLE_J_POLICY_PRESENT=$(lifecycle_journal_field policy_present) || return 1
  LIFECYCLE_J_POLICY_MODE=$(lifecycle_journal_field policy_mode) || return 1
  LIFECYCLE_J_CONTAINER_PRESENT=$(lifecycle_journal_field container_present) || return 1
  LIFECYCLE_J_CONTAINER_ID=$(lifecycle_journal_field container_id) || return 1
  LIFECYCLE_J_CONTAINER_NAME=$(lifecycle_journal_field container_name) || return 1
  LIFECYCLE_J_CONTAINER_PROJECT=$(lifecycle_journal_field container_project) || return 1
  LIFECYCLE_J_CONTAINER_OWNER=$(lifecycle_journal_field container_owner) || return 1
  LIFECYCLE_J_CONTAINER_WORKDIR=$(lifecycle_journal_field container_workdir) || return 1
  LIFECYCLE_J_CONTAINER_STATE=$(lifecycle_journal_field container_state) || return 1
  LIFECYCLE_J_NM_ENABLED=$(lifecycle_journal_field networkmanager_enabled) || return 1
  LIFECYCLE_J_NM_CONFIGURED=$(lifecycle_journal_field nm_configured) || return 1
  LIFECYCLE_J_NM_ACTIVE=$(lifecycle_journal_field nm_active) || return 1
  LIFECYCLE_J_NM_DEVICE=$(lifecycle_journal_field nm_device) || return 1
  LIFECYCLE_J_NM_OWNERSHIP=$(lifecycle_journal_field nm_ownership) || return 1
  LIFECYCLE_J_NM_OWNED_UUID=$(lifecycle_journal_field nm_owned_uuid) || return 1
  LIFECYCLE_J_RETEST_GENERATION=$(lifecycle_journal_field retest_generation) || return 1
  LIFECYCLE_J_RETEST_REQUEST=$(lifecycle_journal_field retest_request) || return 1

  [[ "$LIFECYCLE_J_OPERATION" == disconnect || "$LIFECYCLE_J_OPERATION" == start || "$LIFECYCLE_J_OPERATION" == stop || \
     "$LIFECYCLE_J_OPERATION" == toggle || "$LIFECYCLE_J_OPERATION" == retest ]] || return 1
  case "$LIFECYCLE_J_PHASE" in preparing|prepared|render|compose-up|compose-up-done|nm-work|host-smoke|nm-disconnect|nm-disconnect-done|compose-down|compose-down-done|toggle-candidate|toggle-candidate-done|policy-commit|go-state|go-state-done|runtime-wait|committing|committed|compensating) ;; *) return 1 ;; esac
  [[ "$LIFECYCLE_J_POST_READY" == yes || "$LIFECYCLE_J_POST_READY" == no ]] || return 1
  if [[ "$LIFECYCLE_J_PHASE" == committed || "$LIFECYCLE_J_PHASE" == committing ]]; then
    [[ "$LIFECYCLE_J_POST_READY" == yes ]] || return 1
  else
    [[ "$LIFECYCLE_J_POST_READY" == no ]] || return 1
  fi
  [[ "$LIFECYCLE_J_BASE" == "$BASE" && "$LIFECYCLE_J_PROJECT" == "$PROJECT" && "$LIFECYCLE_J_OWNER" == "$LOCAL_RESOURCE_OWNER" ]] || return 1
  [[ "$LIFECYCLE_J_ID" =~ ^txn\.[A-Za-z0-9]+$ ]] || return 1
  [[ "$LIFECYCLE_J_DIR" == "$LIFECYCLE_TX_ROOT/$LIFECYCLE_J_ID" ]] || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_J_DIR" || return 1
  if [[ "$LIFECYCLE_J_PHASE" == committed || "$LIFECYCLE_J_PHASE" == committing ]]; then
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/post-state" 600 || return 1
  fi
  [[ "$LIFECYCLE_J_DIR" != *$'\n'* && "$LIFECYCLE_J_DIR" != *$'\r'* ]] || return 1
  path_has_symlink "$LIFECYCLE_J_DIR" && return 1
  [[ -z "$(find -P -- "$LIFECYCLE_J_DIR" -type l -print -quit 2>/dev/null)" ]] || return 1
  [[ -z "$(find -P -- "$LIFECYCLE_J_DIR" -type f -links +1 -print -quit 2>/dev/null)" ]] || return 1
  [[ -z "$(find -P -- "$LIFECYCLE_J_DIR" -type f ! -perm 600 -print -quit 2>/dev/null)" ]] || return 1
  [[ "$LIFECYCLE_J_POLICY_PRESENT" == yes || "$LIFECYCLE_J_POLICY_PRESENT" == no ]] || return 1
  [[ "$LIFECYCLE_J_POLICY_MODE" == none || "$LIFECYCLE_J_POLICY_MODE" =~ ^[0-7]{3,4}$ ]] || return 1
  [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == yes || "$LIFECYCLE_J_CONTAINER_PRESENT" == no ]] || return 1
  if [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == yes ]]; then
    [[ "$LIFECYCLE_J_CONTAINER_ID" != *$'\n'* && -n "$LIFECYCLE_J_CONTAINER_ID" ]] || return 1
    [[ "$LIFECYCLE_J_CONTAINER_STATE" == healthy || "$LIFECYCLE_J_CONTAINER_STATE" == running || "$LIFECYCLE_J_CONTAINER_STATE" == stopped || "$LIFECYCLE_J_CONTAINER_STATE" == unknown ]] || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_ID" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_NAME" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_PROJECT" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_OWNER" || return 1
    lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_WORKDIR" || return 1
  else
    [[ -z "$LIFECYCLE_J_CONTAINER_ID" && -z "$LIFECYCLE_J_CONTAINER_NAME" ]] || return 1
    [[ "$LIFECYCLE_J_CONTAINER_STATE" == absent ]] || return 1
  fi
  [[ "$LIFECYCLE_J_NM_ENABLED" == yes || "$LIFECYCLE_J_NM_ENABLED" == no ]] || return 1
  for value in "$LIFECYCLE_J_NM_CONFIGURED" "$LIFECYCLE_J_NM_ACTIVE" "$LIFECYCLE_J_NM_DEVICE" "$LIFECYCLE_J_NM_OWNERSHIP"; do
    lifecycle_scalar_ok "$value" || return 1
  done
  [[ "$LIFECYCLE_J_NM_OWNED_UUID" == '' || "$LIFECYCLE_J_NM_OWNED_UUID" =~ $NETWORKMANAGER_UUID_PATTERN ]] || return 1
  [[ "$LIFECYCLE_J_RETEST_REQUEST" == absent || "$LIFECYCLE_J_RETEST_REQUEST" == present ]] || return 1
  local marker name
  for marker in policy.present policy.mode policy.value container.present container.id container.name container.project container.owner container.workdir container.state retest.generation retest.request; do
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/$marker" 600 || return 1
  done
  lifecycle_private_file_ok "$LIFECYCLE_J_DIR/policy.present" 600 || return 1
  lifecycle_private_file_ok "$LIFECYCLE_J_DIR/policy.mode" 600 || return 1
  lifecycle_private_file_ok "$LIFECYCLE_J_DIR/policy.value" 600 || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/policy.present")" == "$LIFECYCLE_J_POLICY_PRESENT" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/policy.mode")" == "$LIFECYCLE_J_POLICY_MODE" ]] || return 1
  if [[ "$LIFECYCLE_J_POLICY_PRESENT" == yes ]]; then
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/policy" 600 || return 1
  else
    [[ ! -e "$LIFECYCLE_J_DIR/policy" && ! -L "$LIFECYCLE_J_DIR/policy" ]] || return 1
  fi
  LIFECYCLE_J_POLICY_VALUE=$(<"$LIFECYCLE_J_DIR/policy.value") || return 1
  [[ "$LIFECYCLE_J_POLICY_VALUE" == strict || "$LIFECYCLE_J_POLICY_VALUE" == smart ]] || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_J_DIR/nm" || return 1
  for marker in enabled configured active device ownership owned-uuid foreign-active.present foreign-active.mode; do
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/nm/$marker" 600 || return 1
  done
  [[ "$(<"$LIFECYCLE_J_DIR/nm/enabled")" == "$LIFECYCLE_J_NM_ENABLED" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/nm/configured")" == "$LIFECYCLE_J_NM_CONFIGURED" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/nm/active")" == "$LIFECYCLE_J_NM_ACTIVE" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/nm/device")" == "$LIFECYCLE_J_NM_DEVICE" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/nm/ownership")" == "$LIFECYCLE_J_NM_OWNERSHIP" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/nm/owned-uuid")" == "$LIFECYCLE_J_NM_OWNED_UUID" ]] || return 1
  if [[ "$(<"$LIFECYCLE_J_DIR/nm/foreign-active.present")" == yes ]]; then
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/nm/foreign-active" 600 || return 1
  else
    [[ "$(<"$LIFECYCLE_J_DIR/nm/foreign-active.present")" == no ]] || return 1
  fi
  [[ "$(<"$LIFECYCLE_J_DIR/retest.generation")" == "$LIFECYCLE_J_RETEST_GENERATION" ]] || return 1
  [[ "$(<"$LIFECYCLE_J_DIR/retest.request")" == "$LIFECYCLE_J_RETEST_REQUEST" ]] || return 1
  if [[ "$LIFECYCLE_J_PHASE" == committed ]]; then
    lifecycle_read_post_state || return 1
  fi

  # Load the durable record into the common transaction slots so phase
  # updates during crash recovery preserve every pre-state field verbatim.
  LIFECYCLE_TX_DIR=$LIFECYCLE_J_DIR
  LIFECYCLE_TX_ID=$LIFECYCLE_J_ID
  LIFECYCLE_TX_OPERATION=$LIFECYCLE_J_OPERATION
  LIFECYCLE_TX_PHASE=$LIFECYCLE_J_PHASE
  LIFECYCLE_TX_POST_READY=$LIFECYCLE_J_POST_READY
  LIFECYCLE_TX_PREPARED=1
  LIFECYCLE_POLICY_PRESENT=$LIFECYCLE_J_POLICY_PRESENT
  LIFECYCLE_POLICY_MODE=$LIFECYCLE_J_POLICY_MODE
  LIFECYCLE_POLICY_BEFORE=$LIFECYCLE_J_POLICY_VALUE
  LIFECYCLE_CONTAINER_PRESENT=$LIFECYCLE_J_CONTAINER_PRESENT
  LIFECYCLE_CONTAINER_ID=$LIFECYCLE_J_CONTAINER_ID
  LIFECYCLE_CONTAINER_NAME=$LIFECYCLE_J_CONTAINER_NAME
  LIFECYCLE_CONTAINER_PROJECT=$LIFECYCLE_J_CONTAINER_PROJECT
  LIFECYCLE_CONTAINER_OWNER=$LIFECYCLE_J_CONTAINER_OWNER
  LIFECYCLE_CONTAINER_WORKDIR=$LIFECYCLE_J_CONTAINER_WORKDIR
  LIFECYCLE_CONTAINER_STATE=$LIFECYCLE_J_CONTAINER_STATE
  LIFECYCLE_NM_ENABLED=$LIFECYCLE_J_NM_ENABLED
  LIFECYCLE_NM_CONFIGURED=$LIFECYCLE_J_NM_CONFIGURED
  LIFECYCLE_NM_ACTIVE=$LIFECYCLE_J_NM_ACTIVE
  LIFECYCLE_NM_DEVICE=$LIFECYCLE_J_NM_DEVICE
  LIFECYCLE_NM_OWNERSHIP=$LIFECYCLE_J_NM_OWNERSHIP
  LIFECYCLE_NM_OWNED_UUID=$LIFECYCLE_J_NM_OWNED_UUID
  LIFECYCLE_RETEST_GENERATION=$LIFECYCLE_J_RETEST_GENERATION
  LIFECYCLE_RETEST_REQUEST=$LIFECYCLE_J_RETEST_REQUEST
}

lifecycle_current_policy_matches_journal() {
  policy_path_is_safe || return 1
  if [[ "$LIFECYCLE_J_POLICY_PRESENT" == yes ]]; then
    [[ -f "$POLICY_FILE" && ! -L "$POLICY_FILE" ]] || return 1
    local mode
    mode=$(stat -c '%a' -- "$POLICY_FILE" 2>/dev/null) || return 1
    [[ "$mode" == "$LIFECYCLE_J_POLICY_MODE" ]] || return 1
    cmp -s -- "$LIFECYCLE_J_DIR/policy" "$POLICY_FILE"
  else
    [[ ! -e "$POLICY_FILE" && ! -L "$POLICY_FILE" ]] || return 1
    [[ "$(safe_policy)" == "$LIFECYCLE_J_POLICY_VALUE" ]]
  fi
}

lifecycle_current_nm_matches_journal() {
  if [[ "$LIFECYCLE_J_NM_ENABLED" == no ]]; then
    networkmanager_enabled && return 1
    return 0
  fi
  networkmanager_enabled || return 1
  read_networkmanager_state || return 1
  [[ "$NM_CONFIGURED" == "$LIFECYCLE_J_NM_CONFIGURED" &&
     "$NM_ACTIVE" == "$LIFECYCLE_J_NM_ACTIVE" &&
     "$NM_DEVICE" == "$LIFECYCLE_J_NM_DEVICE" &&
     "$NM_OWNERSHIP" == "$LIFECYCLE_J_NM_OWNERSHIP" &&
     "${NM_OWNED_UUID,,}" == "${LIFECYCLE_J_NM_OWNED_UUID,,}" ]] || return 1
  owned_nm_capability_matches_snapshot "$LIFECYCLE_J_DIR/nm" || return 1
  local active_tmp present
  active_tmp=$(mktemp "$LIFECYCLE_J_DIR/.recovery-active.XXXXXX") || return 1
  chmod 600 "$active_tmp" || { rm -f -- "$active_tmp"; return 1; }
  if ! capture_active_vpn_identities "$active_tmp" "$NM_OWNED_UUID"; then
    rm -f -- "$active_tmp"
    return 1
  fi
  present=$(<"$LIFECYCLE_J_DIR/nm/foreign-active.present") || { rm -f -- "$active_tmp"; return 1; }
  if [[ "$present" == yes ]]; then
    cmp -s "$active_tmp" "$LIFECYCLE_J_DIR/nm/foreign-active"
  else
    [[ ! -s "$active_tmp" ]]
  fi
  local rc=$?
  rm -f -- "$active_tmp"
  return "$rc"
}

lifecycle_current_container_matches_journal() {
  local state current name project owner workdir
  local_compose_preflight || return 1
  current=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == no ]]; then
    [[ -z "$current" ]] || return 1
    return 0
  fi
  [[ "$current" == "$LIFECYCLE_J_CONTAINER_ID" ]] || return 1
  name=$(local_resource_name container "$current") || return 1
  project=$(local_resource_label container "$current" com.docker.compose.project) || return 1
  owner=$(local_resource_label container "$current" com.vpnkit.local.owner) || return 1
  workdir=$(local_resource_label container "$current" com.docker.compose.project.working_dir) || return 1
  [[ "$name" == "$LIFECYCLE_J_CONTAINER_NAME" && "$project" == "$LIFECYCLE_J_CONTAINER_PROJECT" &&
     "$owner" == "$LIFECYCLE_J_CONTAINER_OWNER" && "$workdir" == "$LIFECYCLE_J_CONTAINER_WORKDIR" ]] || return 1
  state=$(container_state "$current")
  [[ "$state" == "$LIFECYCLE_J_CONTAINER_STATE" ]]
}

lifecycle_current_retest_matches_journal() {
  [[ "$LIFECYCLE_J_OPERATION" == retest ]] || return 0
  [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == yes && -n "$LIFECYCLE_J_RETEST_GENERATION" ]] || return 1
  local current
  current=$(container_runtime_generation "$LIFECYCLE_J_CONTAINER_ID" 2>/dev/null) || return 1
  [[ "$current" == "$LIFECYCLE_J_RETEST_GENERATION" ]] || return 1
  container_request_absent "$LIFECYCLE_J_CONTAINER_ID" 2>/dev/null
}

lifecycle_current_post_state_matches_journal() {
  [[ "$LIFECYCLE_J_PHASE" == committed && "$LIFECYCLE_J_POST_READY" == yes ]] || return 1
  lifecycle_recovery_budget_ok || return 1
  policy_path_is_safe || return 1
  if [[ "$LIFECYCLE_J_POST_POLICY_PRESENT" == yes ]]; then
    [[ -f "$POLICY_FILE" && ! -L "$POLICY_FILE" ]] || return 1
    lifecycle_private_file_ok "$POLICY_FILE" || return 1
    local mode
    mode=$(stat -c '%a' -- "$POLICY_FILE" 2>/dev/null) || return 1
    [[ "$mode" == "$LIFECYCLE_J_POST_POLICY_MODE" ]] || return 1
  else
    [[ ! -e "$POLICY_FILE" && ! -L "$POLICY_FILE" ]] || return 1
  fi
  [[ "$(safe_policy)" == "$LIFECYCLE_J_POST_POLICY_VALUE" ]] || return 1

  lifecycle_recovery_budget_ok || return 1
  local current name project owner workdir state
  # This is deliberately a complete ownership proof even for an absent
  # container. A failed read is not equivalent to a verified empty result.
  local_compose_preflight || return 1
  current=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == no ]]; then
    [[ -z "$current" && "$LIFECYCLE_J_POST_CONTAINER_STATE" == absent ]] || return 1
  else
    [[ "$current" == "$LIFECYCLE_J_POST_CONTAINER_ID" ]] || return 1
    name=$(local_resource_name container "$current") || return 1
    project=$(local_resource_label container "$current" com.docker.compose.project) || return 1
    owner=$(local_resource_label container "$current" com.vpnkit.local.owner) || return 1
    workdir=$(local_resource_label container "$current" com.docker.compose.project.working_dir) || return 1
    [[ "$name" == "$LIFECYCLE_J_POST_CONTAINER_NAME" &&
       "$project" == "$LIFECYCLE_J_POST_CONTAINER_PROJECT" &&
       "$owner" == "$LIFECYCLE_J_POST_CONTAINER_OWNER" &&
       "$workdir" == "$LIFECYCLE_J_POST_CONTAINER_WORKDIR" ]] || return 1
    state=$(container_state "$current")
    [[ "$state" == "$LIFECYCLE_J_POST_CONTAINER_STATE" ]] || return 1
  fi

  lifecycle_recovery_budget_ok || return 1
  if [[ "$LIFECYCLE_J_POST_NM_ENABLED" == no ]]; then
    networkmanager_enabled && return 1
  else
    networkmanager_enabled || return 1
    read_networkmanager_state || return 1
    [[ "$NM_CONFIGURED" == "$LIFECYCLE_J_POST_NM_CONFIGURED" &&
       "$NM_ACTIVE" == "$LIFECYCLE_J_POST_NM_ACTIVE" &&
       "$NM_DEVICE" == "$LIFECYCLE_J_POST_NM_DEVICE" &&
       "$NM_OWNERSHIP" == "$LIFECYCLE_J_POST_NM_OWNERSHIP" &&
       "${NM_OWNED_UUID,,}" == "${LIFECYCLE_J_POST_NM_OWNED_UUID,,}" ]] || return 1
  fi

  if [[ "$LIFECYCLE_J_OPERATION" == retest ]]; then
    [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == yes &&
       "$LIFECYCLE_J_POST_CONTAINER_STATE" == healthy &&
       -n "$LIFECYCLE_J_POST_RETEST_GENERATION" &&
       "$LIFECYCLE_J_POST_RETEST_REQUEST" == absent ]] || return 1
    lifecycle_recovery_budget_ok || return 1
    state=$(container_runtime_generation "$LIFECYCLE_J_POST_CONTAINER_ID") || return 1
    [[ "$state" == "$LIFECYCLE_J_POST_RETEST_GENERATION" ]] || return 1
    container_request_absent "$LIFECYCLE_J_POST_CONTAINER_ID" || return 1
  fi

  case "$LIFECYCLE_J_OPERATION" in
    disconnect)
      [[ "$LIFECYCLE_J_POST_CONTAINER_ID" == "$LIFECYCLE_J_CONTAINER_ID" && "$LIFECYCLE_J_POST_CONTAINER_STATE" == "$LIFECYCLE_J_CONTAINER_STATE" ]] || return 1
      [[ "$LIFECYCLE_J_POST_NM_ENABLED" == no || "$LIFECYCLE_J_POST_NM_ACTIVE" == no ]] || return 1
      ;;
    start)
      [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == yes && "$LIFECYCLE_J_POST_CONTAINER_STATE" == healthy ]] || return 1
      ;;
    stop)
      [[ "$LIFECYCLE_J_POST_CONTAINER_PRESENT" == no && "$LIFECYCLE_J_POST_CONTAINER_STATE" == absent ]] || return 1
      ;;
    toggle)
      [[ "$LIFECYCLE_J_POST_POLICY_VALUE" != "$LIFECYCLE_J_POLICY_VALUE" ]] || return 1
      ;;
    retest) ;;
    *) return 1 ;;
  esac
}

lifecycle_recovery_preflight() {
  lifecycle_read_journal || return 1
  policy_path_is_safe || return 1
  if [[ -e "$POLICY_FILE" ]]; then lifecycle_private_file_ok "$POLICY_FILE" || return 1; fi
  # This is an ownership-only preflight. The candidate container and local NM
  # capability are expected to differ after a crash; exact equality is checked
  # before cleanup and after compensation, not used to authorize cleanup.
  local_compose_preflight || return 1
  if [[ "$LIFECYCLE_J_NM_ENABLED" == yes ]]; then
    networkmanager_enabled || return 1
    read_networkmanager_state || return 1
    local active_tmp present
    active_tmp=$(mktemp "$LIFECYCLE_J_DIR/.recovery-preflight.XXXXXX") || return 1
    chmod 600 "$active_tmp" || { rm -f -- "$active_tmp"; return 1; }
    if ! capture_active_vpn_identities "$active_tmp" "$NM_OWNED_UUID"; then
      rm -f -- "$active_tmp"
      return 1
    fi
    present=$(<"$LIFECYCLE_J_DIR/nm/foreign-active.present") || { rm -f -- "$active_tmp"; return 1; }
    if [[ "$present" == yes ]]; then
      cmp -s "$active_tmp" "$LIFECYCLE_J_DIR/nm/foreign-active"
    else
      [[ ! -s "$active_tmp" ]]
    fi
    local rc=$?
    rm -f -- "$active_tmp"
    (( rc == 0 )) || return "$rc"
  else
    networkmanager_enabled && return 1
  fi
  # A retest cannot safely compensate a committed Go state change from this
  # shell. Require its runtime generation/request to remain at the snapshot;
  # otherwise retain the journal and fail closed for a later state-aware retry.
  lifecycle_current_retest_matches_journal
}

lifecycle_restore_policy_from_journal() {
  policy_path_is_safe || return 1
  local parent tmp mode
  parent=$(dirname -- "$POLICY_FILE")
  [[ -d "$parent" && ! -L "$parent" ]] || return 1
  if [[ "$LIFECYCLE_J_POLICY_PRESENT" == yes ]]; then
    lifecycle_private_file_ok "$LIFECYCLE_J_DIR/policy" 600 || return 1
    mode=$LIFECYCLE_J_POLICY_MODE
    tmp=$(mktemp "$parent/.lifecycle-policy-restore.XXXXXX") || return 1
    if ! chmod 600 "$tmp" || ! cat -- "$LIFECYCLE_J_DIR/policy" >"$tmp" || ! chmod "$mode" "$tmp" || ! lifecycle_sync_durable "$tmp"; then
      rm -f -- "$tmp"
      return 1
    fi
    mv -fT -- "$tmp" "$POLICY_FILE" || { rm -f -- "$tmp"; return 1; }
    lifecycle_sync_durable "$POLICY_FILE" || return 1
  else
    [[ ! -L "$POLICY_FILE" ]] || return 1
    [[ ! -e "$POLICY_FILE" ]] || { lifecycle_private_file_ok "$POLICY_FILE" || return 1; rm -f -- "$POLICY_FILE" || return 1; }
    lifecycle_sync_durable "$parent" || return 1
  fi
  lifecycle_current_policy_matches_journal
}

lifecycle_recovery_budget_start() {
  local timeout
  timeout=$(lifecycle_compensation_timeout_seconds) || return 1
  LIFECYCLE_RECOVERY_DEADLINE=$((SECONDS + timeout))
}

lifecycle_recovery_budget_ok() {
  (( LIFECYCLE_RECOVERY_DEADLINE > 0 && SECONDS < LIFECYCLE_RECOVERY_DEADLINE ))
}

lifecycle_bounded_external() {
  lifecycle_recovery_budget_ok || return 124
  local remaining=$((LIFECYCLE_RECOVERY_DEADLINE - SECONDS))
  (( remaining >= 1 )) || return 124
  command -v timeout >/dev/null 2>&1 || return 125
  # Direct shell recovery keeps timeout's normal process-group supervision.
  # Under the TUI, timeout must stay in the already supervised group; its
  # default process-group split would otherwise strand Docker/NM descendants
  # beyond the TUI TERM/KILL boundary.
  if [[ "${VPNKIT_TUI_SUPERVISED:-0}" == 1 ]]; then
    timeout --foreground -k 1s "$remaining" "$@"
  else
    timeout -k 1s "$remaining" "$@"
  fi
}

lifecycle_expected_container_exists() {
  [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == yes ]] || return 1
  lifecycle_scalar_ok "$LIFECYCLE_J_CONTAINER_ID" || return 1
  local_compose_resource_owned container "$LIFECYCLE_J_CONTAINER_ID" || return 1
  local name project owner workdir
  name=$(local_resource_name container "$LIFECYCLE_J_CONTAINER_ID") || return 1
  project=$(local_resource_label container "$LIFECYCLE_J_CONTAINER_ID" com.docker.compose.project) || return 1
  owner=$(local_resource_label container "$LIFECYCLE_J_CONTAINER_ID" com.vpnkit.local.owner) || return 1
  workdir=$(local_resource_label container "$LIFECYCLE_J_CONTAINER_ID" com.docker.compose.project.working_dir) || return 1
  [[ "$name" == "$LIFECYCLE_J_CONTAINER_NAME" && "$project" == "$LIFECYCLE_J_CONTAINER_PROJECT" &&
     "$owner" == "$LIFECYCLE_J_CONTAINER_OWNER" && "$workdir" == "$LIFECYCLE_J_CONTAINER_WORKDIR" ]]
}

lifecycle_restore_container_from_journal() {
  local current expected_state actual_state
  if ! local_compose_preflight; then return 1; fi
  current=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ "$LIFECYCLE_J_CONTAINER_PRESENT" == no ]]; then
    if [[ -n "$current" ]]; then
      local_compose_preflight_for_container "$current" || return 1
      lifecycle_docker_command rm -f "$current" || return 1
    fi
    local_compose_preflight || return 1
    [[ -z "$LOCAL_PREFLIGHT_CONTAINER_ID" ]] || return 1
    return 0
  fi

  # A recreated candidate may have displaced the original. Never substitute a
  # new Compose container for the exact recorded identity. If the old ID is
  # gone, recovery remains journaled and fails closed.
  if [[ -n "$current" && "$current" != "$LIFECYCLE_J_CONTAINER_ID" ]]; then
    local_compose_preflight_for_container "$current" || return 1
    lifecycle_docker_command rm -f "$current" || return 1
    local_compose_preflight || return 1
    [[ -z "$LOCAL_PREFLIGHT_CONTAINER_ID" || "$LOCAL_PREFLIGHT_CONTAINER_ID" == "$LIFECYCLE_J_CONTAINER_ID" ]] || return 1
  fi
  lifecycle_expected_container_exists || return 1
  actual_state=$(lifecycle_docker_command inspect "$LIFECYCLE_J_CONTAINER_ID" --format '{{if not .State.Running}}stopped{{else if .State.Health}}{{.State.Health.Status}}{{else}}running{{end}}' 2>/dev/null) || return 1
  expected_state=$LIFECYCLE_J_CONTAINER_STATE
  case "$expected_state:$actual_state" in
    stopped:stopped|running:running|healthy:healthy) ;;
    stopped:running|stopped:healthy)
      local_compose_preflight_for_container "$LIFECYCLE_J_CONTAINER_ID" || return 1
      lifecycle_docker_command stop "$LIFECYCLE_J_CONTAINER_ID" || return 1
      ;;
    running:stopped|healthy:stopped)
      local_compose_preflight_for_container "$LIFECYCLE_J_CONTAINER_ID" || return 1
      lifecycle_docker_command start "$LIFECYCLE_J_CONTAINER_ID" || return 1
      ;;
    running:healthy) [[ "$expected_state" == running ]] || return 1 ;;
    healthy:running)
      local_compose_preflight_for_container "$LIFECYCLE_J_CONTAINER_ID" || return 1
      lifecycle_docker_command restart "$LIFECYCLE_J_CONTAINER_ID" || return 1
      ;;
    *) return 1 ;;
  esac
  local_compose_preflight || return 1
  [[ "$LOCAL_PREFLIGHT_CONTAINER_ID" == "$LIFECYCLE_J_CONTAINER_ID" ]] || return 1
  if [[ "$expected_state" == healthy ]]; then wait_healthy || return 1; fi
  lifecycle_current_container_matches_journal
}

lifecycle_remove_unpublished_transaction() {
  [[ -n "$LIFECYCLE_TX_DIR" && "$LIFECYCLE_TX_DIR" == "$LIFECYCLE_TX_ROOT/"* ]] || return 0
  [[ ! -e "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]] || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_TX_DIR" || return 1
  rm -rf -- "$LIFECYCLE_TX_DIR" || return 1
  lifecycle_sync_durable "$LIFECYCLE_TX_ROOT"
}

lifecycle_cleanup_orphan_transactions() {
  [[ ! -e "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]] || return 1
  [[ -e "$LIFECYCLE_TX_ROOT" ]] || return 0
  lifecycle_private_dir_ok "$LIFECYCLE_TX_ROOT" || return 1
  local directory name
  while IFS= read -r -d '' directory; do
    name=${directory##*/}
    [[ "$name" =~ ^txn\.[A-Za-z0-9]+$ ]] || return 1
    lifecycle_private_dir_ok "$directory" || return 1
    rm -rf -- "$directory" || return 1
  done < <(find -P -- "$LIFECYCLE_TX_ROOT" -mindepth 1 -maxdepth 1 -print0 2>/dev/null)
  lifecycle_sync_durable "$LIFECYCLE_TX_ROOT"
}

lifecycle_remove_transaction_artifacts() {
  [[ -n "$LIFECYCLE_J_DIR" && "$LIFECYCLE_J_DIR" == "$LIFECYCLE_TX_ROOT/"* ]] || return 1
  lifecycle_private_dir_ok "$LIFECYCLE_J_DIR" || return 1
  rm -rf -- "$LIFECYCLE_J_DIR" || return 1
  [[ ! -e "$LIFECYCLE_J_DIR" && ! -L "$LIFECYCLE_J_DIR" ]] || return 1
  lifecycle_sync_durable "$LIFECYCLE_TX_ROOT" || return 1
  lifecycle_private_file_ok "$LIFECYCLE_JOURNAL" 600 || return 1
  unlink_policy_no_follow "$LIFECYCLE_JOURNAL" || return 1
  lifecycle_sync_durable "$LIFECYCLE_STATE_DIR"
}

lifecycle_recover_pending() {
  if [[ ! -e "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]]; then
    if [[ -n "$LIFECYCLE_TX_DIR" ]]; then lifecycle_remove_unpublished_transaction || return 1; fi
    return 0
  fi
  lifecycle_read_journal || {
    echo 'lifecycle recovery journal is invalid; refusing mutation' >&2
    return 1
  }
  if [[ "$LIFECYCLE_J_PHASE" == committed ]]; then
    # A durable committed record is terminal. Verify only the recorded
    # post-state; never run pre-state compensation for this phase.
    local prior_recovery_mode=${LIFECYCLE_RECOVERY_MODE:-0}
    local prior_recovery_deadline=${LIFECYCLE_RECOVERY_DEADLINE:-0}
    lifecycle_recovery_budget_start || {
      echo 'lifecycle committed-state verification budget is invalid; journal retained' >&2
      return 1
    }
    LIFECYCLE_RECOVERY_MODE=1
    if ! lifecycle_current_post_state_matches_journal; then
      LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
      LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
      echo 'lifecycle committed post-state is unverifiable; journal retained and mutation refused' >&2
      return 1
    fi
    if ! lifecycle_remove_transaction_artifacts; then
      LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
      LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
      echo 'lifecycle committed journal cleanup failed; journal retained' >&2
      return 1
    fi
    LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
    LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
    return 0
  fi
  if [[ "$LIFECYCLE_J_PHASE" == preparing ]]; then
    # No external mutation is authorized before the durable prepared phase.
    # Removing only a complete private preparing transaction is safe.
    lifecycle_remove_transaction_artifacts || {
      echo 'lifecycle preparing journal cleanup failed; refusing mutation' >&2
      return 1
    }
    LIFECYCLE_RECOVERY_MODE=0
    return 0
  fi
  lifecycle_recovery_budget_start || {
    echo 'lifecycle recovery budget is invalid; journal retained and mutation refused' >&2
    return 1
  }
  LIFECYCLE_RECOVERY_MODE=1
  if ! lifecycle_recovery_preflight; then
    echo 'lifecycle recovery preflight failed; journal retained and mutation refused' >&2
    return 1
  fi
  # A signal can arrive after compensation but before journal removal. If the
  # exact old pair is already present, do not repeat external mutations.
  if lifecycle_current_policy_matches_journal && lifecycle_current_container_matches_journal && \
      lifecycle_current_nm_matches_journal && lifecycle_current_retest_matches_journal; then
    lifecycle_remove_transaction_artifacts || {
      echo 'lifecycle recovery cleanup failed; journal retained' >&2
      return 1
    }
    LIFECYCLE_RECOVERY_MODE=0
    return 0
  fi
  lifecycle_recovery_budget_ok || return 124
  # Post-state belongs only to the terminal committed branch. Once recovery
  # chooses compensation, publish the non-terminal phase without claiming a
  # post-state proof.
  LIFECYCLE_TX_POST_READY=no
  lifecycle_transaction_phase compensating || return 1
  # Restore policy/config inputs before any exact-ID Docker restart. Rendering
  # is local file work; all Compose/docker/NM work remains below the lifecycle
  # lock and is re-preflighted after the rendering interval.
  lifecycle_restore_policy_from_journal || {
    echo 'lifecycle policy recovery failed; journal retained' >&2
    return 1
  }
  if [[ "$LIFECYCLE_J_OPERATION" == toggle ]]; then
    render_all "$LIFECYCLE_J_POLICY_VALUE" || {
      echo 'lifecycle rendered policy recovery failed; journal retained' >&2
      return 1
    }
  fi
  if [[ "$LIFECYCLE_J_NM_ENABLED" == yes ]]; then
    lifecycle_recovery_budget_ok || return 1
    lifecycle_current_container_matches_journal || {
      # The current container is allowed to be a transaction candidate here;
      # this check is intentionally only a fresh ownership preflight/read.
      local_compose_preflight || return 1
    }
    restore_networkmanager_capability "$LIFECYCLE_J_DIR/nm" "$LIFECYCLE_J_NM_CONFIGURED" \
      "$LIFECYCLE_J_NM_ACTIVE" "$LIFECYCLE_J_NM_DEVICE" || return 1
    lifecycle_current_nm_matches_journal || return 1
  fi
  lifecycle_recovery_budget_ok || return 1
  lifecycle_restore_container_from_journal || {
    echo 'lifecycle exact container recovery failed; journal retained' >&2
    return 1
  }
  lifecycle_current_policy_matches_journal || return 1
  lifecycle_current_nm_matches_journal || return 1
  lifecycle_current_container_matches_journal || return 1
  lifecycle_current_retest_matches_journal || return 1
  lifecycle_remove_transaction_artifacts || {
    echo 'lifecycle recovery cleanup failed; journal retained' >&2
    return 1
  }
  LIFECYCLE_RECOVERY_MODE=0
}

lifecycle_install_traps() {
  [[ "$LIFECYCLE_TX_TRAPS" == 0 ]] || return 0
  LIFECYCLE_TX_TRAPS=1
  trap 'lifecycle_signal_handler 130' INT
  trap 'lifecycle_signal_handler 143' TERM
  trap 'lifecycle_signal_handler 129' HUP
  trap 'lifecycle_exit_handler $?' EXIT
}

lifecycle_clear_traps() {
  trap - INT TERM HUP EXIT
  LIFECYCLE_TX_TRAPS=0
}

lifecycle_signal_handler() {
  local status=$1
  (( LIFECYCLE_TX_COMPENSATING == 0 )) || exit "$status"
  LIFECYCLE_TX_COMPENSATING=1
  trap - INT TERM HUP EXIT
  if ! lifecycle_recover_pending; then
    echo 'lifecycle signal compensation incomplete; journal retained' >&2
  fi
  exit "$status"
}

lifecycle_exit_handler() {
  local status=$1
  trap - INT TERM HUP EXIT
  if [[ "$LIFECYCLE_TX_FINALIZED" != 1 && -n "$LIFECYCLE_TX_DIR" && "$LIFECYCLE_TX_COMPENSATING" == 0 ]]; then
    LIFECYCLE_TX_COMPENSATING=1
    if ! lifecycle_recover_pending; then
      echo 'lifecycle exit compensation incomplete; journal retained' >&2
      [[ "$status" -eq 0 ]] && status=1
    fi
  fi
  exit "$status"
}

lifecycle_abort_transaction() {
  local rc=${1:-1} phase=
  [[ -n "$LIFECYCLE_TX_DIR" ]] || return "$rc"
  if [[ -f "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]]; then
    phase=$(lifecycle_journal_field phase 2>/dev/null || true)
    if [[ "$phase" == committed ]]; then
      # Once committed is durable, even an error in the verification/cleanup
      # tail must fail closed and retain the terminal journal.
      echo 'lifecycle committed post-state was not verified; journal retained' >&2
      return "$rc"
    fi
  fi
  if [[ ! -e "$LIFECYCLE_JOURNAL" && ! -L "$LIFECYCLE_JOURNAL" ]]; then
    lifecycle_remove_unpublished_transaction || true
    return "$rc"
  fi
  if [[ "$LIFECYCLE_TX_COMPENSATING" == 0 ]]; then
    LIFECYCLE_TX_COMPENSATING=1
    if ! lifecycle_recover_pending; then
      echo 'lifecycle transaction compensation incomplete; journal retained' >&2
      return 1
    fi
  fi
  return "$rc"
}

lifecycle_commit_transaction() {
  [[ "$LIFECYCLE_TX_PREPARED" == 1 ]] || return 1
  # Capture and fsync the minimal visible post-state before publishing the
  # terminal phase. A committed journal without this proof is not valid.
  lifecycle_record_post_state || return 1
  lifecycle_transaction_phase committing || return 1
  lifecycle_transaction_phase committed || return 1
  lifecycle_read_journal || return 1
  local prior_recovery_mode=${LIFECYCLE_RECOVERY_MODE:-0}
  local prior_recovery_deadline=${LIFECYCLE_RECOVERY_DEADLINE:-0}
  lifecycle_recovery_budget_start || return 1
  LIFECYCLE_RECOVERY_MODE=1
  if ! lifecycle_current_post_state_matches_journal; then
    LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
    LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
    return 1
  fi
  if ! lifecycle_remove_transaction_artifacts; then
    LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
    LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
    return 1
  fi
  LIFECYCLE_RECOVERY_MODE=$prior_recovery_mode
  LIFECYCLE_RECOVERY_DEADLINE=$prior_recovery_deadline
  LIFECYCLE_TX_FINALIZED=1
  return 0
}

run_lifecycle_mutation() {
  local operation=$1 function_name=$2 rc
  LIFECYCLE_FAILURE_CAPTURED=0
  lifecycle_compensation_timeout_seconds >/dev/null || return 1
  lifecycle_recover_pending || return 1
  lifecycle_cleanup_orphan_transactions || {
    echo 'unpublished lifecycle transaction state is unsafe; refusing mutation' >&2
    return 1
  }
  if ! lifecycle_begin_transaction "$operation"; then
    lifecycle_remove_unpublished_transaction || true
    echo 'could not create durable lifecycle journal before mutation' >&2
    return 1
  fi
  # The transaction directory exists before the operation-specific exact
  # snapshot is published. Traps cover this read-only window so TERM/HUP also
  # remove an unpublished transaction; no external mutation is allowed yet.
  lifecycle_install_traps
  if "$function_name"; then rc=0; else rc=$?; fi
  if (( rc == 0 )); then
    if ! lifecycle_commit_transaction; then
      rc=1
      lifecycle_abort_transaction "$rc" || true
    fi
  else
    if [[ "$LIFECYCLE_FAILURE_CAPTURED" != 1 ]]; then
      lifecycle_capture_failure_diagnostics || true
    fi
    lifecycle_abort_transaction "$rc" || true
  fi
  lifecycle_clear_traps
  return "$rc"
}

rollback_start_transaction() {
  local stack_attempted=$1 stack_was_healthy=$2 stack_before_id=$3 configured_before=$4 active_before=$5 device_before=$6 nm_snapshot=$7
  local failed=0 restored_id

  if networkmanager_enabled; then
    restore_networkmanager_capability "$nm_snapshot" "$configured_before" "$active_before" "$device_before" || failed=1
  fi
  if [[ "$stack_attempted" == true ]]; then
    # NetworkManager restoration is an external interval. Re-prove the whole
    # local Compose resource set before even reading the stack for rollback.
    if ! local_compose_preflight; then
      failed=1
    elif [[ "$stack_was_healthy" == true ]]; then
      # Never tear down a healthy pre-call local stack.  If the failed Compose
      # call left the exact healthy container in place, preserve it without a
      # second mutation.  Only a changed/missing/unhealthy container needs a
      # bounded Compose-up restoration.
      restored_id=$LOCAL_PREFLIGHT_CONTAINER_ID
      if [[ "$restored_id" != "$stack_before_id" || "$(container_state "$restored_id")" != healthy ]]; then
        # The state read above is another interval; the check directly before
        # Compose up is the authority for the compensating mutation.
        if local_compose_preflight; then
          lifecycle_compose_command up -d --pull never vpnkit >/dev/null 2>&1 || failed=1
          if local_compose_preflight; then
            wait_healthy || failed=1
            restored_id=$LOCAL_PREFLIGHT_CONTAINER_ID
          else
            failed=1
          fi
        else
          failed=1
        fi
      fi
      [[ -z "$stack_before_id" || "$restored_id" == "$stack_before_id" ]] || failed=1
    else
      # This is deliberately immediately before Compose down: no stale
      # preflight result may authorize destructive cleanup.
      if local_compose_preflight; then
        lifecycle_compose_command down --remove-orphans >/dev/null 2>&1 || failed=1
      else
        failed=1
      fi
    fi
  fi
  return "$failed"
}

lifecycle_capture_failure_diagnostics() (
  # Only the supervised TUI opts in: its stderr is a private bounded log.
  # Capture before rollback removes the container and owned tunnel evidence.
  [[ "${VPNKIT_TUI_DIAGNOSTICS:-0}" == 1 && "${VPNKIT_TUI_SUPERVISED:-0}" == 1 ]] || exit 0
  printf 'failure_snapshot=begin\n' >&2
  printf 'snapshot=host-ipv4-rules\n' >&2
  timeout --foreground -k 1s 5 ip -4 rule show >&2 2>&1 || true
  printf 'snapshot=host-ipv4-routes\n' >&2
  timeout --foreground -k 1s 5 ip -4 route show table all >&2 2>&1 || true
  printf 'snapshot=host-route-probes\n' >&2
  timeout --foreground -k 1s 5 ip -4 route get 1.1.1.1 >&2 2>&1 || true
  timeout --foreground -k 1s 5 ip -4 route get 8.8.8.8 >&2 2>&1 || true
  if local_compose_preflight && [[ -n "$LOCAL_PREFLIGHT_CONTAINER_ID" ]]; then
    printf 'snapshot=local-container-health\n' >&2
    timeout --foreground -k 1s 5 docker inspect --format '{{json .State}}' "$LOCAL_PREFLIGHT_CONTAINER_ID" >&2 2>&1 || true
    printf 'snapshot=local-container-log\n' >&2
    timeout --foreground -k 1s 5 docker logs --tail 150 "$LOCAL_PREFLIGHT_CONTAINER_ID" >&2 2>&1 || true
    local diagnostic_cid=$LOCAL_PREFLIGHT_CONTAINER_ID
    if local_compose_preflight_for_container "$diagnostic_cid"; then
      printf 'snapshot=upstream-selection-logs\n' >&2
      timeout --foreground -k 1s 5 docker exec "$diagnostic_cid" tail -n 100 \
        /var/log/vibe-vpn/bootstrap-selection.log /var/log/vibe-vpn/manual-selection.log >&2 2>&1 || true
    fi
  fi
  printf 'snapshot=networkmanager-recent\n' >&2
  timeout --foreground -k 1s 5 journalctl -u NetworkManager --since '-5 minutes' -n 150 --no-pager >&2 2>&1 || true
  printf 'failure_snapshot=end\n' >&2
)

start_failure() {
  local reason=$1 stack_attempted=$2 stack_was_healthy=$3 stack_before_id=$4 configured_before=$5 active_before=$6 device_before=$7 snapshot=$8 nm_snapshot=$9
  local rollback_failed=0 owned_uuid_after= prior_mode=${LIFECYCLE_RECOVERY_MODE:-0} prior_deadline=${LIFECYCLE_RECOVERY_DEADLINE:-0}
  lifecycle_capture_failure_diagnostics || true
  LIFECYCLE_FAILURE_CAPTURED=1
  if lifecycle_recovery_budget_start; then
    LIFECYCLE_RECOVERY_MODE=1
    rollback_start_transaction "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$nm_snapshot" || rollback_failed=1
    if networkmanager_enabled; then
      # Rollback can import/remove the local capability and is itself an external
      # interval. Obtain the post-rollback exclusion only from a fresh, valid
      # helper status proof; a failed proof means no UUID is trusted.
      if read_networkmanager_state; then
        owned_uuid_after=$NM_OWNED_UUID
      else
        rollback_failed=1
      fi
      verify_other_active_vpn_set "$snapshot" "${snapshot}.after" "${snapshot}.before-other" "${snapshot}.after-other" "$owned_uuid_after" || rollback_failed=1
    fi
    LIFECYCLE_RECOVERY_MODE=$prior_mode
    LIFECYCLE_RECOVERY_DEADLINE=$prior_deadline
  else
    rollback_failed=1
  fi
  rm -rf -- "$(dirname -- "$snapshot")"
  if (( rollback_failed )); then
    echo "$reason; local rollback incomplete" >&2
  else
    echo "$reason; local stack was rolled back" >&2
  fi
  return 1
}

start_stack() {
  validate_networkmanager_setting || return

  local snapshot_dir snapshot snapshot_after nm_snapshot
  snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-start.XXXXXX") || {
    echo 'could not create local start transaction snapshot' >&2
    return 1
  }
  chmod 700 "$snapshot_dir"
  snapshot="$snapshot_dir/active-vpn"
  snapshot_after="${snapshot}.after"
  nm_snapshot="$snapshot_dir/nm"
  mkdir -- "$nm_snapshot"
  chmod 700 "$nm_snapshot"

  local configured_before=not-managed active_before=not-managed device_before=not-managed
  local stack_attempted=false stack_was_healthy=false stack_before_id= stack_before_state=absent
  if networkmanager_enabled; then
    if ! read_networkmanager_state; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager status could not be read before local start' >&2
      return 1
    fi
    configured_before=$NM_CONFIGURED
    active_before=$NM_ACTIVE
    device_before=$NM_DEVICE
    if ! capture_active_vpn_identities "$snapshot" "$NM_OWNED_UUID" || ! snapshot_owned_nm_capability "$nm_snapshot"; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager capability could not be snapshotted before local start' >&2
      return 1
    fi
  else
    : >"$snapshot"
    chmod 600 "$snapshot"
  fi

  command -v docker >/dev/null 2>&1 || {
    rm -rf -- "$snapshot_dir"
    echo 'docker is unavailable' >&2
    return 1
  }
  # Probe only this validated Compose project before rendering.  The result is
  # part of the transaction: a healthy pre-call container is never rolled down
  # as compensation for a later local start failure.
  if ! local_compose_preflight; then
    rm -rf -- "$snapshot_dir"
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  stack_before_id=$LOCAL_PREFLIGHT_CONTAINER_ID
  if [[ -n "$stack_before_id" ]]; then
    stack_before_state=$(container_state "$stack_before_id")
    [[ "$stack_before_state" == healthy ]] && stack_was_healthy=true
  fi
  if ! lifecycle_record_prepared_snapshot "$snapshot" "$stack_before_id" "$stack_before_state"; then
    rm -rf -- "$snapshot_dir"
    echo 'durable local start snapshot could not be committed' >&2
    return 1
  fi
  if networkmanager_enabled; then
    "$UNDERLAY_HELPER" verify >/dev/null || {
      rm -rf -- "$snapshot_dir"
      echo 'install and verify the vpnkit local underlay helper before NetworkManager start' >&2
      return 1
    }
    # The underlay helper is an external interval and may observe/mutate host
    # routing state; refresh the Compose ownership proof before rendering.
    if ! local_compose_preflight; then
      rm -rf -- "$snapshot_dir"
      echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
      return 1
    fi
  fi
  if ! lifecycle_transaction_phase render; then
    rm -rf -- "$snapshot_dir"
    echo 'local start journal could not advance before rendering' >&2
    return 1
  fi
  if ! render_all; then
    if networkmanager_enabled; then
      start_failure 'local vpnkit assets could not be rendered' false "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
    else
      rm -rf -- "$snapshot_dir"
      echo 'local vpnkit assets could not be rendered' >&2
    fi
    return
  fi

  # Mark the transaction before calling Compose: a partially-created project
  # is bounded, while a healthy pre-existing project is restored with Compose
  # up rather than destructively brought down. The ownership proof is the
  # final operation before the Compose mutation.
  stack_attempted=true
  if ! lifecycle_transaction_phase compose-up; then
    rm -rf -- "$snapshot_dir"
    echo 'local start journal could not advance before Compose' >&2
    return 1
  fi
  if ! local_compose_preflight; then
    rm -rf -- "$snapshot_dir"
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  # Reuse the local image. Compose builds only when it is missing; ordinary
  # Start must not rebuild or contact the image registry for an existing image.
  if ! lifecycle_compose_command up -d --pull never vpnkit >/dev/null; then
    start_failure 'local vpnkit stack failed to start' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
    return
  fi
  lifecycle_transaction_phase compose-up-done || return 1
  # Verify resources produced by Compose before trusting readiness or handing
  # the container to any later helper/external operation.
  if ! local_compose_preflight; then
    start_failure 'local Compose ownership changed after start' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
    return
  fi
  if ! wait_healthy; then
    start_failure 'local vpnkit failed closed before readiness' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
    return
  fi

  local nm_result=not-changed host_smoke_result=not-managed
  if networkmanager_enabled; then
    lifecycle_transaction_phase nm-work || return 1
    # Preserve an already-owned capability byte-for-byte.  Import is only a
    # creation step when this exact local profile was not configured before the
    # call; a pre-existing active profile needs no mutation at all.
    if [[ "$configured_before" != yes ]]; then
      if ! "$NM_HELPER" import --yes >/dev/null || ! "$NM_HELPER" connect --yes >/dev/null; then
        start_failure 'NetworkManager activation failed' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
        return
      fi
    elif [[ "$active_before" != yes ]]; then
      if ! "$NM_HELPER" connect --yes >/dev/null; then
        start_failure 'NetworkManager activation failed' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
        return
      fi
    fi
    if ! read_networkmanager_state || [[ "$NM_ACTIVE" != yes ]]; then
      start_failure 'NetworkManager did not revalidate vpnkit-local as active' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
      return
    fi
    lifecycle_transaction_phase host-smoke || return 1
    if ! run_host_smoke; then
      start_failure 'same-host local VPN smoke failed' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
      return
    fi
    host_smoke_result=pass
    # The smoke is another external interval. Re-read ownership immediately
    # before taking the after-snapshot so only the exact validated local UUID
    # is excluded; a foreign same-named profile remains observable.
    if ! read_networkmanager_state || [[ "$NM_ACTIVE" != yes ]]; then
      start_failure 'NetworkManager ownership changed during local start' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
      return
    fi
    if ! verify_other_active_vpn_set "$snapshot" "$snapshot_after" "${snapshot}.before-other" "${snapshot}.after-other" "$NM_OWNED_UUID"; then
      start_failure 'active VPN set changed outside vpnkit-local' "$stack_attempted" "$stack_was_healthy" "$stack_before_id" "$configured_before" "$active_before" "$device_before" "$snapshot" "$nm_snapshot"
      return
    fi
    nm_result=connected
  fi

  rm -rf -- "$snapshot_dir"
  printf 'vpnkit_local_start=ok\n'
  printf 'container_state=healthy\n'
  printf 'networkmanager=%s\n' "$nm_result"
  printf 'host_smoke=%s\n' "$host_smoke_result"
  if networkmanager_enabled; then
    printf 'transaction_snapshot=complete\n'
    printf 'active_work_vpn_set=unchanged\n'
    printf 'owned_uuid_ip4_tun=verified\n'
    printf 'openvpn_handshake=verified\n'
  fi
}

disconnect_stack() {
  local disconnect_only=true
  stop_stack
}

stop_stack() {
  validate_networkmanager_setting || return
  local nm_result=not-changed snapshot_dir= snapshot= snapshot_after owned_uuid_after= nm_snapshot=
  # Snapshot before the first Docker call. Stop must compare identities by UUID,
  # not by display name, across the disconnect interval. The volatile copy is
  # retained for the existing compensation path; the lifecycle transaction
  # copies the same bytes into its fsynced private snapshot before mutation.
  snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-stop.XXXXXX") || {
    echo 'could not create local stop transaction snapshot' >&2
    return 1
  }
  chmod 700 "$snapshot_dir"
  snapshot="$snapshot_dir/active-vpn"
  snapshot_after="${snapshot}.after"
  nm_snapshot="$snapshot_dir/nm"
  mkdir -- "$nm_snapshot"
  chmod 700 "$nm_snapshot"
  if networkmanager_enabled; then
    if ! read_networkmanager_state || ! capture_active_vpn_identities "$snapshot" "$NM_OWNED_UUID" || ! snapshot_owned_nm_capability "$nm_snapshot"; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager state could not be snapshotted before local stop' >&2
      return 1
    fi
  else
    : >"$snapshot"
    chmod 600 "$snapshot"
  fi

  if ! local_compose_preflight; then
    rm -rf -- "$snapshot_dir"
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  if ! lifecycle_record_prepared_snapshot "$snapshot" "$LOCAL_PREFLIGHT_CONTAINER_ID" "$(container_state "$LOCAL_PREFLIGHT_CONTAINER_ID")"; then
    rm -rf -- "$snapshot_dir"
    echo 'durable local stop snapshot could not be committed' >&2
    return 1
  fi
  if networkmanager_enabled; then
    lifecycle_transaction_phase nm-disconnect || return 1
    if ! "$NM_HELPER" disconnect --yes >/dev/null; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager disconnect failed' >&2
      return 1
    fi
    nm_result=disconnected

    # Re-read the helper status and active UUID set after disconnect. The
    # exact owned UUID may disappear, but a work-VPN disappearance/addition or
    # a foreign profile named vpnkit-local must not be hidden by its name.
    if ! read_networkmanager_state; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager state could not be read after local stop' >&2
      return 1
    fi
    lifecycle_transaction_phase nm-disconnect-done || return 1
    owned_uuid_after=$NM_OWNED_UUID
    if ! verify_other_active_vpn_set "$snapshot" "$snapshot_after" "${snapshot}.before-other" "${snapshot}.after-other" "$owned_uuid_after"; then
      rm -rf -- "$snapshot_dir"
      echo 'active VPN set changed outside vpnkit-local' >&2
      return 1
    fi
  fi
  if [[ "${disconnect_only:-false}" == true ]]; then
    rm -rf -- "$snapshot_dir"
    printf 'vpnkit_local_disconnect=ok\n'
    return 0
  fi
  # NetworkManager disconnect and the identity comparison are external
  # intervals. Do not let an earlier proof authorize a stale/colliding Compose
  # down.
  if ! local_compose_preflight; then
    rm -rf -- "$snapshot_dir"
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  lifecycle_transaction_phase compose-down || return 1
  if ! lifecycle_compose_command down --remove-orphans >/dev/null; then
    return 1
  fi
  lifecycle_transaction_phase compose-down-done || return 1
  rm -rf -- "$snapshot_dir"
  printf 'vpnkit_local_stop=ok\n'
  printf 'networkmanager=%s\n' "$nm_result"
}

container_runtime_generation() {
  local cid=$1
  docker_exec_local "$cid" sh -lc '
    value=$(cat "$1" 2>/dev/null) || exit 2
    case "$value" in
      ""|*[!0-9]*) exit 2 ;;
      *) printf "%s\n" "$value" ;;
    esac
  ' sh "$SINGBOX_GENERATION_FILE"
}

container_request_absent() {
  local cid=$1
  docker_exec_local "$cid" sh -lc 'test ! -e "$1"' sh "$SINGBOX_RESTART_FILE"
}

wait_for_retest_runtime() {
  local cid=$1 generation_before=$2 deadline current
  [[ "$RETEST_TIMEOUT" =~ ^[0-9]+$ ]] && (( RETEST_TIMEOUT >= 1 && RETEST_TIMEOUT <= 3600 )) || { echo 'VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS must be in 1..3600' >&2; return 2; }
  deadline=$((SECONDS + RETEST_TIMEOUT))
  while (( SECONDS < deadline )); do
    if container_request_absent "$cid" 2>/dev/null; then
      current=$(container_runtime_generation "$cid" 2>/dev/null || true)
      if [[ -n "$current" && "$current" != "$generation_before" ]] && [[ "$(container_state "$cid")" == healthy ]]; then
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

retest_select() {
  local cid generation_before log snapshot_dir snapshot snapshot_after nm_snapshot
  snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-retest.XXXXXX") || {
    echo 'could not create local retest transaction snapshot' >&2
    return 1
  }
  chmod 700 "$snapshot_dir"
  snapshot="$snapshot_dir/active-vpn"
  snapshot_after="${snapshot}.after"
  nm_snapshot="$snapshot_dir/nm"
  mkdir -- "$nm_snapshot"
  chmod 700 "$nm_snapshot"
  LIFECYCLE_RETEST_GENERATION=
  LIFECYCLE_RETEST_REQUEST=absent
  if networkmanager_enabled; then
    if ! read_networkmanager_state || ! capture_active_vpn_identities "$snapshot" "$NM_OWNED_UUID" || ! snapshot_owned_nm_capability "$nm_snapshot"; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager state could not be snapshotted before local retest' >&2
      return 1
    fi
  else
    : >"$snapshot"
    chmod 600 "$snapshot"
  fi
  if ! local_compose_preflight; then
    rm -rf -- "$snapshot_dir"
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  cid=$LOCAL_PREFLIGHT_CONTAINER_ID
  [[ -n "$cid" && $(container_state "$cid") == healthy ]] || { rm -rf -- "$snapshot_dir"; echo 'vpnkit local is not healthy' >&2; return 1; }
  # Publish the container/NM pre-state immediately after the initial ownership
  # proof, before any docker exec read can cross a replacement boundary.
  if ! lifecycle_record_prepared_snapshot "$snapshot" "$cid" healthy 1; then
    rm -rf -- "$snapshot_dir"
    echo 'durable local retest snapshot could not be committed' >&2
    return 1
  fi
  if ! container_request_absent "$cid" >/dev/null 2>&1; then
    rm -rf -- "$snapshot_dir"
    echo 'a prior sing-box restart request is still pending' >&2
    return 1
  fi
  generation_before=$(container_runtime_generation "$cid" 2>/dev/null) || {
    rm -rf -- "$snapshot_dir"
    echo 'sing-box runtime generation is unavailable' >&2
    return 1
  }
  LIFECYCLE_RETEST_GENERATION=$generation_before
  lifecycle_atomic_write_private "$LIFECYCLE_TX_DIR/retest.generation" "$LIFECYCLE_RETEST_GENERATION" || return 1
  lifecycle_journal_write preparing || return 1
  lifecycle_install_traps
  lifecycle_transaction_phase prepared || return 1
  # The request/generation reads are external container intervals. Re-prove
  # ownership immediately before the mutating pick, rather than trusting the
  # initial Compose selection or either unchecked read.
  if ! local_compose_preflight_for_container "$cid"; then
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  log=/var/log/vibe-vpn/manual-selection.log
  lifecycle_transaction_phase go-state || return 1
  if ! docker_exec_local "$cid" sh -lc 'umask 077; vibe-vpn --config /etc/vibe-vpn/config.yaml pick >"$1" 2>&1' sh "$log"; then
    echo 'upstream retest/select failed; details remain in private container log' >&2
    return 1
  fi
  lifecycle_transaction_phase go-state-done || return 1
  # Pick can replace runtime state before the wait loop starts; do not begin
  # post-pick reads from a container whose ownership changed during the exec.
  if ! local_compose_preflight_for_container "$cid"; then
    echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
    return 1
  fi
  lifecycle_transaction_phase runtime-wait || return 1
  if ! wait_for_retest_runtime "$cid" "$generation_before"; then
    echo 'upstream selection completed without a consumed restart and healthy runtime' >&2
    return 1
  fi
  if networkmanager_enabled; then
    if ! read_networkmanager_state || ! verify_other_active_vpn_set "$snapshot" "$snapshot_after" \
        "${snapshot}.before-other" "${snapshot}.after-other" "$NM_OWNED_UUID"; then
      echo 'active VPN set changed outside vpnkit-local' >&2
      return 1
    fi
  fi
  rm -rf -- "$snapshot_dir"
  printf 'vpnkit_local_retest_select=ok\n'
  printf 'selection_details=redacted\n'
}

recreate_local_stack() {
  local policy=${1:-}
  [[ -n "$policy" ]] || policy=$(safe_policy)
  local_compose_preflight || return 1
  render_all "$policy" || return 1
  # Rendering is intentionally followed by a second read-only ownership check
  # immediately before the recreate call.
  local_compose_preflight || return 1
  # Compose up is immediately preceded by the complete ownership proof.
  lifecycle_compose_command up -d --pull never --force-recreate vpnkit >/dev/null || return 1
  local_compose_preflight || return 1
  wait_healthy
}

restore_networkmanager_state() {
  local configured_before=$1 active_before=$2
  networkmanager_enabled || return 0
  local failed=0
  if ! read_networkmanager_state; then return 1; fi

  if [[ "$configured_before" == no && "$NM_CONFIGURED" == yes ]]; then
    lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || failed=1
    lifecycle_nm_mutation remove --yes >/dev/null 2>&1 || failed=1
  elif [[ "$configured_before" == yes && "$NM_CONFIGURED" == no ]]; then
    lifecycle_nm_mutation import --yes >/dev/null 2>&1 || failed=1
  fi

  if [[ "$active_before" == yes && "$NM_ACTIVE" != yes ]]; then
    lifecycle_nm_mutation connect --yes >/dev/null 2>&1 || failed=1
  elif [[ "$active_before" != yes && "$NM_ACTIVE" == yes ]]; then
    lifecycle_nm_mutation disconnect --yes >/dev/null 2>&1 || failed=1
  fi
  read_networkmanager_state || failed=1
  [[ "$NM_CONFIGURED" == "$configured_before" && "$NM_ACTIVE" == "$active_before" ]] || failed=1
  return "$failed"
}

toggle_mode() {
  validate_networkmanager_setting || return
  local current next was_running=false snapshot_dir snapshot snapshot_after nm_snapshot
  snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-toggle.XXXXXX") || {
    echo 'could not create local toggle transaction snapshot' >&2
    return 1
  }
  chmod 700 "$snapshot_dir"
  snapshot="$snapshot_dir/active-vpn"
  snapshot_after="${snapshot}.after"
  nm_snapshot="$snapshot_dir/nm"
  mkdir -- "$nm_snapshot"
  chmod 700 "$nm_snapshot"
  current=$(safe_policy) || { rm -rf -- "$snapshot_dir"; return 1; }
  [[ "$current" == strict ]] && next=smart || next=strict

  local configured_before=not-managed active_before=not-managed device_before=not-managed
  if networkmanager_enabled; then
    if ! read_networkmanager_state || ! capture_active_vpn_identities "$snapshot" "$NM_OWNED_UUID" || ! snapshot_owned_nm_capability "$nm_snapshot"; then
      rm -rf -- "$snapshot_dir"
      echo 'NetworkManager state could not be snapshotted before policy toggle' >&2
      return 1
    fi
    configured_before=$NM_CONFIGURED
    active_before=$NM_ACTIVE
    device_before=$NM_DEVICE
  else
    : >"$snapshot"
    chmod 600 "$snapshot"
  fi
  local stack_before_id= stack_before_state=absent
  LOCAL_PREFLIGHT_CONTAINER_ID=
  if selected=$(compose_selected_container); then
    if ! local_compose_preflight; then
      rm -rf -- "$snapshot_dir"
      echo "local Compose preflight failed: $LOCAL_COMPOSE_PREFLIGHT_ERROR" >&2
      return 1
    fi
    stack_before_id=$LOCAL_PREFLIGHT_CONTAINER_ID
  else
    local selected_rc=$?
    if (( selected_rc != 2 )); then
      rm -rf -- "$snapshot_dir"
      echo 'local Compose container selection failed before policy toggle' >&2
      return 1
    fi
  fi
  if [[ -n "$stack_before_id" ]]; then stack_before_state=$(container_state "$stack_before_id"); fi
  [[ "$stack_before_state" == healthy ]] && was_running=true
  if ! lifecycle_record_prepared_snapshot "$snapshot" "$stack_before_id" "$stack_before_state"; then
    rm -rf -- "$snapshot_dir"
    echo 'durable local toggle snapshot could not be committed' >&2
    return 1
  fi

  local render_attempted=false stack_attempted=false toggle_failed=0
  if [[ "$was_running" == true ]]; then
    render_attempted=true
    stack_attempted=true
    # Render and recreate the candidate policy before committing its state
    # file.  A failed runtime transaction therefore cannot commit `next` even
    # if compensation itself later encounters an injected filesystem failure.
    if ! lifecycle_transaction_phase toggle-candidate || ! recreate_local_stack "$next"; then toggle_failed=1; fi
    if (( toggle_failed == 0 )); then lifecycle_transaction_phase toggle-candidate-done || toggle_failed=1; fi
  fi
  if (( toggle_failed == 0 )) && networkmanager_enabled; then
    if ! read_networkmanager_state || [[ "$NM_CONFIGURED" != "$configured_before" || "$NM_ACTIVE" != "$active_before" ]]; then
      toggle_failed=1
    elif ! verify_other_active_vpn_set "$snapshot" "$snapshot_after" "${snapshot}.before-other" "${snapshot}.after-other" "$NM_OWNED_UUID"; then
      toggle_failed=1
    fi
  fi
  if (( toggle_failed == 0 )); then
    if ! lifecycle_transaction_phase policy-commit || ! write_policy "$next"; then toggle_failed=1; fi
  fi

  if (( toggle_failed != 0 )); then
    local rollback_failed=0 prior_mode=${LIFECYCLE_RECOVERY_MODE:-0} prior_deadline=${LIFECYCLE_RECOVERY_DEADLINE:-0}
    if lifecycle_recovery_budget_start; then
      LIFECYCLE_RECOVERY_MODE=1
      # The old policy file was never replaced until the candidate runtime had
      # passed. Re-render the old policy only when a runtime candidate ran.
      if [[ "$render_attempted" == true ]]; then
        if ! recreate_local_stack "$current"; then rollback_failed=1; fi
      elif [[ "$stack_attempted" == true ]]; then
        if local_compose_preflight; then
          lifecycle_compose_command down --remove-orphans >/dev/null 2>&1 || rollback_failed=1
        else
          rollback_failed=1
        fi
      fi
      restore_networkmanager_state "$configured_before" "$active_before" || rollback_failed=1
      LIFECYCLE_RECOVERY_MODE=$prior_mode
      LIFECYCLE_RECOVERY_DEADLINE=$prior_deadline
    else
      rollback_failed=1
    fi
    rm -rf -- "$snapshot_dir"
    if (( rollback_failed )); then
      echo 'policy switch failed; prior policy/runtime rollback incomplete' >&2
    else
      echo 'policy switch failed; prior policy/runtime was restored' >&2
    fi
    return 1
  fi

  rm -rf -- "$snapshot_dir"
  printf 'vpnkit_local_toggle=ok\n'
  printf 'routing_policy=%s\n' "$next"
}

diagnostics() {
  local state policy helper=not-installed nm_configured=unavailable nm_active=unavailable
  state=$(container_state)
  policy=$(safe_policy)
  if [[ -x "$UNDERLAY_HELPER" ]]; then helper=available; fi
  if read_networkmanager_state; then
    nm_configured=$NM_CONFIGURED
    nm_active=$NM_ACTIVE
  fi
  printf 'vpnkit_local_diagnostics=ok\n'
  printf 'container_state=%s\n' "$state"
  printf 'routing_policy=%s\n' "$policy"
  printf 'subscription=%s\n' "$([[ -s "$BASE/vibe-vpn/sub_url" ]] && echo configured || echo missing)"
  printf 'client_profile=%s\n' "$([[ -s "$BASE/openvpn/client/vpnkit-local.ovpn" ]] && echo ready || echo missing)"
  printf 'host_route_helper=%s\n' "$helper"
  printf 'networkmanager=configured:%s active:%s\n' "$nm_configured" "$nm_active"
  printf 'private_values=not_printed\n'
}

status_json() {
  local nm_configured nm_active helper=not-installed profile=missing
  [[ -x "$UNDERLAY_HELPER" ]] && helper=available
  [[ -s "$BASE/openvpn/client/vpnkit-local.ovpn" ]] && profile=ready
  read -r nm_configured nm_active < <(networkmanager_status_values)
  python3 - "$(container_state)" "$(safe_policy)" "$([[ -s "$BASE/vibe-vpn/sub_url" ]] && echo configured || echo missing)" "$nm_configured" "$nm_active" "$profile" "$helper" <<'PY'
import json,sys
print(json.dumps({
    "schema": 1,
    "container": sys.argv[1],
    "routing_policy": sys.argv[2],
    "subscription": sys.argv[3],
    "networkmanager": {"configured": sys.argv[4], "active": sys.argv[5]},
    "client_profile": sys.argv[6],
    "host_route_helper": sys.argv[7],
}, sort_keys=True))
PY
}

usage() {
  cat <<'EOF'
Usage: scripts/vpnkit/vpnkit-local.sh backend start|start|disconnect|stop|status [--json]|retest select|toggle mode|diagnostics
       scripts/vpnkit/vpnkit-local.sh servers list|refresh|test-all|ping <opaque-id>|speed <opaque-id>|select <opaque-id>|current
       scripts/vpnkit/vpnkit-local.sh servers availability <opaque-id> <https-url>
       scripts/vpnkit/vpnkit-local.sh servers check-batch <1-to-1000-comma-separated-ids> <https-url>

This lifecycle adapter manages only the isolated local Compose project and the
fixed `vpnkit-local` NetworkManager profile. It never targets production `vpnkit`.
The servers command is a fixed redacted-JSON bridge, not a generic passthrough.
EOF
}

# Read-only status/diagnostics deliberately do not take the lifecycle lock.
# Every mutating entrypoint takes it before this dispatch and keeps the
# descriptor through snapshot, Compose/docker/NM/Go work, commit, and recovery.
if lifecycle_mutating_invocation "$@"; then
  acquire_lifecycle_lock "$@" || exit $?
fi

case "${1:-}" in
  start) [[ $# -eq 1 ]] || { usage >&2; exit 2; };
    if networkmanager_enabled; then ensure_backend_ready || exit 1; fi
    run_lifecycle_mutation start start_stack ;;
  backend) [[ "${2:-}" == start && $# -eq 2 ]] || { usage >&2; exit 2; };
    # A backend-only action cannot compensate a transaction that owns host VPN
    # state. Leave the journal intact for explicit lifecycle recovery.
    if [[ -e "$LIFECYCLE_JOURNAL" ]]; then
      echo 'lifecycle recovery required before backend-only start' >&2
      exit 1
    fi
    MANAGE_NETWORKMANAGER=false
    export VPNKIT_BOOTSTRAP_PICK_ON_START=false
    export VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION=true
    local_compose_preflight || exit 1
    if [[ -n "$LOCAL_PREFLIGHT_CONTAINER_ID" ]]; then
      [[ "$(container_state "$LOCAL_PREFLIGHT_CONTAINER_ID")" == healthy ]]
      exit $?
    fi
    run_lifecycle_mutation start start_stack ;;
  disconnect) [[ $# -eq 1 ]] || { usage >&2; exit 2; }; run_lifecycle_mutation disconnect disconnect_stack ;;
  stop) [[ $# -eq 1 ]] || { usage >&2; exit 2; }; run_lifecycle_mutation stop stop_stack ;;
  status) [[ $# -le 2 ]] || { usage >&2; exit 2; }; [[ "${2:-}" == --json ]] && status_json || diagnostics ;;
  retest) [[ "${2:-}" == select && $# -eq 2 ]] || { usage >&2; exit 2; };
    ensure_backend_ready || exit 1
    run_lifecycle_mutation retest retest_select ;;
  toggle) [[ "${2:-}" == mode && $# -eq 2 ]] || { usage >&2; exit 2; }; run_lifecycle_mutation toggle toggle_mode ;;
  diagnostics) [[ $# -eq 1 ]] || { usage >&2; exit 2; }; diagnostics ;;
  servers)
    case "${2:-}:$#" in
      check-batch:4) server_browser_command "$2" "$3" "$4" ;;
      availability:4) server_browser_command "$2" "$3" "$4" ;;
      refresh:2|list:2|test-all:2|current:2) server_browser_command "$2" ;;
      speed:3|ping:3|select:3)
        server_browser_id_valid "${3:-}" || { usage >&2; exit 2; }
        server_browser_command "$2" "$3"
        ;;
      *) usage >&2; exit 2 ;;
    esac
    ;;
  -h|--help|'') usage ;;
  *) usage >&2; exit 2 ;;
esac
