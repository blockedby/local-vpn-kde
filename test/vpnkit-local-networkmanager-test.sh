#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
helper="${VPNKIT_NM_TEST_HELPER:-$root/scripts/vpnkit/vpnkit-local-networkmanager.sh}"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/secrets/openvpn/client" "$tmp/profiles"
printf '%s\n' 'client' 'dev tun' 'proto udp' 'remote 127.0.0.1 21194' >"$tmp/secrets/openvpn/client/vpnkit-local.ovpn"
: >"$tmp/log"; : >"$tmp/connections"; : >"$tmp/active"
: >"$tmp/active-ip4"; : >"$tmp/ip-addresses"; : >"$tmp/pending-ip4-queries"
: >"$tmp/pending-route-queries"; : >"$tmp/ip.log"
printf '1\n' >"$tmp/import-number"
rm -f -- "$tmp/failure-marker"

cat >"$tmp/bin/nmcli" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'nmcli' >>"$MOCK_LOG"
printf ' %q' "$@" >>"$MOCK_LOG"
printf '\n' >>"$MOCK_LOG"

connections="$MOCK_CONNECTIONS"
active="$MOCK_ACTIVE"
profiles="$MOCK_PROFILES"
next_file="$MOCK_IMPORT_NUMBER"
active_ip4="$MOCK_ACTIVE_IP4"
pending_ip4_queries="$MOCK_PENDING_IP4_QUERIES"

# Keep the mock close to the real command shape while allowing the helper to
# own a bounded readiness wait after nmcli returns.
while [[ "${1:-}" == --wait || "${1:-}" == -w ]]; do
  shift 2
done

rewrite_name() {
  local uuid=$1 name=$2 line current_uuid current_name current_type
  local tmp_file; tmp_file=$(mktemp)
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    IFS=: read -r current_name current_uuid current_type _ <<<"$line"
    if [[ "$current_uuid" == "$uuid" ]]; then
      printf '%s:%s:%s\n' "$name" "$current_uuid" "$current_type" >>"$tmp_file"
    else
      printf '%s\n' "$line" >>"$tmp_file"
    fi
  done <"$connections"
  mv -- "$tmp_file" "$connections"

  local profile="$profiles/$uuid" data profile_line
  profile_line=$(<"$profile")
  data=$(printf '%s\n' "$profile_line" | cut -d: -f5-)
  printf '%s:%s:vpn:org.freedesktop.NetworkManager.openvpn:%s\n' "$name" "$uuid" "$data" >"$profile"
  if grep -Fq ":$uuid:" "$active"; then
    rewrite_active_name "$uuid" "$name"
  fi
}

rewrite_active_name() {
  local uuid=$1 name=$2 line current_uuid current_name current_type current_device
  local tmp_file; tmp_file=$(mktemp)
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    IFS=: read -r current_name current_uuid current_type current_device _ <<<"$line"
    if [[ "$current_uuid" == "$uuid" ]]; then
      printf '%s:%s:%s:%s\n' "$name" "$current_uuid" "$current_type" "$current_device" >>"$tmp_file"
    else
      printf '%s\n' "$line" >>"$tmp_file"
    fi
  done <"$active"
  mv -- "$tmp_file" "$active"
}

remove_uuid() {
  local uuid=$1 line current_name current_uuid current_type
  local tmp_file; tmp_file=$(mktemp)
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    IFS=: read -r current_name current_uuid current_type _ <<<"$line"
    [[ "$current_uuid" == "$uuid" ]] || printf '%s\n' "$line" >>"$tmp_file"
  done <"$connections"
  mv -- "$tmp_file" "$connections"
  rm -f -- "$profiles/$uuid"
  tmp_file=$(mktemp)
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    IFS=: read -r current_name current_uuid current_type _ <<<"$line"
    [[ "$current_uuid" == "$uuid" ]] || printf '%s\n' "$line" >>"$tmp_file"
  done <"$active"
  mv -- "$tmp_file" "$active"
}

fail_once() {
  local step=$1
  [[ "${MOCK_FAIL_STEP:-}" == "$step" && ! -e "$MOCK_FAILURE_MARKER" ]] || return 1
  : >"$MOCK_FAILURE_MARKER"
  return 0
}

