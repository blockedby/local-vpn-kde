#!/usr/bin/env bash
# Local-only sing-box DNS failover watchdog.
#
# Server selection and this watchdog both mutate the persisted sing-box runtime.
# They therefore share the state-dir lock before reading or replacing the
# runtime config.  Do not change the default lock path
# (/var/lib/vibe-vpn/.vibe-vpn.lock): it is the same path used by
# internal/state.AcquireLock.
set -Eeuo pipefail
umask 077

STATE_DIR=${VPNKIT_DNS_FAILOVER_STATE_DIR:-/var/lib/vibe-vpn}
RUNTIME=${VPNKIT_DNS_FAILOVER_RUNTIME:-/var/lib/vpnkit/sing-box/config.json}
RESTART_FILE=${VPNKIT_DNS_FAILOVER_RESTART_FILE:-/run/vpnkit/restart-sing-box}
LOCK_FILE=${VPNKIT_DNS_FAILOVER_LOCK_FILE:-$STATE_DIR/.vibe-vpn.lock}
SING_BOX_BIN=${VPNKIT_DNS_FAILOVER_SING_BOX_BIN:-/usr/local/bin/sing-box}
INTERVAL_SECONDS=${VPNKIT_DNS_FAILOVER_INTERVAL_SECONDS:-15}
LOCK_WAIT_SECONDS=${VPNKIT_DNS_FAILOVER_LOCK_WAIT_SECONDS:-2}
SCRIPT_PATH=$(realpath -m -- "$0")

MODE=watch
TARGET=
MAIN_PID=
ONCE=0

usage() {
  cat <<'EOF'
Usage: dns-failover-watchdog.sh --pid PID
       dns-failover-watchdog.sh --once [--target remote-dns|remote-dns-fallback]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pid) MAIN_PID=${2:?missing PID}; shift 2 ;;
    --once) ONCE=1; shift ;;
    --target) TARGET=${2:?missing DNS target}; shift 2 ;;
    --switch-locked)
      MODE=switch-locked
      TARGET=${2:?missing DNS target}
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unsupported argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$TARGET" in
  ""|remote-dns|remote-dns-fallback) ;;
  *) echo "unsupported DNS target: $TARGET" >&2; exit 2 ;;
esac
[[ "$INTERVAL_SECONDS" =~ ^[0-9]+$ ]] && (( INTERVAL_SECONDS >= 1 && INTERVAL_SECONDS <= 3600 )) || {
  echo 'VPNKIT_DNS_FAILOVER_INTERVAL_SECONDS must be in 1..3600' >&2
  exit 2
}
command -v flock >/dev/null 2>&1 || { echo 'flock is unavailable; DNS failover watchdog cannot run' >&2; exit 1; }

runtime_has_dns_failover_servers() {
  [[ -s "$RUNTIME" ]] || return 1
  grep -Eq '"tag"[[:space:]]*:[[:space:]]*"remote-dns"' "$RUNTIME" || return 1
  grep -Eq '"tag"[[:space:]]*:[[:space:]]*"remote-dns-fallback"' "$RUNTIME" || return 1
}

probe() {
  local host=$1 address=$2
  curl --silent --show-error --fail --connect-timeout 4 --max-time 8 \
    --socks5 127.0.0.1:2080 \
    --resolve "$host:443:$address" \
    -H 'accept: application/dns-json' \
    "https://$host/dns-query?name=example.com&type=A" >/dev/null 2>&1
}

write_restart_token() {
  local target=$1 restart_dir restart_base token token_tmp
  restart_dir=$(dirname -- "$RESTART_FILE")
  restart_base=$(basename -- "$RESTART_FILE")
  mkdir -p -- "$restart_dir"
  token="dns-failover-${target}-$(date -u +%s%N)"
  [[ -n "$token" ]] || return 1
  token_tmp=$(mktemp "$restart_dir/.${restart_base}.dns-failover.XXXXXX")
  if ! printf '%s\n' "$token" >"$token_tmp" || ! [[ -s "$token_tmp" ]]; then
    rm -f -- "$token_tmp"
    return 1
  fi
  chmod 0600 "$token_tmp"
  if ! mv -f -- "$token_tmp" "$RESTART_FILE"; then
    rm -f -- "$token_tmp"
    return 1
  fi
}

