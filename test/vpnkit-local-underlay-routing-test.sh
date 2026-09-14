#!/usr/bin/env bash
set -Eeuo pipefail

# Mock-only contract tests for the issue #40 host routing helper.  The mock
# commands are first in PATH; no host ip, nmcli, systemd, Docker, or live
# networking command is reached by this test.

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
HELPER="$ROOT_DIR/scripts/vpnkit/vpnkit-local-underlay-routing.sh"
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-underlay.XXXXXX")
MOCK_BIN="$TMP_DIR/bin"
MOCK_LOG="$TMP_DIR/commands.log"
MOCK_RULES="$TMP_DIR/rules"
MOCK_SERVICE_ENABLED="$TMP_DIR/service-enabled"
MOCK_SERVICE_ACTIVE="$TMP_DIR/service-active"
FAKE_ROOT="$TMP_DIR/root"
mkdir -p "$MOCK_BIN" "$FAKE_ROOT"
touch "$MOCK_LOG"
printf '%s\n' enabled >"$MOCK_SERVICE_ENABLED"
printf '%s\n' active >"$MOCK_SERVICE_ACTIVE"

cleanup() {
  rm -rf -- "$TMP_DIR"
}
trap cleanup EXIT

fail_test() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_file_contains() {
  local path=$1
  local pattern=$2
  grep -Fq -- "$pattern" "$path" || fail_test "missing expected file content"
}

assert_no_file_pattern() {
  local path=$1
  local pattern=$2
  ! grep -Eq -- "$pattern" "$path" || fail_test "unexpected file content"
}

cat > "$MOCK_RULES" <<'EOF_RULES'
0:	from all lookup local
1000:	from 198.18.0.0/24 lookup 51840
1001:	from 198.18.0.0/24 unreachable
32766:	from all lookup main
32767:	from all lookup default
EOF_RULES

cat > "$MOCK_BIN/ip" <<'EOF_IP'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'ip %s\n' "$*" >> "$MOCK_LOG"

if [[ "$*" == "-4 rule show" ]]; then
  cat "$MOCK_RULES"
  exit 0
fi
if [[ "$1" == "-4" && "$2" == "rule" && "$3" == "add" ]]; then
  priority=${5:-}
  subnet=${VPNKIT_LOCAL_DOCKER_SUBNET:-198.18.0.0/24}
  if [[ "$*" == *" to $subnet lookup main"* ]]; then
    printf '%s:\tfrom all to %s lookup main suppress_prefixlength 0\n' "$priority" "$subnet" >> "$MOCK_RULES"
  elif [[ "$*" == *" to $subnet unreachable" ]]; then
    printf '%s:\tfrom all to %s unreachable\n' "$priority" "$subnet" >> "$MOCK_RULES"
  elif [[ "$*" == *" unreachable" ]]; then
    printf '%s:\tfrom %s unreachable\n' "$priority" "$subnet" >> "$MOCK_RULES"
  else
    printf '%s:\tfrom %s lookup 51840\n' "$priority" "$subnet" >> "$MOCK_RULES"
  fi
  sort -t: -k1,1n "$MOCK_RULES" -o "$MOCK_RULES"
  exit 0
fi
if [[ "$1" == "-4" && "$2" == "rule" && "$3" == "del" ]]; then
  priority=${5:-}
  awk -v p="${priority}:" '$1 != p { print }' "$MOCK_RULES" > "${MOCK_RULES}.tmp"
  mv -f "${MOCK_RULES}.tmp" "$MOCK_RULES"
  if [[ "$*" == *'rule del priority 998 '* && -n "${MOCK_SIGNAL_ON_RULE_DEL:-}" && ! -e "${MOCK_SIGNAL_MARKER:-}" ]]; then
    : >"$MOCK_SIGNAL_MARKER"
    kill -"$MOCK_SIGNAL_ON_RULE_DEL" "$PPID"
  fi
  exit 0
fi

if [[ "$1" == "-4" && "$2" == "route" && "$3" == "get" ]]; then
  target=${4:-}
  subnet=${VPNKIT_LOCAL_DOCKER_SUBNET:-198.18.0.0/24}
  if grep -Eq "to ${subnet} (lookup|table) main" "$MOCK_RULES"; then
    if [[ ${MOCK_DOCKER_ROUTE_ABSENT:-0} == 1 ]]; then
      printf '%s\n' 'RTNETLINK answers: Network is unreachable' >&2
      exit 2
    fi
    printf '%s\n' "$target dev br-vpnkit src 198.18.0.1"
    exit 0
  fi
  if grep -Eq "to ${subnet} (lookup|table) 51841" "$MOCK_RULES"; then
    printf '%s\n' "$target dev enp42s0 src 192.0.2.10"
    exit 0
  fi
  exit 2
fi

if [[ "$*" == "-4 route show table all" ]]; then
  if [[ ${MOCK_TABLE_MISSING:-0} != 1 ]]; then
    if [[ ${MOCK_BAD_UPLINK:-0} == 1 ]]; then
      printf '%s\n' 'default via 192.0.2.1 dev ppp0 proto dhcp src 192.0.2.10 metric 100'
    else
      printf '%s\n' 'default via 192.0.2.1 dev enp42s0 table 51840 proto 242 metric 42040'
    fi
  fi
  if [[ ${MOCK_BAD_UPLINK:-0} == 1 ]]; then
    cat <<'EOF_BAD_ROUTES'
default via 192.0.2.1 dev ppp0 proto dhcp src 192.0.2.10 metric 100
192.0.2.0/24 dev ppp0 proto kernel scope link src 192.0.2.10 metric 100
EOF_BAD_ROUTES
  else
    cat <<'EOF_ROUTES'
default via 192.0.2.1 dev enp42s0 proto dhcp src 192.0.2.10 metric 100
192.0.2.0/24 dev enp42s0 proto kernel scope link src 192.0.2.10 metric 100
EOF_ROUTES
  fi
  if [[ ${MOCK_DOCKER_ROUTE_ABSENT:-0} != 1 ]]; then
    printf '%s\n' '198.18.0.0/24 dev br-vpnkit proto kernel scope link src 198.18.0.1 metric 0'
  fi
  exit 0
fi
if [[ "$*" == "-4 route show table 51840" ]]; then
  if [[ ${MOCK_TABLE_MISSING:-0} == 1 ]]; then exit 2; fi
  cat <<'EOF_MANAGED'
192.0.2.0/24 dev enp42s0 proto 99 scope link metric 42040
default via 192.0.2.1 dev enp42s0 proto 99 metric 42040
EOF_MANAGED
  exit 0
fi

# Route add/replace/delete are intentionally accepted but never touch the
# kernel.  Their arguments remain in the mock log for exact-cleanup asserts.
exit 0
EOF_IP

cat > "$MOCK_BIN/nmcli" <<'EOF_NMCLI'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'nmcli %s\n' "$*" >> "$MOCK_LOG"
if [[ ${MOCK_BAD_UPLINK:-0} == 1 ]]; then
  printf '%s\n' 'ppp0:ethernet:connected'
  printf '%s\n' 'vpn0:ethernet:connected'
else
  printf '%s\n' 'enp99s0:ethernet:connected'
  printf '%s\n' 'enp42s0:ethernet:connected'
  printf '%s\n' 'tun-work:tun:connected'
fi
EOF_NMCLI

cat > "$MOCK_BIN/systemctl" <<'EOF_SYSTEMCTL'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'systemctl %s\n' "$*" >> "$MOCK_LOG"
if [[ ${MOCK_SYSTEMCTL_FAIL:-0} == 1 ]]; then
  exit 1
fi
if [[ ${MOCK_SYSTEMCTL_FAIL:-0} == enable && "${1:-}" == enable && "${2:-}" == --now ]]; then
  exit 1
fi
case "${1:-}" in
  is-enabled)
    [[ -s "$MOCK_SERVICE_ENABLED" ]] && grep -Fxq enabled "$MOCK_SERVICE_ENABLED"
    ;;
  is-active)
    [[ -s "$MOCK_SERVICE_ACTIVE" ]] && grep -Fxq active "$MOCK_SERVICE_ACTIVE"
    ;;
  enable)
    printf '%s\n' enabled >"$MOCK_SERVICE_ENABLED"
    [[ "${2:-}" == --now ]] && printf '%s\n' active >"$MOCK_SERVICE_ACTIVE"
    ;;
  disable)
    printf '%s\n' disabled >"$MOCK_SERVICE_ENABLED"
    [[ "${2:-}" == --now ]] && printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE"
    ;;
  start) printf '%s\n' active >"$MOCK_SERVICE_ACTIVE" ;;
  stop) printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE" ;;
