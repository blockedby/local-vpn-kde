#!/usr/bin/env bash
# Managed by vpnkit-local-underlay-routing; do not edit.
set -Eeuo pipefail

# Public-safe, host-side underlay policy routing helper for issue #40.
#
# This file deliberately has no Docker or NetworkManager mutation path.  The
# only NetworkManager integration is the dispatcher installed by `install`;
# it invokes the read/route-only internal refresh action below.  All commands
# that can change host routing are reached only through install, uninstall, or
# the private systemd/dispatcher refresh actions.

TOOL_ID="vpnkit-local-underlay-routing"
MARKER="# Managed by ${TOOL_ID}; do not edit."
DEFAULT_DOCKER_SUBNET="172.30.89.0/24"
DEFAULT_ROUTE_TABLE="51840"
CONFIG_VERSION_V1="1"
CONFIG_VERSION_V2="2"
CURRENT_CONFIG_VERSION="$CONFIG_VERSION_V2"
# VERSION=1 predates the persisted helper digest.  Keep the digest of the
# source-controlled v1 helper so a marker cannot turn an arbitrary executable
# into a migration target.  This is the immutable legacy identity represented
# by the original helper shipped before the VERSION=2 policy extension.
LEGACY_HELPER_DIGEST="d65d83e46988040fd7e6b04e9e183d37a5c669c3bcd1943e93e94d8b4c602399"
# A VERSION=1 config is the original source-only policy.  VERSION=2 adds the
# destination escape/fail-closed pair below; runtime loading must never infer
# that pair from missing keys in an older config.
CONFIG_VERSION="$CURRENT_CONFIG_VERSION"
# No installed config means a clean VERSION=2 candidate using the defaults
# declared below; parsing an installed file replaces these presence markers.
CONFIG_HAS_DESTINATION_RULE_PRIORITY=1
CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=1
CONFIG_HAS_HELPER_DIGEST=1
HELPER_DIGEST=""
# Destination protection runs before the existing source-underlay pair.  The
# slots are intentionally adjacent so no unrelated rule can sit between the
# main-table lookup and its fail-closed fallback.
DEFAULT_DESTINATION_RULE_PRIORITY="998"
DEFAULT_DESTINATION_FAIL_CLOSED_PRIORITY="999"
DEFAULT_RULE_PRIORITY="1000"
DEFAULT_FAIL_CLOSED_PRIORITY="1001"
# These two values identify only routes created by this helper.  Cleanup never
# flushes a table and never deletes a route without both markers.
# Use an unassigned numeric protocol so iproute2 prints it consistently. The
# original prototype used 99, which CachyOS renders as the named protocol
# `openr`; both legacy forms remain removable from the dedicated table.
ROUTE_PROTO="242"
ROUTE_METRIC="42040"

ROOT_PREFIX=${VPNKIT_LOCAL_UNDERLAY_ROOT:-}
if [[ "$ROOT_PREFIX" == "/" ]]; then
  ROOT_PREFIX=""
else
  ROOT_PREFIX=${ROOT_PREFIX%/}
fi

# A prefixed root is the disposable/mock namespace used by the contract tests;
# its top-level inode may be owned by the unprivileged test runner.  In the
# real host namespace the only accepted owner is UID 0.
ROOT_NAMESPACE_OWNER=0
if [[ -n "$ROOT_PREFIX" && -d "$ROOT_PREFIX" ]]; then
  ROOT_NAMESPACE_OWNER=$(stat -c '%u' -- "$ROOT_PREFIX" 2>/dev/null) || ROOT_NAMESPACE_OWNER=0
fi

root_path() {
  local suffix=$1
  if [[ -n "$ROOT_PREFIX" ]]; then
    printf '%s%s\n' "$ROOT_PREFIX" "$suffix"
  else
    printf '%s\n' "$suffix"
  fi
}

CONFIG_PATH=$(root_path "/etc/${TOOL_ID}.conf")
HELPER_PATH=$(root_path "/usr/local/libexec/${TOOL_ID}")
SERVICE_PATH=$(root_path "/etc/systemd/system/${TOOL_ID}.service")
DISPATCHER_PATH=$(root_path "/etc/NetworkManager/dispatcher.d/90-${TOOL_ID}")
STATE_DIR=$(root_path "/var/lib/${TOOL_ID}")
STATE_PATH="${STATE_DIR}/routes.state"
SERVICE_NAME="${TOOL_ID}.service"
# All mutating entry points share this advisory lock.  Keep it outside the
# removable state directory so uninstall never removes the coordination inode;
# tests and disposable roots may override the path.
LOCK_PATH=${VPNKIT_LOCAL_UNDERLAY_LOCK_FILE:-$(root_path "/run/lock/${TOOL_ID}.lock")}
LOCK_WAIT_SECONDS=${VPNKIT_LOCAL_UNDERLAY_LOCK_WAIT_SECONDS:-5}
LOCK_FD=""
LOCK_HELD=0

DOCKER_SUBNET=${VPNKIT_LOCAL_DOCKER_SUBNET:-$DEFAULT_DOCKER_SUBNET}
ROUTE_TABLE=${VPNKIT_LOCAL_ROUTE_TABLE:-$DEFAULT_ROUTE_TABLE}
DESTINATION_RULE_PRIORITY=${VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY:-$DEFAULT_DESTINATION_RULE_PRIORITY}
DESTINATION_FAIL_CLOSED_PRIORITY=${VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY:-$DEFAULT_DESTINATION_FAIL_CLOSED_PRIORITY}
RULE_PRIORITY=${VPNKIT_LOCAL_RULE_PRIORITY:-$DEFAULT_RULE_PRIORITY}
FAIL_CLOSED_PRIORITY=${VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY:-$DEFAULT_FAIL_CLOSED_PRIORITY}
UPLINK_IFACE=${VPNKIT_LOCAL_UPLINK_IFACE:-}
UPLINK_TABLE=${VPNKIT_LOCAL_UPLINK_TABLE:-}
UPLINK_GATEWAY=${VPNKIT_LOCAL_UPLINK_GATEWAY:-}

ENV_DOCKER_SET=0
ENV_ROUTE_TABLE_SET=0
ENV_DESTINATION_RULE_PRIORITY_SET=0
ENV_DESTINATION_FAIL_CLOSED_PRIORITY_SET=0
ENV_RULE_PRIORITY_SET=0
ENV_FAIL_CLOSED_PRIORITY_SET=0
ENV_UPLINK_IFACE_SET=0
ENV_UPLINK_TABLE_SET=0
ENV_UPLINK_GATEWAY_SET=0
[[ ${VPNKIT_LOCAL_DOCKER_SUBNET+x} ]] && ENV_DOCKER_SET=1
[[ ${VPNKIT_LOCAL_ROUTE_TABLE+x} ]] && ENV_ROUTE_TABLE_SET=1
[[ ${VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY+x} ]] && ENV_DESTINATION_RULE_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY+x} ]] && ENV_DESTINATION_FAIL_CLOSED_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_RULE_PRIORITY+x} ]] && ENV_RULE_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY+x} ]] && ENV_FAIL_CLOSED_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_IFACE+x} ]] && ENV_UPLINK_IFACE_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_TABLE+x} ]] && ENV_UPLINK_TABLE_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_GATEWAY+x} ]] && ENV_UPLINK_GATEWAY_SET=1

CLI_DOCKER_SET=0
CLI_ROUTE_TABLE_SET=0
CLI_DESTINATION_RULE_PRIORITY_SET=0
CLI_DESTINATION_FAIL_CLOSED_PRIORITY_SET=0
CLI_RULE_PRIORITY_SET=0
CLI_FAIL_CLOSED_PRIORITY_SET=0
CLI_UPLINK_IFACE_SET=0
CLI_UPLINK_TABLE_SET=0
CLI_UPLINK_GATEWAY_SET=0
YES=0
COMMAND="plan"

# These are deliberately test-only seams.  They are evaluated only by the
# install transaction and never persisted into a managed file.
TEST_FAILPOINT=${VPNKIT_LOCAL_UNDERLAY_TEST_FAILPOINT:-${VPNKIT_LOCAL_INSTALL_FAILPOINT:-${VPNKIT_LOCAL_TEST_FAILPOINT:-}}}
TEST_FAILPOINT_ACTION=${VPNKIT_LOCAL_UNDERLAY_TEST_FAILPOINT_ACTION:-${VPNKIT_LOCAL_INSTALL_FAILPOINT_ACTION:-${VPNKIT_LOCAL_TEST_FAILPOINT_ACTION:-}}}
TEST_SIGNAL_AFTER=${VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_AFTER:-${VPNKIT_LOCAL_INSTALL_SIGNAL_AFTER:-${VPNKIT_LOCAL_TEST_SIGNAL_AFTER:-}}}
TEST_SIGNAL=${VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL:-${VPNKIT_LOCAL_INSTALL_SIGNAL:-${VPNKIT_LOCAL_TEST_SIGNAL:-}}}
TEST_SIGNAL_DELAY=${VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_DELAY:-${VPNKIT_LOCAL_INSTALL_SIGNAL_DELAY:-${VPNKIT_LOCAL_TEST_SIGNAL_DELAY:-0}}}
TEST_EXIT_AFTER=${VPNKIT_LOCAL_UNDERLAY_TEST_EXIT_AFTER:-${VPNKIT_LOCAL_INSTALL_EXIT_AFTER:-${VPNKIT_LOCAL_TEST_EXIT_AFTER:-}}}
TEST_EXIT_STATUS=${VPNKIT_LOCAL_UNDERLAY_TEST_EXIT_STATUS:-${VPNKIT_LOCAL_INSTALL_EXIT_STATUS:-${VPNKIT_LOCAL_TEST_EXIT_STATUS:-77}}}
TEST_SIGNAL_INJECTOR_PID=""

# Discovery results are kept in variables and are never printed.
ROUTE_SNAPSHOT=""
RULE_SNAPSHOT=""
NM_SNAPSHOT=""
DISCOVERED_IFACE=""
DISCOVERED_TABLE=""
DISCOVERED_GATEWAY=""
DISCOVERED_LINK_PREFIX=""

MANAGED_DEFAULT_COUNT=0
MANAGED_LINK_COUNT=0
DESTINATION_LOOKUP_RULE_COUNT=0
DESTINATION_BLOCK_RULE_COUNT=0
DESTINATION_LOOKUP_RULE_COLLISION=0
DESTINATION_BLOCK_RULE_COLLISION=0
DESTINATION_LOOKUP_RULE_WRONG_PRIORITY=0
DESTINATION_BLOCK_RULE_WRONG_PRIORITY=0
DESTINATION_PRECEDING_BYPASS=0
LOOKUP_RULE_COUNT=0
BLOCK_RULE_COUNT=0
LOOKUP_RULE_COLLISION=0
BLOCK_RULE_COLLISION=0
LOOKUP_RULE_WRONG_PRIORITY=0
BLOCK_RULE_WRONG_PRIORITY=0
PRECEDING_BYPASS=0

# Install transaction state is global because EXIT and signal traps can run
# after install_action's local scope has unwound.  The state is intentionally
# explicit: one rollback, no recursive traps, and a stable original status.
TRANSACTION_ACTIVE=0
TRANSACTION_ROLLBACK_RUNNING=0
TRANSACTION_ROLLBACK_DONE=0
TRANSACTION_ROLLBACK_FAILED=0
TRANSACTION_PENDING_SIGNAL=0
TRANSACTION_ORIGINAL_STATUS=0
INSTALL_BACKUP_DIR=""
INSTALL_BACKED_UP=()
INSTALL_NEWLY_CREATED=()
INSTALL_HAD_PRIOR_CONFIG=0
INSTALL_FILES_STARTED=0
INSTALL_RUNTIME_STARTED=0
INSTALL_PRIOR_SERVICE_ENABLED=0
INSTALL_PRIOR_SERVICE_ACTIVE=0
INSTALL_PRIOR_SERVICE_FILE_PRESENT=0
INSTALL_PRIOR_CONFIG_VERSION=""
INSTALL_PRIOR_DOCKER_SUBNET=""
INSTALL_PRIOR_ROUTE_TABLE=""
INSTALL_PRIOR_DESTINATION_RULE_PRIORITY=""
INSTALL_PRIOR_DESTINATION_FAIL_CLOSED_PRIORITY=""
INSTALL_PRIOR_RULE_PRIORITY=""
INSTALL_PRIOR_FAIL_CLOSED_PRIORITY=""
INSTALL_PRIOR_UPLINK_IFACE=""
INSTALL_PRIOR_UPLINK_TABLE=""
INSTALL_PRIOR_UPLINK_GATEWAY=""
INSTALL_PRIOR_CONFIG_HAS_DESTINATION_RULE_PRIORITY=0
INSTALL_PRIOR_CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=0
INSTALL_PRIOR_CONFIG_HAS_HELPER_DIGEST=0
INSTALL_PRIOR_HELPER_DIGEST=""
INSTALL_PRIOR_DESTINATION_LOOKUP_PRESENT=0
INSTALL_PRIOR_DESTINATION_BLOCK_PRESENT=0
INSTALL_PRIOR_LOOKUP_PRESENT=0
INSTALL_PRIOR_BLOCK_PRESENT=0
INSTALL_PRIOR_MANAGED_ROUTES=()
UNINSTALL_PREFLIGHT_ACTION=""

fail() {
  # Keep failures intentionally value-free.  In particular, do not include
  # route, gateway, interface, endpoint, or command output in diagnostics.
  printf '%s\n' "$1" >&2
  exit "${2:-1}"
}

