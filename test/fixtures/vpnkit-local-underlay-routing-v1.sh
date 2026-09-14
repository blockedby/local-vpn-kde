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

DOCKER_SUBNET=${VPNKIT_LOCAL_DOCKER_SUBNET:-$DEFAULT_DOCKER_SUBNET}
ROUTE_TABLE=${VPNKIT_LOCAL_ROUTE_TABLE:-$DEFAULT_ROUTE_TABLE}
RULE_PRIORITY=${VPNKIT_LOCAL_RULE_PRIORITY:-$DEFAULT_RULE_PRIORITY}
FAIL_CLOSED_PRIORITY=${VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY:-$DEFAULT_FAIL_CLOSED_PRIORITY}
UPLINK_IFACE=${VPNKIT_LOCAL_UPLINK_IFACE:-}
UPLINK_TABLE=${VPNKIT_LOCAL_UPLINK_TABLE:-}
UPLINK_GATEWAY=${VPNKIT_LOCAL_UPLINK_GATEWAY:-}

ENV_DOCKER_SET=0
ENV_ROUTE_TABLE_SET=0
ENV_RULE_PRIORITY_SET=0
ENV_FAIL_CLOSED_PRIORITY_SET=0
ENV_UPLINK_IFACE_SET=0
ENV_UPLINK_TABLE_SET=0
ENV_UPLINK_GATEWAY_SET=0
[[ ${VPNKIT_LOCAL_DOCKER_SUBNET+x} ]] && ENV_DOCKER_SET=1
[[ ${VPNKIT_LOCAL_ROUTE_TABLE+x} ]] && ENV_ROUTE_TABLE_SET=1
[[ ${VPNKIT_LOCAL_RULE_PRIORITY+x} ]] && ENV_RULE_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY+x} ]] && ENV_FAIL_CLOSED_PRIORITY_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_IFACE+x} ]] && ENV_UPLINK_IFACE_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_TABLE+x} ]] && ENV_UPLINK_TABLE_SET=1
[[ ${VPNKIT_LOCAL_UPLINK_GATEWAY+x} ]] && ENV_UPLINK_GATEWAY_SET=1

CLI_DOCKER_SET=0
CLI_ROUTE_TABLE_SET=0
CLI_RULE_PRIORITY_SET=0
CLI_FAIL_CLOSED_PRIORITY_SET=0
CLI_UPLINK_IFACE_SET=0
CLI_UPLINK_TABLE_SET=0
CLI_UPLINK_GATEWAY_SET=0
YES=0
COMMAND="plan"

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
LOOKUP_RULE_COUNT=0
BLOCK_RULE_COUNT=0
LOOKUP_RULE_COLLISION=0
BLOCK_RULE_COLLISION=0
LOOKUP_RULE_WRONG_PRIORITY=0
BLOCK_RULE_WRONG_PRIORITY=0
PRECEDING_BYPASS=0

fail() {
  # Keep failures intentionally value-free.  In particular, do not include
  # route, gateway, interface, endpoint, or command output in diagnostics.
  printf '%s\n' "$1" >&2
  exit "${2:-1}"
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
  --docker-subnet CIDR       Dedicated source subnet (IPv4 CIDR).
  --route-table ID            Helper-owned policy table (numeric ID).
  --rule-priority N           Lookup rule priority.
  --fail-closed-priority N    Unreachable rule priority.
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
  [[ -r "$path" ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || return 1
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
      VERSION) [[ "$value" == 1 ]] || return 1 ;;
      DOCKER_SUBNET) DOCKER_SUBNET=$value ;;
      ROUTE_TABLE) ROUTE_TABLE=$value ;;
      RULE_PRIORITY) RULE_PRIORITY=$value ;;
      FAIL_CLOSED_PRIORITY) FAIL_CLOSED_PRIORITY=$value ;;
      UPLINK_IFACE) UPLINK_IFACE=$value ;;
      UPLINK_TABLE) UPLINK_TABLE=$value ;;
      UPLINK_GATEWAY) UPLINK_GATEWAY=$value ;;
      *) return 1 ;;
    esac
  done < "$path"
}