case "${1:-}" in
  -t)
    # Real-shaped terse connection inventory and profile detail responses.
    if [[ "${2:-}" == "-f" && "${4:-}" == "connection" && "${5:-}" == "show" ]]; then
      fields=${3:-}
      if [[ "$fields" == "IP4.ADDRESS" && "${6:-}" == "uuid" ]]; then
        uuid=${7:-}
        if grep -Fq ":$uuid:" "$active"; then
          if [[ -s "$pending_ip4_queries" ]]; then
            pending=$(<"$pending_ip4_queries")
            if [[ "$pending" =~ ^[0-9]+$ && pending -gt 0 ]]; then
              printf '%s\n' "$((pending - 1))" >"$pending_ip4_queries"
              exit 0
            fi
          fi
          cat "$active_ip4"
        fi
        exit 0
      fi
      if [[ "${6:-}" == "--active" ]]; then
        [[ "$fields" == "NAME,UUID,TYPE,DEVICE" ]] && cat "$active"
        exit 0
      fi
      if [[ "${6:-}" == "uuid" ]]; then
        uuid=${7:-}
        [[ "$fields" == "connection.id,connection.uuid,connection.type,vpn.service-type,vpn.data" ]] || exit 1
        cat "$profiles/$uuid"
        exit 0
      fi
      [[ "$fields" == "NAME,UUID,TYPE" ]] && cat "$connections"
      exit 0
    fi
    exit 1
    ;;
  connection)
    case "${2:-}" in
      import)
        fail_once import && exit 1
        profile=${6:-}
        n=$(<"$next_file")
        uuid=$(printf '22222222-2222-4222-8222-%012d' "$n")
        printf '%s\n' "$((n + 1))" >"$next_file"
        port=$(awk '$1 == "remote" { print $3; exit }' "$profile")
        printf 'vpnkit-local:%s:vpn:org.freedesktop.NetworkManager.openvpn:connection-type=tls,dev=tun,proto=udp,remote=127.0.0.1\\:%s,remote-cert-tls=server\n' "$uuid" "$port" >"$profiles/$uuid"
        printf 'vpnkit-local:%s:vpn\n' "$uuid" >>"$connections"
        if fail_once post-import-no-output; then exit 1; fi
        printf '%s\n' "Connection 'vpnkit-local' ($uuid) successfully added."
        if fail_once post-import; then exit 1; fi
        ;;
      modify)
        uuid=${4:-}; name=; shift 4
        while [[ $# -gt 0 ]]; do
          if [[ "$1" == connection.id ]]; then name=${2:-}; break; fi
          shift
        done
        [[ -n "$uuid" && -n "$name" ]] || exit 1
        fail_once rename && exit 1
        rewrite_name "$uuid" "$name"
        ;;
      up)
        [[ "${3:-}" == uuid ]] || exit 1
        uuid=${4:-}
        grep -Fq ":$uuid:" "$connections" || exit 1
        # A real NM VPN row may expose its transport/base device (Meta), not
        # the tunnel. The helper must prove the latter from IP4 + kernel data.
        grep -Fq ":$uuid:" "$active" || awk -F: -v wanted="$uuid" '$2 == wanted { printf "%s:%s:%s:Meta\n", $1, $2, $3; found=1 } END { exit !found }' "$connections" >>"$active"
        ;;
      down)
        [[ "${3:-}" == uuid ]] || exit 1
        uuid=${4:-}
        tmp_file=$(mktemp)
        awk -F: -v wanted="$uuid" '$2 != wanted { print }' "$active" >"$tmp_file"
        mv -- "$tmp_file" "$active"
        ;;
      delete)
        [[ "${3:-}" == uuid ]] || exit 1
        fail_once cleanup && exit 1
        remove_uuid "${4:-}"
        ;;
      *) exit 1 ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/nmcli" "$helper"
cat >"$tmp/bin/ip" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'ip %s\n' "$*" >>"$MOCK_IP_LOG"
if [[ "$*" == '-o -4 addr' ]]; then
  cat "$MOCK_IP_ADDRS"
  exit 0
