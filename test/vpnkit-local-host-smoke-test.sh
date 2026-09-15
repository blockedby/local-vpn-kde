#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
script="$root/scripts/vpnkit/vpnkit-local-host-smoke.sh"
(cd "$root" && go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-host-smoke.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

cat >"$tmp/bin/ip" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
target=${!#}
printf 'ip %s\n' "$*" >>"${MOCK_IP_LOG:?}"
if [[ "$*" == '-4 route get '* ]]; then
  device=${MOCK_ROUTE_DEVICE:-tun7}
  case "${MOCK_ROUTE_MODE:-all-tun}:$target" in
    split:1.1.1.1) ;;
    split:*) device=uplink0 ;;
    dns-uplink:93.184.216.34) device=uplink0 ;;
    lower-half-only:208.67.222.222) device=uplink0 ;;
    converging:1.1.1.1|converging:208.67.222.222)
      pending_file=${MOCK_ROUTE_PENDING_FILE:?}
      pending=0
      [[ -s "$pending_file" ]] && pending=$(<"$pending_file")
      [[ "$pending" =~ ^[0-9]+$ ]] || exit 4
      if (( pending > 0 )); then
        printf '%s\n' "$((pending - 1))" >"$pending_file"
        device=uplink0
      fi
      ;;
    *) ;;
  esac
  printf '%s\n' "$target dev $device src 10.89.0.2"
  case "${MOCK_ROUTE_TRAILER:-none}" in
    cache) printf '    cache\n' ;;
    extra-route) printf '%s\n' "$target dev $device src 10.89.0.3" ;;
    none) ;;
    *) exit 4 ;;
  esac
  exit 0
fi
if [[ "$*" == '-6 route get '* ]]; then
  if [[ "${MOCK_IPV6_ROUTE_STALL:-0}" == 1 ]]; then exec sleep 60; fi
  case "${MOCK_IPV6_ROUTE_MODE:-none}" in
    reachable) printf '%s dev uplink0 src 2001:db8::2\n' "$target"; exit 0 ;;
    reachable-error) printf '%s dev uplink0 src 2001:db8::2\n' "$target"; exit 2 ;;
    blocked) printf 'unreachable %s\n' "$target"; exit 2 ;;
    *) exit 2 ;;
  esac
fi
exit 1
EOF
cat >"$tmp/bin/getent" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'getent %s\n' "$*" >>"${MOCK_IP_LOG:?}"
case "${MOCK_DNS_MODE:-one}" in
  one) printf '%s\n' '93.184.216.34 STREAM example.com' ;;
  duplicate)
    printf '%s\n' '93.184.216.34 STREAM example.com'
    printf '%s\n' '93.184.216.34 DGRAM example.com'
    printf '%s\n' '093.184.216.034 RAW example.com'
    ;;
  malformed) printf '%s\n' 'not-an-ip STREAM example.com' ;;
  empty) exit 0 ;;
  *) exit 3 ;;
esac
EOF
cat >"$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'curl %s\n' "$*" >>"${MOCK_PROBE_LOG:?}"
# Ignore per-user curl config and proxies: they can bypass the tested route.
[[ "${1:-}" == -q && "${2:-}" == --noproxy && "${3:-}" == '*' ]] || exit 90
# The hostname probe must use the exact addresses whose routes were checked.
if [[ "${!#}" == https://example.com/ ]]; then
  [[ " $* " == *' --resolve example.com:443:93.184.216.34 '* ]] || exit 91
fi
exit "${MOCK_CURL_STATUS:-0}"
EOF
cat >"$tmp/bin/ping" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'ping %s\n' "$*" >>"${MOCK_PROBE_LOG:?}"
case " $* " in
  *' -6 '*) exit "${MOCK_PING6_STATUS:-1}" ;;
  *) exit "${MOCK_PING_STATUS:-0}" ;;
