#!/usr/bin/env bash
# Bounded, read-only same-host smoke for the local NetworkManager VPN.
set -Eeuo pipefail
umask 077

ROUTE_IP=${VPNKIT_LOCAL_SMOKE_ROUTE_IP:-1.1.1.1}
PING_IPS=${VPNKIT_LOCAL_SMOKE_PING_IPS:-1.1.1.1,8.8.8.8}
HOSTNAME=${VPNKIT_LOCAL_SMOKE_HOSTNAME:-example.com}
IPV6_ADDRESS=${VPNKIT_LOCAL_SMOKE_IPV6_ADDRESS:-2606:4700:4700::1111}
TIMEOUT=${VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS:-8}
DEVICE=${VPNKIT_LOCAL_SMOKE_DEVICE:-}

# OpenVPN's redirect-gateway def1 installs two broad IPv4 halves. Keep two
# independent, redacted route-policy anchors so a single host route cannot
# masquerade as full-tunnel coverage.
ROUTE_POLICY_LOWER_IP=1.1.1.1
ROUTE_POLICY_UPPER_IP=208.67.222.222

SMOKE_CHECK=inputs
fail() {
  printf 'host_smoke_failed_check=%s\n' "$SMOKE_CHECK" >&2
  printf '%s\n' "$1" >&2
  exit 1
}