fi
if [[ "$*" == '-4 route get '* ]]; then
  target=${!#}
  device=$(awk '$3 == "inet" { value=$2; sub(/:$/, "", value); print value; exit }' "$MOCK_IP_ADDRS")
  [[ -n "$device" ]] || device=tun0
  pending=0
  [[ -s "$MOCK_PENDING_ROUTE_QUERIES" ]] && pending=$(<"$MOCK_PENDING_ROUTE_QUERIES")
  [[ "$pending" =~ ^[0-9]+$ ]] || exit 2
  if (( pending > 0 )); then
    printf '%s\n' "$((pending - 1))" >"$MOCK_PENDING_ROUTE_QUERIES"
    device=uplink0
  fi
  printf '%s dev %s src 10.89.0.2\n' "$target" "$device"
  exit 0
fi
exit 1
EOF
chmod +x "$tmp/bin/ip"
cat >"$tmp/bin/mv" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
target=${!#}
if [[ "${MOCK_FAIL_STEP:-}" == state-commit && "$target" == "$MOCK_STATE_FILE" && ! -e "$MOCK_FAILURE_MARKER" ]]; then
  : >"$MOCK_FAILURE_MARKER"
  exit 1
fi
exec /bin/mv "$@"
EOF
chmod +x "$tmp/bin/mv"
export PATH="$tmp/bin:$PATH" MOCK_LOG="$tmp/log" MOCK_CONNECTIONS="$tmp/connections" MOCK_ACTIVE="$tmp/active" MOCK_PROFILES="$tmp/profiles" MOCK_IMPORT_NUMBER="$tmp/import-number" MOCK_FAILURE_MARKER="$tmp/failure-marker" MOCK_STATE_FILE="$tmp/secrets/state/networkmanager-state" MOCK_ACTIVE_IP4="$tmp/active-ip4" MOCK_IP_ADDRS="$tmp/ip-addresses" MOCK_PENDING_IP4_QUERIES="$tmp/pending-ip4-queries" MOCK_PENDING_ROUTE_QUERIES="$tmp/pending-route-queries" MOCK_IP_LOG="$tmp/ip.log" VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS=3 VPNKIT_LOCAL_SECRETS_DIR="$tmp/secrets" VPNKIT_LOCAL_TEST_FIXTURE=1

bash -n "$helper"
plan=$($helper plan)
grep -q 'mutation=none' <<<"$plan"
grep -q 'profile=ready' <<<"$plan"
grep -q 'ownership=missing' <<<"$plan"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
if $helper import >/dev/null 2>&1; then echo 'ungated import succeeded' >&2; exit 1; fi

# An import frontend can fail after it has created a profile.  The helper must
# identify and remove only that new UUID even when no old state exists.
rm -f -- "$MOCK_FAILURE_MARKER"
if MOCK_FAIL_STEP=post-import $helper import --yes >/dev/null 2>&1; then
  echo 'post-import failure unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -s "$tmp/connections" && ! -e "$tmp/secrets/state/networkmanager-state" ]]
[[ ! -e "$tmp/profiles/22222222-2222-4222-8222-000000000001" ]]
unset MOCK_FAIL_STEP

$helper import --yes >/dev/null
owned_uuid=$(<"$tmp/secrets/state/networkmanager-uuid")
[[ "$owned_uuid" =~ ^[0-9a-f-]{36}$ ]]
[[ -s "$tmp/secrets/state/networkmanager-profile-fingerprint" ]]
[[ -s "$tmp/secrets/state/networkmanager-state" ]]
read -r state_uuid state_fingerprint <"$tmp/secrets/state/networkmanager-state"
[[ "$state_uuid" == "$owned_uuid" && "$state_fingerprint" =~ ^[0-9a-f]{64}$ ]]
old_profile_snapshot=$(<"$tmp/profiles/$owned_uuid")
# The read-only verify action proves the exact owned UUID -> active IP4 ->
# kernel tun mapping used as the OpenVPN handshake readiness gate.
printf 'vpnkit-local:%s:vpn:Meta\n' "$owned_uuid" >"$tmp/active"
printf '10.89.0.2/24\n' >"$tmp/active-ip4"
printf '7 tun7 inet 10.89.0.2/24 scope global\n' >"$tmp/ip-addresses"
verify_output=$($helper verify)
grep -Fxq 'networkmanager_mapping=pass' <<<"$verify_output"
grep -Fxq 'owned_uuid_ip4_tun=pass' <<<"$verify_output"
grep -Fxq 'openvpn_handshake=pass' <<<"$verify_output"
grep -Fxq 'device=tun7' <<<"$verify_output"
grep -q "connection modify uuid $owned_uuid" "$tmp/log"
grep -q "connection.id vpnkit-local" "$tmp/log"
! grep -Eq 'connection (up|down|delete) id ' "$tmp/log"

# REV-014: a matching UUID/fingerprint and localhost OpenVPN structure do not
# authorize a profile whose NetworkManager name is foreign.  Reject every
# owned-capability path before legacy migration or NM mutation.
foreign_name=foreign-name
canonical_connection_line=$(grep -F ":$owned_uuid:" "$tmp/connections")
canonical_profile_line=$(<"$tmp/profiles/$owned_uuid")
foreign_connection_line=${canonical_connection_line/vpnkit-local:/$foreign_name:}
foreign_profile_line=${canonical_profile_line/vpnkit-local:/$foreign_name:}
printf '%s\n' "$foreign_connection_line" >"$tmp/connections"
printf '%s\n' "$foreign_profile_line" >"$tmp/profiles/$owned_uuid"

assert_foreign_name_rejected() {
  local action=$1
  shift
  : >"$tmp/log"
  if "$helper" "$action" "$@" >/dev/null 2>&1; then
    echo "foreign-name $action unexpectedly succeeded" >&2
    exit 1
  fi
  ! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
}

# With only the legacy pair present, the foreign profile must be rejected
# before the atomic state file is created.
rm -f -- "$tmp/secrets/state/networkmanager-state"
assert_foreign_name_rejected plan
assert_foreign_name_rejected status
assert_foreign_name_rejected import --yes
assert_foreign_name_rejected connect --yes
assert_foreign_name_rejected disconnect --yes
assert_foreign_name_rejected remove --yes
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]

# Restore the canonical name and prove the valid legacy migration still works,
# then repeat the no-mutation check with an already-atomic capability.
printf '%s\n' "$canonical_connection_line" >"$tmp/connections"
printf '%s\n' "$canonical_profile_line" >"$tmp/profiles/$owned_uuid"
$helper status >/dev/null
[[ -s "$tmp/secrets/state/networkmanager-state" ]]
cp -- "$tmp/secrets/state/networkmanager-state" "$tmp/foreign-name-atomic-state.snapshot"
printf '%s\n' "$foreign_connection_line" >"$tmp/connections"
printf '%s\n' "$foreign_profile_line" >"$tmp/profiles/$owned_uuid"
: >"$tmp/log"
atomic_plan_output=$($helper plan)
grep -Fxq 'ownership=invalid' <<<"$atomic_plan_output"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
: >"$tmp/log"
atomic_status_output=$($helper status)
grep -Fxq 'ownership=invalid' <<<"$atomic_status_output"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
assert_foreign_name_rejected import --yes
assert_foreign_name_rejected connect --yes
assert_foreign_name_rejected disconnect --yes
assert_foreign_name_rejected remove --yes
cmp -s "$tmp/foreign-name-atomic-state.snapshot" "$tmp/secrets/state/networkmanager-state"
printf '%s\n' "$canonical_connection_line" >"$tmp/connections"
printf '%s\n' "$canonical_profile_line" >"$tmp/profiles/$owned_uuid"

# REV-014: a valid legacy-only pair is promoted before either import or status
# can claim ownership.  Neither path should touch NetworkManager on no-drift.
cp -- "$tmp/secrets/state/networkmanager-uuid" "$tmp/legacy-uuid.snapshot"
cp -- "$tmp/secrets/state/networkmanager-profile-fingerprint" "$tmp/legacy-fingerprint.snapshot"
rm -f -- "$tmp/secrets/state/networkmanager-state"
: >"$tmp/log"
legacy_import_output=$($helper import --yes)
grep -Fxq 'networkmanager_import=already-configured' <<<"$legacy_import_output"
[[ -s "$tmp/secrets/state/networkmanager-state" ]]
cmp -s "$tmp/legacy-uuid.snapshot" "$tmp/secrets/state/networkmanager-uuid"
cmp -s "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-state"
: >"$tmp/log"
legacy_status_output=$($helper status)
grep -Fxq 'configured=yes' <<<"$legacy_status_output"
grep -Fxq 'ownership=owned' <<<"$legacy_status_output"
[[ -s "$tmp/secrets/state/networkmanager-state" ]]
cmp -s "$tmp/legacy-uuid.snapshot" "$tmp/secrets/state/networkmanager-uuid"
cmp -s "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"

# Shell-specific mv fault injection is retained for the legacy adapter. The
# native writer's equivalent is TestNMLegacyMigrationFailurePreservesCapability.
if [[ -z "${VPNKIT_NM_TEST_HELPER:-}" ]]; then
# An injected atomic-commit failure must not consume or rewrite the valid
# legacy pair, and neither import nor status may report success afterward.
rm -f -- "$tmp/secrets/state/networkmanager-state" "$MOCK_FAILURE_MARKER"
: >"$tmp/log"
if legacy_failure_output=$(MOCK_FAIL_STEP=state-commit "$helper" import --yes 2>&1); then
  echo 'legacy migration unexpectedly succeeded after state commit failure' >&2
  exit 1
fi
grep -Eq 'migration|preserved|ownership' <<<"$legacy_failure_output"
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]
cmp -s "$tmp/legacy-uuid.snapshot" "$tmp/secrets/state/networkmanager-uuid"
cmp -s "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$MOCK_FAILURE_MARKER"
: >"$tmp/log"
if legacy_failure_output=$(MOCK_FAIL_STEP=state-commit "$helper" status 2>&1); then
  echo 'legacy status unexpectedly succeeded after state commit failure' >&2
  exit 1