esac
EOF
chmod +x "$tmp/bin"/*
export PATH="$tmp/bin:$PATH"
export MOCK_IP_LOG="$tmp/ip.log" MOCK_PROBE_LOG="$tmp/probes.log" MOCK_ROUTE_PENDING_FILE="$tmp/route-pending"
: >"$MOCK_IP_LOG"
: >"$MOCK_PROBE_LOG"
: >"$MOCK_ROUTE_PENDING_FILE"

bash -n "$script"
if grep -Eiq '(^|[^[:alnum:]_])(docker|nmcli|sudo)([^[:alnum:]_]|$)' "$script"; then
  echo 'host smoke contains a forbidden mutating integration' >&2
  exit 1
fi
if ! output=$(VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" 2>&1); then
  echo "$output" >&2
  exit 1
fi
for marker in \
  host_smoke=pass route=pass route_policy=pass \
  route_policy_lower=pass route_policy_upper=pass route_dns=pass \
  route_literal_ip=pass route_ping=pass dns=pass \
  literal_ip_https=pass ipv4_ping=pass ipv6_block=pass; do
  grep -Fxq "$marker" <<<"$output"
done
# The successful run must have checked the two policy anchors, the resolved
# hostname target, and both configured ping destinations before probing.
grep -Fq 'ip -4 route get 1.1.1.1' "$MOCK_IP_LOG"
grep -Fq 'ip -4 route get 208.67.222.222' "$MOCK_IP_LOG"
grep -Fq 'ip -4 route get 8.8.8.8' "$MOCK_IP_LOG"
grep -Fq 'ip -4 route get 93.184.216.34' "$MOCK_IP_LOG"
grep -Fq 'ping -4 -c 1 -W 2 8.8.8.8' "$MOCK_PROBE_LOG"

# Current iproute2 may append one indented `cache` metadata line. It is valid,
# while a second route record must remain fail-closed.
if ! MOCK_ROUTE_TRAILER=cache VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >/dev/null 2>&1; then
  echo 'host smoke rejected the bounded iproute2 cache trailer' >&2
  exit 1
fi
if MOCK_ROUTE_TRAILER=extra-route VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >/dev/null 2>&1; then
  echo 'host smoke accepted multiple IPv4 route records' >&2
  exit 1
fi

# Resolver duplicates, including a zero-padded spelling of the same address,
# are canonicalized and routed only once.
: >"$MOCK_IP_LOG"
: >"$MOCK_PROBE_LOG"
if ! output=$(MOCK_DNS_MODE=duplicate VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" 2>&1); then
  echo "$output" >&2
  exit 1
fi
[[ $(grep -Fc 'ip -4 route get 93.184.216.34' "$MOCK_IP_LOG") -eq 1 ]]

# Regression: NM can expose the owned UUID/IP/tun mapping one scheduler turn
# before redirect-gateway routes move to that tun. The smoke must wait for the
# exact two-anchor route contract instead of rolling the just-started stack
# back on the first uplink result. No application probe may run while waiting.
: >"$MOCK_IP_LOG"
: >"$MOCK_PROBE_LOG"
printf '1\n' >"$MOCK_ROUTE_PENDING_FILE"
if ! output=$(MOCK_ROUTE_MODE=converging VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" 2>&1); then
  echo "$output" >&2
  echo 'host smoke did not tolerate bounded NetworkManager route convergence' >&2
  exit 1
fi
grep -Fxq 'host_smoke=pass' <<<"$output"
[[ $(grep -Fc 'ip -4 route get 1.1.1.1' "$MOCK_IP_LOG") -ge 2 ]]

# A split route that only covers the first policy anchor must fail before any
# curl or ping is attempted; this is the REV-002 leak reproducer.
: >"$MOCK_IP_LOG"
: >"$MOCK_PROBE_LOG"
if MOCK_ROUTE_MODE=split VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=1 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/split.out" 2>&1; then
  echo 'host smoke accepted a split-route leak' >&2
  exit 1
fi
[[ ! -s "$MOCK_PROBE_LOG" ]]
! grep -Fq 'getent' "$MOCK_IP_LOG"
grep -Fq 'ip -4 route get 1.1.1.1' "$MOCK_IP_LOG"
grep -Fq 'ip -4 route get 208.67.222.222' "$MOCK_IP_LOG"

# Both former anchors (1.1.1.1 and 8.8.8.8) are in 0.0.0.0/1. A missing
# 128.0.0.0/1 redirect route must not pass just because the lower half works.
: >"$MOCK_PROBE_LOG"
if MOCK_ROUTE_MODE=lower-half-only VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=1 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/upper-half.out" 2>&1; then
  echo 'host smoke accepted a missing upper-half full-tunnel route' >&2
  exit 1
fi
[[ ! -s "$MOCK_PROBE_LOG" ]]
grep -Fxq 'host_smoke_failed_check=route-policy' "$tmp/upper-half.out"

# DNS failures and a DNS target routed to the uplink must also fail before
# either HTTPS probe or ping.
for dns_mode in malformed empty; do
  : >"$MOCK_PROBE_LOG"
  if MOCK_DNS_MODE="$dns_mode" VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
      VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/dns-$dns_mode.out" 2>&1; then
    echo "host smoke accepted DNS mode: $dns_mode" >&2
    exit 1
  fi
  [[ ! -s "$MOCK_PROBE_LOG" ]]
done
grep -Fq 'DNS hostname smoke returned no addresses' "$tmp/dns-empty.out"
: >"$MOCK_PROBE_LOG"
if MOCK_ROUTE_MODE=dns-uplink VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/dns-uplink.out" 2>&1; then
  echo 'host smoke accepted an uplink-routed DNS target' >&2
  exit 1
fi
[[ ! -s "$MOCK_PROBE_LOG" ]]
grep -Fq 'IPv4 route did not use the exact local VPN device' "$tmp/dns-uplink.out"

# IPv6 remains fail-closed even if the route command emits a reachable route
# while returning an error, or if the echo command unexpectedly succeeds.
for ipv6_mode in reachable reachable-error; do
  if MOCK_IPV6_ROUTE_MODE="$ipv6_mode" VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
      VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/ipv6-$ipv6_mode.out" 2>&1; then
    echo "host smoke accepted IPv6 route mode: $ipv6_mode" >&2
    exit 1
  fi
done
if MOCK_PING6_STATUS=0 VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=2 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/ipv6-ping.out" 2>&1; then
  echo 'host smoke accepted successful IPv6 ping' >&2
  exit 1
fi

# A timed-out negative probe is inconclusive, never evidence of IPv6 blocking.
if MOCK_IPV6_ROUTE_STALL=1 VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=1 \
    VPNKIT_LOCAL_SMOKE_DEVICE=tun7 bash "$script" >"$tmp/ipv6-timeout.out" 2>&1; then
  echo 'host smoke accepted a stalled IPv6 route probe' >&2
  exit 1
fi
grep -Fxq 'host_smoke_failed_check=ipv6' "$tmp/ipv6-timeout.out"

# Exact-device routing is required even when every application probe is
# mocked successful. A different tun, ppp, or vpn device must fail closed.
for bad_device in tun8 ppp0 vpn0 uplink0; do
  if MOCK_ROUTE_DEVICE="$bad_device" VPNKIT_LOCAL_SMOKE_DEVICE=tun7 VPNKIT_LOCAL_SMOKE_TIMEOUT_SECONDS=1 bash "$script" >/dev/null 2>&1; then
    echo "route accepted non-owned device: $bad_device" >&2
    exit 1
  fi
done
if VPNKIT_LOCAL_SMOKE_DEVICE=ppp0 MOCK_ROUTE_DEVICE=ppp0 bash "$script" >/dev/null 2>&1; then
  echo 'host smoke accepted forbidden ppp0' >&2
  exit 1
fi
printf 'vpnkit local host smoke mock tests passed\n'
