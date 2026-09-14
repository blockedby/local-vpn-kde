#!/usr/bin/env bash
# Install the bounded local issue-40 vpnkit flow for the current worktree.
#
# This wrapper owns orchestration only.  The lifecycle adapter and Python TUI
# remain the implementation boundary: the wrapper prepares a safe local env,
# installs the host underlay, starts the container without NetworkManager, and
# imports the owned profile without activating it.
set -Eeuo pipefail
umask 077

DRY_RUN=0
PHASE=preflight
ENV_SOURCE=
ENV_FILE_PRESENT=0

usage() {
  cat <<'USAGE'
Usage: scripts/vpnkit/vpnkit-local-install.sh [--dry-run]
       scripts/vpnkit/vpnkit-local-install.sh --help

Prepare and install the local CachyOS/KDE vpnkit flow from the current
worktree. Run as the normal desktop user, not root. The normal flow:
  1. validates/creates config/vpnkit-local.local.env with mode 0600;
  2. shows the redacted underlay plan and installs it through sudo;
  3. starts only the bounded local Docker container stack;
  4. imports the owned NetworkManager profile named vpnkit-local, but never
     connects it; and
  5. prints only redacted status markers.

Options:
  --dry-run, --plan  Read-only preflight. No env file is created, sudo is not
                     invoked, and Docker/NetworkManager lifecycle mutation is
                     not attempted.
  -h, --help         Show this help without probing or mutating the host.

Before the normal flow, enter the subscription through:
  scripts/vpnkit/vpnkit-local-tui.sh

Bounded rollback/uninstall (run each only when needed):
  scripts/vpnkit/vpnkit-local-networkmanager.sh disconnect --yes
  scripts/vpnkit/vpnkit-local-networkmanager.sh remove --yes
  scripts/vpnkit/vpnkit-local.sh stop
  sudo scripts/vpnkit/vpnkit-local-underlay-routing.sh uninstall --yes

The rollback sequence does not delete local PKI, rendered files, or the
subscription. Review and remove those ignored local files separately.
USAGE
}

die() {
  printf 'ERROR: %s\n' "$1" >&2
  exit "${2:-1}"
}

rollback_hint() {
  cat >&2 <<'ROLLBACK'
If a partial runtime setup remains, bounded rollback is:
  scripts/vpnkit/vpnkit-local-networkmanager.sh disconnect --yes
  scripts/vpnkit/vpnkit-local-networkmanager.sh remove --yes
  scripts/vpnkit/vpnkit-local.sh stop
  sudo scripts/vpnkit/vpnkit-local-underlay-routing.sh uninstall --yes
The rollback does not print or remove the local subscription/profile files.
ROLLBACK
}