acquire_mutation_lock() {
  (( LOCK_HELD == 1 )) && return 0
  command -v flock >/dev/null 2>&1 || fail "flock is unavailable" 10
  [[ "$LOCK_WAIT_SECONDS" =~ ^[0-9]+([.][0-9]+)?$ ]] || fail "invalid lock wait" 2
  [[ -n "$LOCK_PATH" ]] || fail "invalid lock path" 2

  local lock_dir=${LOCK_PATH%/*}
  [[ "$lock_dir" == "$LOCK_PATH" ]] && lock_dir=.
  local old_umask
  old_umask=$(umask)
  umask 077
  mkdir -p -- "$lock_dir" || {
    umask "$old_umask"
    fail "cannot prepare routing lock" 20
  }
  # Append-open never truncates a path if an external actor swaps in a
  # symlink between preflight and open; the preflight above must already have
  # rejected such a path before any host cleanup.  A new coordination inode is
  # deliberately created as root 0600 and remains empty.
  if ! exec {LOCK_FD}>>"$LOCK_PATH"; then
    umask "$old_umask"
    fail "cannot open routing lock" 20
  fi
  umask "$old_umask"
  if ! flock -x -w "$LOCK_WAIT_SECONDS" "$LOCK_FD"; then
    eval "exec ${LOCK_FD}>&-" || true
    LOCK_FD=""
    fail "routing transaction lock is busy" 20
  fi
  LOCK_HELD=1
}

release_mutation_lock() {
  if (( LOCK_HELD == 1 )); then
    flock -u "$LOCK_FD" >/dev/null 2>&1 || true
    eval "exec ${LOCK_FD}>&-" || true
    LOCK_FD=""
    LOCK_HELD=0
  fi
}

signal_number() {
  case "$1" in
    HUP|SIGHUP) printf '%s\n' 1 ;;
    INT|SIGINT) printf '%s\n' 2 ;;
    TERM|SIGTERM) printf '%s\n' 15 ;;
    *) return 1 ;;
  esac
}

transaction_signal_trap() {
  local signal=$1
  local number
  number=$(signal_number "$signal") || return 0

  # Never enter rollback recursively.  A second signal during compensation is
  # remembered and dealt with after the fail-closed sequence completes.
  if (( TRANSACTION_ROLLBACK_RUNNING == 1 )); then
    (( TRANSACTION_PENDING_SIGNAL == 0 )) && TRANSACTION_PENDING_SIGNAL=$number
    return 0
  fi
  if (( TRANSACTION_ACTIVE == 1 )); then
    TRANSACTION_PENDING_SIGNAL=$number
    TRANSACTION_ORIGINAL_STATUS=$((128 + number))
    transaction_rollback "$TRANSACTION_ORIGINAL_STATUS"
    exit "$TRANSACTION_ORIGINAL_STATUS"
  fi

  # Outside a transaction retain normal shell signal semantics.
  trap - "$signal"
  kill -"$signal" "$$"
}

transaction_exit_trap() {
  local status=$?
  if (( TRANSACTION_ACTIVE == 1 && TRANSACTION_ROLLBACK_DONE == 0 )); then
    TRANSACTION_ORIGINAL_STATUS=$status
    transaction_rollback "$status"
  fi

  # A signal received while rollback was already running must not cause a
  # second rollback.  Preserve the signal's conventional 128+N status.
  if (( TRANSACTION_PENDING_SIGNAL > 0 )); then
    status=$((128 + TRANSACTION_PENDING_SIGNAL))
  elif (( TRANSACTION_ROLLBACK_FAILED == 1 && status == 0 )); then
    status=21
  fi
  TRANSACTION_ROLLBACK_RUNNING=0
  trap - EXIT INT TERM HUP
  release_mutation_lock
  exit "$status"
}

transaction_begin() {
  TRANSACTION_ACTIVE=1
  TRANSACTION_ROLLBACK_RUNNING=0
  TRANSACTION_ROLLBACK_DONE=0
  TRANSACTION_ROLLBACK_FAILED=0
  TRANSACTION_PENDING_SIGNAL=0
  TRANSACTION_ORIGINAL_STATUS=0
  trap transaction_exit_trap EXIT
  trap 'transaction_signal_trap INT' INT
  trap 'transaction_signal_trap TERM' TERM
  trap 'transaction_signal_trap HUP' HUP
}

stop_signal_injector() {
  if [[ -n "$TEST_SIGNAL_INJECTOR_PID" ]]; then
    kill "$TEST_SIGNAL_INJECTOR_PID" >/dev/null 2>&1 || true
    wait "$TEST_SIGNAL_INJECTOR_PID" >/dev/null 2>&1 || true
    TEST_SIGNAL_INJECTOR_PID=""
  fi
}

install_test_failpoint() {
  local point=$1
  local action=""
  local signal=""
  local status="$TEST_EXIT_STATUS"
  local delay="$TEST_SIGNAL_DELAY"
  local window_after=${VPNKIT_LOCAL_UNDERLAY_TEST_WINDOW_AFTER:-${VPNKIT_LOCAL_INSTALL_WINDOW_AFTER:-${VPNKIT_LOCAL_TEST_WINDOW_AFTER:-}}}
  local window_seconds=${VPNKIT_LOCAL_UNDERLAY_TEST_WINDOW_SECONDS:-${VPNKIT_LOCAL_INSTALL_WINDOW_SECONDS:-${VPNKIT_LOCAL_TEST_WINDOW_SECONDS:-0}}}

  if [[ "$TEST_SIGNAL_AFTER" == "$point" ]]; then
    action=signal
    signal=$TEST_SIGNAL
  elif [[ "$TEST_EXIT_AFTER" == "$point" ]]; then
    action=exit
  elif [[ "$TEST_FAILPOINT" == "$point" || "$TEST_FAILPOINT" == "$point:"* ]]; then
    action=${TEST_FAILPOINT_ACTION:-}
    signal=$TEST_SIGNAL
    if [[ "$TEST_FAILPOINT" == "$point:"* ]]; then
      local encoded_signal=${TEST_FAILPOINT#*:}
      signal=${signal:-$encoded_signal}
      action=${action:-signal}
    elif [[ -n "$signal" ]]; then
      action=${action:-signal}
    else
      action=${action:-failure}
    fi
  else
    [[ "$window_after" == "$point" ]] || return 0
  fi

  if [[ "$window_after" == "$point" ]]; then
    [[ "$window_seconds" =~ ^[0-9]+([.][0-9]+)?$ ]] || fail "invalid test window" 2
    sleep "$window_seconds"
  fi

  case "$action" in
    "") return 0 ;;
    failure|fail)
      return 1
      ;;
    exit)
      [[ "$status" =~ ^[0-9]+$ ]] || fail "invalid test exit status" 2
      exit "$status"
      ;;
    signal)
      [[ -n "$signal" ]] || fail "test signal is missing" 2
      signal_number "$signal" >/dev/null || fail "invalid test signal" 2
      [[ "$delay" =~ ^[0-9]+([.][0-9]+)?$ ]] || fail "invalid test signal delay" 2
      # Use a child injector rather than relying on shell-timing races.  This
      # exercises the same parent/child boundary as systemd or ip subprocesses.
      (
        trap - INT TERM HUP
        sleep "$delay"
        # `$$` remains the transaction shell's PID inside a Bash subshell;
        # PPID may instead name the test harness when job control re-parents
        # the injector.
        kill -"$signal" "$$"
      ) &
      TEST_SIGNAL_INJECTOR_PID=$!
      wait "$TEST_SIGNAL_INJECTOR_PID" >/dev/null 2>&1 || true
      TEST_SIGNAL_INJECTOR_PID=""
      ;;
    *)
      fail "invalid test failpoint action" 2
      ;;
  esac
}

rollback_route_line_is_owned() {
  local line=$1
  local dest proto metric iface gateway
  dest=${line%% *}
  proto=$(route_field "$line" proto 2>/dev/null) || proto=""
  metric=$(route_field "$line" metric 2>/dev/null) || metric=""
  iface=$(route_field "$line" dev 2>/dev/null) || iface=""
  gateway=$(route_field "$line" via 2>/dev/null) || gateway=""
  route_proto_is_owned "$proto" || return 1
  [[ "$metric" == "$ROUTE_METRIC" ]] || return 1
  [[ "$dest" == default ]] || valid_ipv4_cidr "$dest" || return 1
  valid_physical_iface_name "$iface" || return 1
  [[ -z "$gateway" ]] || valid_ipv4 "$gateway" || return 1
}

rollback_add_route_from_line() {
  local line=$1
  rollback_route_line_is_owned "$line" || return 1
  local dest proto metric iface gateway scope src
  dest=${line%% *}
  proto=$(route_field "$line" proto 2>/dev/null) || return 1
  metric=$(route_field "$line" metric 2>/dev/null) || return 1
  iface=$(route_field "$line" dev 2>/dev/null) || return 1
  gateway=$(route_field "$line" via 2>/dev/null) || gateway=""
  scope=$(route_field "$line" scope 2>/dev/null) || scope=""
  src=$(route_field "$line" src 2>/dev/null) || src=""
  local -a args=(-4 route replace "$dest")
  [[ -z "$gateway" ]] || args+=(via "$gateway")
  args+=(dev "$iface")
  [[ "$scope" == link ]] && args+=(scope link)
  [[ -n "$src" ]] && args+=(src "$src")
  args+=(table "$ROUTE_TABLE" proto "$proto" metric "$metric")
  run_ip_mutation "${args[@]}"
}

rollback_retry() {
  local attempt
  for ((attempt = 0; attempt < 2; attempt++)); do
    "$@" && return 0
  done
  return 1
}

rollback_add_rule_if_missing() {
  local kind=$1
  local count priority
  read_rule_snapshot || return 1
  scan_rules
  case "$kind" in
    destination-block)
      count=$DESTINATION_BLOCK_RULE_COUNT
      priority=$DESTINATION_FAIL_CLOSED_PRIORITY
      (( count == 0 )) || return 0
      (( DESTINATION_BLOCK_RULE_COLLISION == 0 && DESTINATION_BLOCK_RULE_WRONG_PRIORITY == 0 )) || return 1
      run_ip_mutation -4 rule add priority "$priority" to "$DOCKER_SUBNET" unreachable
      ;;
    source-block)
      count=$BLOCK_RULE_COUNT
      priority=$FAIL_CLOSED_PRIORITY
      (( count == 0 )) || return 0
      (( BLOCK_RULE_COLLISION == 0 && BLOCK_RULE_WRONG_PRIORITY == 0 )) || return 1
      run_ip_mutation -4 rule add priority "$priority" from "$DOCKER_SUBNET" unreachable
      ;;
    destination-lookup)
      count=$DESTINATION_LOOKUP_RULE_COUNT
      priority=$DESTINATION_RULE_PRIORITY
      (( count == 0 )) || return 0
      (( DESTINATION_LOOKUP_RULE_COLLISION == 0 && DESTINATION_LOOKUP_RULE_WRONG_PRIORITY == 0 )) || return 1
      run_ip_mutation -4 rule add priority "$priority" to "$DOCKER_SUBNET" lookup main suppress_prefixlength 0
      ;;
    source-lookup)
      count=$LOOKUP_RULE_COUNT
      priority=$RULE_PRIORITY
      (( count == 0 )) || return 0
      (( LOOKUP_RULE_COLLISION == 0 && LOOKUP_RULE_WRONG_PRIORITY == 0 )) || return 1
      run_ip_mutation -4 rule add priority "$priority" from "$DOCKER_SUBNET" table "$ROUTE_TABLE"
      ;;
    *) return 1 ;;
  esac
}

rollback_remove_candidate_runtime() {
  # The candidate config is still loaded here.  Keep blockers in place while
  # removing lookups/routes; if a lookup was installed before its blocker, add
  # the blocker first.  Every operation is exact and table-scoped.
  local failed=0
  local destination_boundary_ready=1
  local source_boundary_ready=1
  read_rule_snapshot || return 1
  scan_rules
  if (( DESTINATION_LOOKUP_RULE_COUNT > 0 && DESTINATION_BLOCK_RULE_COUNT == 0 )); then
    if ! rollback_retry rollback_add_rule_if_missing destination-block; then
      destination_boundary_ready=0
      failed=1
    fi
  fi
  read_rule_snapshot || return 1
  scan_rules
  if (( LOOKUP_RULE_COUNT > 0 && BLOCK_RULE_COUNT == 0 )); then
    if ! rollback_retry rollback_add_rule_if_missing source-block; then
      source_boundary_ready=0
      failed=1
    fi
  fi

  if (( destination_boundary_ready == 1 )); then
    rollback_retry remove_destination_lookup_rules || failed=1
  fi
  if (( source_boundary_ready == 1 )); then
    rollback_retry remove_lookup_rules || failed=1
  fi
  rollback_retry remove_marked_routes || failed=1
  # If a lookup could not be removed, retain its blocker.  That is the
  # fail-closed outcome even when a signal killed an ip subprocess mid-step.
  if (( destination_boundary_ready == 1 )); then
    rollback_retry remove_destination_block_rules || failed=1
  fi
  if (( source_boundary_ready == 1 )); then
    rollback_retry remove_block_rules || failed=1
  fi
  return "$failed"
}

rollback_restore_prior_rules() {
  # Restore only the rule forms that existed before the transaction.  Foreign
  # rules are never flushed, adopted, or removed.
  if config_uses_destination_policy; then
    if (( INSTALL_PRIOR_DESTINATION_BLOCK_PRESENT > 0 )); then
      rollback_add_rule_if_missing destination-block || return 1
    fi
  fi
  if (( INSTALL_PRIOR_BLOCK_PRESENT > 0 )); then
    rollback_add_rule_if_missing source-block || return 1
  fi

  if config_uses_destination_policy; then
    if (( INSTALL_PRIOR_DESTINATION_LOOKUP_PRESENT > 0 )); then
      rollback_add_rule_if_missing destination-lookup || return 1
    fi
  fi
  if (( INSTALL_PRIOR_LOOKUP_PRESENT > 0 )); then
    rollback_add_rule_if_missing source-lookup || return 1
  fi
}

rollback_restore_prior_routes() {
  # Replace the helper-owned routes in the old table with the exact pre-call
  # snapshot, without touching unrelated routes in that table.
  read_owned_route_snapshot || return 1
  remove_marked_routes || return 1
  local line
  for line in "${INSTALL_PRIOR_MANAGED_ROUTES[@]}"; do
    rollback_add_route_from_line "$line" || return 1
  done
}

rollback_restore_service_state() {
  local failed=0
  # A clean install has no prior unit to enable or disable.  Once the
  # candidate file is removed, daemon-reload already restores the desired
  # inactive/disabled state; issuing `systemctl disable` for a nonexistent
  # unit would turn a successful rollback into a false failure.
  if (( INSTALL_PRIOR_SERVICE_FILE_PRESENT == 0 && INSTALL_PRIOR_SERVICE_ENABLED == 0 && INSTALL_PRIOR_SERVICE_ACTIVE == 0 )); then
    return 0
  fi
  if (( INSTALL_PRIOR_SERVICE_ENABLED == 1 )); then
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || failed=1
  else
    systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || {
      # Disabled/not-found is already the desired state for a prior inactive
      # unit; only a still-enabled unit makes compensation incomplete.
      systemctl is-enabled --quiet "$SERVICE_NAME" >/dev/null 2>&1 && failed=1 || true
    }
  fi
  if (( INSTALL_PRIOR_SERVICE_ACTIVE == 1 )); then
    systemctl start "$SERVICE_NAME" >/dev/null 2>&1 || failed=1
  else
    systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || {
      systemctl is-active --quiet "$SERVICE_NAME" >/dev/null 2>&1 && failed=1 || true
    }
  fi
  (( failed == 0 ))
}

rollback_restore_files() {
  local entry original backup failed=0
  for entry in "${INSTALL_BACKED_UP[@]}"; do
    original=${entry%%|*}
    backup=${entry#*|}
    cp -p -- "$backup" "$original" || failed=1
  done
  for original in "${INSTALL_NEWLY_CREATED[@]}"; do
    rm -f -- "$original" || failed=1
  done
  return "$failed"
}

transaction_rollback() {
  local requested_status=${1:-1}
  local failed=0
  local entry original backup

  (( TRANSACTION_ROLLBACK_DONE == 0 )) || return "$TRANSACTION_ROLLBACK_FAILED"
  TRANSACTION_ROLLBACK_DONE=1
  TRANSACTION_ACTIVE=0
  TRANSACTION_ROLLBACK_RUNNING=1
  TRANSACTION_ORIGINAL_STATUS=$requested_status
  # Compensation is best effort but never allowed to abort halfway through
  # because of errexit or an inherited ERR trap.
  set +e
  trap - ERR
  stop_signal_injector

  # A candidate service must not execute the candidate helper while files and
  # policy are being put back.  Do this even when enable --now was never
  # reached: the pre-call service may have been active.
  if (( INSTALL_FILES_STARTED == 1 )); then
    systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || {
      systemctl is-active --quiet "$SERVICE_NAME" >/dev/null 2>&1 && failed=1 || true
    }
    # Disable a candidate that was not enabled before the transaction while
    # its unit file still exists.  This also covers clean installs whose old
    # unit path was absent; after file removal there is nothing to disable.
    if (( INSTALL_PRIOR_SERVICE_ENABLED == 0 )); then
      systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || {
        systemctl is-enabled --quiet "$SERVICE_NAME" >/dev/null 2>&1 && failed=1 || true
      }
    fi
  fi

  if (( INSTALL_RUNTIME_STARTED == 1 )); then
    if ! rollback_remove_candidate_runtime; then
      failed=1
    fi
    # Load the saved v1/v2 values from memory, not from the candidate file.
    if (( INSTALL_HAD_PRIOR_CONFIG == 1 )); then
      CONFIG_VERSION=$INSTALL_PRIOR_CONFIG_VERSION
      DOCKER_SUBNET=$INSTALL_PRIOR_DOCKER_SUBNET
      ROUTE_TABLE=$INSTALL_PRIOR_ROUTE_TABLE
      DESTINATION_RULE_PRIORITY=$INSTALL_PRIOR_DESTINATION_RULE_PRIORITY
      DESTINATION_FAIL_CLOSED_PRIORITY=$INSTALL_PRIOR_DESTINATION_FAIL_CLOSED_PRIORITY
      RULE_PRIORITY=$INSTALL_PRIOR_RULE_PRIORITY
      FAIL_CLOSED_PRIORITY=$INSTALL_PRIOR_FAIL_CLOSED_PRIORITY
      UPLINK_IFACE=$INSTALL_PRIOR_UPLINK_IFACE
      UPLINK_TABLE=$INSTALL_PRIOR_UPLINK_TABLE
      UPLINK_GATEWAY=$INSTALL_PRIOR_UPLINK_GATEWAY
      CONFIG_HAS_DESTINATION_RULE_PRIORITY=$INSTALL_PRIOR_CONFIG_HAS_DESTINATION_RULE_PRIORITY
      CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=$INSTALL_PRIOR_CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY
      CONFIG_HAS_HELPER_DIGEST=$INSTALL_PRIOR_CONFIG_HAS_HELPER_DIGEST
      HELPER_DIGEST=$INSTALL_PRIOR_HELPER_DIGEST
      rollback_retry rollback_restore_prior_rules || failed=1
      rollback_retry rollback_restore_prior_routes || failed=1
    fi
  fi

  rollback_retry rollback_restore_files || failed=1
  systemctl daemon-reload >/dev/null 2>&1 || failed=1
  if (( INSTALL_FILES_STARTED == 1 )); then
    rollback_retry rollback_restore_service_state || failed=1
  fi
  [[ -z "$INSTALL_BACKUP_DIR" ]] || rm -rf -- "$INSTALL_BACKUP_DIR" || failed=1

  if (( failed != 0 )); then
    TRANSACTION_ROLLBACK_FAILED=1
    printf '%s\n' "install rollback incomplete" >&2
  fi
  return "$failed"
}

usage() {
  cat <<'EOF'
Usage: scripts/vpnkit/vpnkit-local-underlay-routing.sh [command] [options]

Commands (default: plan; all except install/uninstall are read-only):
  plan       Show a redacted, non-mutating installation plan.
  status     Show redacted file, route, and rule state.
  verify     Verify the fail-closed policy and managed runtime state.
  install    Install bounded systemd/NetworkManager hooks and apply routing.
  uninstall  Remove only this helper's files, rules, and marked routes.

Mutation safety:
  install and uninstall require both direct root and --yes.
  No command invokes a privilege-escalation helper.  No command manages
  Docker or imports/changes a NetworkManager connection.  The installed
  dispatcher only refreshes routes.

Options:
  --docker-subnet CIDR       Dedicated Docker source/destination subnet (IPv4 CIDR).
  --route-table ID            Helper-owned policy table (numeric ID).
  --destination-rule-priority N
                              Docker-destination main lookup priority.
  --destination-fail-closed-priority N
                              Docker-destination unreachable priority.
  --rule-priority N           Source-underlay lookup rule priority.
  --fail-closed-priority N    Source-underlay unreachable rule priority.
  --uplink-iface NAME         Physical Wi-Fi/Ethernet interface, or auto.
  --uplink-table ID           Existing physical route table, or auto/main.
  --uplink-gateway IPv4       Gateway, or auto.
  --yes                       Explicit confirmation required for mutation.
  -h, --help                  Show this help without probing or mutating.

Environment equivalents are VPNKIT_LOCAL_* names matching the option names.
Values are kept in the root-owned local config and are never printed by this
helper.
EOF
}

need_arg() {
  [[ $# -ge 2 && -n ${2:-} ]] || fail "missing option argument" 2
}

parse_args() {
  if [[ $# -gt 0 ]]; then
    case "$1" in
      plan|status|verify|install|uninstall)
        COMMAND=$1
        shift
        ;;
      --runtime-refresh)
        COMMAND="runtime-refresh"
        shift
        ;;
      --runtime-clean)
        COMMAND="runtime-clean"
        shift
        ;;
      -*) ;;
      *)
        fail "unknown command" 2
        ;;
    esac
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --docker-subnet)
        need_arg "$@"
        DOCKER_SUBNET=$2
        CLI_DOCKER_SET=1
        shift 2
        ;;
      --route-table)
        need_arg "$@"
        ROUTE_TABLE=$2
        CLI_ROUTE_TABLE_SET=1
        shift 2
        ;;
      --destination-rule-priority)
        need_arg "$@"
        DESTINATION_RULE_PRIORITY=$2
        CLI_DESTINATION_RULE_PRIORITY_SET=1
        shift 2
        ;;
      --destination-fail-closed-priority)
        need_arg "$@"
        DESTINATION_FAIL_CLOSED_PRIORITY=$2
        CLI_DESTINATION_FAIL_CLOSED_PRIORITY_SET=1
        shift 2
        ;;
      --rule-priority)
        need_arg "$@"
        RULE_PRIORITY=$2
        CLI_RULE_PRIORITY_SET=1
        shift 2
        ;;
      --fail-closed-priority)
        need_arg "$@"
        FAIL_CLOSED_PRIORITY=$2
        CLI_FAIL_CLOSED_PRIORITY_SET=1
        shift 2
        ;;
      --uplink-iface)
        need_arg "$@"
        UPLINK_IFACE=$2
        CLI_UPLINK_IFACE_SET=1
        shift 2
        ;;
      --uplink-table)
        need_arg "$@"
        UPLINK_TABLE=$2
        CLI_UPLINK_TABLE_SET=1
        shift 2
        ;;
      --uplink-gateway)
        need_arg "$@"
        UPLINK_GATEWAY=$2
        CLI_UPLINK_GATEWAY_SET=1
        shift 2
        ;;
      --yes)
        YES=1
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      --version)
        printf '%s\n' "$TOOL_ID"
        exit 0
        ;;
      *)
        fail "unknown option" 2
        ;;
    esac
  done
}

# Configuration is a deliberately tiny key/value format.  It is parsed, not
# sourced, so a damaged file cannot turn a refresh into arbitrary shell code.
load_kv_file() {
  local path=$1
  local line key value
  local parsed_version=""
  local parsed_docker_subnet=""
  local parsed_route_table=""
  local parsed_destination_rule_priority=""
  local parsed_destination_fail_closed_priority=""
  local parsed_rule_priority=""
  local parsed_fail_closed_priority=""
  local parsed_uplink_iface=""
  local parsed_uplink_table=""
  local parsed_uplink_gateway=""
  local parsed_helper_digest=""
  local version_seen=0
  local docker_subnet_seen=0
  local route_table_seen=0
  local destination_rule_priority_seen=0
  local destination_fail_closed_priority_seen=0
  local rule_priority_seen=0
  local fail_closed_priority_seen=0
  local uplink_iface_seen=0
  local uplink_table_seen=0
  local uplink_gateway_seen=0
  local helper_digest_seen=0
  [[ -r "$path" ]] || return 1

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || return 1
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
      VERSION)
        (( version_seen == 0 )) || return 1
        [[ "$value" == "$CONFIG_VERSION_V1" || "$value" == "$CONFIG_VERSION_V2" ]] || return 1
        parsed_version=$value
        version_seen=1
        ;;
      DOCKER_SUBNET)
        (( docker_subnet_seen == 0 )) || return 1
        parsed_docker_subnet=$value
        docker_subnet_seen=1
        ;;
      ROUTE_TABLE)
        (( route_table_seen == 0 )) || return 1
        parsed_route_table=$value
        route_table_seen=1
        ;;
      DESTINATION_RULE_PRIORITY)
        (( destination_rule_priority_seen == 0 )) || return 1
        parsed_destination_rule_priority=$value
        destination_rule_priority_seen=1
        ;;
      DESTINATION_FAIL_CLOSED_PRIORITY)
        (( destination_fail_closed_priority_seen == 0 )) || return 1
        parsed_destination_fail_closed_priority=$value
        destination_fail_closed_priority_seen=1
        ;;
      RULE_PRIORITY)
        (( rule_priority_seen == 0 )) || return 1
        parsed_rule_priority=$value
        rule_priority_seen=1
        ;;
      FAIL_CLOSED_PRIORITY)
        (( fail_closed_priority_seen == 0 )) || return 1
        parsed_fail_closed_priority=$value
        fail_closed_priority_seen=1
        ;;
      UPLINK_IFACE)
        (( uplink_iface_seen == 0 )) || return 1
        parsed_uplink_iface=$value
        uplink_iface_seen=1
        ;;
      UPLINK_TABLE)
        (( uplink_table_seen == 0 )) || return 1
        parsed_uplink_table=$value
        uplink_table_seen=1
        ;;
      UPLINK_GATEWAY)
        (( uplink_gateway_seen == 0 )) || return 1
        parsed_uplink_gateway=$value
        uplink_gateway_seen=1
        ;;
      HELPER_DIGEST)
        (( helper_digest_seen == 0 )) || return 1
        parsed_helper_digest=$value
        helper_digest_seen=1
        ;;
      *)
        return 1
        ;;
    esac
  done < "$path"

  # Do not let process defaults fill a missing persisted field.  A file that
  # is syntactically key/value-shaped but incomplete is still malformed.
  (( version_seen == 1 && docker_subnet_seen == 1 && route_table_seen == 1 &&
     rule_priority_seen == 1 && fail_closed_priority_seen == 1 &&
     uplink_iface_seen == 1 && uplink_table_seen == 1 && uplink_gateway_seen == 1 )) || return 1

  if [[ "$parsed_version" == "$CONFIG_VERSION_V1" ]]; then
    # A v1 file is deliberately source-only.  Reject a mixed file rather than
    # silently turning an old helper into a destination-aware one.  V1 predates
    # the helper digest and is retained only as a migration input.
    (( destination_rule_priority_seen == 0 && destination_fail_closed_priority_seen == 0 && helper_digest_seen == 0 )) || return 1
  else
    # VERSION=2 is a complete persisted contract.  Do not infer any field or
    # accept a pre-digest shape: the helper digest binds this config to the
    # exact helper bytes that are about to be migrated or updated.
    (( destination_rule_priority_seen == 1 && destination_fail_closed_priority_seen == 1 && helper_digest_seen == 1 )) || return 1
    [[ "$parsed_helper_digest" =~ ^[0-9a-f]{64}$ ]] || return 1
  fi

  CONFIG_VERSION=$parsed_version
  DOCKER_SUBNET=$parsed_docker_subnet
  ROUTE_TABLE=$parsed_route_table
  DESTINATION_RULE_PRIORITY=$parsed_destination_rule_priority
  DESTINATION_FAIL_CLOSED_PRIORITY=$parsed_destination_fail_closed_priority
  RULE_PRIORITY=$parsed_rule_priority
  FAIL_CLOSED_PRIORITY=$parsed_fail_closed_priority
  UPLINK_IFACE=$parsed_uplink_iface
  UPLINK_TABLE=$parsed_uplink_table
  UPLINK_GATEWAY=$parsed_uplink_gateway
  HELPER_DIGEST=$parsed_helper_digest
  CONFIG_HAS_DESTINATION_RULE_PRIORITY=$destination_rule_priority_seen
  CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=$destination_fail_closed_priority_seen
  CONFIG_HAS_HELPER_DIGEST=$helper_digest_seen
}

load_installed_config_if_allowed() {
  [[ -r "$CONFIG_PATH" ]] || return 0
  # Read-only commands ignore an unrelated config; install/uninstall perform
  # their own ownership checks before any mutation.
  managed_file "$CONFIG_PATH" || return 0
  local saved_docker=$DOCKER_SUBNET
  local saved_route_table=$ROUTE_TABLE
  local saved_destination_rule_priority=$DESTINATION_RULE_PRIORITY
  local saved_destination_fail_closed_priority=$DESTINATION_FAIL_CLOSED_PRIORITY
  local saved_rule_priority=$RULE_PRIORITY
  local saved_fail_closed_priority=$FAIL_CLOSED_PRIORITY
  local saved_iface=$UPLINK_IFACE
  local saved_uplink_table=$UPLINK_TABLE
  local saved_gateway=$UPLINK_GATEWAY

  # Parse into the working variables first; restore fields explicitly supplied
  # by the operator afterwards.
  load_kv_file "$CONFIG_PATH" || fail "managed config is invalid"
  if (( CLI_DOCKER_SET || ENV_DOCKER_SET )); then DOCKER_SUBNET=$saved_docker; fi
  if (( CLI_ROUTE_TABLE_SET || ENV_ROUTE_TABLE_SET )); then ROUTE_TABLE=$saved_route_table; fi
  if (( CLI_DESTINATION_RULE_PRIORITY_SET || ENV_DESTINATION_RULE_PRIORITY_SET )); then DESTINATION_RULE_PRIORITY=$saved_destination_rule_priority; fi
  if (( CLI_DESTINATION_FAIL_CLOSED_PRIORITY_SET || ENV_DESTINATION_FAIL_CLOSED_PRIORITY_SET )); then DESTINATION_FAIL_CLOSED_PRIORITY=$saved_destination_fail_closed_priority; fi
  if (( CLI_RULE_PRIORITY_SET || ENV_RULE_PRIORITY_SET )); then RULE_PRIORITY=$saved_rule_priority; fi
  if (( CLI_FAIL_CLOSED_PRIORITY_SET || ENV_FAIL_CLOSED_PRIORITY_SET )); then FAIL_CLOSED_PRIORITY=$saved_fail_closed_priority; fi
  if (( CLI_UPLINK_IFACE_SET || ENV_UPLINK_IFACE_SET )); then UPLINK_IFACE=$saved_iface; fi
  if (( CLI_UPLINK_TABLE_SET || ENV_UPLINK_TABLE_SET )); then UPLINK_TABLE=$saved_uplink_table; fi
  if (( CLI_UPLINK_GATEWAY_SET || ENV_UPLINK_GATEWAY_SET )); then UPLINK_GATEWAY=$saved_gateway; fi
}

load_config_for_runtime() {
  [[ -r "$CONFIG_PATH" ]] || fail "managed config is missing"
  managed_file "$CONFIG_PATH" || fail "managed config ownership check failed"
  load_kv_file "$CONFIG_PATH" || fail "managed config is invalid"
}

valid_ipv4() {
  local value=$1
  local old_ifs=$IFS
  local -a octets
  IFS=.
  read -r -a octets <<< "$value"
  IFS=$old_ifs
  [[ ${#octets[@]} -eq 4 ]] || return 1
  local octet
  for octet in "${octets[@]}"; do
    [[ "$octet" =~ ^[0-9]{1,3}$ ]] || return 1
    (( 10#$octet <= 255 )) || return 1
  done
}

valid_ipv4_cidr() {
  local value=$1
  [[ "$value" == */* ]] || return 1
  local address=${value%/*}
  local prefix=${value##*/}
  valid_ipv4 "$address" || return 1
  [[ "$prefix" =~ ^[0-9]{1,2}$ ]] || return 1
  (( prefix >= 1 && prefix <= 32 )) || return 1

  local old_ifs=$IFS
  local -a octets
  IFS=.
  read -r -a octets <<< "$address"
  IFS=$old_ifs
  local number=$(( (10#${octets[0]} << 24) | (10#${octets[1]} << 16) | (10#${octets[2]} << 8) | 10#${octets[3]} ))
  local mask
  if (( prefix == 32 )); then
    mask=4294967295
  else
    mask=$(( (4294967295 << (32 - prefix)) & 4294967295 ))
  fi
  (( (number & mask) == number )) || return 1
}

ipv4_number() {
  local address=$1
  local old_ifs=$IFS
  local -a octets
  IFS=.
  read -r -a octets <<< "$address"
  IFS=$old_ifs
  printf '%s\n' "$(( (10#${octets[0]} << 24) | (10#${octets[1]} << 16) | (10#${octets[2]} << 8) | 10#${octets[3]} ))"
}

cidrs_overlap() {
  local first=$1
  local second=$2
  valid_ipv4_cidr "$first" || return 1
  valid_ipv4_cidr "$second" || return 1
  local first_address=${first%/*}
  local first_prefix=${first##*/}
  local second_address=${second%/*}
  local second_prefix=${second##*/}
  local first_number second_number common_prefix mask
  first_number=$(ipv4_number "$first_address")
  second_number=$(ipv4_number "$second_address")
  common_prefix=$first_prefix
  (( second_prefix < common_prefix )) && common_prefix=$second_prefix
  if (( common_prefix == 32 )); then
    mask=4294967295
  else
    mask=$(( (4294967295 << (32 - common_prefix)) & 4294967295 ))
  fi
  (( (first_number & mask) == (second_number & mask) ))
}

valid_table_id() {
  local value=$1
  [[ "$value" =~ ^[0-9]{1,10}$ ]] || return 1
  (( value >= 1 && value <= 4294967295 ))
}

valid_uplink_table() {
  [[ -z "$1" || "$1" == auto || "$1" == main ]] && return 0
  valid_table_id "$1"
}

valid_iface() {
  [[ "$1" =~ ^[A-Za-z0-9_.-]{1,15}$ ]]
}

valid_physical_iface_name() {
  valid_iface "$1" || return 1
  # Positive allow-list: only conventional Ethernet/Wi-Fi/mobile physical
  # uplink names are eligible. VPN, PPP, tunnel, bridge, container, and mesh
  # interfaces are rejected even when NetworkManager reports them connected.
  case "$1" in
    en*|eth*|eno*|ens*|enx*|wl*|wlan*|wlp*|wlo*|ath*|ra*|wwan*|usb*|bond*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

config_uses_destination_policy() {
  [[ "$CONFIG_VERSION" == "$CONFIG_VERSION_V2" ]]
}

validate_config() {
  [[ "$CONFIG_VERSION" == "$CONFIG_VERSION_V1" || "$CONFIG_VERSION" == "$CONFIG_VERSION_V2" ]] || fail "unsupported managed config version" 2
  valid_ipv4_cidr "$DOCKER_SUBNET" || fail "invalid dedicated subnet" 2
  valid_table_id "$ROUTE_TABLE" || fail "invalid route table" 2
  valid_uplink_table "$UPLINK_TABLE" || fail "invalid uplink table" 2
  [[ "$RULE_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid rule priority" 2
  [[ "$FAIL_CLOSED_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid fail-closed priority" 2
  (( RULE_PRIORITY >= 1 && RULE_PRIORITY < FAIL_CLOSED_PRIORITY && FAIL_CLOSED_PRIORITY < 32766 )) || fail "invalid rule ordering" 2

  if config_uses_destination_policy; then
    [[ "$CONFIG_HAS_DESTINATION_RULE_PRIORITY" == 1 && "$CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY" == 1 ]] || fail "managed config is missing destination policy fields" 2
    [[ "$DESTINATION_RULE_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid destination rule priority" 2
    [[ "$DESTINATION_FAIL_CLOSED_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid destination fail-closed priority" 2
    (( DESTINATION_RULE_PRIORITY >= 1 && DESTINATION_RULE_PRIORITY < DESTINATION_FAIL_CLOSED_PRIORITY && DESTINATION_FAIL_CLOSED_PRIORITY < RULE_PRIORITY )) || fail "invalid rule ordering" 2
    (( DESTINATION_FAIL_CLOSED_PRIORITY == DESTINATION_RULE_PRIORITY + 1 )) || fail "destination rules must be adjacent" 2
  fi

  [[ -z "$UPLINK_IFACE" || "$UPLINK_IFACE" == auto ]] || valid_physical_iface_name "$UPLINK_IFACE" || fail "invalid uplink interface" 2
  [[ -z "$UPLINK_GATEWAY" ]] || valid_ipv4 "$UPLINK_GATEWAY" || fail "invalid uplink gateway" 2
  if [[ "$UPLINK_TABLE" =~ ^[0-9]+$ && "$UPLINK_TABLE" == "$ROUTE_TABLE" ]]; then
    fail "uplink and owned tables must differ" 2
  fi
}

read_route_snapshot() {
  command -v ip >/dev/null 2>&1 || return 1
  ROUTE_SNAPSHOT=$(ip -4 route show table all 2>/dev/null) || return 1
  return 0
}

read_rule_snapshot() {
  command -v ip >/dev/null 2>&1 || return 1
  RULE_SNAPSHOT=$(ip -4 rule show 2>/dev/null) || return 1
  return 0
}

read_nm_snapshot() {
  NM_SNAPSHOT=""
  if command -v nmcli >/dev/null 2>&1; then
    NM_SNAPSHOT=$(nmcli -t --escape no -f DEVICE,TYPE,STATE device status 2>/dev/null) || NM_SNAPSHOT=""
  fi
}

route_line_is_default() {
  local line=$1
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  [[ ${fields[0]:-} == default ]]
}

route_line_has_dev() {
  local line=$1
  local wanted=$2
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  local i
  for ((i = 0; i < ${#fields[@]}; i++)); do
    if [[ ${fields[i]} == dev && ${fields[i+1]:-} == "$wanted" ]]; then
      return 0
    fi
  done
  return 1
}

route_line_table() {
  local line=$1
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  local i
  for ((i = 0; i < ${#fields[@]}; i++)); do
    if [[ ${fields[i]} == table ]]; then
      printf '%s\n' "${fields[i+1]:-}"
      return 0
    fi
  done
  printf '%s\n' main
}

table_matches() {
  local wanted=$1
  local actual=$2
  if [[ "$wanted" == "" || "$wanted" == auto ]]; then
    return 0
  fi
  if [[ "$wanted" == main || "$wanted" == 254 ]]; then
    [[ "$actual" == main || "$actual" == 254 ]]
    return
  fi
  [[ "$wanted" == "$actual" ]]
}

route_line_matches() {
  local line=$1
  local iface=$2
  local table=$3
  route_line_has_dev "$line" "$iface" || return 1
  local actual_table
  actual_table=$(route_line_table "$line") || return 1
  table_matches "$table" "$actual_table"
}

extract_after_token() {
  local line=$1
  local token=$2
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  local i
  for ((i = 0; i < ${#fields[@]}; i++)); do
    if [[ ${fields[i]} == "$token" ]]; then
      printf '%s\n' "${fields[i+1]:-}"
      return 0
    fi
  done
  return 1
}

discover_uplink() {
  DISCOVERED_IFACE=""
  DISCOVERED_TABLE=""
  DISCOVERED_GATEWAY=""
  DISCOVERED_LINK_PREFIX=""

  read_route_snapshot || return 1
  read_nm_snapshot

  local -a candidates=()
  local line iface type state candidate seen already
  if [[ -n "$UPLINK_IFACE" && "$UPLINK_IFACE" != auto ]]; then
    candidates+=("$UPLINK_IFACE")
  else
    # Keep every connected physical candidate.  A connected secondary NIC can
    # exist without a default route; discovery must try the next candidate.
    while IFS= read -r line || [[ -n "$line" ]]; do
      [[ -n "$line" ]] || continue
      local old_ifs=$IFS
      IFS=:
      read -r iface type state <<< "$line"
      IFS=$old_ifs
      [[ "$state" == connected* ]] || continue
      case "$type" in
        wifi|ethernet|802-3-ethernet|802-11-wireless)
          valid_physical_iface_name "$iface" || continue
          already=0
          for seen in "${candidates[@]}"; do
            if [[ "$seen" == "$iface" ]]; then already=1; break; fi
          done
          if (( already == 0 )); then candidates+=("$iface"); fi
          ;;
      esac
    done <<< "$NM_SNAPSHOT"

    # Add physical interfaces observed on default routes after NM candidates;
    # this also supports systems without a usable nmcli query.
    while IFS= read -r line || [[ -n "$line" ]]; do
      route_line_is_default "$line" || continue
      candidate=$(extract_after_token "$line" dev 2>/dev/null) || candidate=""
      valid_physical_iface_name "$candidate" || continue
      already=0
      for seen in "${candidates[@]}"; do
        if [[ "$seen" == "$candidate" ]]; then already=1; break; fi
      done
      if (( already == 0 )); then candidates+=("$candidate"); fi
    done <<< "$ROUTE_SNAPSHOT"
  fi
  [[ ${#candidates[@]} -gt 0 ]] || return 1

  # Pick a current default and link route for each candidate.  When a table
  # was explicitly supplied, it is a source-table constraint rather than the
  # helper-owned destination table.
  local selected_default actual_table candidate_gateway candidate_prefix candidate_table
  for DISCOVERED_IFACE in "${candidates[@]}"; do
    selected_default=""
    actual_table=""
    while IFS= read -r line || [[ -n "$line" ]]; do
      route_line_is_default "$line" || continue
      route_line_matches "$line" "$DISCOVERED_IFACE" "$UPLINK_TABLE" || continue
      candidate_table=$(route_line_table "$line") || continue
      [[ "$candidate_table" == 254 ]] && candidate_table=main
      # Once installed, `table all` also contains our own default. Never use it
      # as the physical source route; continue to the real NM/main default.
      [[ "$candidate_table" != "$ROUTE_TABLE" ]] || continue
      selected_default=$line
      actual_table=$candidate_table
      break
    done <<< "$ROUTE_SNAPSHOT"
    [[ -n "$selected_default" && -n "$actual_table" ]] || continue
    candidate_gateway=$(extract_after_token "$selected_default" via 2>/dev/null) || candidate_gateway=""
    valid_ipv4 "$candidate_gateway" || continue
    if [[ -n "$UPLINK_GATEWAY" && "$UPLINK_GATEWAY" != "$candidate_gateway" ]]; then
      continue
    fi

    candidate_prefix=""
    while IFS= read -r line || [[ -n "$line" ]]; do
      route_line_matches "$line" "$DISCOVERED_IFACE" "$actual_table" || continue
      route_line_is_default "$line" && continue
      [[ "$line" == *" scope link"* || "$line" == *" scope link "* ]] || continue
      candidate=${line%% *}
      valid_ipv4_cidr "$candidate" || continue
      candidate_prefix=$candidate
      break
    done <<< "$ROUTE_SNAPSHOT"
    [[ -n "$candidate_prefix" ]] || continue
    # A dedicated Docker source subnet must not overlap the physical gateway
    # network; otherwise the policy route could mis-handle local gateway traffic.
    if cidrs_overlap "$DOCKER_SUBNET" "$candidate_prefix"; then
      continue
    fi

    DISCOVERED_TABLE=$actual_table
    DISCOVERED_GATEWAY=${UPLINK_GATEWAY:-$candidate_gateway}
    DISCOVERED_LINK_PREFIX=$candidate_prefix
    return 0
  done
  DISCOVERED_IFACE=""
  return 1
}

scan_rules() {
  MANAGED_DEFAULT_COUNT=0
  MANAGED_LINK_COUNT=0
  DESTINATION_LOOKUP_RULE_COUNT=0
  DESTINATION_BLOCK_RULE_COUNT=0
  DESTINATION_LOOKUP_RULE_COLLISION=0
  DESTINATION_BLOCK_RULE_COLLISION=0
  DESTINATION_LOOKUP_RULE_WRONG_PRIORITY=0
  DESTINATION_BLOCK_RULE_WRONG_PRIORITY=0
  DESTINATION_PRECEDING_BYPASS=0
  LOOKUP_RULE_COUNT=0
  BLOCK_RULE_COUNT=0
  LOOKUP_RULE_COLLISION=0
  BLOCK_RULE_COLLISION=0
  LOOKUP_RULE_WRONG_PRIORITY=0
  BLOCK_RULE_WRONG_PRIORITY=0
  PRECEDING_BYPASS=0

  local line first priority rest
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    line=$(normalize_words "$line")
    first=${line%% *}
    [[ "$first" == *: ]] || continue
    priority=${first%:}
    [[ "$priority" =~ ^[0-9]+$ ]] || continue
    rest=${line#* }

    if config_uses_destination_policy; then
      # iproute2 may print an implicit `from all` for a destination rule.  The
      # helper owns both normalized forms.  The default-suppressing form is
      # intentional: an unsuppressed main lookup could fall through to the
      # host default route and is therefore treated as a collision/unsafe rule.
      if destination_lookup_rule_matches "$rest"; then
        if [[ "$priority" == "$DESTINATION_RULE_PRIORITY" ]]; then
          DESTINATION_LOOKUP_RULE_COUNT=$((DESTINATION_LOOKUP_RULE_COUNT + 1))
        else
          DESTINATION_LOOKUP_RULE_WRONG_PRIORITY=1
        fi
        continue
      fi
      if destination_block_rule_matches "$rest"; then
        if [[ "$priority" == "$DESTINATION_FAIL_CLOSED_PRIORITY" ]]; then
          DESTINATION_BLOCK_RULE_COUNT=$((DESTINATION_BLOCK_RULE_COUNT + 1))
        else
          DESTINATION_BLOCK_RULE_WRONG_PRIORITY=1
        fi
        continue
      fi
    fi

    if [[ "$rest" == "from $DOCKER_SUBNET lookup $ROUTE_TABLE" || "$rest" == "from $DOCKER_SUBNET table $ROUTE_TABLE" ]]; then
      if [[ "$priority" == "$RULE_PRIORITY" ]]; then
        LOOKUP_RULE_COUNT=$((LOOKUP_RULE_COUNT + 1))
      else
        LOOKUP_RULE_WRONG_PRIORITY=1
      fi
      continue
    fi
    if [[ "$rest" == "from $DOCKER_SUBNET unreachable" ]]; then
      if [[ "$priority" == "$FAIL_CLOSED_PRIORITY" ]]; then
        BLOCK_RULE_COUNT=$((BLOCK_RULE_COUNT + 1))
      else
        BLOCK_RULE_WRONG_PRIORITY=1
      fi
      continue
    fi

    # Every rule at an owned slot is a collision unless the exact owned form
    # was counted above.  Do not replace a work-VPN or other foreign rule.
    if config_uses_destination_policy; then
      if [[ "$priority" == "$DESTINATION_RULE_PRIORITY" ]]; then
        DESTINATION_LOOKUP_RULE_COLLISION=1
      fi
      if [[ "$priority" == "$DESTINATION_FAIL_CLOSED_PRIORITY" ]]; then
        DESTINATION_BLOCK_RULE_COLLISION=1
      fi

      # A foreign rule that can match the Docker destination before the
      # destination fail-closed boundary would route around the main lookup.
      if (( priority > 0 && priority < DESTINATION_FAIL_CLOSED_PRIORITY )); then
        if rule_may_match_docker_destination "$rest"; then
          DESTINATION_PRECEDING_BYPASS=1
        fi
      fi
    fi
    if [[ "$priority" == "$RULE_PRIORITY" ]]; then
      LOOKUP_RULE_COLLISION=1
    fi
    if [[ "$priority" == "$FAIL_CLOSED_PRIORITY" ]]; then
      BLOCK_RULE_COLLISION=1
    fi

    # Rules before the owned source lookup that can match this source would
    # defeat the source fail-closed boundary.  The exact destination policy is
    # intentionally exempt: it is the local bridge escape hatch that must run
    # before the existing source-underlay pair.
    if (( priority > 0 && priority < FAIL_CLOSED_PRIORITY )); then
      if rule_may_match_docker "$rest"; then
        PRECEDING_BYPASS=1
      fi
    fi
  done <<< "$RULE_SNAPSHOT"
}

normalize_words() {
  local value=$1
  local old_ifs=$IFS
  local -a words
  # iproute2 commonly separates the rule priority with a tab on Arch/CachyOS.
  # Normalize all standard shell whitespace before matching exact owned rules.
  IFS=$' \t\n'
  read -r -a words <<< "$value"
  IFS=$old_ifs
  printf '%s\n' "${words[*]}"
}

rule_may_match_docker() {
  local rest=$1
  [[ "$rest" == from\ all* ]] && return 0

  local source=""
  if [[ "$rest" == from\ * || "$rest" == *" from "* ]]; then
    source=$(extract_after_token "$rest" from 2>/dev/null) || source=""
    [[ "$source" == all || "$source" == 0.0.0.0/0 || "$source" == */0 ]] && return 0
    if valid_ipv4 "$source"; then source="${source}/32"; fi
    if valid_ipv4_cidr "$source" && cidrs_overlap "$DOCKER_SUBNET" "$source"; then
      return 0
    fi
    return 1
  fi

  # A rule with no source selector is broad for this source.  Refuse to place
  # the helper behind it if it performs a route lookup before our boundary.
  [[ "$rest" == *" lookup "* || "$rest" == *" blackhole"* || "$rest" == *" prohibit"* || "$rest" == *" unreachable"* ]]
}

destination_lookup_rule_matches() {
  local rest=$1
  [[ "$rest" == from\ all\ * ]] && rest=${rest#from all }
  [[ "$rest" == "to $DOCKER_SUBNET lookup main suppress_prefixlength 0" ]]
}

destination_block_rule_matches() {
  local rest=$1
  [[ "$rest" == from\ all\ * ]] && rest=${rest#from all }
  [[ "$rest" == "to $DOCKER_SUBNET unreachable" ]]
}

destination_rule_matches_subnet() {
  local rest=$1 destination
  [[ "$rest" == to\ * || "$rest" == *" to "* ]] || return 1
  destination=$(extract_after_token "$rest" to 2>/dev/null) || return 1
  [[ "$destination" == all || "$destination" == 0.0.0.0/0 || "$destination" == */0 ]] && return 0
  if valid_ipv4 "$destination"; then destination="${destination}/32"; fi
  valid_ipv4_cidr "$destination" && cidrs_overlap "$DOCKER_SUBNET" "$destination"
}

rule_may_match_docker_destination() {
  local rest=$1
  if [[ "$rest" == to\ * || "$rest" == *" to "* ]]; then
    # A destination selector for another network cannot bypass this boundary;
    # a selector that overlaps the Docker subnet can.
    destination_rule_matches_subnet "$rest" && return 0
    return 1
  fi

  # Broad work-VPN rules commonly use `from all` (or its CIDR equivalent)
  # without a destination.  A narrower source-specific rule is not assumed
  # to match the host's physical source.
  [[ "$rest" == from\ all* ]] && return 0
  if [[ "$rest" == from\ * || "$rest" == *" from "* ]]; then
    local source
    source=$(extract_after_token "$rest" from 2>/dev/null) || source=""
    [[ "$source" == all || "$source" == 0.0.0.0/0 || "$source" == */0 ]] && return 0
    if valid_ipv4 "$source"; then source="${source}/32"; fi
    if valid_ipv4_cidr "$source" && cidrs_overlap "$DOCKER_SUBNET" "$source"; then
      return 0
    fi
    return 1
  fi
  [[ "$rest" == *" lookup "* || "$rest" == *" blackhole"* || "$rest" == *" prohibit"* || "$rest" == *" unreachable"* ]]
}

route_field() {
  local line=$1
  local token=$2
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  local i
  for ((i = 0; i < ${#fields[@]}; i++)); do
    if [[ ${fields[i]} == "$token" ]]; then
      printf '%s\n' "${fields[i+1]:-}"
      return 0
    fi
  done
  return 1
}

route_proto_is_owned() {
  case "$1" in
    "$ROUTE_PROTO"|99|openr) return 0 ;;
    *) return 1 ;;
  esac
}

scan_managed_routes() {
  MANAGED_DEFAULT_COUNT=0
  MANAGED_LINK_COUNT=0
  local line dest proto metric
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    dest=${line%% *}
    proto=$(route_field "$line" proto 2>/dev/null) || proto=""
    metric=$(route_field "$line" metric 2>/dev/null) || metric=""
    route_proto_is_owned "$proto" && [[ "$metric" == "$ROUTE_METRIC" ]] || continue
    if [[ "$dest" == default ]]; then
      MANAGED_DEFAULT_COUNT=$((MANAGED_DEFAULT_COUNT + 1))
      continue
    fi
    if valid_ipv4_cidr "$dest"; then
      MANAGED_LINK_COUNT=$((MANAGED_LINK_COUNT + 1))
    fi
  done <<< "$ROUTE_TABLE_SNAPSHOT"
}

read_owned_route_snapshot() {
  command -v ip >/dev/null 2>&1 || return 1
  local all_routes
  if ! ROUTE_TABLE_SNAPSHOT=$(ip -4 route show table "$ROUTE_TABLE" 2>/dev/null); then
    # iproute2 returns an error when a numeric table has never had a route.
    # Treat that as an empty owned table only when a successful all-table read
    # confirms there is no route tagged with this table ID.
    all_routes=$(ip -4 route show table all 2>/dev/null) || return 1
    if grep -Eq "(^|[[:space:]])table[[:space:]]+$ROUTE_TABLE([[:space:]]|$)" <<<"$all_routes"; then
      return 1
    fi
    ROUTE_TABLE_SNAPSHOT=""
  fi
  scan_managed_routes
}

write_plain_kv() {
  local path=$1
  local mode=$2
  shift 2
  local dir tmp
  dir=${path%/*}
  mkdir -p "$dir" || return 1
  tmp=$(mktemp "${path}.tmp.XXXXXX") || return 1
  {
    printf '%s\n' "$MARKER"
    while [[ $# -gt 1 ]]; do
      printf '%s=%s\n' "$1" "$2"
      shift 2
    done
  } > "$tmp" || { rm -f "$tmp"; return 1; }
  chmod "$mode" "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$path" || { rm -f "$tmp"; return 1; }
}

helper_digest() {
  local digest
  digest=$(sha256sum -- "$HELPER_PATH" 2>/dev/null) || return 1
  digest=${digest%% *}
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "$digest"
}

render_config() {
  printf '%s\n' "$MARKER"
  printf 'VERSION=%s\n' "$CONFIG_VERSION"
  printf 'DOCKER_SUBNET=%s\n' "$DOCKER_SUBNET"
  printf 'ROUTE_TABLE=%s\n' "$ROUTE_TABLE"
  if config_uses_destination_policy; then
    printf 'DESTINATION_RULE_PRIORITY=%s\n' "$DESTINATION_RULE_PRIORITY"
    printf 'DESTINATION_FAIL_CLOSED_PRIORITY=%s\n' "$DESTINATION_FAIL_CLOSED_PRIORITY"
  fi
  printf 'RULE_PRIORITY=%s\n' "$RULE_PRIORITY"
  printf 'FAIL_CLOSED_PRIORITY=%s\n' "$FAIL_CLOSED_PRIORITY"
  printf 'UPLINK_IFACE=%s\n' "$UPLINK_IFACE"
  printf 'UPLINK_TABLE=%s\n' "$UPLINK_TABLE"
  printf 'UPLINK_GATEWAY=%s\n' "$UPLINK_GATEWAY"
  if config_uses_destination_policy; then
    printf 'HELPER_DIGEST=%s\n' "$HELPER_DIGEST"
  fi
}

render_state() {
  local route_table=$1
  local link_prefix=$2
  local link_iface=$3
  local gateway=$4
  printf '%s\n' "$MARKER"
  printf '%s\n' 'VERSION=1'
  printf 'ROUTE_TABLE=%s\n' "$route_table"
  printf 'LINK_PREFIX=%s\n' "$link_prefix"
  printf 'LINK_IFACE=%s\n' "$link_iface"
  printf 'GATEWAY=%s\n' "$gateway"
}

write_config() {
  # The file contains only validated local routing choices, not credentials;
  # read access keeps status/verify usable without recurring root commands.
  [[ "$CONFIG_VERSION" == "$CONFIG_VERSION_V2" ]] || return 1
  local digest
  digest=$(helper_digest) || return 1
  HELPER_DIGEST=$digest
  CONFIG_HAS_HELPER_DIGEST=1
  local rendered
  rendered=$(mktemp "${CONFIG_PATH}.tmp.XXXXXX") || return 1
  render_config > "$rendered" || { rm -f -- "$rendered"; return 1; }
  chmod 0644 "$rendered" || { rm -f -- "$rendered"; return 1; }
  mv -f -- "$rendered" "$CONFIG_PATH" || { rm -f -- "$rendered"; return 1; }
}

write_state() {
  chmod 0700 "$STATE_DIR" >/dev/null 2>&1 || return 1
  if [[ -e "$STATE_PATH" || -L "$STATE_PATH" ]]; then
    managed_file "$STATE_PATH" || return 1
  fi
  write_plain_kv "$STATE_PATH" 0600 \
    VERSION 1 \
    ROUTE_TABLE "$ROUTE_TABLE" \
    LINK_PREFIX "$DISCOVERED_LINK_PREFIX" \
    LINK_IFACE "$DISCOVERED_IFACE" \
    GATEWAY "$DISCOVERED_GATEWAY"
}

load_state() {
  [[ -r "$STATE_PATH" ]] || return 1
  local old_route=$ROUTE_TABLE
  local state_route="" state_prefix="" state_iface="" state_gateway=""
  local line line_no=0
  while IFS= read -r line; do
    line_no=$((line_no + 1))
    case "$line_no" in
      1) [[ "$line" == "$MARKER" ]] || return 1 ;;
      2) [[ "$line" == VERSION=1 ]] || return 1 ;;
      3) [[ "$line" == ROUTE_TABLE=* ]] || return 1; state_route=${line#ROUTE_TABLE=} ;;
      4) [[ "$line" == LINK_PREFIX=* ]] || return 1; state_prefix=${line#LINK_PREFIX=} ;;
      5) [[ "$line" == LINK_IFACE=* ]] || return 1; state_iface=${line#LINK_IFACE=} ;;
      6) [[ "$line" == GATEWAY=* ]] || return 1; state_gateway=${line#GATEWAY=} ;;
      *) return 1 ;;
    esac
  done < "$STATE_PATH"
  (( line_no == 6 )) || return 1
  [[ "$state_route" == "$old_route" ]] || return 1
  valid_ipv4_cidr "$state_prefix" || return 1
  valid_physical_iface_name "$state_iface" || return 1
  valid_ipv4 "$state_gateway" || return 1
  STATE_LINK_PREFIX=$state_prefix
  STATE_LINK_IFACE=$state_iface
  STATE_GATEWAY=$state_gateway
}

run_ip_mutation() {
  # All mutating ip output is suppressed; callers emit only fixed diagnostics.
  ip "$@" >/dev/null 2>&1
}

rule_exists_destination_lookup() {
  (( DESTINATION_LOOKUP_RULE_COUNT > 0 ))
}

rule_exists_destination_block() {
  (( DESTINATION_BLOCK_RULE_COUNT > 0 ))
}

rule_exists_lookup() {
  (( LOOKUP_RULE_COUNT > 0 ))
}

rule_exists_block() {
  (( BLOCK_RULE_COUNT > 0 ))
}

ensure_rule_slots_safe() {
  if config_uses_destination_policy; then
    (( DESTINATION_LOOKUP_RULE_COLLISION == 0 && DESTINATION_LOOKUP_RULE_WRONG_PRIORITY == 0 )) || fail "destination lookup rule priority is occupied" 20
    (( DESTINATION_BLOCK_RULE_COLLISION == 0 && DESTINATION_BLOCK_RULE_WRONG_PRIORITY == 0 )) || fail "destination fail-closed rule priority is occupied" 20
    (( DESTINATION_LOOKUP_RULE_COUNT <= 1 && DESTINATION_BLOCK_RULE_COUNT <= 1 )) || fail "duplicate managed destination rules found" 20
    (( DESTINATION_PRECEDING_BYPASS == 0 )) || fail "earlier destination policy rule would bypass fail-closed boundary" 20
  fi
  (( LOOKUP_RULE_COLLISION == 0 && LOOKUP_RULE_WRONG_PRIORITY == 0 )) || fail "lookup rule priority is occupied" 20
  (( BLOCK_RULE_COLLISION == 0 && BLOCK_RULE_WRONG_PRIORITY == 0 )) || fail "fail-closed rule priority is occupied" 20
  (( LOOKUP_RULE_COUNT <= 1 && BLOCK_RULE_COUNT <= 1 )) || fail "duplicate managed rules found" 20
  (( PRECEDING_BYPASS == 0 )) || fail "earlier policy rule would bypass fail-closed boundary" 20
}

ensure_destination_fail_closed_rule() {
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe
  if ! rule_exists_destination_block; then
    run_ip_mutation -4 rule add priority "$DESTINATION_FAIL_CLOSED_PRIORITY" to "$DOCKER_SUBNET" unreachable \
      || fail "cannot install destination fail-closed rule" 20
  fi
}

ensure_destination_lookup_rule() {
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe
  if ! rule_exists_destination_lookup; then
    # Suppress the main table's default route.  A missing Docker bridge route
    # must reach the immediately following unreachable rule instead of
    # escaping over the host's ordinary physical default.
    run_ip_mutation -4 rule add priority "$DESTINATION_RULE_PRIORITY" to "$DOCKER_SUBNET" lookup main suppress_prefixlength 0 \
      || fail "cannot install destination lookup rule" 20
  fi
}

ensure_fail_closed_rule() {
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe
  if ! rule_exists_block; then
    run_ip_mutation -4 rule add priority "$FAIL_CLOSED_PRIORITY" from "$DOCKER_SUBNET" unreachable \
      || fail "cannot install fail-closed rule" 20
  fi
}

ensure_lookup_rule() {
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe
  if ! rule_exists_lookup; then
    run_ip_mutation -4 rule add priority "$RULE_PRIORITY" from "$DOCKER_SUBNET" table "$ROUTE_TABLE" \
      || fail "cannot install lookup rule" 20
  fi
}

remove_destination_lookup_rules() {
  while :; do
    read_rule_snapshot || return 1
    scan_rules
    (( DESTINATION_LOOKUP_RULE_COUNT > 0 )) || break
    run_ip_mutation -4 rule del priority "$DESTINATION_RULE_PRIORITY" to "$DOCKER_SUBNET" lookup main suppress_prefixlength 0 || return 1
  done
}

remove_destination_block_rules() {
  while :; do
    read_rule_snapshot || return 1
    scan_rules
    (( DESTINATION_BLOCK_RULE_COUNT > 0 )) || break
    run_ip_mutation -4 rule del priority "$DESTINATION_FAIL_CLOSED_PRIORITY" to "$DOCKER_SUBNET" unreachable || return 1
  done
}

remove_lookup_rules() {
  while :; do
    read_rule_snapshot || return 1
    scan_rules
    (( LOOKUP_RULE_COUNT > 0 )) || break
    run_ip_mutation -4 rule del priority "$RULE_PRIORITY" from "$DOCKER_SUBNET" table "$ROUTE_TABLE" || return 1
  done
}

remove_block_rules() {
  while :; do
    read_rule_snapshot || return 1
    scan_rules
    (( BLOCK_RULE_COUNT > 0 )) || break
    run_ip_mutation -4 rule del priority "$FAIL_CLOSED_PRIORITY" from "$DOCKER_SUBNET" unreachable || return 1
  done
}

route_delete_from_line() {
  local line=$1
  local old_ifs=$IFS
  local -a fields
  IFS=' '
  read -r -a fields <<< "$line"
  IFS=$old_ifs
  local dest=${fields[0]:-}
  local proto metric iface gateway
  proto=$(route_field "$line" proto 2>/dev/null) || proto=""
  metric=$(route_field "$line" metric 2>/dev/null) || metric=""
  iface=$(route_field "$line" dev 2>/dev/null) || iface=""
  gateway=$(route_field "$line" via 2>/dev/null) || gateway=""
  route_proto_is_owned "$proto" && [[ "$metric" == "$ROUTE_METRIC" ]] || return 0
  [[ "$dest" == default ]] || valid_ipv4_cidr "$dest" || return 0
  valid_physical_iface_name "$iface" || return 0
  [[ -z "$gateway" ]] || valid_ipv4 "$gateway" || return 0

  local -a args=(-4 route del "$dest")
  [[ -z "$gateway" ]] || args+=(via "$gateway")
  args+=(dev "$iface" table "$ROUTE_TABLE" proto "$proto" metric "$ROUTE_METRIC")
  run_ip_mutation "${args[@]}" || true
}

remove_marked_routes() {
  local output line
  read_owned_route_snapshot || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    route_delete_from_line "$line"
  done <<< "$ROUTE_TABLE_SNAPSHOT"
  return 0
}

apply_discovered_routes() {
  # The fail-closed rule must already exist.  Route replacement is restricted
  # to the helper-owned table and carries both cleanup markers.
  run_ip_mutation -4 route replace "$DISCOVERED_LINK_PREFIX" dev "$DISCOVERED_IFACE" scope link \
    table "$ROUTE_TABLE" proto "$ROUTE_PROTO" metric "$ROUTE_METRIC" \
    || fail "cannot install physical link route" 20
  run_ip_mutation -4 route replace default via "$DISCOVERED_GATEWAY" dev "$DISCOVERED_IFACE" \
    table "$ROUTE_TABLE" proto "$ROUTE_PROTO" metric "$ROUTE_METRIC" \
    || fail "cannot install physical default route" 20
}

runtime_refresh_loaded() {
  validate_config
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe

  # Establish blockers before touching routes.  A missing Docker bridge route
  # or stale physical uplink therefore cannot fall through to a work-VPN rule
  # or main/local VPN fallback.  VERSION=1 intentionally establishes only its
  # original source-underlay blocker.
  if config_uses_destination_policy; then
    ensure_destination_fail_closed_rule
  fi
  ensure_fail_closed_rule

  if ! discover_uplink; then
    remove_marked_routes || fail "cannot clear stale managed routes" 20
    printf '%s\n' "runtime refresh: physical uplink unavailable; fail-closed policy retained" >&2
    return 21
  fi

  # Record the desired route before mutation so a partial refresh remains
  # removable by uninstall.  Existing marked routes are then removed exactly,
  # never by flushing the table.
  write_state || fail "cannot write routing state" 20
  remove_marked_routes || fail "cannot read managed route table" 20
  apply_discovered_routes
  if config_uses_destination_policy; then
    ensure_destination_lookup_rule
  fi
  ensure_lookup_rule
  return 0
}

runtime_refresh() {
  [[ $(id -u) -eq 0 ]] || fail "runtime refresh requires root" 3
  local acquired=0
  if (( LOCK_HELD == 0 )); then acquire_mutation_lock; acquired=1; fi
  load_config_for_runtime
  local status=0
  if runtime_refresh_loaded; then
    status=0
  else
    status=$?
  fi
  if (( acquired == 1 )); then release_mutation_lock; fi
  return "$status"
}

runtime_clean() {
  [[ $(id -u) -eq 0 ]] || fail "runtime cleanup requires root" 3
  local acquired=0
  if (( LOCK_HELD == 0 )); then acquire_mutation_lock; acquired=1; fi
  load_config_for_runtime
  validate_config
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe

  # If a lookup rule exists, add its blocker before removing it.  VERSION=2
  # owns the destination pair; VERSION=1 must not adopt or delete rules that
  # were introduced by a newer helper.
  if config_uses_destination_policy; then
    if rule_exists_destination_lookup && ! rule_exists_destination_block; then
      ensure_destination_fail_closed_rule
    fi
  fi
  if rule_exists_lookup && ! rule_exists_block; then
    ensure_fail_closed_rule
  fi
  if config_uses_destination_policy; then
    remove_destination_lookup_rules || fail "cannot remove destination lookup rule" 20
  fi
  remove_lookup_rules || fail "cannot remove lookup rule" 20
  remove_marked_routes || fail "cannot remove managed routes" 20
  if config_uses_destination_policy; then
    remove_destination_block_rules || fail "cannot remove destination fail-closed rule" 20
  fi
  remove_block_rules || fail "cannot remove fail-closed rule" 20
  if [[ -e "$STATE_PATH" || -L "$STATE_PATH" ]]; then
    remove_managed_file "$STATE_PATH" || fail "cannot remove routing state" 20
  fi
  if (( acquired == 1 )); then release_mutation_lock; fi
  return 0
}

managed_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  grep -Fqx -- "$MARKER" < "$path" 2>/dev/null
}

write_managed_text() {
  local path=$1
  local mode=$2
  local content=$3
  local dir tmp
  dir=${path%/*}
  mkdir -p "$dir" || return 1
  if [[ -e "$path" || -L "$path" ]]; then
    managed_file "$path" || return 2
  fi
  tmp=$(mktemp "${path}.tmp.XXXXXX") || return 1
  printf '%s\n' "$content" > "$tmp" || { rm -f "$tmp"; return 1; }
  chmod "$mode" "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$path" || { rm -f "$tmp"; return 1; }
}

service_template() {
  cat <<EOF
$MARKER
[Unit]
Description=Local vpnkit underlay policy routing (issue #40)
After=NetworkManager.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$HELPER_PATH --runtime-refresh
RemainAfterExit=yes
NoNewPrivileges=no
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
CapabilityBoundingSet=CAP_NET_ADMIN CAP_DAC_READ_SEARCH
AmbientCapabilities=CAP_NET_ADMIN
ReadWritePaths=$STATE_DIR ${LOCK_PATH%/*}

[Install]
WantedBy=multi-user.target
EOF
}

# This is the byte-for-byte VERSION=1 unit emitted by the original helper.
# Keep it separate from the current template: adding a current-only writable
# path or any other directive must not make an arbitrary marked unit a valid
# legacy migration/uninstall target.
legacy_service_template() {
  cat <<EOF
$MARKER
[Unit]
Description=Local vpnkit underlay policy routing (issue #40)
After=NetworkManager.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$HELPER_PATH --runtime-refresh
RemainAfterExit=yes
NoNewPrivileges=no
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
CapabilityBoundingSet=CAP_NET_ADMIN CAP_DAC_READ_SEARCH
AmbientCapabilities=CAP_NET_ADMIN
ReadWritePaths=$STATE_DIR

[Install]
WantedBy=multi-user.target
EOF
}

dispatcher_template() {
  cat <<EOF
#!/usr/bin/env bash
$MARKER
set -Eeuo pipefail

# NetworkManager supplies the interface and event state.  The helper performs
# its own physical-uplink discovery and never edits NetworkManager profiles.
case "\${2:-}" in
  up|dhcp4-change|dhcp6-change|connectivity-change|reapply|down)
    exec "$HELPER_PATH" --runtime-refresh
    ;;
  *)
    exit 0
    ;;
esac
EOF
}

# Uninstall is deliberately stricter than the read-only status path.  A marker
# is an ownership signal, not a complete identity proof: an unrelated file can
# copy it.  These checks are read-only and run before the lock is opened, so a
# rejected or mixed installation cannot stop a service, touch policy rules, or
# remove a byte.  The lock is the one exception to the marker contract: it is
# an empty coordination inode at this exact path and is never removed by this
# helper.
path_has_symlink_component() {
  local path=$1 parent
  while [[ "$path" != "/" && "$path" != "." && -n "$path" ]]; do
    [[ -L "$path" ]] && return 0
    parent=${path%/*}
    [[ "$parent" == "$path" ]] && break
    [[ -n "$parent" ]] || parent=/
    path=$parent
  done
  return 1
}

path_kind() {
  local path=$1
  if path_has_symlink_component "$path"; then
    printf '%s\n' symlink
  elif [[ ! -e "$path" ]]; then
    printf '%s\n' absent
  elif [[ -f "$path" && ! -L "$path" ]]; then
    printf '%s\n' regular
  elif [[ -d "$path" && ! -L "$path" ]]; then
    printf '%s\n' directory
  else
    printf '%s\n' other
  fi
}

# Validate the directory chain before accepting an absent target.  Otherwise a
# missing file below a foreign regular file or user-owned directory would only
# fail after the transaction had opened its lock and started mkdir/write work.
path_parent_tree_is_safe() {
  local path=$1 parent current
  parent=${path%/*}
  [[ "$parent" == "$path" ]] && parent=.
  current=$parent
  while [[ -n "$current" && "$current" != "/" && "$current" != "." ]]; do
    [[ ! -L "$current" ]] || return 1
    if [[ -e "$current" ]]; then
      [[ -d "$current" && ! -L "$current" ]] || return 1
      path_owner_is_canonical "$current" || return 1
    fi
    parent=${current%/*}
    [[ "$parent" == "$current" ]] && break
    [[ -n "$parent" ]] || parent=/
    current=$parent
  done
  [[ "$current" != "/" ]] || [[ "$(path_owner_uid /)" == 0 ]]
}

path_owner_uid() {
  stat -c '%u' -- "$1" 2>/dev/null
}

path_owner_is_canonical() {
  local path=$1 owner
  owner=$(path_owner_uid "$path") || return 1
  [[ "$owner" == 0 ]] && return 0
  [[ -n "$ROOT_PREFIX" && "$ROOT_NAMESPACE_OWNER" != 0 ]] || return 1
  # The test namespace may itself live below an unprivileged temporary
  # directory.  Permit that same namespace owner on the prefix and its
  # existing ancestors, while the unprefixed host path remains UID 0 only.
  [[ "$path" == "$ROOT_PREFIX" || "$path" == "$ROOT_PREFIX/"* || \
     "$ROOT_PREFIX" == "$path"/* ]] || return 1
  [[ "$owner" == "$ROOT_NAMESPACE_OWNER" ]]
}

path_mode() {
  stat -c '%a' -- "$1" 2>/dev/null
}

path_link_count() {
  stat -c '%h' -- "$1" 2>/dev/null
}

canonical_regular_file() {
  local path=$1 expected_mode=$2 expected_owner=${3:-0}
  [[ "$(path_kind "$path")" == regular ]] || return 1
  managed_file "$path" || return 1
  [[ "$(path_mode "$path")" == "$expected_mode" ]] || return 1
  [[ "$(path_link_count "$path")" == 1 ]] || return 1
  path_owner_is_canonical "$path" || return 1
}

legacy_helper_identity_is_valid() {
  local path=$1 digest
  digest=$(sha256sum -- "$path" 2>/dev/null) || return 1
  digest=${digest%% *}
  # VERSION=1 has a fixed historical helper identity.  A marker plus
  # recognizable snippets is not a sufficient migration credential.
  [[ "$digest" == "$LEGACY_HELPER_DIGEST" ]] || return 1
}

canonical_helper_file() {
  local owner=${1:-0} actual_digest expected_digest
  canonical_regular_file "$HELPER_PATH" 755 0 || return 1
  [[ "$(sed -n '2p' -- "$HELPER_PATH" 2>/dev/null)" == "$MARKER" ]] || return 1
  if config_uses_destination_policy; then
    # The persisted digest is not an ownership token by itself.  It must agree
    # with both the exact trusted source being installed and the target bytes.
    cmp -s "$HELPER_PATH" "$SCRIPT_SOURCE" || return 1
    actual_digest=$(sha256sum -- "$HELPER_PATH" 2>/dev/null) || return 1
    actual_digest=${actual_digest%% *}
    expected_digest=$(sha256sum -- "$SCRIPT_SOURCE" 2>/dev/null) || return 1
    expected_digest=${expected_digest%% *}
    [[ "$CONFIG_HAS_HELPER_DIGEST" == 1 && "$HELPER_DIGEST" == "$expected_digest" && "$actual_digest" == "$expected_digest" ]] || return 1
  else
    # VERSION=1 has no digest field, so bind it to the immutable legacy bytes
    # rather than accepting a marker plus recognizable snippets.
    legacy_helper_identity_is_valid "$HELPER_PATH" || return 1
  fi
}

canonical_service_file() {
  local owner=${1:-0}
  canonical_regular_file "$SERVICE_PATH" 644 0 || return 1
  [[ "$(head -n 1 -- "$SERVICE_PATH" 2>/dev/null)" == "$MARKER" ]] || return 1
  if config_uses_destination_policy; then
    cmp -s "$SERVICE_PATH" <(service_template) || return 1
  else
    # Process substitution gives cmp a read-only descriptor for the expected
    # bytes; no temporary path is created while validating an untrusted unit.
    cmp -s "$SERVICE_PATH" <(legacy_service_template) || return 1
  fi
}

canonical_dispatcher_file() {
  local owner=${1:-0}
  canonical_regular_file "$DISPATCHER_PATH" 755 0 || return 1
  # VERSION=1 and VERSION=2 intentionally share the same dispatcher bytes;
  # only the helper/config policy version changes across the migration.
  cmp -s "$DISPATCHER_PATH" <(dispatcher_template) || return 1
}

canonical_config_file() {
  canonical_regular_file "$CONFIG_PATH" 644 0 || return 1
  [[ "$(head -n 1 -- "$CONFIG_PATH" 2>/dev/null)" == "$MARKER" ]] || return 1
  load_kv_file "$CONFIG_PATH" || return 1
  ( validate_config ) >/dev/null 2>&1 || return 1
  cmp -s "$CONFIG_PATH" <(render_config) || return 1
}

canonical_state_file() {
  local owner=${1:-0}
  canonical_regular_file "$STATE_PATH" 600 0 || return 1
  load_state || return 1
  cmp -s "$STATE_PATH" <(render_state "$ROUTE_TABLE" "$STATE_LINK_PREFIX" "$STATE_LINK_IFACE" "$STATE_GATEWAY") || return 1
}

canonical_lock_file() {
  local kind
  kind=$(path_kind "$LOCK_PATH")
  [[ "$kind" == absent ]] && return 0
  [[ "$kind" == regular ]] || return 1
  [[ "$(path_mode "$LOCK_PATH")" == 600 ]] || return 1
  [[ "$(path_link_count "$LOCK_PATH")" == 1 ]] || return 1
  path_owner_is_canonical "$LOCK_PATH" || return 1
  [[ "$(stat -c '%s' -- "$LOCK_PATH" 2>/dev/null)" == 0 ]] || return 1
}

canonical_state_directory() {
  local owner=${1:-0} kind entry base state_kind
  kind=$(path_kind "$STATE_DIR")
  [[ "$kind" == absent ]] && return 0
  [[ "$kind" == directory ]] || return 1
  [[ "$(path_mode "$STATE_DIR")" == 700 ]] || return 1
  path_owner_is_canonical "$STATE_DIR" || return 1

  # A failed transaction must not leave an unreviewed backup or foreign file
  # for uninstall to remove.  The only permitted child is the owned state.
  for entry in "$STATE_DIR"/* "$STATE_DIR"/.[!.]* "$STATE_DIR"/..?*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    base=${entry##*/}
    [[ "$base" == routes.state ]] || return 1
  done
  state_kind=$(path_kind "$STATE_PATH")
  case "$state_kind" in
    absent) return 0 ;;
    regular) canonical_state_file "$owner" ;;
    *) return 1 ;;
  esac
}

empty_state_directory_is_canonical() {
  local kind entry
  kind=$(path_kind "$STATE_DIR")
  [[ "$kind" == directory ]] || return 1
  [[ "$(path_mode "$STATE_DIR")" == 700 ]] || return 1
  local state_parent=${STATE_DIR%/*}
  [[ "$state_parent" == "$STATE_DIR" ]] && state_parent=.
  [[ "$(path_kind "$state_parent")" == directory ]] || return 1
  path_owner_is_canonical "$STATE_DIR" || return 1
  path_owner_is_canonical "$state_parent" || return 1
  for entry in "$STATE_DIR"/* "$STATE_DIR"/.[!.]* "$STATE_DIR"/..?*; do
    if [[ -e "$entry" || -L "$entry" ]]; then
      return 1
    fi
  done
  return 0
}

uninstall_preflight() {
  local config_kind helper_kind service_kind dispatcher_kind state_kind
  local core_count=0

  # Inspect every fixed target path, including optional paths, before any
  # command that can create the lock or alter host state.  Parent symlinks and
  # foreign parent directories are rejected too.
  for target in \
    "$CONFIG_PATH" "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" \
    "$STATE_DIR" "$STATE_PATH" "$LOCK_PATH"; do
    path_parent_tree_is_safe "$target" || return 1
  done
  config_kind=$(path_kind "$CONFIG_PATH")
  helper_kind=$(path_kind "$HELPER_PATH")
  service_kind=$(path_kind "$SERVICE_PATH")
  dispatcher_kind=$(path_kind "$DISPATCHER_PATH")
  state_kind=$(path_kind "$STATE_PATH")
  [[ "$config_kind" != symlink && "$helper_kind" != symlink && \
     "$service_kind" != symlink && "$dispatcher_kind" != symlink && \
     "$state_kind" != symlink ]] || return 1
  canonical_lock_file || return 1

  [[ "$config_kind" != absent ]] && core_count=$((core_count + 1))
  [[ "$helper_kind" != absent ]] && core_count=$((core_count + 1))
  [[ "$service_kind" != absent ]] && core_count=$((core_count + 1))
  [[ "$dispatcher_kind" != absent ]] && core_count=$((core_count + 1))

  if (( core_count == 0 )); then
    # A state file without its config cannot identify the policy table/rules it
    # belongs to.  Refuse the orphan rather than deleting bytes or guessing.
    [[ "$state_kind" == absent ]] || return 1
    case "$(path_kind "$STATE_DIR")" in
      absent)
        UNINSTALL_PREFLIGHT_ACTION=noop
        ;;
      directory)
        # An interrupted file-only transaction may leave an empty, canonical
        # state directory.  It is safe to remove that directory after the same
        # all-path preflight; a directory containing any child is not safe.
        empty_state_directory_is_canonical || return 1
        UNINSTALL_PREFLIGHT_ACTION=empty-state-directory
        ;;
      *)
        return 1
        ;;
    esac
    return 0
  fi

  # The four hook/config files are one ownership unit.  A missing one or an
  # extra foreign/incorrectly typed one is a partial mixed state, never a
  # reason to run only part of cleanup.
  (( core_count == 4 )) || return 1
  [[ "$config_kind" == regular && "$helper_kind" == regular && \
     "$service_kind" == regular && "$dispatcher_kind" == regular ]] || return 1

  canonical_config_file || return 1
  canonical_helper_file 0 || return 1
  canonical_service_file 0 || return 1
  canonical_dispatcher_file 0 || return 1
  canonical_state_directory 0 || return 1
  canonical_lock_file || return 1

  UNINSTALL_PREFLIGHT_ACTION=cleanup
  return 0
}

# Installation is a migration boundary.  The four core files are one
# ownership unit; a state file/directory and empty lock are optional only when
# they have the exact canonical identity below.  Run this whole read-only
# check in a subshell so loading a persisted config cannot alter the candidate
# process context used after the lock is acquired.
install_preflight() {
  (
    local config_kind helper_kind service_kind dispatcher_kind state_kind
    local state_dir_kind core_absent=0 core_complete=0

    # Check every target's existing parent chain before accepting an absent
    # final path.  This rejects symlink ancestors, foreign parent directories,
    # and non-directory blockers before mkdir or lock creation.
    for target in \
      "$CONFIG_PATH" "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" \
      "$STATE_DIR" "$STATE_PATH" "$LOCK_PATH"; do
      path_parent_tree_is_safe "$target" || return 1
    done

    config_kind=$(path_kind "$CONFIG_PATH")
    helper_kind=$(path_kind "$HELPER_PATH")
    service_kind=$(path_kind "$SERVICE_PATH")
    dispatcher_kind=$(path_kind "$DISPATCHER_PATH")
    state_kind=$(path_kind "$STATE_PATH")

    # A clean install is truly empty at all core targets.  A stale empty
    # canonical state directory and/or empty canonical lock are the only
    # optional remnants permitted in this state.
    if [[ "$config_kind" == absent && "$helper_kind" == absent && \
          "$service_kind" == absent && "$dispatcher_kind" == absent ]]; then
      core_absent=1
    elif [[ "$config_kind" == regular && "$helper_kind" == regular && \
            "$service_kind" == regular && "$dispatcher_kind" == regular ]]; then
      core_complete=1
    else
      # Includes marker-spoofed files, symlinks, directories, and every
      # partial/mixed core set.  Never fall through to the later ownership
      # loop, backup, systemd, routing, or state-directory creation.
      return 1
    fi

    canonical_lock_file || return 1
    if (( core_absent == 1 )); then
      [[ "$state_kind" == absent ]] || return 1
      canonical_state_directory 0 || return 1
      return 0
    fi

    (( core_complete == 1 )) || return 1
    canonical_config_file || return 1
    canonical_helper_file 0 || return 1
    canonical_service_file 0 || return 1
    canonical_dispatcher_file 0 || return 1
    canonical_state_directory 0 || return 1
  )
}

install_files() {
  write_managed_text "$HELPER_PATH" 0755 "$(cat "$SCRIPT_SOURCE")" || return 1
  write_managed_text "$SERVICE_PATH" 0644 "$(service_template)" || return 1
  write_managed_text "$DISPATCHER_PATH" 0755 "$(dispatcher_template)" || return 1
  write_config || return 1
}

snapshot_install_state() {
  local candidate_config_version=$CONFIG_VERSION
  local candidate_docker_subnet=$DOCKER_SUBNET
  local candidate_route_table=$ROUTE_TABLE
  local candidate_destination_rule_priority=$DESTINATION_RULE_PRIORITY
  local candidate_destination_fail_closed_priority=$DESTINATION_FAIL_CLOSED_PRIORITY
  local candidate_rule_priority=$RULE_PRIORITY
  local candidate_fail_closed_priority=$FAIL_CLOSED_PRIORITY
  local candidate_iface=$UPLINK_IFACE
  local candidate_uplink_table=$UPLINK_TABLE
  local candidate_gateway=$UPLINK_GATEWAY
  local candidate_has_destination_rule=$CONFIG_HAS_DESTINATION_RULE_PRIORITY
  local candidate_has_destination_block=$CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY
  local candidate_helper_digest=$HELPER_DIGEST
  local candidate_has_helper_digest=$CONFIG_HAS_HELPER_DIGEST
  local line

  INSTALL_PRIOR_MANAGED_ROUTES=()

  if (( INSTALL_HAD_PRIOR_CONFIG == 1 )); then
    load_kv_file "$CONFIG_PATH" || return 1
    INSTALL_PRIOR_CONFIG_VERSION=$CONFIG_VERSION
    INSTALL_PRIOR_DOCKER_SUBNET=$DOCKER_SUBNET
    INSTALL_PRIOR_ROUTE_TABLE=$ROUTE_TABLE
    INSTALL_PRIOR_DESTINATION_RULE_PRIORITY=$DESTINATION_RULE_PRIORITY
    INSTALL_PRIOR_DESTINATION_FAIL_CLOSED_PRIORITY=$DESTINATION_FAIL_CLOSED_PRIORITY
    INSTALL_PRIOR_RULE_PRIORITY=$RULE_PRIORITY
    INSTALL_PRIOR_FAIL_CLOSED_PRIORITY=$FAIL_CLOSED_PRIORITY
    INSTALL_PRIOR_UPLINK_IFACE=$UPLINK_IFACE
    INSTALL_PRIOR_UPLINK_TABLE=$UPLINK_TABLE
    INSTALL_PRIOR_UPLINK_GATEWAY=$UPLINK_GATEWAY
    INSTALL_PRIOR_CONFIG_HAS_DESTINATION_RULE_PRIORITY=$CONFIG_HAS_DESTINATION_RULE_PRIORITY
    INSTALL_PRIOR_CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=$CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY
    INSTALL_PRIOR_CONFIG_HAS_HELPER_DIGEST=$CONFIG_HAS_HELPER_DIGEST
    INSTALL_PRIOR_HELPER_DIGEST=$HELPER_DIGEST
  else
    INSTALL_PRIOR_CONFIG_VERSION=$candidate_config_version
    INSTALL_PRIOR_DOCKER_SUBNET=$candidate_docker_subnet
    INSTALL_PRIOR_ROUTE_TABLE=$candidate_route_table
    INSTALL_PRIOR_DESTINATION_RULE_PRIORITY=$candidate_destination_rule_priority
    INSTALL_PRIOR_DESTINATION_FAIL_CLOSED_PRIORITY=$candidate_destination_fail_closed_priority
    INSTALL_PRIOR_RULE_PRIORITY=$candidate_rule_priority
    INSTALL_PRIOR_FAIL_CLOSED_PRIORITY=$candidate_fail_closed_priority
    INSTALL_PRIOR_UPLINK_IFACE=$candidate_iface
    INSTALL_PRIOR_UPLINK_TABLE=$candidate_uplink_table
    INSTALL_PRIOR_UPLINK_GATEWAY=$candidate_gateway
    INSTALL_PRIOR_CONFIG_HAS_DESTINATION_RULE_PRIORITY=$candidate_has_destination_rule
    INSTALL_PRIOR_CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=$candidate_has_destination_block
    INSTALL_PRIOR_CONFIG_HAS_HELPER_DIGEST=$candidate_has_helper_digest
    INSTALL_PRIOR_HELPER_DIGEST=$candidate_helper_digest
  fi

  read_rule_snapshot || return 1
  scan_rules
  INSTALL_PRIOR_DESTINATION_LOOKUP_PRESENT=$DESTINATION_LOOKUP_RULE_COUNT
  INSTALL_PRIOR_DESTINATION_BLOCK_PRESENT=$DESTINATION_BLOCK_RULE_COUNT
  INSTALL_PRIOR_LOOKUP_PRESENT=$LOOKUP_RULE_COUNT
  INSTALL_PRIOR_BLOCK_PRESENT=$BLOCK_RULE_COUNT

  read_owned_route_snapshot || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -n "$line" ]] || continue
    rollback_route_line_is_owned "$line" && INSTALL_PRIOR_MANAGED_ROUTES+=("$line")
  done <<< "$ROUTE_TABLE_SNAPSHOT"

  INSTALL_PRIOR_SERVICE_ENABLED=0
  INSTALL_PRIOR_SERVICE_ACTIVE=0
  INSTALL_PRIOR_SERVICE_FILE_PRESENT=0
  [[ -e "$SERVICE_PATH" || -L "$SERVICE_PATH" ]] && INSTALL_PRIOR_SERVICE_FILE_PRESENT=1
  systemctl is-enabled --quiet "$SERVICE_NAME" >/dev/null 2>&1 && INSTALL_PRIOR_SERVICE_ENABLED=1 || true
  systemctl is-active --quiet "$SERVICE_NAME" >/dev/null 2>&1 && INSTALL_PRIOR_SERVICE_ACTIVE=1 || true

  # Restore the requested candidate context.  The persisted file, not a
  # process override, is the source of the exact rollback snapshot.
  CONFIG_VERSION=$candidate_config_version
  DOCKER_SUBNET=$candidate_docker_subnet
  ROUTE_TABLE=$candidate_route_table
  DESTINATION_RULE_PRIORITY=$candidate_destination_rule_priority
  DESTINATION_FAIL_CLOSED_PRIORITY=$candidate_destination_fail_closed_priority
  RULE_PRIORITY=$candidate_rule_priority
  FAIL_CLOSED_PRIORITY=$candidate_fail_closed_priority
  UPLINK_IFACE=$candidate_iface
  UPLINK_TABLE=$candidate_uplink_table
  UPLINK_GATEWAY=$candidate_gateway
  CONFIG_HAS_DESTINATION_RULE_PRIORITY=$candidate_has_destination_rule
  CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=$candidate_has_destination_block
  CONFIG_HAS_HELPER_DIGEST=$candidate_has_helper_digest
  HELPER_DIGEST=$candidate_helper_digest
}

remove_managed_file() {
  local path=$1
  [[ -e "$path" || -L "$path" ]] || return 0
  managed_file "$path" || return 1
  rm -f -- "$path"
}

install_action() {
  [[ $(id -u) -eq 0 ]] || fail "install requires direct root" 3
  (( YES == 1 )) || fail "install requires --yes" 3
  validate_config
  command -v ip >/dev/null 2>&1 || fail "ip is unavailable" 10
  command -v systemctl >/dev/null 2>&1 || fail "systemctl is unavailable" 10
  # A VERSION=1 service is accepted only when its complete legacy bytes match
  # the known template.  This read-only check must precede the lock and all
  # migration/runtime/file mutations.
  install_preflight || fail "managed installation preflight failed" 20

  # Every new install, including a VERSION=1 -> VERSION=2 migration, writes
  # and applies the current destination-aware policy.  The old version remains
  # in the transaction snapshot and is used for exact rollback below.
  CONFIG_VERSION=$CURRENT_CONFIG_VERSION
  [[ -n "$DESTINATION_RULE_PRIORITY" ]] || DESTINATION_RULE_PRIORITY=$DEFAULT_DESTINATION_RULE_PRIORITY
  [[ -n "$DESTINATION_FAIL_CLOSED_PRIORITY" ]] || DESTINATION_FAIL_CLOSED_PRIORITY=$DEFAULT_DESTINATION_FAIL_CLOSED_PRIORITY
  CONFIG_HAS_DESTINATION_RULE_PRIORITY=1
  CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY=1
  validate_config

  acquire_mutation_lock
  # Re-read the same complete ownership contract after taking the lock and
  # before the first route probe, mkdir, backup, systemd call, or file write.
  # The initial pass is what guarantees invalid installs do not create the
  # lock; this second pass closes the normal preflight-to-transaction window.
  if ! install_preflight; then
    release_mutation_lock
    fail "managed installation preflight changed" 20
  fi
  discover_uplink || fail "no usable physical uplink was discovered" 20
  [[ "$DISCOVERED_TABLE" != "$ROUTE_TABLE" ]] || fail "uplink table collides with owned table" 20

  INSTALL_BACKUP_DIR=""
  INSTALL_BACKED_UP=()
  INSTALL_NEWLY_CREATED=()
  INSTALL_HAD_PRIOR_CONFIG=0
  INSTALL_FILES_STARTED=0
  INSTALL_RUNTIME_STARTED=0
  if managed_file "$CONFIG_PATH"; then INSTALL_HAD_PRIOR_CONFIG=1; fi

  # Do not replace unknown files.  Existing managed files are overwritten only
  # after all paths have passed the ownership check.  Include routes.state in
  # the snapshot: a failed candidate must not leave a new runtime state file.
  local install_failed=0
  local path backup
  mkdir -p -- "$STATE_DIR" || install_failed=1
  chmod 0700 "$STATE_DIR" >/dev/null 2>&1 || install_failed=1
  for path in "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" "$CONFIG_PATH" "$STATE_PATH"; do
    if [[ -e "$path" || -L "$path" ]]; then
      managed_file "$path" || { install_failed=1; break; }
    fi
  done
  if (( install_failed != 0 )); then
    fail "managed installation ownership check failed" 20
  fi

  transaction_begin
  if ! INSTALL_BACKUP_DIR=$(mktemp -d "${STATE_DIR}/install-backup.XXXXXX"); then
    install_failed=1
  fi
  if (( install_failed == 0 )); then
    for path in "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" "$CONFIG_PATH" "$STATE_PATH"; do
      if [[ -e "$path" ]]; then
        backup="$INSTALL_BACKUP_DIR/${#INSTALL_BACKED_UP[@]}"
        if cp -p -- "$path" "$backup"; then
          INSTALL_BACKED_UP+=("$path|$backup")
        else
          install_failed=1
          break
        fi
      else
        INSTALL_NEWLY_CREATED+=("$path")
      fi
    done
  fi
  if (( install_failed == 0 )); then
    snapshot_install_state || install_failed=1
  fi
  if (( install_failed == 0 )); then
    INSTALL_FILES_STARTED=1
    install_files || install_failed=1
  fi
  if (( install_failed == 0 )); then
    systemctl daemon-reload >/dev/null 2>&1 || install_failed=1
  fi
  if (( install_failed == 0 )); then
    # Use the local config just written; this is still within the explicit
    # install confirmation and has the blocker-first ordering above.
    INSTALL_RUNTIME_STARTED=1
    if ! runtime_refresh; then install_failed=1; fi
  fi
  if (( install_failed == 0 )); then
    install_test_failpoint after-runtime-refresh || install_failed=1
  fi
  if (( install_failed == 0 )); then
    systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
    systemctl enable --now "$SERVICE_NAME" >/dev/null 2>&1 || install_failed=1
  fi
  if (( install_failed == 0 )); then
    install_test_failpoint after-service-enable || install_failed=1
  fi

  if (( install_failed != 0 )); then
    # The EXIT/signal handlers are already armed.  Call compensation directly
    # so ordinary candidate failures and injected signals share one path.
    transaction_rollback 20 || true
    local final_status=20
    if (( TRANSACTION_PENDING_SIGNAL > 0 )); then
      final_status=$((128 + TRANSACTION_PENDING_SIGNAL))
    elif (( TRANSACTION_ROLLBACK_FAILED == 1 )); then
      final_status=21
    fi
    exit "$final_status"
  fi

  rm -rf -- "$INSTALL_BACKUP_DIR" || true
  INSTALL_BACKUP_DIR=""
  TRANSACTION_ACTIVE=0
  trap - EXIT INT TERM HUP
  release_mutation_lock
  printf '%s\n' "install complete; routing hooks are installed (values redacted)"
}

uninstall_action() {
  [[ $(id -u) -eq 0 ]] || fail "uninstall requires direct root" 3
  (( YES == 1 )) || fail "uninstall requires --yes" 3

  # This is intentionally before acquire_mutation_lock.  Opening an absent
  # lock creates a file, and even that would violate the zero-mutation promise
  # for a foreign, malformed, symlinked, or partial installation.
  if ! uninstall_preflight; then
    fail "managed installation preflight failed" 20
  fi
  if [[ "$UNINSTALL_PREFLIGHT_ACTION" == noop ]]; then
    printf '%s\n' "uninstall complete; no managed installation found"
    return 0
  fi

  if [[ "$UNINSTALL_PREFLIGHT_ACTION" == cleanup ]]; then
    command -v ip >/dev/null 2>&1 || fail "ip is unavailable" 10
    command -v systemctl >/dev/null 2>&1 || fail "systemctl is unavailable" 10
  fi
  acquire_mutation_lock

  # Close the read/preflight-to-lock interval.  A changed path is rejected
  # while the only mutation so far is the helper's own coordination lock.
  if ! uninstall_preflight; then
    release_mutation_lock
    fail "managed installation preflight changed" 20
  fi
  if [[ "$UNINSTALL_PREFLIGHT_ACTION" == empty-state-directory ]]; then
    rmdir -- "$STATE_DIR" || {
      release_mutation_lock
      fail "cannot remove empty routing state directory" 20
    }
    release_mutation_lock
    printf '%s\n' "uninstall complete; no managed installation found"
    return 0
  fi
  [[ "$UNINSTALL_PREFLIGHT_ACTION" == cleanup ]] || {
    release_mutation_lock
    fail "managed installation preflight changed" 20
  }

  # Stop the hook before removing its routes.  The unit has no broad ExecStop;
  # the explicit cleanup below owns the exact ordering and deletion scope.
  if ! systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1; then
    if systemctl is-active --quiet "$SERVICE_NAME" >/dev/null 2>&1; then
      fail "could not stop routing hook" 20
    fi
  fi
  runtime_clean
  # Re-check the file set after runtime cleanup and immediately before the
  # first unlink.  This prevents a path swap during the systemd/ip interval
  # from turning the final file removal into foreign cleanup.
  if ! uninstall_preflight || [[ "$UNINSTALL_PREFLIGHT_ACTION" != cleanup ]]; then
    release_mutation_lock
    fail "managed installation preflight changed" 20
  fi
  remove_managed_file "$DISPATCHER_PATH" || fail "dispatcher ownership check failed" 20
  remove_managed_file "$SERVICE_PATH" || fail "service ownership check failed" 20
  remove_managed_file "$HELPER_PATH" || fail "helper ownership check failed" 20
  remove_managed_file "$CONFIG_PATH" || fail "config ownership check failed" 20
  rmdir "$STATE_DIR" >/dev/null 2>&1 || true
  systemctl daemon-reload >/dev/null 2>&1 || fail "systemd reload failed" 20
  release_mutation_lock
  printf '%s\n' "uninstall complete; only tool-owned state was removed"
}

print_check() {
  printf '%s\n' "$1"
}

plan_action() {
  validate_config
  local uplink_state=unavailable
  if discover_uplink; then uplink_state=ready; fi
  printf '%s\n' "command=plan"
  printf '%s\n' "mutation=none"
  printf '%s\n' "dedicated_source_subnet=redacted"
  printf '%s\n' "owned_policy_table=redacted"
  printf '%s\n' "physical_uplink_table=${uplink_state}"
  if config_uses_destination_policy; then
    printf '%s\n' "config_version=2"
    printf '%s\n' "policy_rule_order=destination-lookup-before-destination-unreachable-before-source-lookup-before-source-unreachable"
    printf '%s\n' "fail_closed_boundary=destination-main-route-or-unreachable-before-work-vpn"
  else
    printf '%s\n' "config_version=1"
    printf '%s\n' "policy_rule_order=source-lookup-before-source-unreachable"
    printf '%s\n' "fail_closed_boundary=before-main-and-local-vpn-fallback"
  fi
  printf '%s\n' "route_cleanup=marked-entries-only"
  printf '%s\n' "systemd_template=bounded-public-safe"
  printf '%s\n' "networkmanager_dispatcher=route-refresh-only"
  if [[ "$uplink_state" == ready ]]; then
    printf '%s\n' "install_preflight=ready"
  else
    printf '%s\n' "install_preflight=needs-physical-uplink"
  fi
}

status_action() {
  load_installed_config_if_allowed
  validate_config
  local config_state=no service_state=no dispatcher_state=no helper_state=no
  managed_file "$CONFIG_PATH" && config_state=yes || true
  managed_file "$SERVICE_PATH" && service_state=yes || true
  managed_file "$DISPATCHER_PATH" && dispatcher_state=yes || true
  managed_file "$HELPER_PATH" && helper_state=yes || true

  local destination_rules_state=not-applicable destination_block_state=not-applicable
  local rules_state=unavailable block_state=unavailable route_state=unavailable uplink_state=unavailable
  if read_rule_snapshot; then
    scan_rules
    if config_uses_destination_policy; then
      (( DESTINATION_LOOKUP_RULE_COUNT == 1 && DESTINATION_LOOKUP_RULE_COLLISION == 0 && DESTINATION_LOOKUP_RULE_WRONG_PRIORITY == 0 && DESTINATION_PRECEDING_BYPASS == 0 )) \
        && destination_rules_state=yes || destination_rules_state=no
      (( DESTINATION_BLOCK_RULE_COUNT == 1 && DESTINATION_BLOCK_RULE_COLLISION == 0 && DESTINATION_BLOCK_RULE_WRONG_PRIORITY == 0 && DESTINATION_PRECEDING_BYPASS == 0 )) \
        && destination_block_state=yes || destination_block_state=no
    fi
    (( LOOKUP_RULE_COUNT == 1 && LOOKUP_RULE_COLLISION == 0 && LOOKUP_RULE_WRONG_PRIORITY == 0 && PRECEDING_BYPASS == 0 )) \
      && rules_state=yes || rules_state=no
    (( BLOCK_RULE_COUNT == 1 && BLOCK_RULE_COLLISION == 0 && BLOCK_RULE_WRONG_PRIORITY == 0 && PRECEDING_BYPASS == 0 )) \
      && block_state=yes || block_state=no
  fi
  if read_owned_route_snapshot; then
    if (( MANAGED_DEFAULT_COUNT >= 1 && MANAGED_LINK_COUNT >= 1 )); then route_state=yes; else route_state=no; fi
  fi
  discover_uplink && uplink_state=yes || true

  printf '%s\n' "command=status"
  printf '%s\n' "mutation=none"
  print_check config="$config_state"
  print_check helper="$helper_state"
  print_check systemd="$service_state"
  print_check dispatcher="$dispatcher_state"
  print_check destination_lookup_rule="$destination_rules_state"
  print_check destination_fail_closed_rule="$destination_block_state"
  print_check lookup_rule="$rules_state"
  print_check fail_closed_rule="$block_state"
  print_check managed_routes="$route_state"
  print_check physical_uplink="$uplink_state"
  printf '%s\n' "diagnostics=redacted"
}

canonical_priorities_are_current() {
  # This is a deliberately narrow evidence marker for the live local KDE
  # acceptance path. Generic helper users may choose any valid adjacent policy
  # slots; only the installed v2 config and the current kernel rules at the
  # repository's managed 998/999/1000/1001 slots can produce this marker.
  [[ "$CONFIG_VERSION" == "$CONFIG_VERSION_V2" ]] || return 1
  [[ "$CONFIG_HAS_DESTINATION_RULE_PRIORITY" == 1 && "$CONFIG_HAS_DESTINATION_FAIL_CLOSED_PRIORITY" == 1 ]] || return 1
  [[ "$DESTINATION_RULE_PRIORITY" == "$DEFAULT_DESTINATION_RULE_PRIORITY" ]] || return 1
  [[ "$DESTINATION_FAIL_CLOSED_PRIORITY" == "$DEFAULT_DESTINATION_FAIL_CLOSED_PRIORITY" ]] || return 1
  [[ "$RULE_PRIORITY" == "$DEFAULT_RULE_PRIORITY" && "$FAIL_CLOSED_PRIORITY" == "$DEFAULT_FAIL_CLOSED_PRIORITY" ]] || return 1
  managed_file "$CONFIG_PATH" || return 1
  managed_file "$HELPER_PATH" || return 1
  managed_file "$SERVICE_PATH" || return 1
  managed_file "$DISPATCHER_PATH" || return 1
  read_rule_snapshot || return 1
  scan_rules
  (( DESTINATION_LOOKUP_RULE_COUNT == 1 && DESTINATION_BLOCK_RULE_COUNT == 1 && \
     LOOKUP_RULE_COUNT == 1 && BLOCK_RULE_COUNT == 1 && \
     DESTINATION_LOOKUP_RULE_COLLISION == 0 && DESTINATION_BLOCK_RULE_COLLISION == 0 && \
     DESTINATION_LOOKUP_RULE_WRONG_PRIORITY == 0 && DESTINATION_BLOCK_RULE_WRONG_PRIORITY == 0 && \
     LOOKUP_RULE_COLLISION == 0 && BLOCK_RULE_COLLISION == 0 && \
     DESTINATION_PRECEDING_BYPASS == 0 && PRECEDING_BYPASS == 0 ))
}

verify_action() {
  load_installed_config_if_allowed
  validate_config
  local failed=0
  managed_file "$CONFIG_PATH" && print_check config=pass || { print_check config=fail; failed=1; }
  managed_file "$HELPER_PATH" && print_check helper=pass || { print_check helper=fail; failed=1; }
  managed_file "$SERVICE_PATH" && print_check systemd=pass || { print_check systemd=fail; failed=1; }
  managed_file "$DISPATCHER_PATH" && print_check dispatcher=pass || { print_check dispatcher=fail; failed=1; }
  if config_uses_destination_policy; then
    (( DESTINATION_RULE_PRIORITY < DESTINATION_FAIL_CLOSED_PRIORITY && DESTINATION_FAIL_CLOSED_PRIORITY < RULE_PRIORITY && RULE_PRIORITY < FAIL_CLOSED_PRIORITY )) \
      && print_check priority_order=pass \
      || { print_check priority_order=fail; failed=1; }
  else
    (( RULE_PRIORITY < FAIL_CLOSED_PRIORITY )) \
      && print_check priority_order=pass \
      || { print_check priority_order=fail; failed=1; }
  fi

  if read_rule_snapshot; then
    scan_rules
    if config_uses_destination_policy; then
      (( DESTINATION_LOOKUP_RULE_COUNT == 1 )) && print_check destination_lookup_rule=pass || { print_check destination_lookup_rule=fail; failed=1; }
      (( DESTINATION_BLOCK_RULE_COUNT == 1 )) && print_check destination_fail_closed_rule=pass || { print_check destination_fail_closed_rule=fail; failed=1; }
    else
      print_check destination_lookup_rule=not-applicable
      print_check destination_fail_closed_rule=not-applicable
    fi
    (( LOOKUP_RULE_COUNT == 1 )) && print_check lookup_rule=pass || { print_check lookup_rule=fail; failed=1; }
    (( BLOCK_RULE_COUNT == 1 )) && print_check fail_closed_rule=pass || { print_check fail_closed_rule=fail; failed=1; }
    if config_uses_destination_policy; then
      (( DESTINATION_LOOKUP_RULE_COLLISION == 0 && DESTINATION_BLOCK_RULE_COLLISION == 0 && \
         DESTINATION_LOOKUP_RULE_WRONG_PRIORITY == 0 && DESTINATION_BLOCK_RULE_WRONG_PRIORITY == 0 && \
         LOOKUP_RULE_COLLISION == 0 && BLOCK_RULE_COLLISION == 0 && \
         DESTINATION_PRECEDING_BYPASS == 0 && PRECEDING_BYPASS == 0 )) \
        && print_check rule_order=pass \
        || { print_check rule_order=fail; failed=1; }
    else
      (( LOOKUP_RULE_COLLISION == 0 && BLOCK_RULE_COLLISION == 0 && PRECEDING_BYPASS == 0 )) \
        && print_check rule_order=pass \
        || { print_check rule_order=fail; failed=1; }
    fi
  else
    print_check rule_read=fail
    failed=1
  fi

  if read_owned_route_snapshot; then
    (( MANAGED_DEFAULT_COUNT >= 1 && MANAGED_LINK_COUNT >= 1 )) \
      && print_check managed_routes=pass \
      || { print_check managed_routes=fail; failed=1; }
  else
    print_check route_read=fail
    failed=1
  fi

  if discover_uplink; then
    print_check physical_uplink=pass
  else
    print_check physical_uplink=fail
    failed=1
  fi
  if (( failed == 0 )) && canonical_priorities_are_current; then
    printf '%s\n' 'canonical_priorities=ok'
  fi
  printf '%s\n' "diagnostics=redacted"
  (( failed == 0 )) || return 20
}

main() {
  parse_args "$@"
  if [[ "$COMMAND" == runtime-refresh || "$COMMAND" == runtime-clean ]]; then
    load_config_for_runtime
  elif [[ "$COMMAND" == status || "$COMMAND" == verify || "$COMMAND" == uninstall || "$COMMAND" == install || "$COMMAND" == plan ]]; then
    load_installed_config_if_allowed
  fi

  case "$COMMAND" in
    plan) plan_action ;;
    status) status_action ;;
    verify) verify_action ;;
    install) install_action ;;
    uninstall) uninstall_action ;;
    runtime-refresh) runtime_refresh ;;
    runtime-clean) runtime_clean ;;
    *) fail "unsupported command" 2 ;;
  esac
}

SCRIPT_SOURCE=${BASH_SOURCE[0]}
main "$@"
