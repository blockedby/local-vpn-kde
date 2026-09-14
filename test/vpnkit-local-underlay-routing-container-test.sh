#!/usr/bin/env bash
# Exercise the host helper against a real disposable Linux network namespace.
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=${VPNKIT_LOCAL_UNDERLAY_TEST_IMAGE:-vpnkit-local-lab-vpnkit}
name=${VPNKIT_LOCAL_UNDERLAY_TEST_CONTAINER:-vpnkit-route-helper-lab-$$}
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

docker image inspect "$image" >/dev/null 2>&1 || {
  echo "missing test image: $image (run local-docker fixture build first)" >&2
  exit 2
}

docker run --rm --privileged --name "$name" \
  -v "$root:/src:ro" --entrypoint bash "$image" -lc '
set -Eeuo pipefail
mkdir -p /tmp/mockbin /sandbox
cat >/tmp/mockbin/systemctl <<"EOF"
#!/usr/bin/env bash
exit 0
EOF
chmod +x /tmp/mockbin/systemctl
export PATH=/tmp/mockbin:$PATH
export VPNKIT_LOCAL_UNDERLAY_ROOT=/sandbox
export VPNKIT_LOCAL_DOCKER_SUBNET=198.18.0.0/24
# Keep fixture discovery on the real physical main-table route; the separate
# work-style table intentionally also contains a default route.
export VPNKIT_LOCAL_UPLINK_TABLE=main
helper=/src/scripts/vpnkit/vpnkit-local-underlay-routing.sh
subnet=$VPNKIT_LOCAL_DOCKER_SUBNET
target=198.18.0.2
bridge=vpnkitbr0
bridge_address=198.18.0.1/24
work_table=51841
work_priority=2000
read -r _ _ _ _ uplink _ < <(ip -4 route show default)
test -n "$uplink"
ip link add "$bridge" type bridge
ip addr add "$bridge_address" dev "$bridge"
ip link set "$bridge" up
ip route replace default dev "$uplink" table "$work_table"
ip rule add priority "$work_priority" to "$subnet" lookup "$work_table"
cleanup_lab() {
  ip rule del priority "$work_priority" to "$subnet" lookup "$work_table" >/dev/null 2>&1 || true
  ip route del default dev "$uplink" table "$work_table" >/dev/null 2>&1 || true
  ip link del "$bridge" >/dev/null 2>&1 || true
}
trap cleanup_lab EXIT

# Before the helper, a later work-style policy rule wins even though the main
# table has the local Docker bridge route.
baseline_route=$(ip -4 route get "$target")
grep -Eq "(^|[[:space:]])dev[[:space:]]+$uplink([[:space:]]|$)" <<<"$baseline_route"
$helper plan | grep -q "install_preflight=ready"
$helper install --yes >/dev/null
$helper verify >/dev/null
installed=/sandbox/usr/local/libexec/vpnkit-local-underlay-routing
$installed --runtime-refresh
$helper verify >/dev/null
$helper install --yes >/dev/null
$helper verify >/dev/null
selected_route=$(ip -4 route get "$target")
grep -Eq "(^|[[:space:]])dev[[:space:]]+$bridge([[:space:]]|$)" <<<"$selected_route"

# Removing the local bridge route must not reopen the later work-VPN rule; the
# helper-owned destination unreachable rule must fail closed instead.
ip addr del "$bridge_address" dev "$bridge"
ip route del "$subnet" dev "$bridge" >/dev/null 2>&1 || true
! ip -4 route show table main "$subnet" | grep -q .
if ip -4 route get "$target" >/tmp/destination-fail-closed 2>&1; then
  cat /tmp/destination-fail-closed >&2
  exit 1
fi
grep -Eiq "unreachable|network is unreachable" /tmp/destination-fail-closed

$helper uninstall --yes >/dev/null
! ip -4 rule show | grep -Eq "(to|from)[[:space:]]+198\.18\.0\.0/24[[:space:]]+(lookup main|unreachable|lookup 51840)"
grep -Eq "^${work_priority}:[[:space:]]+from all to 198\.18\.0\.0/24 lookup ${work_table}$" <(ip -4 rule show)
if ip -4 route show table 51840 >/tmp/table 2>/dev/null; then test ! -s /tmp/table; fi
for p in \
 /sandbox/etc/vpnkit-local-underlay-routing.conf \
 /sandbox/usr/local/libexec/vpnkit-local-underlay-routing \
 /sandbox/etc/systemd/system/vpnkit-local-underlay-routing.service \
 /sandbox/etc/NetworkManager/dispatcher.d/90-vpnkit-local-underlay-routing; do
  test ! -e "$p"
done
printf "actual_kernel_underlay_lab=pass\n"
'
