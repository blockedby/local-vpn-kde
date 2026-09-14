#!/usr/bin/env bash
set -euo pipefail

mode=${VPNKIT_ROUTING_MODE:-redirect}
ovpn_cidr=${OVPN_CIDR:-10.89.0.0/24}
tun_iface=${SINGBOX_TUN_IFACE:-sb-tun0}
tun_table=${SINGBOX_TUN_TABLE:-101}
tproxy_mark=${TPROXY_MARK:-0x1}
tproxy_table=${TPROXY_TABLE:-100}
singbox_config=${SINGBOX_CONFIG:-/var/lib/vpnkit/sing-box/config.json}
singbox_generation_file=${VPNKIT_SINGBOX_GENERATION_FILE:-${SINGBOX_GENERATION_FILE:-/run/vpnkit/sing-box-generation}}
openvpn_fail_closed_chain=${OPENVPN_FAIL_CLOSED_CHAIN:-OVPN_FAIL_CLOSED}

fail_closed_barrier_absent() {
  command -v iptables >/dev/null 2>&1 || return 1
  ! iptables -t filter -C INPUT -s "$ovpn_cidr" -j "$openvpn_fail_closed_chain" 2>/dev/null \
    && ! iptables -t filter -C FORWARD -s "$ovpn_cidr" -j "$openvpn_fail_closed_chain" 2>/dev/null \
    && ! iptables -t filter -S "$openvpn_fail_closed_chain" >/dev/null 2>&1
}

pgrep -x openvpn >/dev/null
pgrep -x sing-box >/dev/null
ip link show tun0 >/dev/null
[[ -r "$singbox_config" ]]
# Config validity and the production SOCKS inbound are part of the same
# predicate used before a request-token acknowledgement is published.
sing-box check -c "$singbox_config" >/dev/null 2>&1
[[ -r "$singbox_generation_file" ]]
generation=$(<"$singbox_generation_file")
[[ "$generation" =~ ^[0-9]+$ && "$generation" -ge 1 ]]
fail_closed_barrier_absent

case "${mode,,}" in
  tun)
    ip link show "$tun_iface" >/dev/null
    ip rule show | grep -Fq "from $ovpn_cidr lookup $tun_table"
    ip route show table "$tun_table" | grep -Eq '^default '
    iptables -C FORWARD -i tun0 -o "$tun_iface" -s "$ovpn_cidr" -j ACCEPT >/dev/null 2>&1
    iptables -C FORWARD -i "$tun_iface" -o tun0 -d "$ovpn_cidr" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT >/dev/null 2>&1
    ;;
  redirect)
    ss -ltn | grep -Eq ':[[:space:]]*2082\b|:2082\b'
    ss -lun | grep -Eq ':[[:space:]]*5353\b|:5353\b'
    iptables -t nat -C PREROUTING -i tun0 -s "$ovpn_cidr" -j OVPN_REDIRECT_TO_SINGBOX >/dev/null 2>&1
    iptables -t nat -S OVPN_REDIRECT_TO_SINGBOX | grep -Eq -- '--to-ports[[:space:]]+2082|--to-destination[[:space:]]+2082'
    ;;
  tproxy)
    ss -ltn | grep -Eq ':[[:space:]]*2082\b|:2082\b'
    ip rule show | grep -Fq "fwmark $tproxy_mark lookup $tproxy_table"
    ip route show table "$tproxy_table" | grep -Eq '^local default '
    iptables -t mangle -C PREROUTING -i tun0 -s "$ovpn_cidr" -j OVPN_TO_SINGBOX >/dev/null 2>&1
    ;;
  *)
    echo "unsupported routing mode" >&2
    exit 2
    ;;
esac

ss -ltn | grep -Eq ':[[:space:]]*2080\b|:2080\b'
