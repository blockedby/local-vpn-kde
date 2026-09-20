#!/usr/bin/env bash
# Disposable systemd + nested Docker; no host Docker socket or host state mounts.
set -Eeuo pipefail
umask 077
if (( $# )); then
  if [[ $# == 1 && ( $1 == --help || $1 == -h ) ]]; then
    cat <<'HELP'
Usage: test/vpnkit-local-install-container-test.sh
Runs a disposable Debian/systemd/nested-Docker installation and subscription smoke.
Requires Docker, local-vpn-kde:latest, and an existing physical underlay table.
VPNKIT_LAB_UNDERLAY_TABLE (default 51840), VPNKIT_LAB_SUBNET (172.30.190.0/24),
VPNKIT_LAB_ADDRESS (172.30.190.2), VPNKIT_LAB_RULE_PRIORITY (990; also uses 991).
Optional VPNKIT_LAB_RELEASE_ARCHIVE tests a locally built release archive.
Supply VPNKIT_LAB_SUBSCRIPTION_URL or VPNKIT_LAB_SUBSCRIPTION_FILE;
otherwise uses the saved local subscription. Never prints credentials.
Temporarily adds source rules for the test subnet; removes them on exit.
Private logs: secrets/install-lab/. Does not test the graphical KDE session.
HELP
    exit 0
  fi
  echo 'Unknown argument; use --help' >&2; exit 2
fi
root=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$root"
prefix="vpnkit-install-lab-$$"
network="$prefix-net"
image=vpnkit-install-lab:debian13
subnet=${VPNKIT_LAB_SUBNET:-172.30.190.0/24}
address=${VPNKIT_LAB_ADDRESS:-172.30.190.2}
table=${VPNKIT_LAB_UNDERLAY_TABLE:-51840}
priority=${VPNKIT_LAB_RULE_PRIORITY:-990}
[[ $subnet =~ ^([0-9]{1,3}\.){3}0/24$ && $address == "${subnet%0/24}"* && $address =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]
[[ $table =~ ^[0-9]+$ && $priority =~ ^[0-9]+$ ]]
private="$root/secrets/install-lab/$prefix"
mkdir -p "$private"
log="$private/run.log"
route_added=0
barrier_added=0
helper_image=local-vpn-kde:latest
host_ip() { docker run --rm --network host --cap-add NET_ADMIN --entrypoint ip "$helper_image" "$@"; }
cleanup() {
  local result=$?
  docker exec "$prefix" cat /tmp/lab-result.json > "$private/last-probe.json" 2>/dev/null || true
  docker exec "$prefix" journalctl --no-pager -n 300 > "$private/system.log" 2>&1 || true
  if (( result != 0 )) && [[ ${VPNKIT_LAB_KEEP_FAILED:-0} == 1 ]]; then
    echo "DEBUG retained container $prefix, network $network, source rules $priority/$((priority+1)); remove after inspection"
    return
  fi
  docker exec "$prefix" journalctl -u vpnkit-local-underlay-routing --no-pager -n 80 > "$private/routing-service.log" 2>&1 || true
  docker cp "$prefix:/home/tester/local-vpn-kde/.build/install-lab-terminal.log" "$private/terminal.log" >/dev/null 2>&1 || true
  docker cp "$prefix:/home/tester/local-vpn-kde/secrets/vpnkit-local/diagnostics" "$private/diagnostics" >/dev/null 2>&1 || true
  docker rm -f "$prefix" >/dev/null 2>&1 || true
  if (( barrier_added )); then host_ip rule del priority "$((priority+1))" from "$subnet" unreachable >/dev/null 2>&1 || true; fi
  if (( route_added )); then host_ip rule del priority "$priority" from "$subnet" table "$table" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
ip -j route show table "$table" | python3 -c '
import json,sys,os
rows=json.load(sys.stdin)
defaults=[r for r in rows if r.get("dst")=="default"]
assert len(defaults)==1 and defaults[0].get("gateway")
assert os.path.exists("/sys/class/net/"+defaults[0]["dev"]+"/device")
'
if ip rule show | grep -Eq "^($priority|$((priority+1))):"; then echo 'FAIL: lab rule priority occupied'; exit 1; fi
printf 'Private log: %s\n' "$log"
echo 'Building clean lab image…'
docker build -t "$image" test/install-lab > "$log" 2>&1
docker network create --subnet "$subnet" "$network" >> "$log" 2>&1
host_ip rule add priority "$((priority+1))" from "$subnet" unreachable
barrier_added=1
host_ip rule add priority "$priority" from "$subnet" table "$table"
route_added=1
bridge="br-$(docker network inspect -f '{{.Id}}' "$network" | cut -c1-12)"
ip -j route get 1.1.1.1 from "$address" iif "$bridge" | python3 -c '
import json,sys,os
r=json.load(sys.stdin)[0]
assert os.path.exists("/sys/class/net/"+r["dev"]+"/device")
'
echo 'PASS physical underlay route'
docker run -d --name "$prefix" --hostname install-lab --privileged --cgroupns private --network "$network" --ip "$address" --dns 8.8.8.8 --tmpfs /run --tmpfs /run/lock "$image" >> "$log"
if [[ -n ${VPNKIT_LAB_RELEASE_ARCHIVE:-} ]]; then
  mkdir "$private/release-source"
  tar -xzf "$VPNKIT_LAB_RELEASE_ARCHIVE" --strip-components=1 -C "$private/release-source"
  mkdir -p "$private/release-source/test"
  cp -a test/install-lab "$private/release-source/test/"
  tar -C "$private/release-source" -cf "$private/source.tar" .
else
  git ls-files -co --exclude-standard -z | tar --null -T - -cf "$private/source.tar"
fi
docker exec "$prefix" mkdir -p /home/tester/local-vpn-kde
docker cp "$private/source.tar" "$prefix:/root/lab-source.tar"
docker exec "$prefix" bash -ec 'tar -xf /root/lab-source.tar -C /home/tester/local-vpn-kde; rm /root/lab-source.tar; chown -R tester:tester /home/tester/local-vpn-kde'
for ((i=0;i<30;i++)); do
  if docker exec "$prefix" systemctl show --property=SystemState --value 2>/dev/null | grep -Eq 'running|degraded'; then break; fi
  sleep 1
done
if ! docker exec "$prefix" systemctl show --property=SystemState --value 2>/dev/null | grep -Eq 'running|degraded'; then
  echo "FAIL systemd startup; private log: $log"
  docker logs "$prefix" >> "$log" 2>&1
  exit 1
fi
# Keep credentials out of Docker environment metadata and command arguments.
subscription_file=${VPNKIT_LAB_SUBSCRIPTION_FILE:-$root/secrets/vpnkit-local/vibe-vpn/sub_url}
if [[ -n ${VPNKIT_LAB_SUBSCRIPTION_URL:-} ]]; then
  printf '%s\n' "$VPNKIT_LAB_SUBSCRIPTION_URL" > "$private/sub_url"
elif [[ -s $subscription_file ]]; then
  cp "$subscription_file" "$private/sub_url"
else
  echo 'FAIL: set VPNKIT_LAB_SUBSCRIPTION_URL or VPNKIT_LAB_SUBSCRIPTION_FILE'; exit 1
fi
chmod 600 "$private/sub_url"
docker cp "$private/sub_url" "$prefix:/root/lab-subscription"
if ! timeout --signal=TERM --kill-after=30s 45m docker exec "$prefix" bash /home/tester/local-vpn-kde/test/install-lab/inside.sh >> "$log" 2>&1; then
  echo "FAIL lab; private log: $log"
  exit 1
fi
echo "PASS clean installation and subscription smoke; private log: $log"
