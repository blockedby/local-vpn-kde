#!/usr/bin/env bash
set -euo pipefail

SINGBOX_SOURCE_CONFIG=${SINGBOX_SOURCE_CONFIG:-/etc/sing-box/config.json}
SINGBOX_CONFIG=${SINGBOX_CONFIG:-/var/lib/vpnkit/sing-box/config.json}
SINGBOX_RESTART_FILE=${SINGBOX_RESTART_FILE:-/run/vpnkit/restart-sing-box}
SINGBOX_GENERATION_FILE=${SINGBOX_GENERATION_FILE:-/run/vpnkit/sing-box-generation}
SINGBOX_ACK_FILE=${SINGBOX_ACK_FILE:-${VPNKIT_SINGBOX_ACK_FILE:-${SINGBOX_GENERATION_FILE}.ack}}
OPENVPN_CONFIG=${OPENVPN_CONFIG:-/etc/openvpn/server.conf}
VIBE_VPN_CONFIG=${VIBE_VPN_CONFIG:-/etc/vibe-vpn/config.yaml}
VPNKIT_BOOTSTRAP_PICK_ON_START=${VPNKIT_BOOTSTRAP_PICK_ON_START:-false}
VPNKIT_BOOTSTRAP_MAX_NODES=${VPNKIT_BOOTSTRAP_MAX_NODES:-50}

# setup-routing and the lifecycle adapter use these exact paths; export custom
# values so a restart and its generation marker share the same namespace.
export SINGBOX_RESTART_FILE SINGBOX_GENERATION_FILE SINGBOX_ACK_FILE

if [[ ! -r "$SINGBOX_SOURCE_CONFIG" ]]; then
  echo "missing sing-box source config: $SINGBOX_SOURCE_CONFIG" >&2
  exit 1
fi
if [[ ! -r "$OPENVPN_CONFIG" ]]; then
  echo "missing OpenVPN server config: $OPENVPN_CONFIG" >&2
  exit 1
fi

mkdir -p "$(dirname "$SINGBOX_CONFIG")" "$(dirname "$SINGBOX_RESTART_FILE")"
VIBE_VPN_DEFER_TRANSACTION_RECOVERY=1 vibe-vpn sync-sing-box-config --source "$SINGBOX_SOURCE_CONFIG" --runtime "$SINGBOX_CONFIG"

sing-box check -c "$SINGBOX_CONFIG"
SINGBOX_PID=""
OVPN_PID=""
BOOTSTRAP_PID=""
RECOVERY_PID=""
WATCH_PID=""

start_singbox() {
  sing-box run -c "$SINGBOX_CONFIG" &
  SINGBOX_PID=$!
  echo "started sing-box pid=$SINGBOX_PID config=$SINGBOX_CONFIG"
}

singbox_tcp_ready() {
  local port=$1
  ss -ltn sport = :"$port" | grep -q ":$port"
}

singbox_udp_ready() {
  local port=$1
  ss -lun sport = :"$port" | grep -q ":$port"
}

singbox_tun_ready() {
  local iface=${SINGBOX_TUN_IFACE:-sb-tun0}
  ip link show "$iface" >/dev/null 2>&1
}

wait_for_singbox_inbounds() {
  local mode=${VPNKIT_ROUTING_MODE:-redirect}
  mode=${mode,,}
  local deadline=$((SECONDS + ${SINGBOX_STARTUP_TIMEOUT_SECONDS:-30}))
  local ready_message timeout_message

  case "$mode" in
    tun)
      ready_message="sing-box tun inbound ready on ${SINGBOX_TUN_IFACE:-sb-tun0}"
      timeout_message="timed out waiting for sing-box tun interface ${SINGBOX_TUN_IFACE:-sb-tun0}"
      ;;
    tproxy)
      ready_message="sing-box tproxy inbound ready on tcp/2082"
      timeout_message="timed out waiting for sing-box tproxy inbound on tcp/2082"
      ;;
    redirect|*)
      ready_message="sing-box redirect inbounds ready on tcp/2082 and udp/5353"
      timeout_message="timed out waiting for sing-box redirect inbounds on tcp/2082 and udp/5353"
      ;;
  esac

  until case "$mode" in
    tun) singbox_tun_ready ;;
    tproxy) singbox_tcp_ready 2082 ;;
    redirect|*) singbox_tcp_ready 2082 && singbox_udp_ready 5353 ;;
  esac; do
    if ! kill -0 "$SINGBOX_PID" 2>/dev/null; then
      echo "sing-box exited before inbounds became ready" >&2
      wait "$SINGBOX_PID"
    fi
    if (( SECONDS >= deadline )); then
      echo "$timeout_message" >&2
      return 1
    fi
    sleep 0.2
  done
  echo "$ready_message"
}