fi
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]
cmp -s "$tmp/legacy-uuid.snapshot" "$tmp/secrets/state/networkmanager-uuid"
cmp -s "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$MOCK_FAILURE_MARKER"
$helper status >/dev/null

fi

# Partial and drifted legacy pairs are rejected before the import/refresh path.
rm -f -- "$tmp/secrets/state/networkmanager-state" "$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then
  echo 'partial legacy pair was accepted' >&2
  exit 1
fi
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
cp -- "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
printf '%064d\n' 0 >"$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then
  echo 'mismatched legacy fingerprint was refreshed' >&2
  exit 1
fi
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]
cmp -s "$tmp/legacy-uuid.snapshot" "$tmp/secrets/state/networkmanager-uuid"
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
cp -- "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
$helper import --yes >/dev/null

# Legacy links are not capabilities.  Reject them before any NM mutation.
rm -f -- "$tmp/secrets/state/networkmanager-state"
mv -- "$tmp/secrets/state/networkmanager-uuid" "$tmp/legacy-uuid.link-target"
ln -s -- "$tmp/legacy-uuid.link-target" "$tmp/secrets/state/networkmanager-uuid"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then
  echo 'legacy symlink was accepted' >&2
  exit 1
fi
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-uuid"
mv -- "$tmp/legacy-uuid.link-target" "$tmp/secrets/state/networkmanager-uuid"