valid_ipv4() {
  local value=${1:-} old_ifs=$IFS
  local -a octets
  [[ "$value" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  IFS=.
  read -r -a octets <<<"$value"
  IFS=$old_ifs
  [[ ${#octets[@]} -eq 4 ]] || return 1
  local octet
  for octet in "${octets[@]}"; do
    (( 10#$octet <= 255 )) || return 1
  done
}

canonical_ipv4() {
  local value=${1:-} old_ifs=$IFS
  local -a octets
  valid_ipv4 "$value" || return 1
  IFS=.
  read -r -a octets <<<"$value"
  IFS=$old_ifs
  printf '%d.%d.%d.%d\n' \
    "$((10#${octets[0]}))" "$((10#${octets[1]}))" \
    "$((10#${octets[2]}))" "$((10#${octets[3]}))"
}

valid_ipv6() {
  [[ "${1:-}" == *:* && "${1:-}" != *[[:space:]/]* ]] || return 1
  python3 - "$1" <<'PY' >/dev/null 2>&1
import ipaddress
import sys
ipaddress.IPv6Address(sys.argv[1])
PY
}

valid_device() {
  local device=$1
  [[ "$device" =~ ^tun[A-Za-z0-9_.-]{0,14}$ ]] || return 1
  (( ${#device} <= 15 )) || return 1
  [[ "$device" != ppp0 && "$device" != vpn0 ]]
}

[[ "$TIMEOUT" =~ ^[0-9]+$ ]] && (( TIMEOUT >= 1 && TIMEOUT <= 30 )) || fail 'VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS must be in 1..30'
valid_device "$DEVICE" || fail 'local smoke VPN device is missing or unsafe'
valid_ipv4 "$ROUTE_IP" || fail 'local smoke route address is invalid'
valid_ipv6 "$IPV6_ADDRESS" || fail 'local smoke IPv6 address is invalid'
[[ "$HOSTNAME" =~ ^[A-Za-z0-9.-]+$ && "$HOSTNAME" != .* && "$HOSTNAME" != *..* ]] || fail 'local smoke hostname is invalid'

command -v ip >/dev/null 2>&1 || fail 'ip is unavailable for local smoke'
command -v getent >/dev/null 2>&1 || fail 'getent is unavailable for local smoke'
command -v curl >/dev/null 2>&1 || fail 'curl is unavailable for local smoke'
command -v ping >/dev/null 2>&1 || fail 'ping is unavailable for local smoke'
command -v timeout >/dev/null 2>&1 || fail 'timeout is unavailable for local smoke'

parse_ping_addresses() {
  local old_ifs=$IFS
  local -a parsed
  [[ "$PING_IPS" =~ ^[0-9.,]+$ ]] || fail 'local smoke ping addresses are malformed'
  IFS=,
  read -r -a parsed <<<"$PING_IPS"
  IFS=$old_ifs
  [[ ${#parsed[@]} -gt 0 ]] || fail 'ping smoke has no addresses'
  local address
  for address in "${parsed[@]}"; do
    valid_ipv4 "$address" || fail 'local smoke ping address is invalid'
  done
  PING_ADDRESSES=("${parsed[@]}")
}

# NetworkManager can publish the owned UUID/IP/tun mapping just before it
# finishes installing redirect-gateway routes.  Treat only that narrow route
# transition as bounded readiness: no DNS, HTTPS, or ping probe runs until both
# full-tunnel anchors already resolve to the exact owned device.
route4_ready() {
  local target=$1 route4 route_primary route_cache= route_target route_device
  route4=$(ip -4 route get "$target" 2>/dev/null) || return 1
  [[ -n "$route4" ]] || return 1
  route_primary=${route4%%$'\n'*}
  if [[ "$route4" == *$'\n'* ]]; then
    route_cache=${route4#*$'\n'}
    [[ "$route_cache" != *$'\n'* && "${route_cache//[[:space:]]/}" == cache ]] || return 1
  fi
  [[ ! "$route_primary" =~ (^|[[:space:]])(unreachable|prohibit|blackhole|throw)([[:space:]]|$) ]] || return 1
  route_target=$(awk '{ print $1; exit }' <<<"$route_primary")
  [[ "$route_target" == "$target" ]] || return 1
  route_device=$(awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "dev") {
          if (seen || i == NF) bad=1
          else { seen=1; device=$(i + 1) }
        }
      }
    }
    END {
      if (bad || !seen || device == "") exit 1
      print device
    }
  ' <<<"$route_primary") || return 1
  [[ "$route_device" == "$DEVICE" ]]
}

wait_for_route_policy() {
  local deadline=$((SECONDS + TIMEOUT))
  while :; do
    if route4_ready "$ROUTE_POLICY_LOWER_IP" && route4_ready "$ROUTE_POLICY_UPPER_IP"; then
      return 0
    fi
    (( SECONDS >= deadline )) && fail 'IPv4 full-tunnel routes did not converge on the local VPN device'
    sleep 1
  done
}

# Check one IPv4 destination without printing the route itself. The returned
# destination and route shape are validated as well as the exact owned device;
# accepting only a matching `dev` token would otherwise allow malformed or
# unreachable mock/host output to become evidence.
route4_exact() {
  local target=$1 route4 route_primary route_cache= route_target route_device
  route4=$(ip -4 route get "$target" 2>/dev/null) || fail 'IPv4 route lookup failed'
  [[ -n "$route4" ]] || fail 'IPv4 route lookup returned malformed output'
  route_primary=${route4%%$'\n'*}
  if [[ "$route4" == *$'\n'* ]]; then
    route_cache=${route4#*$'\n'}
    # Current iproute2 emits a second, indented `cache` line. Accept only that
    # exact optional metadata line; multiple route records remain fail-closed.
    [[ "$route_cache" != *$'\n'* && "${route_cache//[[:space:]]/}" == cache ]] || \
      fail 'IPv4 route lookup returned malformed output'
  fi
  [[ "$route_primary" =~ (^|[[:space:]])(unreachable|prohibit|blackhole|throw)([[:space:]]|$) ]] && \
    fail 'IPv4 route lookup returned a blocked route'
  route_target=$(awk '{ print $1; exit }' <<<"$route_primary")
  [[ "$route_target" == "$target" ]] || fail 'IPv4 route lookup returned the wrong destination'
  route_device=$(awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "dev") {
          if (seen || i == NF) bad=1
          else { seen=1; device=$(i + 1) }
        }
      }
    }
    END {
      if (bad || !seen || device == "") exit 1
      print device
    }
  ' <<<"$route_primary") || fail 'IPv4 route lookup omitted a unique device'
  [[ "$route_device" == "$DEVICE" ]] || fail 'IPv4 route did not use the exact local VPN device'
}

resolve_hostname_ipv4() {
  local resolved line canonical
  local -a fields
  local -A seen=()
  HOSTNAME_ADDRESSES=()
  if ! resolved=$(timeout "$TIMEOUT" getent ahostsv4 "$HOSTNAME" 2>/dev/null); then
    fail 'DNS hostname smoke failed'
  fi
  [[ -n "${resolved//[[:space:]]/}" ]] || fail 'DNS hostname smoke returned no addresses'
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ [^[:space:]] ]] || fail 'DNS hostname smoke returned a malformed line'
    fields=()
    read -r -a fields <<<"$line"
    [[ ${#fields[@]} -gt 0 ]] || fail 'DNS hostname smoke returned a malformed line'
    valid_ipv4 "${fields[0]}" || fail 'DNS hostname smoke returned a malformed address'
    canonical=$(canonical_ipv4 "${fields[0]}") || fail 'DNS hostname smoke returned an invalid address'
    if [[ -z "${seen[$canonical]+present}" ]]; then
      seen["$canonical"]=1
      HOSTNAME_ADDRESSES+=("$canonical")
    fi
  done <<<"$resolved"
  [[ ${#HOSTNAME_ADDRESSES[@]} -gt 0 ]] || fail 'DNS hostname smoke returned no addresses'
}

PING_ADDRESSES=()
parse_ping_addresses

# Establish full-tunnel route-policy evidence before any application probe.
# These two redacted anchors cover the two public policy halves expected from
# redirect-gateway def1; neither may be inferred from ROUTE_IP alone. The
# readiness wait closes the NM UUID/IP/tun -> redirect-route publication race;
# the strict checks immediately afterward remain the acceptance evidence.
SMOKE_CHECK=route-policy
wait_for_route_policy
route4_exact "$ROUTE_POLICY_LOWER_IP"
route4_exact "$ROUTE_POLICY_UPPER_IP"

# DNS itself is an input to the hostname probe. Every unique, validated IPv4
# target returned by the resolver must use the exact tunnel before curl runs.
SMOKE_CHECK=dns
resolve_hostname_ipv4
SMOKE_CHECK=dns-route
for address in "${HOSTNAME_ADDRESSES[@]}"; do
  route4_exact "$address"
done

# The literal-IP HTTPS target gets its own route lookup immediately before the
# probe, rather than inheriting the route evidence from a different address.
SMOKE_CHECK=literal-ip-route
route4_exact "$ROUTE_IP"
SMOKE_CHECK=literal-ip-https
curl -q --noproxy '*' -4 -kfsS --connect-timeout "$TIMEOUT" --max-time "$TIMEOUT" -o /dev/null "https://$ROUTE_IP/" \
  >/dev/null 2>&1 || fail 'literal-IP HTTPS smoke failed'

SMOKE_CHECK=hostname-https
curl -q --noproxy '*' -4 -fsS --connect-timeout "$TIMEOUT" --max-time "$TIMEOUT" -o /dev/null "https://$HOSTNAME/" \
  >/dev/null 2>&1 || fail 'hostname HTTPS smoke failed'

for address in "${PING_ADDRESSES[@]}"; do
  # Keep the route check adjacent to the actual echo request so each ping
  # destination has fresh exact-device evidence.
  SMOKE_CHECK=ping-route
  route4_exact "$address"
  SMOKE_CHECK=ping
  ping -4 -c 1 -W "$TIMEOUT" "$address" >/dev/null 2>&1 || fail 'IPv4 ping smoke failed'
done

# The local profile is intentionally IPv4-only. Both route lookup and an
# actual IPv6 echo must fail; accepting either would permit an IPv6 leak.
SMOKE_CHECK=ipv6
route6=
route6_status=0
route6=$(ip -6 route get "$IPV6_ADDRESS" 2>/dev/null) || route6_status=$?
if [[ -n "$route6" ]]; then
  [[ "$route6" != *$'\n'* ]] || fail 'IPv6 route lookup returned malformed output'
  if ! [[ "$route6" =~ (^|[[:space:]])(unreachable|prohibit|blackhole)([[:space:]]|$) ]]; then
    fail 'IPv6 route is unexpectedly available'
  fi
elif (( route6_status == 0 )); then
  fail 'IPv6 route lookup returned malformed output'
fi
if ping -6 -c 1 -W "$TIMEOUT" "$IPV6_ADDRESS" >/dev/null 2>&1; then
  fail 'IPv6 ping unexpectedly succeeded'
fi

printf 'host_smoke=pass\n'
printf 'route=pass\n'
printf 'route_policy=pass\n'
printf 'route_policy_lower=pass\n'
printf 'route_policy_upper=pass\n'
printf 'route_dns=pass\n'
printf 'route_literal_ip=pass\n'
printf 'route_ping=pass\n'
printf 'dns=pass\n'
printf 'hostname_https=pass\n'
printf 'literal_ip_https=pass\n'
printf 'ipv4_ping=pass\n'
printf 'ipv6_block=pass\n'