load_installed_config_if_allowed() {
  [[ -r "$CONFIG_PATH" ]] || return 0
  # Read-only commands ignore an unrelated config; install/uninstall perform
  # their own ownership checks before any mutation.
  managed_file "$CONFIG_PATH" || return 0
  local saved_docker=$DOCKER_SUBNET
  local saved_route_table=$ROUTE_TABLE
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

validate_config() {
  valid_ipv4_cidr "$DOCKER_SUBNET" || fail "invalid dedicated subnet" 2
  valid_table_id "$ROUTE_TABLE" || fail "invalid route table" 2
  valid_uplink_table "$UPLINK_TABLE" || fail "invalid uplink table" 2
  [[ "$RULE_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid rule priority" 2
  [[ "$FAIL_CLOSED_PRIORITY" =~ ^[0-9]{1,5}$ ]] || fail "invalid fail-closed priority" 2
  (( RULE_PRIORITY >= 1 && RULE_PRIORITY < FAIL_CLOSED_PRIORITY && FAIL_CLOSED_PRIORITY < 32766 )) || fail "invalid rule ordering" 2
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
    if [[ "$priority" == "$RULE_PRIORITY" ]]; then
      LOOKUP_RULE_COLLISION=1
    fi
    if [[ "$priority" == "$FAIL_CLOSED_PRIORITY" ]]; then
      BLOCK_RULE_COLLISION=1
    fi

    # Rules before the owned lookup rule that can match this source would
    # defeat the fail-closed boundary.  Local priority zero is expected.
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

write_config() {
  # The file contains only validated local routing choices, not credentials;
  # read access keeps status/verify usable without recurring root commands.
  write_plain_kv "$CONFIG_PATH" 0644 \
    VERSION 1 \
    DOCKER_SUBNET "$DOCKER_SUBNET" \
    ROUTE_TABLE "$ROUTE_TABLE" \
    RULE_PRIORITY "$RULE_PRIORITY" \
    FAIL_CLOSED_PRIORITY "$FAIL_CLOSED_PRIORITY" \
    UPLINK_IFACE "$UPLINK_IFACE" \
    UPLINK_TABLE "$UPLINK_TABLE" \
    UPLINK_GATEWAY "$UPLINK_GATEWAY"
}

write_state() {
  chmod 0700 "$STATE_DIR" >/dev/null 2>&1 || return 1
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
  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || return 1
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
      VERSION) [[ "$value" == 1 ]] || return 1 ;;
      ROUTE_TABLE) state_route=$value ;;
      LINK_PREFIX) state_prefix=$value ;;
      LINK_IFACE) state_iface=$value ;;
      GATEWAY) state_gateway=$value ;;
      *) return 1 ;;
    esac
  done < "$STATE_PATH"
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

rule_exists_lookup() {
  (( LOOKUP_RULE_COUNT > 0 ))
}

rule_exists_block() {
  (( BLOCK_RULE_COUNT > 0 ))
}