mv -- "$tmp/secrets/state/networkmanager-uuid" "$tmp/legacy-uuid.hardlink-target"
ln -- "$tmp/legacy-uuid.hardlink-target" "$tmp/secrets/state/networkmanager-uuid"
: >"$tmp/log"
if $helper status >/dev/null 2>&1; then
  echo 'legacy hardlink was accepted' >&2
  exit 1
fi
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-uuid"
mv -- "$tmp/legacy-uuid.hardlink-target" "$tmp/secrets/state/networkmanager-uuid"
mv -- "$tmp/secrets/state/networkmanager-profile-fingerprint" "$tmp/legacy-fingerprint.hardlink-target"
ln -- "$tmp/legacy-fingerprint.hardlink-target" "$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then
  echo 'legacy fingerprint hardlink was accepted' >&2
  exit 1
fi
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-profile-fingerprint"
mv -- "$tmp/legacy-fingerprint.hardlink-target" "$tmp/secrets/state/networkmanager-profile-fingerprint"
$helper import --yes >/dev/null

# The authoritative state target is also rejected when redirected or shared.
cp -- "$tmp/secrets/state/networkmanager-state" "$tmp/state-authority.snapshot"
rm -f -- "$tmp/secrets/state/networkmanager-state"
ln -s -- "$tmp/state-authority.snapshot" "$tmp/secrets/state/networkmanager-state"
: >"$tmp/log"
if $helper status >/dev/null 2>&1; then
  echo 'authoritative state symlink was accepted' >&2
  exit 1