apply_target_locked() {
  local target=$1 from_target runtime_dir runtime_base candidate old_copy
  case "$target" in
    remote-dns) from_target=remote-dns-fallback ;;
    remote-dns-fallback) from_target=remote-dns ;;
    *) return 2 ;;
  esac

  runtime_has_dns_failover_servers || return 0
  grep -Eq "\"final\"[[:space:]]*:[[:space:]]*\"$from_target\"" "$RUNTIME" || return 0

  runtime_dir=$(dirname -- "$RUNTIME")
  runtime_base=$(basename -- "$RUNTIME")
  mkdir -p -- "$runtime_dir"
  candidate=$(mktemp "$runtime_dir/.${runtime_base}.dns-failover.XXXXXX")
  old_copy=$(mktemp "$runtime_dir/.${runtime_base}.dns-failover-old.XXXXXX")
  if ! cp -- "$RUNTIME" "$candidate" || ! cp -- "$RUNTIME" "$old_copy"; then
    rm -f -- "$candidate" "$old_copy"
    return 1
  fi
  chmod --reference="$RUNTIME" "$candidate" "$old_copy"

  # Only dns.final is changed.  The candidate stays in the runtime directory
  # so the final rename is one filesystem operation and never exposes a
  # partially-written runtime config.
  if ! sed -E -i "0,/^([[:space:]]*)\"final\"[[:space:]]*:[[:space:]]*\"$from_target\"([[:space:]]*,?)[[:space:]]*$/s//\1\"final\": \"$target\"\2/" "$candidate"; then
    rm -f -- "$candidate" "$old_copy"
    return 1
  fi
  grep -Eq "\"final\"[[:space:]]*:[[:space:]]*\"$target\"" "$candidate" || {
    rm -f -- "$candidate" "$old_copy"
    return 1
  }

  # Validate before rename.  This is the equivalent of `sing-box check -c
  # "$candidate"`; sing-box reads the candidate, never the live runtime,
  # while the shared state lock is held.
  if ! "$SING_BOX_BIN" check -c "$candidate" >/dev/null 2>&1; then
    rm -f -- "$candidate" "$old_copy"
    echo 'dns failover candidate rejected by sing-box' >&2
    return 1
  fi
  if ! mv -f -- "$candidate" "$RUNTIME"; then
    rm -f -- "$candidate" "$old_copy"
    return 1
  fi

  # The restart request is deliberately written only after the validated
  # runtime rename.  If it cannot be published, restore the old runtime
  # atomically rather than leaving sing-box and the request file inconsistent.
  if ! write_restart_token "$target"; then
    mv -f -- "$old_copy" "$RUNTIME" || true
    rm -f -- "$old_copy"
    echo 'dns failover restart token could not be written' >&2
    return 1
  fi
  rm -f -- "$old_copy"
  printf 'dns_failover_active=%s\n' "$target"
}

switch_dns() {
  local target=$1 lock_dir
  lock_dir=$(dirname -- "$LOCK_FILE")
  mkdir -p -- "$lock_dir"
  # With defaults this is `flock -x "$STATE_DIR/.vibe-vpn.lock"`; util-linux
  # flock on this exact path interoperates with the Go syscall.Flock used by
  # internal/state.AcquireLock.
  flock -x -w "$LOCK_WAIT_SECONDS" "$LOCK_FILE" "$SCRIPT_PATH" --switch-locked "$target"
}

watch_once() {
  local target
  runtime_has_dns_failover_servers || return 0
  if probe cloudflare-dns.com 1.1.1.1; then
    target=remote-dns
  elif probe dns.google 8.8.8.8; then
    target=remote-dns-fallback
  else
    return 0
  fi
  if ! switch_dns "$target"; then
    printf 'dns_failover_apply=skipped target=%s\n' "$target" >&2
  fi
}

if [[ "$MODE" == switch-locked ]]; then
  apply_target_locked "$TARGET"
  exit $?
fi

if [[ -n "$TARGET" ]]; then
  # A target is useful for direct, network-free tests and for bounded repair;
  # it still takes the same shared lock and follows the same validation path.
  switch_dns "$TARGET"
  exit $?
fi

if (( ONCE )); then
  watch_once
  exit 0
fi

[[ "$MAIN_PID" =~ ^[0-9]+$ ]] || { echo '--pid is required for watch mode' >&2; usage >&2; exit 2; }
while kill -0 "$MAIN_PID" 2>/dev/null; do
  watch_once || true
  sleep "$INTERVAL_SECONDS"
done
# A normal-looking watchdog exit is still a supervisor failure: the main
# entrypoint disappeared, so the container must be restarted rather than left
# running without DNS failover supervision.
echo "main entrypoint pid $MAIN_PID is no longer alive; watchdog stopping" >&2
exit 1
