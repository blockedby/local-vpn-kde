#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
cd /home/tester/local-vpn-kde
export PATH="$PWD/.build/tools/bun/bin:$PATH"
if command -v go || command -v bun; then echo "FAIL: image is not clean"; exit 1; fi
echo 'PASS clean image: Go and Bun absent'
# Docker bind-mounts resolv.conf. A desktop NM must be able to replace it.
# Unmount only the lab's copy, leaving the host resolver untouched.
if mountpoint -q /etc/resolv.conf; then umount /etc/resolv.conf; fi
printf 'nameserver 8.8.8.8\n' > /etc/resolv.conf
# Initialize only this network namespace before NetworkManager is installed.
systemctl start systemd-udevd
udevadm trigger --subsystem-match=net --action=add
udevadm settle --timeout=10
# Emulate desktop NM authorization only inside the disposable systemd host.
install -d -m 755 /etc/polkit-1/rules.d
cat > /etc/polkit-1/rules.d/49-install-lab.rules <<'RULE'
polkit.addRule(function(action, subject) {
  if (subject.user == "tester" && action.id.indexOf("org.freedesktop.NetworkManager.") == 0)
    return polkit.Result.YES;
});
RULE
chmod 644 /etc/polkit-1/rules.d/49-install-lab.rules
sudo -u tester -H git init -q
sudo -u tester -H python3 test/install-lab/terminal.py
echo 'PASS install.sh from clean image'
sudo -u tester -H env PATH="$PATH" bash -c 'cd scripts/vpnkit/tui; bun run check'
# Docker supplies a static uplink, not a logged-in KDE network connection.
# Give NM an explicit base connection before exercising VPN activation.
address=$(ip -4 -o address show dev eth0 scope global | awk '{print $4; exit}')
gateway=$(ip -4 route show default | awk '{print $3; exit}')
nmcli device set eth0 managed yes
nmcli connection add type ethernet ifname eth0 con-name lab-underlay ipv4.method manual ipv4.addresses "$address" ipv4.gateway "$gateway" ipv4.dns 8.8.8.8 ipv6.method disabled >/dev/null
nmcli connection up lab-underlay >/dev/null
install -d -o tester -g tester -m 700 secrets/vpnkit-local/vibe-vpn
install -o tester -g tester -m 600 /root/lab-subscription secrets/vpnkit-local/vibe-vpn/sub_url
rm /root/lab-subscription
sudo -u tester -H bash -lc 'set -e; cd /home/tester/local-vpn-kde; scripts/vpnkit/vpnkit-local.sh backend start; scripts/vpnkit/vpnkit-local.sh servers refresh > /tmp/lab-catalog.json'
echo 'PASS subscription and gateway'
mapfile -t candidates < <(python3 -c 'import json; d=json.load(open("/tmp/lab-catalog.json")); rows=d.get("servers",[]); assert rows; rows.sort(key=lambda r:not r.get("selected",False)); print("\n".join(r["server_id"] for r in rows[:5]))')
(( ${#candidates[@]} )) || { echo 'FAIL empty subscription catalog'; exit 1; }
passed=0
candidate=0
probe() {
  local action=$1
  shift
  local rc=0
  sudo -u tester -H scripts/vpnkit/vpnkit-local.sh servers "$action" "$@" > /tmp/lab-result.json || rc=$?
  local status
  status=$(python3 -c 'import json; print(json.load(open("/tmp/lab-result.json"))["status"])') || exit 1
  case "$status" in
    ok) (( rc == 0 )) || exit 1; return 0 ;;
    failed) echo "FAIL candidate $candidate: $action"; return 1 ;;
    *) echo "FAIL backend during $action"; exit 1 ;;
  esac
}
for id in "${candidates[@]}"; do
  candidate=$((candidate+1))
  probe ping "$id" || continue
  probe speed "$id" || continue
  probe availability "$id" https://example.com/ || continue
  probe select "$id" || continue
  passed=1
  echo 'PASS server ping, speed, website availability and selection'
  break
done
(( passed )) || { echo 'FAIL no candidate completed smoke checks'; exit 1; }
sudo -u tester -H scripts/vpnkit/vpnkit-local.sh start
echo 'PASS VPN connection and DNS smoke inside lab'
if [[ ! -f internal/localvpn/autostart_linux.go ]]; then
  echo 'SKIP autostart: selected release predates this feature'
  exit 0
fi

# Exercise the real user manager only inside the disposable lab. Restarting it
# simulates a new login and proves default.target activation, without --now.
lab_uid=$(id -u tester)
user_command() {
  sudo -u tester -H env XDG_RUNTIME_DIR="/run/user/$lab_uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$lab_uid/bus" "$@"
}
autostart() { user_command .build/local-vpn-kde.bin autostart --repo "$PWD" --action "$1"; }
assert_runtime() {
  sudo -u tester -H scripts/vpnkit/vpnkit-local.sh status --json > /tmp/lab-runtime.json
  python3 - "$1" "$2" <<'PY'
import json,sys
s=json.load(open('/tmp/lab-runtime.json'))
assert s['container']==sys.argv[1], 'gateway state mismatch'
assert s['networkmanager']['active']==sys.argv[2], 'VPN state mismatch'
PY
}
assert_selection() {
  sudo -u tester -H scripts/vpnkit/vpnkit-local.sh servers list > /tmp/lab-autostart-catalog.json
  python3 - <<'PY'
import json
s=json.load(open('/tmp/lab-autostart-catalog.json'))
selected=[r['server_id'] for r in s['servers'] if r.get('selected')]
assert selected==json.load(open('/tmp/lab-autostart-selected.json')), 'selected server changed'
PY
}
await_login_service() {
  local state
  for ((i=0;i<180;i++)); do
    state=$(user_command systemctl --user show local-vpn-kde-autostart.service --property=ActiveState --value)
    case "$state" in
      active) return 0 ;;
      failed) echo 'FAIL login autostart service'; return 1 ;;
    esac
    sleep 2
  done
  echo 'FAIL login autostart service timeout'; return 1
}
sudo -u tester -H scripts/vpnkit/vpnkit-local.sh servers list > /tmp/lab-autostart-catalog.json
python3 - <<'PY'
import json
s=json.load(open('/tmp/lab-autostart-catalog.json'))
selected=[r['server_id'] for r in s['servers'] if r.get('selected')]
assert len(selected)==1
json.dump(selected,open('/tmp/lab-autostart-selected.json','w'))
PY
systemctl start "user@$lab_uid.service"
autostart off
assert_runtime healthy yes
sudo -u tester -H scripts/vpnkit/vpnkit-local.sh stop
assert_runtime absent no
autostart gateway
assert_runtime absent no
systemctl restart "user@$lab_uid.service"
await_login_service
assert_runtime healthy no
assert_selection
echo 'PASS login gateway autostart; no immediate start; selection preserved'
autostart connect
assert_runtime healthy no
systemctl restart "user@$lab_uid.service"
await_login_service
assert_runtime healthy yes
assert_selection
# Lifecycle start performs its ordinary DNS smoke before reporting success.
echo 'PASS login VPN autoconnect and DNS smoke; selection preserved'
autostart off
assert_runtime healthy yes
if user_command systemctl --user is-enabled --quiet local-vpn-kde-autostart.service; then
  echo 'FAIL autostart service remains enabled'; exit 1
fi
echo 'PASS disabling autostart preserves running VPN'