fi
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-state"
cp -- "$tmp/state-authority.snapshot" "$tmp/secrets/state/networkmanager-state"
rm -f -- "$tmp/secrets/state/networkmanager-state"
cp -- "$tmp/state-authority.snapshot" "$tmp/state-authority.hardlink-target"
ln -- "$tmp/state-authority.hardlink-target" "$tmp/secrets/state/networkmanager-state"
: >"$tmp/log"
if $helper status >/dev/null 2>&1; then
  echo 'authoritative state hardlink was accepted' >&2
  exit 1
fi
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-state"
cp -- "$tmp/state-authority.snapshot" "$tmp/secrets/state/networkmanager-state"

# The active NM row deliberately reports the transport/base interface Meta.
# The owned active UUID's IP4 data and the kernel address table identify tun0.
printf 'IP4.ADDRESS:10.89.0.2/24\n' >"$tmp/active-ip4"
printf '7: tun0    inet 10.89.0.2 peer 10.89.0.1/32 scope global tun0\n' >"$tmp/ip-addresses"
printf '1\n' >"$tmp/pending-ip4-queries"
# Regression: an assigned owned IP/tun mapping may precede NetworkManager's
# redirect-gateway routes. connect must remain pending until both IPv4 halves
# use that exact tun, otherwise lifecycle's immediate host smoke rolls back.
printf '1\n' >"$tmp/pending-route-queries"
: >"$tmp/ip.log"
connect_output=$($helper connect --yes)
grep -Fxq 'device=tun0' <<<"$connect_output"
grep -Fq 'ip -4 route get 1.1.1.1' "$tmp/ip.log"
grep -Fq 'ip -4 route get 8.8.8.8' "$tmp/ip.log"
[[ ! -s "$tmp/pending-route-queries" || $(<"$tmp/pending-route-queries") == 0 ]]
$helper status | grep -q 'configured=yes'
$helper status | grep -q 'active=yes'
$helper status | grep -Fxq 'device=tun0'
grep -q "connection up uuid $owned_uuid" "$tmp/log"
grep -q "IP4.ADDRESS connection show uuid $owned_uuid" "$tmp/log"

# No-IP activation is bounded rather than reported as an unsafe device forever.
: >"$tmp/active-ip4"
if timeout_output=$($helper connect --yes 2>&1); then
  echo 'no-IP activation unexpectedly succeeded' >&2
  exit 1
fi
grep -Fq 'timed out before the owned tunnel and full-tunnel routes became ready' <<<"$timeout_output"
printf 'IP4.ADDRESS:10.89.0.2/24\n' >"$tmp/active-ip4"

# A runtime address that does not exist on the kernel table, or that exists on
# more than one tunnel, is not an arbitrary-interface fallback.
printf '7: tun0    inet 10.89.0.3/24 scope global tun0\n' >"$tmp/ip-addresses"
if $helper status >/dev/null 2>&1; then
  echo 'owned IP mapping mismatch was accepted' >&2
  exit 1
fi
printf '7: tun0    inet 10.89.0.2/24 scope global tun0\n8: tun1    inet 10.89.0.2/24 scope global tun1\n' >"$tmp/ip-addresses"
if $helper status >/dev/null 2>&1; then
  echo 'duplicate owned IP mapping was accepted' >&2
  exit 1
fi

# Disconnect must still stop the exact owned UUID when the safe tunnel proof
# is unavailable. Foreign/work VPN rows are left untouched.
: >"$tmp/active-ip4"
: >"$tmp/ip-addresses"
printf 'vpnkit-local:%s:vpn:Meta\nwork-vpn:99999999-9999-4999-8999-999999999999:vpn:Meta\n' "$owned_uuid" >"$tmp/active"
: >"$tmp/log"
$helper disconnect --yes >/dev/null
grep -q "connection down uuid $owned_uuid" "$tmp/log"
grep -q 'work-vpn:99999999-9999-4999-8999-999999999999:vpn:Meta' "$tmp/active"