write_restart_health_ack() {
  local token=$1 generation=$2 dir tmp
  dir=${SINGBOX_ACK_FILE%/*}
  [[ "$dir" != "$SINGBOX_ACK_FILE" ]] || dir=.
  mkdir -p "$dir"
  tmp=$(mktemp "${SINGBOX_ACK_FILE}.tmp.XXXXXX")
  chmod 600 "$tmp"
  printf 'token=%s\ngeneration=%s\nhealth=healthy\n' "$token" "$generation" >"$tmp"
  # The Go waiter accepts this file only after the request disappears and the
  # generation file matches. Sync the file and containing directory before
  # publishing it so an acknowledged restart cannot lose its proof at power
  # loss.
  sync -f "$tmp" 2>/dev/null || true
  mv -f -- "$tmp" "$SINGBOX_ACK_FILE"
  sync -f "$dir" 2>/dev/null || true
}

runtime_health_predicate() {
  # This is intentionally the same complete predicate used by the container
  # healthcheck: live sing-box, valid config, required inbounds/TUN, OpenVPN,
  # routing policy, and the fail-closed barrier removed only after setup.
  [[ "${SINGBOX_PID:-}" =~ ^[1-9][0-9]*$ ]] && kill -0 "$SINGBOX_PID" 2>/dev/null || return 1
  /usr/local/bin/vpnkit-healthcheck.sh >/dev/null 2>&1
}

restart_singbox() {
  local request_token=$1 generation_before generation_after
  generation_before=$(cat "$SINGBOX_GENERATION_FILE" 2>/dev/null || true)
  echo "restart requested for sing-box"
  # Re-establish the exact source-CIDR barrier before interrupting the current
  # proxy. setup-routing removes it only after the replacement is ready.
  /usr/local/bin/setup-routing.sh --install-fail-closed-barrier
  sing-box check -c "$SINGBOX_CONFIG"
  if [[ -n "${SINGBOX_PID:-}" ]] && kill -0 "$SINGBOX_PID" 2>/dev/null; then
    kill "$SINGBOX_PID" 2>/dev/null || true
    wait "$SINGBOX_PID" 2>/dev/null || true
  fi
  start_singbox
  wait_for_singbox_inbounds
  /usr/local/bin/setup-routing.sh
  runtime_health_predicate || {
    echo "sing-box restart health predicate failed; keeping fail-closed routing" >&2
    return 1
  }
  generation_after=$(cat "$SINGBOX_GENERATION_FILE" 2>/dev/null || true)
  [[ -n "$generation_after" && "$generation_after" != "$generation_before" ]] || {
    echo "sing-box restart generation did not advance" >&2
    return 1
  }
  write_restart_health_ack "$request_token" "$generation_after"
}

process_restart_request() {
  local current
  [[ -f "$SINGBOX_RESTART_FILE" ]] || return 0
  # The request body is an opaque per-invocation token. It is never logged or
  # copied into general diagnostics; only the matching ack file carries it.
  current=$(tr -d '\r\n' <"$SINGBOX_RESTART_FILE" 2>/dev/null || true)
  [[ -n "$current" && "$current" != "$last_restart_seen" ]] || return 0
  last_restart_seen=$current
  if ! restart_singbox "$current"; then
    echo "sing-box restart request failed; container is stopping fail-closed" >&2
    return 1
  fi
  # Do not consume a newer request that arrived while this restart ran. The
  # Go waiter deliberately waits for this exact token to disappear.
  if [[ "$(tr -d '\r\n' <"$SINGBOX_RESTART_FILE" 2>/dev/null || true)" == "$current" ]]; then
    rm -f -- "$SINGBOX_RESTART_FILE" || return 1
  fi
}

poll_recovery_process() {
  [[ -n "${RECOVERY_PID:-}" ]] || return 0
  if kill -0 "$RECOVERY_PID" 2>/dev/null; then
    return 0
  fi
  if ! wait "$RECOVERY_PID" 2>/dev/null; then
    echo "runtime/state transaction recovery failed; stopping fail-closed" >&2
    return 1
  fi
  RECOVERY_PID=""
}

stop_pid() {
  local pid=${1:-}
  [[ "$pid" =~ ^[0-9]+$ ]] || return 0
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  # Stop producers before removing the barrier. If setup-routing failed, the
  # DROP rules remain in force until all OpenVPN processes are gone.
  stop_pid "${WATCH_PID:-}"
  stop_pid "${BOOTSTRAP_PID:-}"
  stop_pid "${RECOVERY_PID:-}"
  stop_pid "${OVPN_PID:-}"
  stop_pid "${SINGBOX_PID:-}"
  /usr/local/bin/setup-routing.sh --remove-fail-closed-barrier >/dev/null 2>&1 || true
}
trap cleanup INT TERM EXIT

start_singbox
wait_for_singbox_inbounds

# This is deliberately before the OpenVPN process is created. OpenVPN cannot
# accept client data without the exact temporary source-CIDR barrier in place.
/usr/local/bin/setup-routing.sh --install-fail-closed-barrier

openvpn --config "$OPENVPN_CONFIG" &
OVPN_PID=$!


until ip link show tun0 >/dev/null 2>&1; do
  if ! kill -0 "$OVPN_PID" 2>/dev/null; then
    echo "openvpn exited before tun0 appeared" >&2
    wait "$OVPN_PID"
  fi
  sleep 0.5
done

/usr/local/bin/setup-routing.sh
runtime_health_predicate || {
  echo "initial vpnkit runtime health predicate failed" >&2
  exit 1
}

last_restart_seen=""
# If a previous Go process died after runtime acknowledgement, recover its
# journal only after this process has a healthy supervisor/routing predicate.
# The child may wait for a request ack; the parent loop below remains free to
# consume the request, so this cannot deadlock on the state lock.
if [[ -r "$VIBE_VPN_CONFIG" ]]; then
  vibe-vpn --config "$VIBE_VPN_CONFIG" recover-transactions >/dev/null 2>&1 &
  RECOVERY_PID=$!
fi
case "${VPNKIT_BOOTSTRAP_PICK_ON_START,,}" in
  1|true|yes|on)
    if [[ ! -r "$VIBE_VPN_CONFIG" ]]; then
      echo "missing vibe-vpn config for bootstrap selection: $VIBE_VPN_CONFIG" >&2
      exit 1
    fi
    install -d -m 0700 /var/log/vibe-vpn
    bootstrap_log=/var/log/vibe-vpn/bootstrap-selection.log
    : >"$bootstrap_log"
    chmod 600 "$bootstrap_log"
    [[ "$VPNKIT_BOOTSTRAP_MAX_NODES" =~ ^[0-9]+$ ]] && (( VPNKIT_BOOTSTRAP_MAX_NODES >= 1 && VPNKIT_BOOTSTRAP_MAX_NODES <= 1000 )) \
      || { echo "VPNKIT_BOOTSTRAP_MAX_NODES must be in 1..1000" >&2; exit 2; }
    rm -f -- "$SINGBOX_RESTART_FILE"
    # Bootstrap runs only after the OpenVPN/TUN/routing predicate is healthy.
    # Keep the same request supervisor active while Go waits for the token,
    # generation, and health acknowledgement; no async state commit exists.
    vibe-vpn --config "$VIBE_VPN_CONFIG" pick --max "$VPNKIT_BOOTSTRAP_MAX_NODES" >"$bootstrap_log" 2>&1 &
    bootstrap_pid=$!
    BOOTSTRAP_PID=$bootstrap_pid
    while kill -0 "$bootstrap_pid" 2>/dev/null; do
      poll_recovery_process || exit 1
      process_restart_request || exit 1
      kill -0 "$SINGBOX_PID" 2>/dev/null || { echo "sing-box exited during bootstrap" >&2; exit 1; }
      kill -0 "$OVPN_PID" 2>/dev/null || { echo "openvpn exited during bootstrap" >&2; exit 1; }
      sleep 0.1
    done
    if ! wait "$bootstrap_pid"; then
      echo "bootstrap upstream selection failed; OpenVPN was not left in service" >&2
      exit 1
    fi
    BOOTSTRAP_PID=""
    [[ ! -e "$SINGBOX_RESTART_FILE" ]] || { echo "bootstrap left a restart request pending" >&2; exit 1; }
    echo "bootstrap upstream selection completed (details kept in private local log)"
    ;;
  0|false|no|off) ;;
  *)
    echo "unsupported VPNKIT_BOOTSTRAP_PICK_ON_START value" >&2
    exit 2
    ;;
esac


while true; do
  poll_recovery_process || exit 1
  process_restart_request || exit 1
  if ! kill -0 "$SINGBOX_PID" 2>/dev/null; then
    echo "sing-box exited" >&2
    wait "$SINGBOX_PID"
    exit 1
  fi
  if ! kill -0 "$OVPN_PID" 2>/dev/null; then
    echo "openvpn exited" >&2
    wait "$OVPN_PID"
    exit 1
  fi
  sleep 1
done