ensure_rule_slots_safe() {
  (( LOOKUP_RULE_COLLISION == 0 && LOOKUP_RULE_WRONG_PRIORITY == 0 )) || fail "lookup rule priority is occupied" 20
  (( BLOCK_RULE_COLLISION == 0 && BLOCK_RULE_WRONG_PRIORITY == 0 )) || fail "fail-closed rule priority is occupied" 20
  (( LOOKUP_RULE_COUNT <= 1 && BLOCK_RULE_COUNT <= 1 )) || fail "duplicate managed rules found" 20
  (( PRECEDING_BYPASS == 0 )) || fail "earlier policy rule would bypass fail-closed boundary" 20
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

runtime_refresh() {
  [[ $(id -u) -eq 0 ]] || fail "runtime refresh requires root" 3
  load_config_for_runtime
  validate_config
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe

  # Establish the blocker before touching routes.  A missing or stale uplink
  # therefore cannot fall through to main/local VPN rules.
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
  ensure_lookup_rule
  return 0
}

runtime_clean() {
  [[ $(id -u) -eq 0 ]] || fail "runtime cleanup requires root" 3
  load_config_for_runtime
  validate_config
  read_rule_snapshot || fail "cannot read policy rules" 20
  scan_rules
  ensure_rule_slots_safe

  # If a lookup rule exists, add the blocker before removing it.  This keeps
  # cleanup fail-closed even if an interrupted prior install left one rule.
  if rule_exists_lookup && ! rule_exists_block; then
    ensure_fail_closed_rule
  fi
  remove_lookup_rules || fail "cannot remove lookup rule" 20
  remove_marked_routes || fail "cannot remove managed routes" 20
  remove_block_rules || fail "cannot remove fail-closed rule" 20
  rm -f "$STATE_PATH"
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

install_files() {
  write_managed_text "$HELPER_PATH" 0755 "$(cat "$SCRIPT_SOURCE")" || return 1
  write_managed_text "$SERVICE_PATH" 0644 "$(service_template)" || return 1
  write_managed_text "$DISPATCHER_PATH" 0755 "$(dispatcher_template)" || return 1
  write_config || return 1
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
  discover_uplink || fail "no usable physical uplink was discovered" 20
  [[ "$DISCOVERED_TABLE" != "$ROUTE_TABLE" ]] || fail "uplink table collides with owned table" 20

  # Do not replace unknown files.  Existing managed files are overwritten only
  # after all paths have passed the ownership check.  The trap below restores
  # the previous managed set if any bounded step fails.
  local backup_dir="${STATE_DIR}/install-backup.$$"
  local -a backed_up=()
  local -a newly_created=()
  local path backup
  local install_failed=0
  local rollback_failed=0
  local had_prior_config=0
  local runtime_started=0
  local service_start_attempted=0
  if managed_file "$CONFIG_PATH"; then had_prior_config=1; fi
  trap 'install_failed=1' ERR

  mkdir -p "$STATE_DIR" || install_failed=1
  chmod 0700 "$STATE_DIR" >/dev/null 2>&1 || install_failed=1
  for path in "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" "$CONFIG_PATH"; do
    if [[ -e "$path" || -L "$path" ]]; then
      managed_file "$path" || { install_failed=1; break; }
    fi
  done
  if (( install_failed == 0 )); then
    mkdir -p "$backup_dir" || install_failed=1
  fi
  if (( install_failed == 0 )); then
    for path in "$HELPER_PATH" "$SERVICE_PATH" "$DISPATCHER_PATH" "$CONFIG_PATH"; do
      if [[ -e "$path" ]]; then
        backup="$backup_dir/${#backed_up[@]}"
        cp -p -- "$path" "$backup" || { install_failed=1; break; }
        backed_up+=("$path|$backup")
      else
        newly_created+=("$path")
      fi
    done
  fi
  if (( install_failed == 0 )); then
    install_files || install_failed=1
  fi
  if (( install_failed == 0 )); then
    systemctl daemon-reload >/dev/null 2>&1 || install_failed=1
  fi
  if (( install_failed == 0 )); then
    # Use the local config just written; this is still within the explicit
    # install confirmation and has the blocker-first ordering above.
    runtime_started=1
    if ! ( runtime_refresh ); then install_failed=1; fi
  fi
  if (( install_failed == 0 )); then
    service_start_attempted=1
    systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
    systemctl enable --now "$SERVICE_NAME" >/dev/null 2>&1 || install_failed=1
  fi

  if (( install_failed != 0 )); then
    # Keep rollback quiet and value-free.  If routing was partially applied,
    # remove only marked routes/rules before restoring the managed files.
    if (( service_start_attempted )); then
      systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || rollback_failed=1
    fi
    if (( runtime_started )) && managed_file "$CONFIG_PATH"; then
      ( runtime_clean ) >/dev/null 2>&1 || rollback_failed=1
    fi
    for path in "${newly_created[@]}"; do
      rm -f -- "$path" || rollback_failed=1
    done
    local entry original
    for entry in "${backed_up[@]}"; do
      original=${entry%%|*}
      backup=${entry#*|}
      cp -p -- "$backup" "$original" || rollback_failed=1
    done
    # Re-apply the previous managed config after removing the candidate.  This
    # restores the old exact rules/routes when an upgrade was interrupted.
    if (( runtime_started && had_prior_config )) && managed_file "$CONFIG_PATH"; then
      ( runtime_refresh ) >/dev/null 2>&1 || rollback_failed=1
    fi
    rm -rf -- "$backup_dir" || rollback_failed=1
    systemctl daemon-reload >/dev/null 2>&1 || rollback_failed=1
    trap - ERR
    if (( rollback_failed )); then
      fail "install failed; rollback incomplete" 21
    fi
    fail "install failed and was rolled back" 20
  fi

  rm -rf -- "$backup_dir" || true
  trap - ERR
  printf '%s\n' "install complete; routing hooks are installed (values redacted)"
}

uninstall_action() {
  [[ $(id -u) -eq 0 ]] || fail "uninstall requires direct root" 3
  (( YES == 1 )) || fail "uninstall requires --yes" 3
  if ! managed_file "$CONFIG_PATH"; then
    if managed_file "$HELPER_PATH" || managed_file "$SERVICE_PATH" || managed_file "$DISPATCHER_PATH"; then
      fail "managed config ownership check failed" 20
    fi
    printf '%s\n' "uninstall complete; no managed installation found"
    return 0
  fi
  load_installed_config_if_allowed
  validate_config
  command -v ip >/dev/null 2>&1 || fail "ip is unavailable" 10
  command -v systemctl >/dev/null 2>&1 || fail "systemctl is unavailable" 10

  # Stop the hook before removing its routes.  The unit has no broad ExecStop;
  # the explicit cleanup below owns the exact ordering and deletion scope.
  if ! systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1; then
    if systemctl is-active --quiet "$SERVICE_NAME" >/dev/null 2>&1; then
      fail "could not stop routing hook" 20
    fi
  fi
  runtime_clean
  remove_managed_file "$DISPATCHER_PATH" || fail "dispatcher ownership check failed" 20
  remove_managed_file "$SERVICE_PATH" || fail "service ownership check failed" 20
  remove_managed_file "$HELPER_PATH" || fail "helper ownership check failed" 20
  remove_managed_file "$CONFIG_PATH" || fail "config ownership check failed" 20
  rmdir "$STATE_DIR" >/dev/null 2>&1 || true
  systemctl daemon-reload >/dev/null 2>&1 || fail "systemd reload failed" 20
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
  printf '%s\n' "policy_rule_order=lookup-before-unreachable"
  printf '%s\n' "fail_closed_boundary=before-main-and-local-vpn-fallback"
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

  local rules_state=unavailable block_state=unavailable route_state=unavailable uplink_state=unavailable
  if read_rule_snapshot; then
    scan_rules
    (( LOOKUP_RULE_COUNT == 1 )) && rules_state=yes || rules_state=no
    (( BLOCK_RULE_COUNT == 1 )) && block_state=yes || block_state=no
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
  print_check lookup_rule="$rules_state"
  print_check fail_closed_rule="$block_state"
  print_check managed_routes="$route_state"
  print_check physical_uplink="$uplink_state"
  printf '%s\n' "diagnostics=redacted"
}

verify_action() {
  load_installed_config_if_allowed
  validate_config
  local failed=0
  managed_file "$CONFIG_PATH" && print_check config=pass || { print_check config=fail; failed=1; }
  managed_file "$HELPER_PATH" && print_check helper=pass || { print_check helper=fail; failed=1; }
  managed_file "$SERVICE_PATH" && print_check systemd=pass || { print_check systemd=fail; failed=1; }
  managed_file "$DISPATCHER_PATH" && print_check dispatcher=pass || { print_check dispatcher=fail; failed=1; }
  (( RULE_PRIORITY < FAIL_CLOSED_PRIORITY )) && print_check priority_order=pass || { print_check priority_order=fail; failed=1; }

  if read_rule_snapshot; then
    scan_rules
    (( LOOKUP_RULE_COUNT == 1 )) && print_check lookup_rule=pass || { print_check lookup_rule=fail; failed=1; }
    (( BLOCK_RULE_COUNT == 1 )) && print_check fail_closed_rule=pass || { print_check fail_closed_rule=fail; failed=1; }
    (( LOOKUP_RULE_COLLISION == 0 && BLOCK_RULE_COLLISION == 0 && PRECEDING_BYPASS == 0 )) \
      && print_check rule_order=pass \
      || { print_check rule_order=fail; failed=1; }
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