# Every refresh failure point must preserve the old UUID, its profile, and a
# complete ownership capability.  The mock fails once at the named step so
# compensating operations remain observable.
printf '%s\n' 'client' 'dev tun' 'proto udp' 'remote localhost 21195' >"$tmp/secrets/openvpn/client/vpnkit-local.ovpn"
run_refresh_failure() {
  local step=$1 output
  rm -f -- "$MOCK_FAILURE_MARKER"
  : >"$tmp/log"
  if output=$(MOCK_FAIL_STEP="$step" "$helper" import --yes 2>&1); then
    echo "refresh unexpectedly succeeded after $step" >&2
    exit 1
  fi
  [[ "$(<"$tmp/secrets/state/networkmanager-uuid")" == "$owned_uuid" ]]
  [[ "$(<"$tmp/secrets/state/networkmanager-profile-fingerprint")" == "$state_fingerprint" ]]
  [[ -s "$tmp/secrets/state/networkmanager-state" ]]
  read -r rollback_uuid rollback_fingerprint <"$tmp/secrets/state/networkmanager-state"
  [[ "$rollback_uuid" == "$owned_uuid" && "$rollback_fingerprint" == "$state_fingerprint" ]]
  grep -q "^vpnkit-local:$owned_uuid:vpn$" "$tmp/connections"
  [[ -f "$tmp/profiles/$owned_uuid" ]]
  [[ "$(<"$tmp/profiles/$owned_uuid")" == "$old_profile_snapshot" ]]
  [[ $(grep -c '^vpnkit-local:' "$tmp/connections") == 1 ]]
  if [[ "$step" != import ]]; then
    grep -Eq 'restored|rollback' <<<"$output"
  fi
  [[ "$($helper status | awk -F= '$1 == "ownership" { print $2 }')" == drift ]]
}
run_refresh_failure import
run_refresh_failure post-import
run_refresh_failure post-import-no-output
run_refresh_failure rename
if [[ -z "${VPNKIT_NM_TEST_HELPER:-}" ]]; then run_refresh_failure state-commit; fi
run_refresh_failure cleanup
unset MOCK_FAIL_STEP
rm -f -- "$MOCK_FAILURE_MARKER"
printf '%s\n' 'client' 'dev tun' 'proto udp' 'remote 127.0.0.1 21194' >"$tmp/secrets/openvpn/client/vpnkit-local.ovpn"

: >"$tmp/active-ip4"
: >"$tmp/ip-addresses"
printf 'vpnkit-local:%s:vpn:Meta\n' "$owned_uuid" >"$tmp/active"
: >"$tmp/log"
$helper remove --yes >/dev/null
grep -q "connection down uuid $owned_uuid" "$tmp/log"
grep -q "connection delete uuid $owned_uuid" "$tmp/log"
[[ ! -e "$tmp/secrets/state/networkmanager-state" && ! -e "$tmp/secrets/state/networkmanager-uuid" && ! -e "$tmp/secrets/state/networkmanager-profile-fingerprint" ]]

# A real-looking, same-named foreign profile is a collision, not an import target.
foreign_uuid=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
printf 'vpnkit-local:%s:802-3-ethernet\n' "$foreign_uuid" >"$tmp/connections"
printf 'vpnkit-local:%s:802-3-ethernet:--:not-a-vpn\n' "$foreign_uuid" >"$tmp/profiles/$foreign_uuid"
printf '%s\n' "$foreign_uuid" >"$tmp/secrets/state/networkmanager-uuid"
cp -- "$tmp/legacy-fingerprint.snapshot" "$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then
  echo 'foreign legacy pair was adopted' >&2
  exit 1
fi
[[ ! -e "$tmp/secrets/state/networkmanager-state" ]]
! grep -Eq 'connection (import|modify|up|down|delete)' "$tmp/log"
rm -f -- "$tmp/secrets/state/networkmanager-uuid" "$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then echo 'foreign collision was adopted' >&2; exit 1; fi
! grep -q 'connection import' "$tmp/log"
! grep -q 'connection modify\|connection delete' "$tmp/log"
foreign_uuid_two=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
printf 'vpnkit-local:%s:vpn\n' "$foreign_uuid_two" >>"$tmp/connections"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then echo 'duplicate collision was adopted' >&2; exit 1; fi
! grep -q 'connection import\|connection modify\|connection delete' "$tmp/log"
printf 'vpnkit-local:%s:802-3-ethernet\n' "$foreign_uuid" >"$tmp/connections"