on_exit() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    if (( DRY_RUN == 1 )) || [[ "$PHASE" == preflight || "$PHASE" == underlay-plan ]]; then
      printf 'No container, NetworkManager, or sudo mutation was attempted.\n' >&2
    else
      printf 'Local setup stopped during %s.\n' "$PHASE" >&2
      rollback_hint
    fi
  fi
  exit "$status"
}
trap on_exit EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run|--plan)
      (( DRY_RUN == 0 )) || die 'only one mode may be selected' 2
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      [[ $# -eq 0 ]] || die 'unexpected arguments after --' 2
      ;;
    *)
      usage >&2
      die 'unknown option' 2
      ;;
  esac
done

(( EUID != 0 )) || die 'run this installer as your normal desktop user, not as root' 3

# Resolve the invoked file before deriving any other path. A tracked helper
# must not be reached through a symlinked worktree/component or an alternate
# repository selected by the caller's working directory.
path_has_symlink_component() {
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
    if [[ "$current" == / ]]; then current="/$part"; else current="$current/$part"; fi
    [[ -L "$current" ]] && return 0
  done
  return 1
}

SOURCE_REQUEST=${BASH_SOURCE[0]}
case "$SOURCE_REQUEST" in
  /*) ;;
  *) SOURCE_REQUEST="$(pwd -P)/$SOURCE_REQUEST" ;;
esac
if path_has_symlink_component "$SOURCE_REQUEST"; then
  die 'installer path contains a symlink component' 20
else
  source_path_rc=$?
  (( source_path_rc == 1 )) || die 'installer path is not absolute' 20
fi
command -v realpath >/dev/null 2>&1 || die 'realpath is unavailable' 10
command -v git >/dev/null 2>&1 || die 'git is unavailable for canonical worktree discovery' 10
SCRIPT_PATH=$(realpath -e -- "$SOURCE_REQUEST" 2>/dev/null) || die 'installer source is unavailable' 20
[[ -f "$SCRIPT_PATH" && ! -L "$SCRIPT_PATH" ]] || die 'installer source is not a regular file' 20
SCRIPT_DIR=$(cd -- "$(dirname -- "$SCRIPT_PATH")" && pwd -P) || die 'installer directory is unavailable' 20
REPO_ROOT=$(git -C "$SCRIPT_DIR/../.." rev-parse --show-toplevel 2>/dev/null) || die 'current directory is not a git worktree' 20
REPO_ROOT=$(realpath -e -- "$REPO_ROOT" 2>/dev/null) || die 'git worktree root is unavailable' 20
EXPECTED_SCRIPT_DIR="$REPO_ROOT/scripts/vpnkit"
[[ "$SCRIPT_DIR" == "$EXPECTED_SCRIPT_DIR" ]] || die 'installer is not the canonical scripts/vpnkit helper' 20
[[ -d "$REPO_ROOT/.git" || -f "$REPO_ROOT/.git" ]] || die 'git worktree metadata is missing' 20

ENV_FILE="$REPO_ROOT/config/vpnkit-local.local.env"
ENV_EXAMPLE="$REPO_ROOT/config/vpnkit-local.env.example"
PATH_GUARD="$SCRIPT_DIR/vpnkit-local-path-guard.sh"
LIFECYCLE="$SCRIPT_DIR/vpnkit-local.sh"
UNDERLAY="$SCRIPT_DIR/vpnkit-local-underlay-routing.sh"
NM_HELPER="$SCRIPT_DIR/vpnkit-local-networkmanager.sh"
ASSETS="$SCRIPT_DIR/vpnkit-local-assets.sh"
RENDERER="$SCRIPT_DIR/vpnkit-render-local-kde-configs.sh"
HOST_SMOKE="$SCRIPT_DIR/vpnkit-local-host-smoke.sh"
TUI="$SCRIPT_DIR/vpnkit_local_kde_tui.py"

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1" 10
}

for command_name in bash python3 docker nmcli ip sudo openssl realpath stat git; do
  require_command "$command_name"
done
docker compose version >/dev/null 2>&1 || die 'Docker Compose plugin is unavailable' 10

require_no_symlink() {
  local path=$1 label=$2 rc
  if path_has_symlink_component "$path"; then
    die "$label contains a symlink component" 20
  else
    rc=$?
    (( rc == 1 )) || die "$label is not an absolute path" 20
  fi
}

require_owned_regular() {
  local path=$1 label=$2 expected_mode=${3:-} links mode
  require_no_symlink "$path" "$label"
  [[ -f "$path" && ! -L "$path" ]] || die "$label must be an owned regular file" 20
  [[ -O "$path" ]] || die "$label must be owned by the invoking user" 20
  links=$(stat -c '%h' -- "$path" 2>/dev/null) || die "$label link count could not be checked" 20
  [[ "$links" == 1 ]] || die "$label must not be hard-linked" 20
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || die "$label mode could not be checked" 20
  if [[ -n "$expected_mode" && "$mode" != "$expected_mode" ]]; then
    die "$label must have mode $expected_mode" 20
  fi
}

require_source_file() {
  local path=$1 label=$2 mode
  require_owned_regular "$path" "$label"
  [[ -x "$path" ]] || die "$label must be executable" 20
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || die "$label mode could not be checked" 20
  # The owner may execute/write, but group/other must never write a source
  # helper that this wrapper is about to execute.
  [[ "${mode: -2}" != *[2367]* ]] || die "$label is writable by group or other" 20
}

require_directory() {
  local path=$1 label=$2 expected_mode=${3:-} mode
  require_no_symlink "$path" "$label"
  [[ -d "$path" && ! -L "$path" ]] || die "$label must be an owned directory" 20
  [[ -O "$path" ]] || die "$label must be owned by the invoking user" 20
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || die "$label mode could not be checked" 20
  if [[ -n "$expected_mode" && "$mode" != "$expected_mode" ]]; then
    die "$label must have mode $expected_mode" 20
  fi
  [[ "${mode: -2}" != *[2367]* ]] || die "$label is writable by group or other" 20
}

# Check every tracked helper before sourcing or executing it. This also makes
# a hard-linked/symlinked source fail closed rather than operating on another
# inode selected by a local filesystem race.
require_source_file "$SCRIPT_PATH" 'installer source'
require_owned_regular "$PATH_GUARD" 'local path guard'
require_source_file "$LIFECYCLE" 'local lifecycle helper'
require_source_file "$UNDERLAY" 'local underlay helper'
require_source_file "$NM_HELPER" 'local NetworkManager helper'
require_source_file "$ASSETS" 'local asset helper'
require_source_file "$RENDERER" 'local config renderer'
require_source_file "$HOST_SMOKE" 'local host smoke helper'
require_owned_regular "$TUI" 'local Python TUI source'
require_owned_regular "$ENV_EXAMPLE" 'tracked local env example'
require_directory "$REPO_ROOT/config" 'config directory'

bash -n "$SCRIPT_PATH" "$PATH_GUARD" "$LIFECYCLE" "$UNDERLAY" "$NM_HELPER" "$ASSETS" "$RENDERER" "$HOST_SMOKE" \
  >/dev/null 2>&1 || die 'tracked local shell sources are syntactically invalid' 20
python3 - "$TUI" >/dev/null 2>&1 <<'PY' || die 'tracked Python TUI source is syntactically invalid' 20
from pathlib import Path
import sys

source = Path(sys.argv[1])
compile(source.read_text(encoding="utf-8"), str(source), "exec")
PY

# The installer deliberately ignores inherited VPNKIT_* control seams. Only
# values from the checked local env file below are passed to the local flow;
# test roots, alternate helpers, host smoke paths, and underlay namespaces can
# never be smuggled into a normal-user install through the parent environment.
clear_inherited_overrides() {
  local name
  while IFS= read -r name; do
    case "$name" in
      VPNKIT_LOCAL_*|VPNKIT_OPENVPN_*|VPNKIT_ROUTING_MODE|VPNKIT_RULESET_SOURCE_MODE|VPNKIT_SELECTED_OUTBOUND_MODE)
        unset "$name" || true
        ;;
    esac
  done < <(compgen -v)
}
clear_inherited_overrides

# A missing env file is created only by the normal flow. Dry-run validates the
# tracked example as the would-be input and remains entirely read-only.
if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
  ENV_FILE_PRESENT=1
  require_owned_regular "$ENV_FILE" 'local env file' 600
  ENV_SOURCE="$ENV_FILE"
else
  if (( DRY_RUN == 1 )); then
    ENV_SOURCE="$ENV_EXAMPLE"
  else
    python3 - "$ENV_EXAMPLE" "$ENV_FILE" >/dev/null 2>&1 <<'PY' || die 'could not create the private local env file' 20
import os
import sys

source_path, destination_path = sys.argv[1:]
flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_DIRECTORY"):
    raise SystemExit("secure file creation flags are unavailable")
flags |= os.O_NOFOLLOW
parent_path, name = os.path.split(destination_path)
parent_fd = os.open(parent_path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
fd = -1
try:
    with open(source_path, "rb") as source:
        data = source.read()
    fd = os.open(name, flags, 0o600, dir_fd=parent_fd)
    os.fchmod(fd, 0o600)
    view = memoryview(data)
    while view:
        written = os.write(fd, view)
        if written <= 0:
            raise OSError("short private env write")
        view = view[written:]
    os.fsync(fd)
    os.fsync(parent_fd)
finally:
    if fd != -1:
        os.close(fd)
    os.close(parent_fd)
PY
    ENV_FILE_PRESENT=1
    require_owned_regular "$ENV_FILE" 'local env file' 600
    ENV_SOURCE="$ENV_FILE"
  fi
fi

# Parse a deliberately small assignment language instead of sourcing the file.
# This prevents command substitutions, redirections, shell functions, and
# arbitrary environment names from executing during installer preflight.
declare -A ENV_SEEN=()
parse_env_file() {
  local line name value
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" != *$'\r'* ]] || die 'local env file contains a carriage return' 20
    case "$line" in
      ''|'#'*) continue ;;
    esac
    [[ "$line" =~ ^([A-Z_][A-Z0-9_]*)=([A-Za-z0-9_./:-]+)$ ]] || die 'local env file contains an unsafe assignment' 20
    name=${BASH_REMATCH[1]}
    value=${BASH_REMATCH[2]}
    case "$name" in
      VPNKIT_LOCAL_SECRETS_DIR|VPNKIT_LOCAL_ENDPOINT|VPNKIT_LOCAL_OPENVPN_PORT|VPNKIT_LOCAL_OPENVPN_PUSH_DNS|\
      VPNKIT_LOCAL_COMPOSE_PROJECT|VPNKIT_LOCAL_DOCKER_SUBNET|VPNKIT_LOCAL_CONTAINER_ADDRESS|\
      VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY|VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY|\
      VPNKIT_LOCAL_RULE_PRIORITY|VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY|VPNKIT_LOCAL_POLICY|\
      VPNKIT_BOOTSTRAP_PICK_ON_START|VPNKIT_BOOTSTRAP_MAX_NODES|VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS|\
      VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS|VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS|\
      VPNKIT_LOCAL_LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS|VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS|\
      VPNKIT_LOCAL_MANAGE_NETWORKMANAGER)
        ;;
      *)
        die 'local env file contains an unsupported variable' 20
        ;;
    esac
    [[ -z "${ENV_SEEN[$name]+present}" ]] || die 'local env file contains a duplicate variable' 20
    ENV_SEEN["$name"]=1
    export "$name=$value"
  done < "$ENV_SOURCE"
}
parse_env_file
[[ "$ENV_SOURCE" == "$ENV_EXAMPLE" || "$ENV_FILE_PRESENT" == 1 ]] || die 'local env source is not canonical' 20
if (( ENV_FILE_PRESENT == 1 )); then
  require_owned_regular "$ENV_FILE" 'local env file' 600
fi

require_env_key() {
  local key=$1
  [[ -n "${ENV_SEEN[$key]+present}" ]] || die "local env file is missing $key" 20
}
validate_integer() {
  local label=$1 value=$2 minimum=$3 maximum=$4
  [[ "$value" =~ ^[0-9]+$ ]] || die "$label must be an integer" 20
  (( 10#$value >= minimum && 10#$value <= maximum )) || die "$label is outside its safe range" 20
}

for key in \
  VPNKIT_LOCAL_SECRETS_DIR VPNKIT_LOCAL_ENDPOINT VPNKIT_LOCAL_OPENVPN_PORT VPNKIT_LOCAL_OPENVPN_PUSH_DNS \
  VPNKIT_LOCAL_COMPOSE_PROJECT VPNKIT_LOCAL_DOCKER_SUBNET VPNKIT_LOCAL_CONTAINER_ADDRESS \
  VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY \
  VPNKIT_LOCAL_RULE_PRIORITY VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY VPNKIT_LOCAL_POLICY \
  VPNKIT_LOCAL_MANAGE_NETWORKMANAGER; do
  require_env_key "$key"
done

[[ "${VPNKIT_LOCAL_SECRETS_DIR:-}" == secrets/vpnkit-local ]] || die 'local env secrets root must be the canonical local root' 20
[[ "${VPNKIT_LOCAL_ENDPOINT:-}" == 127.0.0.1 ]] || die 'local env endpoint must be 127.0.0.1' 20
validate_integer VPNKIT_LOCAL_OPENVPN_PORT "${VPNKIT_LOCAL_OPENVPN_PORT:-}" 1024 65535
case "${VPNKIT_LOCAL_OPENVPN_PUSH_DNS:-}" in 8.8.8.8|8.8.4.4) ;; *) die 'local env pushed DNS must be Google DNS' 20 ;; esac
[[ "${VPNKIT_LOCAL_COMPOSE_PROJECT:-}" == vpnkit-local ]] || die 'local env Compose project must be vpnkit-local' 20
[[ "${VPNKIT_LOCAL_DOCKER_SUBNET:-}" == 172.30.89.0/24 ]] || die 'local env Docker subnet is not the canonical local subnet' 20
[[ "${VPNKIT_LOCAL_CONTAINER_ADDRESS:-}" == 172.30.89.2 ]] || die 'local env container address is not canonical' 20
[[ "${VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY:-}" == 998 ]] || die 'local env destination priority is not canonical' 20
[[ "${VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY:-}" == 999 ]] || die 'local env destination fail-closed priority is not canonical' 20
[[ "${VPNKIT_LOCAL_RULE_PRIORITY:-}" == 1000 ]] || die 'local env source priority is not canonical' 20
[[ "${VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY:-}" == 1001 ]] || die 'local env source fail-closed priority is not canonical' 20
case "${VPNKIT_LOCAL_POLICY:-}" in strict|smart) ;; *) die 'local env policy must be strict or smart' 20 ;; esac
case "${VPNKIT_LOCAL_MANAGE_NETWORKMANAGER:-}" in true|false) ;; *) die 'local env NetworkManager setting is invalid' 20 ;; esac
validate_integer VPNKIT_BOOTSTRAP_MAX_NODES "${VPNKIT_BOOTSTRAP_MAX_NODES:-50}" 1 100
validate_integer VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS "${VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS:-1200}" 1 7200
validate_integer VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS "${VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS:-180}" 1 3600
validate_integer VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS "${VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS:-5}" 0 3600
validate_integer VPNKIT_LOCAL_LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS "${VPNKIT_LOCAL_LIFECYCLE_COMPENSATION_TIMEOUT_SECONDS:-30}" 1 3600
validate_integer VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS "${VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS:-30}" 1 120
case "${VPNKIT_BOOTSTRAP_PICK_ON_START:-true}" in true|false) ;; *) die 'local env bootstrap selection setting is invalid' 20 ;; esac

# Source the shared read-only guard after the env contract is fixed. The
# canonical local root may be missing during dry-run; normal setup requires it
# to have been prepared by the TUI/subscription flow.
# shellcheck disable=SC1090
. "$PATH_GUARD"
BASE="$REPO_ROOT/secrets/vpnkit-local"
vpnkit_local_path_guard_validate_secret_root "$BASE" "$REPO_ROOT" >/dev/null 2>&1 || die 'canonical local secret root failed validation' 20
BASE="$VPNKIT_LOCAL_PATH_GUARD_BASE"
[[ "$BASE" == "$REPO_ROOT/secrets/vpnkit-local" ]] || die 'local secret root resolved outside the canonical tree' 20
vpnkit_local_path_guard_validate_secret_tree "$BASE" >/dev/null 2>&1 || die 'local secret tree failed validation' 20

redact_underlay_plan() {
  local output=$1 line key value
  while IFS= read -r line; do
    [[ "$line" == *=* ]] || continue
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
      command|mutation|dedicated_source_subnet|owned_policy_table|physical_uplink_table|config_version|\
      policy_rule_order|fail_closed_boundary|route_cleanup|systemd_template|networkmanager_dispatcher|\
      install_preflight)
        case "$key" in
          dedicated_source_subnet|owned_policy_table) value=redacted ;;
          *) [[ "$value" =~ ^[a-z0-9-]+$ ]] || value=redacted ;;
        esac
        printf '%s=%s\n' "$key" "$value"
        ;;
    esac
  done <<<"$output"
}

run_underlay_plan() {
  local output
  if ! output=$("$UNDERLAY" plan 2>/dev/null); then
    die 'underlay plan failed' 20
  fi
  printf 'underlay_plan=redacted\n'
  redact_underlay_plan "$output"
}

if (( DRY_RUN == 1 )); then
  PHASE=underlay-plan
  if (( ENV_FILE_PRESENT == 1 )); then printf 'env_file=present\n'; else printf 'env_file=would-create-mode-0600\n'; fi
  if [[ -d "$BASE" && ! -L "$BASE" ]]; then
    require_directory "$BASE" 'local secret root' 700
    printf 'secret_root=canonical\n'
  else
    printf 'secret_root=ready-to-create\n'
  fi
  run_underlay_plan
  printf 'mutation=none\nprivate_values=not_printed\n'
  exit 0
fi

if [[ ! -d "$BASE" || -L "$BASE" ]]; then
  die 'local subscription is not configured; run scripts/vpnkit/vpnkit-local-tui.sh first' 20
fi
require_directory "$BASE" 'local secret root' 700
if [[ ! -d "$BASE/vibe-vpn" || -L "$BASE/vibe-vpn" ]]; then
  die 'local subscription is not configured; run scripts/vpnkit/vpnkit-local-tui.sh first' 20
fi
require_directory "$BASE/vibe-vpn" 'local subscription directory' 700
SUBSCRIPTION="$BASE/vibe-vpn/sub_url"
require_owned_regular "$SUBSCRIPTION" 'local subscription file' 600
[[ -s "$SUBSCRIPTION" ]] || die 'local subscription is not configured; run scripts/vpnkit/vpnkit-local-tui.sh first' 20

PHASE=underlay-plan
printf '\n==> Underlay plan (redacted)\n'
run_underlay_plan

PHASE=underlay-install
printf '\n==> Install/update the local underlay helper (sudo may ask for your password)\n'
if ! sudo -- "$UNDERLAY" install --yes >/dev/null 2>&1; then
  die 'underlay installation failed; the helper should have rolled back its own transaction' 20
fi
if ! "$UNDERLAY" verify >/dev/null 2>&1; then
  die 'underlay verification failed' 20
fi
printf 'underlay_install=verified\n'

PHASE=container-start
printf '\n==> Build and start the localhost-only vpnkit container (NetworkManager is not managed here)\n'
if ! VPNKIT_LOCAL_ENV_FILE=/dev/null VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false \
    VPNKIT_LOCAL_SECRETS_DIR="$BASE" VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local \
    "$LIFECYCLE" start >/dev/null 2>&1; then
  die 'local container start failed' 20
fi
printf 'container_start=complete\n'

# Revalidate the generated profile before handing it to the owned NM adapter.
# The profile itself is never read into output and is never printed.
vpnkit_local_path_guard_validate_secret_tree "$BASE" >/dev/null 2>&1 || die 'local secret tree changed unsafely during container start' 20
PROFILE="$BASE/openvpn/client/vpnkit-local.ovpn"
require_owned_regular "$PROFILE" 'generated local OpenVPN profile' 600
[[ -s "$PROFILE" ]] || die 'local OpenVPN profile was not generated' 20

PHASE=networkmanager-import
printf '\n==> Import the owned NetworkManager profile without connecting it\n'
if ! VPNKIT_LOCAL_ENV_FILE=/dev/null VPNKIT_LOCAL_SECRETS_DIR="$BASE" \
    VPNKIT_LOCAL_PROFILE="$PROFILE" VPNKIT_LOCAL_NM_CONNECTION=vpnkit-local \
    "$NM_HELPER" import --yes >/dev/null 2>&1; then
  die 'NetworkManager profile import failed' 20
fi
printf 'networkmanager_import=complete\nprofile_activation=manual\n'

PHASE=status
printf '\n==> Redacted status\n'
NM_STATUS=
LIFECYCLE_STATUS=
if ! NM_STATUS=$(VPNKIT_LOCAL_ENV_FILE=/dev/null VPNKIT_LOCAL_SECRETS_DIR="$BASE" \
    VPNKIT_LOCAL_PROFILE="$PROFILE" VPNKIT_LOCAL_NM_CONNECTION=vpnkit-local \
    "$NM_HELPER" status 2>/dev/null); then
  die 'NetworkManager status could not be read' 20
fi
if ! LIFECYCLE_STATUS=$(VPNKIT_LOCAL_ENV_FILE=/dev/null VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false \
    VPNKIT_LOCAL_SECRETS_DIR="$BASE" VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local \
    "$LIFECYCLE" status --json 2>/dev/null); then
  die 'local lifecycle status could not be read' 20
fi

printf 'networkmanager_status=redacted\n'
while IFS= read -r status_line; do
  [[ "$status_line" == *=* ]] || continue
  status_key=${status_line%%=*}
  status_value=${status_line#*=}
  case "$status_key" in
    command|mutation|connection|configured|active|ownership|device|profile|private_values)
      case "$status_key" in
        command) [[ "$status_value" == status ]] || status_value=redacted ;;
        mutation) [[ "$status_value" == none ]] || status_value=redacted ;;
        connection) [[ "$status_value" == vpnkit-local ]] || status_value=redacted ;;
        configured|active) [[ "$status_value" == yes || "$status_value" == no || "$status_value" == not-managed || "$status_value" == unavailable ]] || status_value=redacted ;;
        ownership) [[ "$status_value" == owned || "$status_value" == missing || "$status_value" == stale || "$status_value" == foreign || "$status_value" == foreign-collision || "$status_value" == duplicate || "$status_value" == invalid || "$status_value" == source-invalid || "$status_value" == drift || "$status_value" == not-managed || "$status_value" == unavailable ]] || status_value=redacted ;;
        device) [[ "$status_value" == none || "$status_value" == not-managed || "$status_value" == unavailable || "$status_value" =~ ^tun[A-Za-z0-9_.-]{0,14}$ ]] || status_value=redacted ;;
        profile) [[ "$status_value" == ready || "$status_value" == missing ]] || status_value=redacted ;;
        private_values) status_value=not_printed ;;
      esac
      printf '%s=%s\n' "$status_key" "$status_value"
      ;;
  esac
done <<<"$NM_STATUS"

# Parse only a fixed allowlist from lifecycle JSON. No path, UUID, endpoint,
# or arbitrary JSON value is ever forwarded to the terminal.
printf '%s' "$LIFECYCLE_STATUS" | python3 -c '
import json
import sys

try:
    value = json.load(sys.stdin)
except Exception:
    print("lifecycle_status=unavailable")
    print("private_values=not_printed")
    raise SystemExit(0)
if not isinstance(value, dict):
    print("lifecycle_status=unavailable")
    print("private_values=not_printed")
    raise SystemExit(0)

allowed = {
    "container": {"absent", "stopped", "running", "healthy", "unknown"},
    "routing_policy": {"strict", "smart", "unknown"},
    "subscription": {"configured", "missing", "unknown"},
}
def bounded(name, candidate):
    if isinstance(candidate, str) and candidate in allowed[name]:
        return candidate
    return "unknown"

print("lifecycle_container=" + bounded("container", value.get("container")))
print("lifecycle_policy=" + bounded("routing_policy", value.get("routing_policy")))
print("lifecycle_subscription=" + bounded("subscription", value.get("subscription")))
nm = value.get("networkmanager")
if not isinstance(nm, dict):
    nm = {}
nm_states = {"yes", "no", "not-managed", "unavailable"}
for key in ("configured", "active"):
    candidate = nm.get(key)
    if not isinstance(candidate, str) or candidate not in nm_states:
        candidate = "unknown"
    print("lifecycle_nm_" + key + "=" + candidate)
print("private_values=not_printed")
'

cat <<'READY'

READY FOR MANUAL CHECK

KDE profile name: vpnkit-local
The profile is imported but NOT connected. Activate "vpnkit-local" yourself
from KDE Network settings. Existing work VPN connections are not selected or
modified by this installer.

TUI command:
  scripts/vpnkit/vpnkit-local-tui.sh
READY

PHASE=complete
exit 0