esac
exit 0
EOF_SYSTEMCTL

cat > "$MOCK_BIN/id" <<'EOF_ID'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == "-u" ]]; then
  if [[ ${MOCK_NONROOT:-0} == 1 ]]; then
    printf '1000\n'
  else
    printf '0\n'
  fi
else
  printf 'mock\n'
fi
EOF_ID

cat > "$MOCK_BIN/rm" <<'EOF_RM'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'rm %s\n' "$*" >>"$MOCK_LOG"
exec /usr/bin/rm "$@"
EOF_RM

chmod +x "$MOCK_BIN"/*
export PATH="$MOCK_BIN:$PATH"
export MOCK_LOG MOCK_RULES MOCK_SERVICE_ENABLED MOCK_SERVICE_ACTIVE
export VPNKIT_LOCAL_UNDERLAY_ROOT="$FAKE_ROOT"
export VPNKIT_LOCAL_UNDERLAY_LOCK_FILE="$FAKE_ROOT/run/lock/vpnkit-local-underlay-routing.lock"
export VPNKIT_LOCAL_DOCKER_SUBNET=198.18.0.0/24
export VPNKIT_LOCAL_ROUTE_TABLE=51840

# Static safety contract: no privilege escalation helper, Docker command, or
# broad route/rule flush is present in the implementation.
assert_no_file_pattern "$HELPER" '(^|[[:space:]])sudo([[:space:]]|$)'
assert_no_file_pattern "$HELPER" 'ip([[:space:]].*)?(route|rule)[[:space:]]+flush'
assert_no_file_pattern "$HELPER" 'nmcli[[:space:]].*(connection|con)[[:space:]]+(up|down|modify|delete|add)'
grep -Fq 'DEFAULT_DOCKER_SUBNET="172.30.89.0/24"' "$HELPER" || fail_test "helper default subnet drifted from compose.local.yaml"
grep -Fq 'VPNKIT_LOCAL_DOCKER_SUBNET:-172.30.89.0/24' "$ROOT_DIR/compose.local.yaml" || fail_test "compose local subnet default drifted from helper"

# Explicit and automatic discovery must reject PPP/VPN interfaces even when a
# fixture marks them connected or gives them a default route.
for bad_iface in ppp0 vpn0; do
  if VPNKIT_LOCAL_UPLINK_IFACE="$bad_iface" bash "$HELPER" plan >/dev/null 2>&1; then
    fail_test "explicit non-physical uplink was accepted: $bad_iface"
  fi
done
bad_auto_output=$(MOCK_BAD_UPLINK=1 bash "$HELPER" plan 2>&1) || fail_test 'automatic discovery plan unexpectedly errored'
grep -Fq 'physical_uplink_table=unavailable' <<<"$bad_auto_output" || fail_test 'automatic discovery accepted a PPP/VPN uplink'
VPNKIT_LOCAL_UPLINK_IFACE=auto bash "$HELPER" plan >/dev/null 2>&1 || fail_test 'auto uplink mode rejected valid physical fixture'
if VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY=997 VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY=999 \
    bash "$HELPER" plan >/dev/null 2>&1; then
  fail_test 'non-adjacent destination policy slots were accepted'
fi

# Help is non-mutating and does not even need mocked host discovery.
: > "$MOCK_LOG"
bash "$HELPER" --help >/dev/null 2>&1 || fail_test "help contract failed"
[[ ! -s "$MOCK_LOG" ]] || fail_test "help probed a host command"

# Default invocation is plan and must remain read-only.  Its diagnostics must
# not echo the fixture's route values.
: > "$MOCK_LOG"
plan_output=$(bash "$HELPER" 2>&1) || fail_test "default plan failed"
grep -Fq 'mutation=none' <<< "$plan_output" || fail_test "plan was not non-mutating"
grep -Fq 'physical_uplink_table=ready' <<< "$plan_output" || fail_test "plan did not discover fixture uplink"
if grep -Eq '192\.0\.2\.|enp42s0|51840' <<< "$plan_output"; then
  fail_test "plan leaked route values"
fi
if grep -Eq 'ip .* (route (add|replace|del)|rule (add|del))' "$MOCK_LOG"; then
  fail_test "plan mutated routing"
fi
if grep -Fq 'systemctl' "$MOCK_LOG"; then
  fail_test "plan touched systemd"
fi

# Install requires the explicit confirmation flag before even attempting a
# host mutation.
: > "$MOCK_LOG"
if bash "$HELPER" install >/dev/null 2>&1; then
  fail_test "install accepted missing confirmation"
fi
[[ ! -s "$MOCK_LOG" ]] || fail_test "unconfirmed install reached a host command"

# An unrelated config at the bounded target path is never adopted or
# overwritten by an install attempt.
unmanaged_config="$FAKE_ROOT/etc/vpnkit-local-underlay-routing.conf"
mkdir -p "${unmanaged_config%/*}"
printf '%s\n' 'unrelated=true' > "$unmanaged_config"
: > "$MOCK_LOG"
if bash "$HELPER" install --yes >/dev/null 2>&1; then
  fail_test "install adopted unmanaged config"
fi
assert_file_contains "$unmanaged_config" 'unrelated=true'
if grep -Eq 'ip .* (route (add|replace|del)|rule (add|del))' "$MOCK_LOG"; then
  fail_test "unmanaged config caused route mutation"
fi
rm -f -- "$unmanaged_config"

CONFIG="$FAKE_ROOT/etc/vpnkit-local-underlay-routing.conf"
SERVICE="$FAKE_ROOT/etc/systemd/system/vpnkit-local-underlay-routing.service"
DISPATCHER="$FAKE_ROOT/etc/NetworkManager/dispatcher.d/90-vpnkit-local-underlay-routing"
INSTALLED_HELPER="$FAKE_ROOT/usr/local/libexec/vpnkit-local-underlay-routing"
STATE_DIR="$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing"
STATE_PATH="$STATE_DIR/routes.state"
V1_CONFIG="$TMP_DIR/vpnkit-local-underlay-v1.conf"
V1_HELPER="$TMP_DIR/vpnkit-local-underlay-v1-helper"
V1_SERVICE="$TMP_DIR/vpnkit-local-underlay-v1.service"
V1_DISPATCHER="$TMP_DIR/vpnkit-local-underlay-v1-dispatcher"
V1_RULES="$TMP_DIR/vpnkit-local-underlay-v1.rules"
V1_HELPER_FIXTURE="$ROOT_DIR/test/fixtures/vpnkit-local-underlay-routing-v1.sh"

# This fixture represents the immutable source-only helper from the VERSION=1
# release.  Keep it tracked and independent of the immediate parent revision:
# this test also runs on branches whose parent already contains VERSION=2.
[[ -s "$V1_HELPER_FIXTURE" ]] || fail_test 'cannot read the canonical VERSION=1 helper fixture'
cp -f -- "$V1_HELPER_FIXTURE" "$V1_HELPER"
chmod 0755 "$V1_HELPER"
cat > "$V1_SERVICE" <<EOF_V1_SERVICE
# Managed by vpnkit-local-underlay-routing; do not edit.
[Unit]
Description=Local vpnkit underlay policy routing (issue #40)
After=NetworkManager.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$FAKE_ROOT/usr/local/libexec/vpnkit-local-underlay-routing --runtime-refresh
RemainAfterExit=yes
NoNewPrivileges=no
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
CapabilityBoundingSet=CAP_NET_ADMIN CAP_DAC_READ_SEARCH
AmbientCapabilities=CAP_NET_ADMIN
ReadWritePaths=$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing

[Install]
WantedBy=multi-user.target
EOF_V1_SERVICE
cat > "$V1_DISPATCHER" <<EOF_V1_DISPATCHER
#!/usr/bin/env bash
# Managed by vpnkit-local-underlay-routing; do not edit.
set -Eeuo pipefail

# NetworkManager supplies the interface and event state.  The helper performs
# its own physical-uplink discovery and never edits NetworkManager profiles.
case "\${2:-}" in
  up|dhcp4-change|dhcp6-change|connectivity-change|reapply|down)
    exec "$FAKE_ROOT/usr/local/libexec/vpnkit-local-underlay-routing" --runtime-refresh
    ;;
  *)
    exit 0
    ;;
esac
EOF_V1_DISPATCHER
chmod 0755 "$V1_DISPATCHER"
cat > "$V1_CONFIG" <<'EOF_V1_CONFIG'
# Managed by vpnkit-local-underlay-routing; do not edit.
VERSION=1
DOCKER_SUBNET=198.18.0.0/24
ROUTE_TABLE=51840
RULE_PRIORITY=1000
FAIL_CLOSED_PRIORITY=1001
UPLINK_IFACE=
UPLINK_TABLE=
UPLINK_GATEWAY=
EOF_V1_CONFIG

seed_v1_installation() {
  mkdir -p "${CONFIG%/*}" "${SERVICE%/*}" "${DISPATCHER%/*}" "${INSTALLED_HELPER%/*}"
  cp -f -- "$V1_CONFIG" "$CONFIG"
  cp -f -- "$V1_SERVICE" "$SERVICE"
  cp -f -- "$V1_DISPATCHER" "$DISPATCHER"
  cp -f -- "$V1_HELPER" "$INSTALLED_HELPER"
  chmod 0755 "$INSTALLED_HELPER"
}

# REV-001: a stateful VERSION=1 installation must roll back a failed v2
# migration.  The enable failure happens after candidate 998/999 and the
# route/source policy have been applied, so the final state must be the exact
# original v1 rules, not merely an absence of destination rules.
printf '%s\n' \
  $'0:\tfrom all lookup local' \
  $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
  $'1001:\tfrom 198.18.0.0/24 unreachable' \
  $'32766:\tfrom all lookup main' \
  $'32767:\tfrom all lookup default' > "$MOCK_RULES"
cp -f -- "$MOCK_RULES" "$V1_RULES"
seed_v1_installation
v1_verify_output=$(bash "$HELPER" verify 2>&1) || fail_test "v1 source-only verify unexpectedly failed"
grep -Fq 'destination_lookup_rule=not-applicable' <<< "$v1_verify_output" || fail_test "v1 runtime inferred a destination lookup policy"
grep -Fq 'destination_fail_closed_rule=not-applicable' <<< "$v1_verify_output" || fail_test "v1 runtime inferred a destination blocker"
: > "$MOCK_LOG"
export MOCK_SYSTEMCTL_FAIL=enable
if bash "$HELPER" install --yes >/dev/null 2>&1; then
  fail_test "v1 migration unexpectedly succeeded through forced enable failure"
fi
unset MOCK_SYSTEMCTL_FAIL
grep -Fq 'ip -4 rule add priority 998 to 198.18.0.0/24 lookup main suppress_prefixlength 0' "$MOCK_LOG" \
  || fail_test "forced enable failure did not occur after candidate destination routing"
grep -Fq 'systemctl enable --now vpnkit-local-underlay-routing.service' "$MOCK_LOG" \
  || fail_test "forced systemctl enable failure was not exercised"
grep -Fq 'ip -4 rule del priority 998 to 198.18.0.0/24 lookup main suppress_prefixlength 0' "$MOCK_LOG" \
  || fail_test "rollback did not remove candidate destination lookup"
grep -Fq 'ip -4 rule del priority 999 to 198.18.0.0/24 unreachable' "$MOCK_LOG" \
  || fail_test "rollback did not remove candidate destination blocker"
cmp -s "$CONFIG" "$V1_CONFIG" || fail_test "failed v1 migration did not restore the v1 config"
cmp -s "$INSTALLED_HELPER" "$V1_HELPER" || fail_test "failed v1 migration did not restore the old helper"
cmp -s "$MOCK_RULES" "$V1_RULES" || fail_test "failed v1 migration did not restore the exact source-only rules"
! grep -Eq '^(998|999):' "$MOCK_RULES" || fail_test "failed v1 migration left candidate destination rules"

# The restored old helper must be able to uninstall the restored v1 policy;
# no candidate destination rule may be orphaned by that old cleanup path.
"$INSTALLED_HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "restored v1 helper uninstall failed"
[[ ! -e "$CONFIG" && ! -e "$SERVICE" && ! -e "$DISPATCHER" && ! -e "$INSTALLED_HELPER" ]] \
  || fail_test "restored v1 helper uninstall left managed files"
! grep -Eq '^(998|999):' "$MOCK_RULES" || fail_test "restored v1 helper uninstall left destination rules"
! grep -Eq '^(1000|1001):' "$MOCK_RULES" || fail_test "restored v1 helper uninstall left source rules"

# R5 REV-001: the VERSION=1 unit is an exact legacy identity, not a marker
# plus a few required lines.  Every crafted unit below remains marked and
# systemd-shaped, but must be rejected before either migration or uninstall
# can create the lock, invoke systemctl/ip, or touch any managed file.
R5_LOCK="$FAKE_ROOT/run/lock/vpnkit-local-underlay-routing.lock"
R5_VARIANT_SERVICE="$TMP_DIR/r5-variant.service"

insert_v1_service_line() {
  local section=$1 line=$2
  awk -v section="$section" -v line="$line" '
    $0 == "[" section "]" { print; print line; next }
    { print }
  ' "$V1_SERVICE" > "$SERVICE"
}

write_v1_service_variant() {
  local variant=$1
  cp -f -- "$V1_SERVICE" "$SERVICE"
  case "$variant" in
    exec-start-pre) insert_v1_service_line Service 'ExecStartPre=/bin/true' ;;
    exec-start-post) insert_v1_service_line Service 'ExecStartPost=/bin/true' ;;
    exec-stop) insert_v1_service_line Service 'ExecStop=/bin/true' ;;
    exec-stop-post) insert_v1_service_line Service 'ExecStopPost=/bin/true' ;;
    environment) insert_v1_service_line Service 'Environment=VPNKIT_R5=unexpected' ;;
    shell-wrapper)
      sed -i "s|^ExecStart=.*|ExecStart=/bin/sh -c 'exec $FAKE_ROOT/usr/local/libexec/vpnkit-local-underlay-routing --runtime-refresh'|" "$SERVICE"
      ;;
    duplicate-directive) insert_v1_service_line Service 'RemainAfterExit=yes' ;;
    extra-unit-key) insert_v1_service_line Unit 'ConditionPathExists=/run/vpnkit-local-underlay-routing' ;;
    extra-service-key) insert_v1_service_line Service 'Restart=no' ;;
    extra-install-key) insert_v1_service_line Install 'Alias=vpnkit-r5-foreign.service' ;;
    extra-section)
      printf '%s\n' '[X-Vpnkit-R5-Foreign]' 'Key=value' >>"$SERVICE"
      ;;
    appended-text) printf '%s\n' '# appended after canonical unit' >>"$SERVICE" ;;
    *) fail_test "unknown R5 service variant: $variant" ;;
  esac
}

run_v1_service_rejection_case() {
  local variant=$1
  local output
  rm -f -- "$CONFIG" "$SERVICE" "$DISPATCHER" "$INSTALLED_HELPER" "$STATE_PATH" "$R5_LOCK"
  rmdir -- "$STATE_DIR" >/dev/null 2>&1 || true
  printf '%s\n' \
    $'0:\tfrom all lookup local' \
    $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
    $'1001:\tfrom 198.18.0.0/24 unreachable' \
    $'32766:\tfrom all lookup main' \
    $'32767:\tfrom all lookup default' >"$MOCK_RULES"
  printf '%s\n' enabled >"$MOCK_SERVICE_ENABLED"
  printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE"
  seed_v1_installation
  write_v1_service_variant "$variant"
  cp -p -- "$SERVICE" "$R5_VARIANT_SERVICE"

  : >"$MOCK_LOG"
  output="$TMP_DIR/r5-${variant}-uninstall.out"
  if bash "$HELPER" uninstall --yes >"$output" 2>&1; then
    fail_test "crafted v1 service was accepted by uninstall: $variant"
  fi
  [[ ! -s "$MOCK_LOG" ]] || fail_test "uninstall touched a host command for crafted v1 service: $variant"
  [[ ! -e "$R5_LOCK" ]] || fail_test "uninstall created the mutation lock for crafted v1 service: $variant"
  cmp -s "$SERVICE" "$R5_VARIANT_SERVICE" || fail_test "uninstall changed crafted service: $variant"
  cmp -s "$CONFIG" "$V1_CONFIG" || fail_test "uninstall changed v1 config: $variant"
  cmp -s "$MOCK_RULES" "$V1_RULES" || fail_test "uninstall changed v1 rules: $variant"

  : >"$MOCK_LOG"
  output="$TMP_DIR/r5-${variant}-migration.out"
  if bash "$HELPER" install --yes >"$output" 2>&1; then
    fail_test "crafted v1 service was accepted by migration: $variant"
  fi
  [[ ! -s "$MOCK_LOG" ]] || fail_test "migration touched a host command for crafted v1 service: $variant"
  [[ ! -e "$R5_LOCK" ]] || fail_test "migration created the mutation lock for crafted v1 service: $variant"
  cmp -s "$SERVICE" "$R5_VARIANT_SERVICE" || fail_test "migration changed crafted service: $variant"
  cmp -s "$CONFIG" "$V1_CONFIG" || fail_test "migration changed v1 config: $variant"
  cmp -s "$MOCK_RULES" "$V1_RULES" || fail_test "migration changed v1 rules: $variant"
}

for r5_variant in \
  exec-start-pre exec-start-post exec-stop exec-stop-post environment \
  shell-wrapper duplicate-directive extra-unit-key extra-service-key \
  extra-install-key extra-section appended-text; do
  run_v1_service_rejection_case "$r5_variant"
done

assert_v1_restored() {
  cmp -s "$CONFIG" "$V1_CONFIG" || fail_test "signal rollback did not restore the exact v1 config"
  cmp -s "$INSTALLED_HELPER" "$V1_HELPER" || fail_test "signal rollback did not restore the exact v1 helper"
  cmp -s "$SERVICE" "$V1_SERVICE" || fail_test "signal rollback did not restore the exact v1 service"
  cmp -s "$DISPATCHER" "$V1_DISPATCHER" || fail_test "signal rollback did not restore the exact v1 dispatcher"
  cmp -s "$MOCK_RULES" "$V1_RULES" || fail_test "signal rollback did not restore the exact v1 rules"
  ! grep -Eq '^(998|999):' "$MOCK_RULES" || fail_test "signal rollback left candidate destination rules"
  [[ ! -e "$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state" ]] \
    || fail_test "signal rollback left candidate route state"
}

run_signal_rollback_case() {
  local signal=$1
  local point=$2
  local enabled=$3
  local active=$4
  local rc expected
  case "$signal" in INT) expected=130 ;; TERM) expected=143 ;; HUP) expected=129 ;; *) fail_test "unsupported signal test" ;; esac
  printf '%s\n' "$enabled" >"$MOCK_SERVICE_ENABLED"
  printf '%s\n' "$active" >"$MOCK_SERVICE_ACTIVE"
  printf '%s\n' \
    $'0:\tfrom all lookup local' \
    $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
    $'1001:\tfrom 198.18.0.0/24 unreachable' \
    $'32766:\tfrom all lookup main' \
    $'32767:\tfrom all lookup default' >"$MOCK_RULES"
  cp -f -- "$MOCK_RULES" "$V1_RULES"
  seed_v1_installation
  : >"$MOCK_LOG"
  set +e
  env VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_AFTER="$point" \
    VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL="$signal" \
    VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_DELAY=0.01 \
    bash "$HELPER" install --yes >"$TMP_DIR/signal-${signal}-${point}.out" 2>&1
  rc=$?
  set -e
  [[ "$rc" == "$expected" ]] \
    || fail_test "${signal} at ${point} returned ${rc}, expected signal status"
  assert_v1_restored
  [[ "$(<"$MOCK_SERVICE_ENABLED")" == "$enabled" ]] \
    || fail_test "${signal} at ${point} changed service enable state"
  [[ "$(<"$MOCK_SERVICE_ACTIVE")" == "$active" ]] \
    || fail_test "${signal} at ${point} changed service active state"
  [[ $(grep -c 'ip -4 rule del priority 998 ' "$MOCK_LOG" || true) == 1 ]] \
    || fail_test "${signal} at ${point} did not execute one destination lookup rollback"
  [[ $(grep -c 'ip -4 rule del priority 999 ' "$MOCK_LOG" || true) == 1 ]] \
    || fail_test "${signal} at ${point} did not execute one destination blocker rollback"
}

# Signals are injected by a child after each transaction window.  The parent
# must roll back once, retain the signal status, and restore both service
# booleans.  Exercise inactive-enabled and active-disabled prior states.
run_signal_rollback_case INT after-runtime-refresh enabled inactive
run_signal_rollback_case TERM after-service-enable disabled active
run_signal_rollback_case HUP after-runtime-refresh enabled inactive

# A second signal delivered by an ip child during compensation is recorded,
# not allowed to recurse into rollback or remove a fail-closed blocker early.
printf '%s\n' enabled >"$MOCK_SERVICE_ENABLED"
printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE"
printf '%s\n' \
  $'0:\tfrom all lookup local' \
  $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
  $'1001:\tfrom 198.18.0.0/24 unreachable' \
  $'32766:\tfrom all lookup main' \
  $'32767:\tfrom all lookup default' >"$MOCK_RULES"
cp -f -- "$MOCK_RULES" "$V1_RULES"
seed_v1_installation
second_signal_marker="$TMP_DIR/second-signal.marker"
rm -f -- "$second_signal_marker"
: >"$MOCK_LOG"
set +e
env VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_AFTER=after-service-enable \
  VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL=INT VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_DELAY=0.01 \
  MOCK_SIGNAL_ON_RULE_DEL=TERM MOCK_SIGNAL_MARKER="$second_signal_marker" \
  bash "$HELPER" install --yes >/dev/null 2>&1
second_signal_rc=$?
set -e
[[ "$second_signal_rc" == 130 ]] || fail_test "second rollback signal changed original signal status"
[[ -e "$second_signal_marker" ]] || fail_test "rollback signal child did not inject its signal"
assert_v1_restored
[[ $(grep -c 'ip -4 rule del priority 998 ' "$MOCK_LOG" || true) == 1 ]] \
  || fail_test "second rollback signal caused recursive destination cleanup"

# An EXIT after candidate runtime has begun uses the same one-shot rollback
# path and preserves its original non-signal status.
printf '%s\n' enabled >"$MOCK_SERVICE_ENABLED"
printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE"
printf '%s\n' \
  $'0:\tfrom all lookup local' \
  $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
  $'1001:\tfrom 198.18.0.0/24 unreachable' \
  $'32766:\tfrom all lookup main' \
  $'32767:\tfrom all lookup default' >"$MOCK_RULES"
cp -f -- "$MOCK_RULES" "$V1_RULES"
seed_v1_installation
: >"$MOCK_LOG"
set +e
env VPNKIT_LOCAL_UNDERLAY_TEST_EXIT_AFTER=after-runtime-refresh \
  VPNKIT_LOCAL_UNDERLAY_TEST_EXIT_STATUS=77 \
  bash "$HELPER" install --yes >"$TMP_DIR/exit-after-runtime.out" 2>&1
exit_rc=$?
set -e
[[ "$exit_rc" == 77 ]] || fail_test "unexpected EXIT status was not preserved"
assert_v1_restored
[[ "$(<"$MOCK_SERVICE_ENABLED")" == enabled && "$(<"$MOCK_SERVICE_ACTIVE")" == inactive ]] \
  || fail_test "unexpected EXIT changed prior service state"

# Rollback and candidate mutation must remain serialized with the helper lock;
# a held lock rejects before any candidate file or rule is touched.
lock_path="$FAKE_ROOT/run/lock/vpnkit-local-underlay-routing.lock"
mkdir -p "${lock_path%/*}"
flock -x "$lock_path" -c 'sleep 0.25' &
lock_pid=$!
sleep 0.03
: >"$MOCK_LOG"
set +e
VPNKIT_LOCAL_UNDERLAY_LOCK_WAIT_SECONDS=0 bash "$HELPER" install --yes >/dev/null 2>&1
lock_rc=$?
set -e
wait "$lock_pid"
[[ "$lock_rc" != 0 ]] || fail_test "held routing lock did not reject install"
cmp -s "$CONFIG" "$V1_CONFIG" || fail_test "held routing lock changed v1 config"
cmp -s "$MOCK_RULES" "$V1_RULES" || fail_test "held routing lock changed v1 rules"
! grep -Eq 'ip -4 (rule|route) (add|replace|del)' "$MOCK_LOG" \
  || fail_test "held routing lock allowed routing mutation"

# A clean v2 transaction has no prior unit to restore, but a signal after
# service enable must still remove every candidate file and rule cleanly.
printf '%s\n' disabled >"$MOCK_SERVICE_ENABLED"
printf '%s\n' inactive >"$MOCK_SERVICE_ACTIVE"
printf '%s\n' \
  $'0:\tfrom all lookup local' \
  $'32766:\tfrom all lookup main' \
  $'32767:\tfrom all lookup default' >"$MOCK_RULES"
rm -f -- "$CONFIG" "$SERVICE" "$DISPATCHER" "$INSTALLED_HELPER" \
  "$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state"
: >"$MOCK_LOG"
set +e
env VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_AFTER=after-service-enable \
  VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL=TERM VPNKIT_LOCAL_UNDERLAY_TEST_SIGNAL_DELAY=0.01 \
  bash "$HELPER" install --yes >/dev/null 2>&1
clean_signal_rc=$?
set -e
[[ "$clean_signal_rc" == 143 ]] || fail_test "clean v2 signal status was not preserved"
for clean_path in "$CONFIG" "$SERVICE" "$DISPATCHER" "$INSTALLED_HELPER" \
  "$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state"; do
  [[ ! -e "$clean_path" ]] || fail_test "clean v2 signal rollback left $clean_path"
done
! grep -Eq '^(998|999|1000|1001):' "$MOCK_RULES" \
  || fail_test "clean v2 signal rollback left managed policy rules"
[[ "$(<"$MOCK_SERVICE_ENABLED")" == disabled && "$(<"$MOCK_SERVICE_ACTIVE")" == inactive ]] \
  || fail_test "clean v2 signal rollback changed absent service state"

# VERSION=2 is not allowed to borrow absent destination fields from process
# defaults.  The parser must fail before any host probe or mutation.
cat > "$CONFIG" <<'EOF_INCOMPLETE_V2_CONFIG'
# Managed by vpnkit-local-underlay-routing; do not edit.
VERSION=2
DOCKER_SUBNET=198.18.0.0/24
ROUTE_TABLE=51840
RULE_PRIORITY=1000
FAIL_CLOSED_PRIORITY=1001
UPLINK_IFACE=
UPLINK_TABLE=
UPLINK_GATEWAY=
EOF_INCOMPLETE_V2_CONFIG
: > "$MOCK_LOG"
if bash "$HELPER" verify >/dev/null 2>&1; then
  fail_test "incomplete v2 config silently used destination defaults"
fi
[[ ! -s "$MOCK_LOG" ]] || fail_test "incomplete v2 config reached a host command"
rm -f -- "$CONFIG"

# A successful migration consumes the same v1 state, installs all four rules,
# and persists the explicit VERSION=2 destination-aware config.
printf '%s\n' \
  $'0:\tfrom all lookup local' \
  $'1000:\tfrom 198.18.0.0/24 lookup 51840' \
  $'1001:\tfrom 198.18.0.0/24 unreachable' \
  $'32766:\tfrom all lookup main' \
  $'32767:\tfrom all lookup default' > "$MOCK_RULES"
seed_v1_installation
bash "$HELPER" install --yes >/dev/null 2>&1 || fail_test "successful v1 migration failed"
assert_file_contains "$CONFIG" 'VERSION=2'
assert_file_contains "$CONFIG" 'DESTINATION_RULE_PRIORITY=998'
assert_file_contains "$CONFIG" 'DESTINATION_FAIL_CLOSED_PRIORITY=999'
for expected_rule in \
  '998:[[:space:]]+from all to 198\.18\.0\.0/24 lookup main suppress_prefixlength 0' \
  '999:[[:space:]]+from all to 198\.18\.0\.0/24 unreachable' \
  '1000:[[:space:]]+from 198\.18\.0\.0/24 lookup 51840' \
  '1001:[[:space:]]+from 198\.18\.0\.0/24 unreachable'; do
  grep -Eq "^${expected_rule}$" "$MOCK_RULES" || fail_test "successful v1 migration missed rule: $expected_rule"
done
bash "$HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "successful v1 migration cleanup failed"

# Mock-root installation exercises the route/rule ordering and public-safe
# bounded templates without touching the real /etc, systemd, or route state.
cat > "$MOCK_RULES" <<'EOF_BUILTIN_RULES'
0:	from all lookup local
2000:	from all to 198.18.0.0/24 lookup 51841
32766:	from all lookup main
32767:	from all lookup default
EOF_BUILTIN_RULES
: > "$MOCK_LOG"
baseline_route=$(ip -4 route get 198.18.0.2 2>&1) || fail_test "mock work-VPN route lookup failed"
grep -Fq 'dev enp42s0' <<< "$baseline_route" || fail_test "mock baseline did not select later work-style route"
bash "$HELPER" install --yes >/dev/null 2>&1 || fail_test "mock install failed"
destination_block_add_line=$(grep -n 'ip -4 rule add priority 999 to 198.18.0.0/24 unreachable' "$MOCK_LOG" | head -1 | cut -d: -f1)
source_block_add_line=$(grep -n 'ip -4 rule add priority 1001' "$MOCK_LOG" | head -1 | cut -d: -f1)
route_replace_line=$(grep -n 'ip -4 route replace' "$MOCK_LOG" | head -1 | cut -d: -f1)
destination_lookup_add_line=$(grep -n 'ip -4 rule add priority 998 to 198.18.0.0/24 lookup main' "$MOCK_LOG" | head -1 | cut -d: -f1)
lookup_add_line=$(grep -n 'ip -4 rule add priority 1000' "$MOCK_LOG" | head -1 | cut -d: -f1)
[[ -n "$destination_block_add_line" && -n "$source_block_add_line" && -n "$route_replace_line" && -n "$destination_lookup_add_line" && -n "$lookup_add_line" ]] \
  || fail_test "install did not exercise all managed mutations"
(( destination_block_add_line < source_block_add_line && source_block_add_line < route_replace_line && route_replace_line < destination_lookup_add_line && destination_lookup_add_line < lookup_add_line )) \
  || fail_test "install did not establish destination/source barriers before lookups"

selected_route=$(ip -4 route get 198.18.0.2 2>&1) || fail_test "mock Docker destination route lookup failed"
grep -Fq 'dev br-vpnkit' <<< "$selected_route" || fail_test "destination policy did not select local Docker bridge"
export MOCK_DOCKER_ROUTE_ABSENT=1
if ip -4 route get 198.18.0.2 >/dev/null 2>&1; then
  fail_test "destination policy did not fail closed without the local bridge route"
fi
unset MOCK_DOCKER_ROUTE_ABSENT

[[ -f "$CONFIG" && -f "$SERVICE" && -f "$DISPATCHER" && -x "$INSTALLED_HELPER" ]] || fail_test "bounded install files missing"
assert_file_contains "$SERVICE" 'ExecStart='
assert_file_contains "$DISPATCHER" '--runtime-refresh'
assert_file_contains "$CONFIG" 'VERSION=2'
assert_file_contains "$CONFIG" 'DESTINATION_RULE_PRIORITY=998'
assert_file_contains "$CONFIG" 'DESTINATION_FAIL_CLOSED_PRIORITY=999'
[[ $(head -1 "$DISPATCHER") == '#!/usr/bin/env bash' ]] || fail_test "dispatcher template is not executable-safe"
assert_no_file_pattern "$SERVICE" '192\.0\.2\.|enp42s0|51840'
assert_no_file_pattern "$DISPATCHER" '192\.0\.2\.|enp42s0|51840'

verify_output=$(bash "$HELPER" verify 2>&1) || fail_test "verify failed after mock install"
grep -Fq 'destination_lookup_rule=pass' <<< "$verify_output" || fail_test "destination lookup rule was not verified"
grep -Fq 'destination_fail_closed_rule=pass' <<< "$verify_output" || fail_test "destination fail-closed rule was not verified"
grep -Fq 'lookup_rule=pass' <<< "$verify_output" || fail_test "lookup rule was not verified"
grep -Fq 'fail_closed_rule=pass' <<< "$verify_output" || fail_test "fail-closed rule was not verified"
grep -Fq 'rule_order=pass' <<< "$verify_output" || fail_test "rule ordering was not verified"
grep -Fxq 'canonical_priorities=ok' <<< "$verify_output" || fail_test "default managed priorities did not emit the canonical marker"
if grep -Eq '192\.0\.2\.|enp42s0|51840' <<< "$verify_output"; then
  fail_test "verify leaked route values"
fi

# An unrelated rule in a reserved destination slot must be rejected rather
# than adopted or overwritten.
printf '%s\n' '998: from all lookup 777' >> "$MOCK_RULES"
if bash "$HELPER" verify >/dev/null 2>&1; then
  fail_test "verify accepted a destination rule-slot collision"
fi
awk '!($1 == "998:" && $0 ~ /lookup 777/)' "$MOCK_RULES" > "${MOCK_RULES}.tmp"
mv -f "$MOCK_RULES.tmp" "$MOCK_RULES"

# A never-created numeric table is an empty owned table, not a fatal read
# error. Refresh must install its first routes while the blocker is retained.
export MOCK_TABLE_MISSING=1
bash "$HELPER" --runtime-refresh >/dev/null 2>&1 || fail_test "refresh rejected a never-created owned table"
unset MOCK_TABLE_MISSING

status_output=$(bash "$HELPER" status 2>&1) || fail_test "status failed after mock install"
grep -Fq 'mutation=none' <<< "$status_output" || fail_test "status was not read-only"
grep -Fq 'destination_lookup_rule=yes' <<< "$status_output" || fail_test "status did not report destination lookup rule"
grep -Fq 'destination_fail_closed_rule=yes' <<< "$status_output" || fail_test "status did not report destination fail-closed rule"
if grep -Eq '192\.0\.2\.|enp42s0|51840' <<< "$status_output"; then
  fail_test "status leaked route values"
fi

# A broader, earlier destination rule must fail verification rather than
# bypassing the local-main-or-unreachable boundary.  The source rule check is
# retained independently below.
printf '%s\n' '500: from all to 198.18.0.0/25 lookup 777' >> "$MOCK_RULES"
if bash "$HELPER" verify >/dev/null 2>&1; then
  fail_test "verify accepted an earlier overlapping destination rule"
fi
awk '$1 != "500:" { print }' "$MOCK_RULES" > "${MOCK_RULES}.tmp"
mv -f "${MOCK_RULES}.tmp" "$MOCK_RULES"
printf '%s\n' '500: from 198.18.0.0/25 lookup 777' >> "$MOCK_RULES"
if bash "$HELPER" verify >/dev/null 2>&1; then
  fail_test "verify accepted an earlier overlapping source rule"
fi
awk '$1 != "500:" { print }' "$MOCK_RULES" > "${MOCK_RULES}.tmp"
mv -f "${MOCK_RULES}.tmp" "$MOCK_RULES"

# A failed systemd step must leave the previously managed file set intact.
export MOCK_SYSTEMCTL_FAIL=1
if bash "$HELPER" install --yes >/dev/null 2>&1; then
  fail_test "failed systemd install unexpectedly succeeded"
fi
unset MOCK_SYSTEMCTL_FAIL
[[ -f "$CONFIG" && -f "$SERVICE" && -f "$DISPATCHER" && -x "$INSTALLED_HELPER" ]] \
  || fail_test "failed install did not restore managed files"
grep -Eq '^998:[[:space:]]+from all to 198\.18\.0\.0/24 lookup main suppress_prefixlength 0$' "$MOCK_RULES" || fail_test "destination lookup rule was not restored"
grep -Eq '^999:[[:space:]]+from all to 198\.18\.0\.0/24 unreachable$' "$MOCK_RULES" || fail_test "destination fail-closed rule was not restored"
grep -Eq '^1000:[[:space:]]+from 198\.18\.0\.0/24 lookup 51840$' "$MOCK_RULES" || fail_test "lookup rule was not restored"
grep -Eq '^1001:[[:space:]]+from 198\.18\.0\.0/24 unreachable$' "$MOCK_RULES" || fail_test "fail-closed rule was not restored"
grep -Eq '^2000:[[:space:]]+from all to 198\.18\.0\.0/24 lookup 51841$' "$MOCK_RULES" || fail_test "foreign work-style rule was not preserved"

# Both mutation contracts require direct root as well as --yes.
: > "$MOCK_LOG"
export MOCK_NONROOT=1
if bash "$HELPER" uninstall --yes >/dev/null 2>&1; then
  fail_test "uninstall accepted non-root caller"
fi
unset MOCK_NONROOT
[[ ! -s "$MOCK_LOG" ]] || fail_test "non-root uninstall reached a host command"

# Uninstall must require root plus --yes.  The confirmed mock uninstall must
# issue exact deletes for marked routes/rules and never flush a table.
bash "$HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "mock uninstall failed"
if [[ -e "$CONFIG" || -e "$SERVICE" || -e "$DISPATCHER" || -e "$INSTALLED_HELPER" ]]; then
  fail_test "uninstall left managed files"
fi
grep -Fq 'ip -4 rule del priority 998 to 198.18.0.0/24 lookup main suppress_prefixlength 0' "$MOCK_LOG" \
  || fail_test "uninstall did not remove exact destination lookup rule"
grep -Fq 'ip -4 rule del priority 999 to 198.18.0.0/24 unreachable' "$MOCK_LOG" \
  || fail_test "uninstall did not remove exact destination fail-closed rule"
grep -Fq 'ip -4 rule del priority 1000 from 198.18.0.0/24 table 51840' "$MOCK_LOG" \
  || fail_test "uninstall did not remove exact lookup rule"
grep -Fq 'ip -4 rule del priority 1001 from 198.18.0.0/24 unreachable' "$MOCK_LOG" \
  || fail_test "uninstall did not remove exact fail-closed rule"
grep -Fq 'ip -4 route del default via 192.0.2.1 dev enp42s0 table 51840 proto 99 metric 42040' "$MOCK_LOG" \
  || fail_test "uninstall did not remove exact managed default"
if grep -Eq 'route flush|rule flush|ip route del table 51840$' "$MOCK_LOG"; then
  fail_test "uninstall used broad cleanup"
fi
# The later work-VPN policy is foreign state and must survive exact cleanup.
grep -Eq '^2000:[[:space:]]+from all to 198\.18\.0\.0/24 lookup 51841$' "$MOCK_RULES" \
  || fail_test "uninstall removed the foreign work-style rule"

# Generic helper configurability remains valid, but a non-canonical complete
# installation must not claim the live acceptance marker.
cat > "$MOCK_RULES" <<'EOF_CUSTOM_RULES'
0:\tfrom all lookup local
2000:\tfrom all to 198.18.0.0/24 lookup 51841
32766:\tfrom all lookup main
32767:\tfrom all lookup default
EOF_CUSTOM_RULES
: > "$MOCK_LOG"
custom_install_output=$(VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY=996 \
  VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY=997 \
  VPNKIT_LOCAL_RULE_PRIORITY=1002 VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY=1003 \
  bash "$HELPER" install --yes 2>&1) || fail_test "custom valid-priority install failed"
custom_verify_output=$(VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY=996 \
  VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY=997 \
  VPNKIT_LOCAL_RULE_PRIORITY=1002 VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY=1003 \
  bash "$HELPER" verify 2>&1) || fail_test "custom valid-priority verify failed"
grep -Fq 'destination_lookup_rule=pass' <<< "$custom_verify_output" || fail_test "custom destination lookup was not verified"
! grep -Fxq 'canonical_priorities=ok' <<< "$custom_verify_output" || fail_test "custom priorities emitted the canonical marker"
VPNKIT_LOCAL_DESTINATION_RULE_PRIORITY=996 \
  VPNKIT_LOCAL_DESTINATION_FAIL_CLOSED_PRIORITY=997 \
  VPNKIT_LOCAL_RULE_PRIORITY=1002 VPNKIT_LOCAL_FAIL_CLOSED_PRIORITY=1003 \
  bash "$HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "custom valid-priority uninstall failed"

# R4 REV-001: uninstall owns a read-only all-artifact preflight.  A foreign,
# malformed, partial, or symlinked managed path must not reach the lock,
# systemd, policy routing, or file cleanup.  The cases below are stateful: the
# other artifacts remain a coherent installed set while one path changes.
R4_STATE_DIR="$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing"
R4_LOCK="$FAKE_ROOT/run/lock/vpnkit-local-underlay-routing.lock"
R4_CORE_PATHS=("$CONFIG" "$INSTALLED_HELPER" "$SERVICE" "$DISPATCHER")
R4_OPTIONAL_PATHS=("$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state" "$R4_LOCK")
R4_SNAPSHOT_DIR="$TMP_DIR/r4-snapshot"
mkdir -p "$R4_SNAPSHOT_DIR"
cat > "$MOCK_RULES" <<'EOF_R4_RULES'
0:\tfrom all lookup local
2000:\tfrom all to 198.18.0.0/24 lookup 51841
32766:\tfrom all lookup main
32767:\tfrom all lookup default
EOF_R4_RULES
bash "$HELPER" install --yes >/dev/null 2>&1 || fail_test "R4 fixture install failed"
for r4_path in "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"; do
  [[ -e "$r4_path" && ! -L "$r4_path" ]] || fail_test "R4 fixture path missing: $r4_path"
  cp -p -- "$r4_path" "$R4_SNAPSHOT_DIR/$(basename "$r4_path")" \
    || fail_test "R4 fixture snapshot failed: $r4_path"
done
cp -p -- "$MOCK_RULES" "$R4_SNAPSHOT_DIR/rules"
printf '%s\n' "$(<"$MOCK_SERVICE_ENABLED")" >"$R4_SNAPSHOT_DIR/service-enabled"
printf '%s\n' "$(<"$MOCK_SERVICE_ACTIVE")" >"$R4_SNAPSHOT_DIR/service-active"

# Install has the same all-artifact contract as uninstall, but its regression
# must prove the rejection happens before mkdir, backup, systemctl, ip, or
# lock creation/open.  Snapshot the complete v2 set and compare every target
# after each marker-spoof/partial case.
R4_CASE_DIR="$TMP_DIR/r4-install-case"
r4_restore_fixture() {
  local path name
  rm -f -- "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"
  rm -rf -- "$R4_STATE_DIR"
  mkdir -p -- "$R4_STATE_DIR" "${R4_LOCK%/*}"
  chmod 0700 -- "$R4_STATE_DIR"
  for path in "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"; do
    name=$(basename "$path")
    cp -p -- "$R4_SNAPSHOT_DIR/$name" "$path"
  done
  cp -p -- "$R4_SNAPSHOT_DIR/rules" "$MOCK_RULES"
  cp -p -- "$R4_SNAPSHOT_DIR/service-enabled" "$MOCK_SERVICE_ENABLED"
  cp -p -- "$R4_SNAPSHOT_DIR/service-active" "$MOCK_SERVICE_ACTIVE"
}

r4_capture_install_case() {
  local path name
  rm -rf -- "$R4_CASE_DIR"
  mkdir -p -- "$R4_CASE_DIR"
  for path in "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"; do
    name=$(basename "$path")
    if [[ -L "$path" ]]; then
      printf 'symlink\n%s\n' "$(readlink -- "$path")" >"$R4_CASE_DIR/$name.meta"
    elif [[ -e "$path" ]]; then
      printf 'regular\n%s\n' "$(stat -c '%a:%u:%h' -- "$path")" >"$R4_CASE_DIR/$name.meta"
      cp -p -- "$path" "$R4_CASE_DIR/$name.bytes"
    else
      printf '%s\n' absent >"$R4_CASE_DIR/$name.meta"
    fi
  done
  printf 'regular\n%s\n' "$(stat -c '%a:%u:%h' -- "$R4_STATE_DIR")" >"$R4_CASE_DIR/state-dir.meta"
  cp -p -- "$MOCK_RULES" "$R4_CASE_DIR/rules"
  cp -p -- "$MOCK_SERVICE_ENABLED" "$R4_CASE_DIR/service-enabled"
  cp -p -- "$MOCK_SERVICE_ACTIVE" "$R4_CASE_DIR/service-active"
}

r4_assert_install_case_unchanged() {
  local label=$1 path name kind expected actual backup
  for path in "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"; do
    name=$(basename "$path")
    kind=$(head -n1 "$R4_CASE_DIR/$name.meta")
    case "$kind" in
      absent)
        [[ ! -e "$path" && ! -L "$path" ]] || fail_test "install rejection created target: $label ($name)" ;;
      regular)
        [[ -f "$path" && ! -L "$path" ]] || fail_test "install rejection changed target kind: $label ($name)"
        cmp -s "$path" "$R4_CASE_DIR/$name.bytes" || fail_test "install rejection changed bytes: $label ($name)"
        expected=$(sed -n '2p' "$R4_CASE_DIR/$name.meta")
        actual=$(stat -c '%a:%u:%h' -- "$path")
        [[ "$actual" == "$expected" ]] || fail_test "install rejection changed metadata: $label ($name)"
        ;;
      symlink)
        [[ -L "$path" && "$(readlink -- "$path")" == "$(sed -n '2p' "$R4_CASE_DIR/$name.meta")" ]] \
          || fail_test "install rejection changed symlink: $label ($name)" ;;
      *) fail_test "unknown install-case snapshot kind" ;;
    esac
  done
  expected=$(sed -n '2p' "$R4_CASE_DIR/state-dir.meta")
  actual=$(stat -c '%a:%u:%h' -- "$R4_STATE_DIR")
  [[ "$actual" == "$expected" ]] || fail_test "install rejection changed state directory: $label"
  cmp -s "$MOCK_RULES" "$R4_CASE_DIR/rules" || fail_test "install rejection changed routes/rules: $label"
  cmp -s "$MOCK_SERVICE_ENABLED" "$R4_CASE_DIR/service-enabled" || fail_test "install rejection changed service enablement: $label"
  cmp -s "$MOCK_SERVICE_ACTIVE" "$R4_CASE_DIR/service-active" || fail_test "install rejection changed service activity: $label"
  for backup in "$R4_STATE_DIR"/install-backup.*; do
    [[ ! -e "$backup" && ! -L "$backup" ]] || fail_test "install rejection created backup: $label"
  done
  [[ ! -s "$MOCK_LOG" ]] || fail_test "install rejection reached systemctl/ip/rm: $label"
}

run_install_marker_spoof_case() {
  local label=$1 path=$2 mode=$3
  r4_restore_fixture
  case "$label" in
    config) printf '%s\n' '# Managed by vpnkit-local-underlay-routing; do not edit.' >"$path" ;;
    helper) printf '%s\n' '#!/usr/bin/env bash' '# Managed by vpnkit-local-underlay-routing; do not edit.' 'exit 0' >"$path" ;;
    service) printf '%s\n' '# Managed by vpnkit-local-underlay-routing; do not edit.' '[Service]' 'ExecStart=/bin/true' >"$path" ;;
    dispatcher) printf '%s\n' '#!/usr/bin/env bash' '# Managed by vpnkit-local-underlay-routing; do not edit.' 'exit 0' >"$path" ;;
    state) printf '%s\n' '# Managed by vpnkit-local-underlay-routing; do not edit.' >"$path" ;;
    lock) printf '%s\n' '# Managed by vpnkit-local-underlay-routing; do not edit.' >"$path" ;;
    *) fail_test "unknown install marker-spoof target: $label" ;;
  esac
  chmod "$mode" "$path"
  r4_capture_install_case
  : >"$MOCK_LOG"
  if bash "$HELPER" install --yes >"$TMP_DIR/install-marker-${label}.out" 2>&1; then
    fail_test "install accepted marker-spoofed target: $label"
  fi
  r4_assert_install_case_unchanged "$label"
}

run_install_marker_spoof_case config "$CONFIG" 0644
run_install_marker_spoof_case helper "$INSTALLED_HELPER" 0755
run_install_marker_spoof_case service "$SERVICE" 0644
run_install_marker_spoof_case dispatcher "$DISPATCHER" 0755
run_install_marker_spoof_case state "$STATE_PATH" 0600
run_install_marker_spoof_case lock "$R4_LOCK" 0600

run_install_partial_case() {
  local label=$1 path
  shift
  r4_restore_fixture
  for path in "$@"; do rm -f -- "$path"; done
  r4_capture_install_case
  : >"$MOCK_LOG"
  if bash "$HELPER" install --yes >"$TMP_DIR/install-partial-${label}.out" 2>&1; then
    fail_test "install accepted partial target set: $label"
  fi
  r4_assert_install_case_unchanged "$label"
}

run_install_partial_case missing-service "$SERVICE"
run_install_partial_case missing-config "$CONFIG"
run_install_partial_case missing-helper-dispatcher "$INSTALLED_HELPER" "$DISPATCHER"
run_install_partial_case state-without-core "$CONFIG" "$INSTALLED_HELPER" "$SERVICE" "$DISPATCHER"

# A complete canonical VERSION=2 update remains allowed and must reach the
# transaction path; restore the fixture before the uninstall-specific cases.
r4_restore_fixture
: >"$MOCK_LOG"
bash "$HELPER" install --yes >/dev/null 2>&1 || fail_test "canonical v2 update was rejected"
grep -Eq 'systemctl (daemon-reload|enable --now)' "$MOCK_LOG" || fail_test "canonical v2 update did not reach systemd"
r4_restore_fixture

assert_r4_rejected_unchanged() {
  local label=$1 path=$2
  local target_snapshot="$R4_SNAPSHOT_DIR/target-${label}"
  local target_link=""
  local target_present=0
  if [[ -L "$path" ]]; then
    target_link=$(readlink -- "$path")
    target_present=1
  elif [[ -e "$path" ]]; then
    cp -p -- "$path" "$target_snapshot"
    target_present=1
  fi
  : >"$MOCK_LOG"
  if bash "$HELPER" uninstall --yes >"$TMP_DIR/r4-${label}.out" 2>&1; then
    fail_test "R4 foreign state was accepted: $label"
  fi
  ! grep -Eq 'systemctl (disable|stop)|ip -4 (rule|route) del|(^|[[:space:]])rm([[:space:]]|$)' "$MOCK_LOG" \
    || fail_test "R4 rejection mutated host state: $label"
  [[ "$(<"$MOCK_SERVICE_ENABLED")" == "$(<"$R4_SNAPSHOT_DIR/service-enabled")" ]] \
    || fail_test "R4 rejection changed service enablement: $label"
  [[ "$(<"$MOCK_SERVICE_ACTIVE")" == "$(<"$R4_SNAPSHOT_DIR/service-active")" ]] \
    || fail_test "R4 rejection changed service activity: $label"
  cmp -s "$MOCK_RULES" "$R4_SNAPSHOT_DIR/rules" \
    || fail_test "R4 rejection changed policy rules: $label"
  if (( target_present == 0 )); then
    [[ ! -e "$path" && ! -L "$path" ]] \
      || fail_test "R4 rejection created a missing target: $label"
  elif [[ -L "$path" ]]; then
    [[ "$(readlink -- "$path")" == "$target_link" ]] \
      || fail_test "R4 rejection changed symlink: $label"
  else
    cmp -s "$path" "$target_snapshot" \
      || fail_test "R4 rejection changed target bytes: $label"
  fi
  local other name
  for other in "${R4_CORE_PATHS[@]}" "${R4_OPTIONAL_PATHS[@]}"; do
    [[ "$other" == "$path" ]] && continue
    name=$(basename "$other")
    cmp -s "$other" "$R4_SNAPSHOT_DIR/$name" \
      || fail_test "R4 rejection changed bytes: $label ($name)"
  done
  rm -f -- "$target_snapshot"
}

restore_r4_path() {
  local path=$1 name
  name=$(basename "$path")
  rm -f -- "$path"
  cp -p -- "$R4_SNAPSHOT_DIR/$name" "$path"
}

run_r4_foreign_file_case() {
  local label=$1 path=$2 mode=$3
  printf 'foreign %s\n' "$label" >"$path"
  chmod "$mode" "$path"
  assert_r4_rejected_unchanged "$label" "$path"
  restore_r4_path "$path"
}

run_r4_foreign_file_case config "$CONFIG" 0644
run_r4_foreign_file_case helper "$INSTALLED_HELPER" 0755
# Explicitly retain the requested foreign-service-plus-managed-others case.
run_r4_foreign_file_case service "$SERVICE" 0644
run_r4_foreign_file_case dispatcher "$DISPATCHER" 0755
run_r4_foreign_file_case state "$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state" 0600
run_r4_foreign_file_case lock "$R4_LOCK" 0644
r4_lock_symlink_target="$TMP_DIR/r4-lock-symlink-target"
printf 'foreign lock target\n' >"$r4_lock_symlink_target"
rm -f -- "$R4_LOCK"
ln -s -- "$r4_lock_symlink_target" "$R4_LOCK"
assert_r4_rejected_unchanged lock-symlink "$R4_LOCK"
[[ "$(<"$r4_lock_symlink_target")" == 'foreign lock target' ]] \
  || fail_test "R4 lock symlink target changed"
restore_r4_path "$R4_LOCK"

# A malformed file with the marker is still foreign to the persisted schema;
# main() may parse it, but it must fail before the uninstall preflight can open
# the lock or query systemd/ip.
cat >"$CONFIG" <<'EOF_R4_MALFORMED_CONFIG'
# Managed by vpnkit-local-underlay-routing; do not edit.
VERSION=2
DOCKER_SUBNET=198.18.0.0/24
ROUTE_TABLE=51840
RULE_PRIORITY=1000
FAIL_CLOSED_PRIORITY=1001
UPLINK_IFACE=
UPLINK_TABLE=
UPLINK_GATEWAY=
EOF_R4_MALFORMED_CONFIG
assert_r4_rejected_unchanged malformed-config "$CONFIG"
restore_r4_path "$CONFIG"

# A missing core hook is a partial mixed state, not a file-only cleanup
# invitation.  A final-path symlink is rejected without following its target.
rm -f -- "$SERVICE"
assert_r4_rejected_unchanged partial-service "$SERVICE"
restore_r4_path "$SERVICE"
symlink_target="$TMP_DIR/r4-symlink-target"
printf 'foreign symlink target\n' >"$symlink_target"
rm -f -- "$SERVICE"
ln -s -- "$symlink_target" "$SERVICE"
assert_r4_rejected_unchanged service-symlink "$SERVICE"
[[ "$(<"$symlink_target")" == 'foreign symlink target' ]] \
  || fail_test "R4 symlink target changed"
restore_r4_path "$SERVICE"

# The state file and lock are optional for cleanup.  With the four canonical
# core artifacts intact, an absent state file must not make uninstall refuse.
rm -f -- "$FAKE_ROOT/var/lib/vpnkit-local-underlay-routing/routes.state"
rmdir -- "$R4_STATE_DIR" || fail_test "R4 optional state directory was not empty"
: >"$MOCK_LOG"
bash "$HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "R4 uninstall required absent optional state"
[[ ! -e "$CONFIG" && ! -e "$SERVICE" && ! -e "$DISPATCHER" && ! -e "$INSTALLED_HELPER" ]] \
  || fail_test "R4 successful cleanup left a core artifact"

# A failed file-only transaction may leave an empty canonical state directory;
# this is the one owned partial cleanup state that is safe without config.
mkdir -p -- "$R4_STATE_DIR"
chmod 0700 -- "$R4_STATE_DIR"
: >"$MOCK_LOG"
bash "$HELPER" uninstall --yes >/dev/null 2>&1 || fail_test "R4 empty state-directory cleanup failed"
[[ ! -e "$R4_STATE_DIR" ]] || fail_test "R4 empty state directory was not removed"

printf '%s\n' 'vpnkit-local-underlay-routing mock tests: PASS'