# A stale owned UUID plus a same-named profile is also refused without mutation.
printf '%s\n' "$foreign_uuid" >"$tmp/secrets/state/networkmanager-uuid"
printf '%064d\n' 0 >"$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then echo 'stale collision was adopted' >&2; exit 1; fi
! grep -q 'connection import\|connection modify\|connection delete' "$tmp/log"
rm -f "$tmp/secrets/state/networkmanager-uuid" "$tmp/secrets/state/networkmanager-profile-fingerprint"
: >"$tmp/connections"
rm -f "$tmp/profiles/$foreign_uuid"

# Changing the owned local profile refreshes through a new validated UUID and
# deletes only the previously owned UUID.
$helper import --yes >/dev/null
old_uuid=$(<"$tmp/secrets/state/networkmanager-uuid")
printf '%s\n' 'client' 'dev tun' 'proto udp' 'remote localhost 21195' >"$tmp/secrets/openvpn/client/vpnkit-local.ovpn"
: >"$tmp/log"
if $helper connect --yes >/dev/null 2>&1; then echo 'fingerprint-drifted profile was connected' >&2; exit 1; fi
! grep -q 'connection up' "$tmp/log"
$helper import --yes >/dev/null
new_uuid=$(<"$tmp/secrets/state/networkmanager-uuid")
[[ "$new_uuid" != "$old_uuid" ]]
grep -q "connection delete uuid $old_uuid" "$tmp/log"
grep -q "connection.id vpnkit-local" "$tmp/log"
! grep -q "connection delete uuid $new_uuid" "$tmp/log"
grep -q "vpnkit-local:$new_uuid:vpn" "$tmp/connections"
! grep -q "vpnkit-local:$old_uuid:" "$tmp/connections"

# Source identity validation rejects a non-localhost OpenVPN profile before import.
printf '%s\n' 'client' 'dev tun' 'proto udp' 'remote 192.0.2.10 21194' >"$tmp/secrets/openvpn/client/vpnkit-local.ovpn"
: >"$tmp/log"
if $helper import --yes >/dev/null 2>&1; then echo 'non-localhost profile imported' >&2; exit 1; fi
! grep -q 'connection import' "$tmp/log"
: >"$tmp/log"
if $helper connect --yes >/dev/null 2>&1; then echo 'stale owned profile was connected' >&2; exit 1; fi
! grep -q 'connection up' "$tmp/log"

# Direct helper invocation enforces the same root boundary before filesystem or
# NetworkManager mutation.
unsafe_root="$tmp/arbitrary-secret-root"
rm -rf -- "$unsafe_root"
: >"$tmp/log"
if VPNKIT_LOCAL_TEST_FIXTURE=0 VPNKIT_LOCAL_SECRETS_DIR="$unsafe_root" "$helper" import --yes >/dev/null 2>&1; then
  echo 'arbitrary direct-helper root was accepted' >&2
  exit 1
fi
[[ ! -e "$unsafe_root" ]]
[[ ! -s "$tmp/log" ]]

production_root="$tmp/production-vpn-secrets"
rm -rf -- "$production_root"
: >"$tmp/log"
if VPNKIT_LOCAL_SECRETS_DIR="$production_root" "$helper" import --yes >/dev/null 2>&1; then
  echo 'production-like direct-helper root was accepted' >&2
  exit 1
fi
[[ ! -e "$production_root" ]]
[[ ! -s "$tmp/log" ]]

symlink_root="$tmp/symlink-secret-root"
ln -s -- "$tmp/secrets" "$symlink_root"
: >"$tmp/log"
if VPNKIT_LOCAL_SECRETS_DIR="$symlink_root" "$helper" import --yes >/dev/null 2>&1; then
  echo 'symlinked direct-helper root was accepted' >&2
  exit 1
fi
[[ ! -s "$tmp/log" ]]
rm -f -- "$symlink_root"

! grep -Eiq 'sudo|docker|systemctl|ip route|ip rule' "$tmp/log"
printf 'vpnkit local NetworkManager ownership tests passed\n'
