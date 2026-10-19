#!/usr/bin/env bash
# Offline-runtime package checks in a disposable Arch container; never install
# packages, start services, or manipulate NetworkManager on the host.
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=${VPNKIT_NATIVE_ARCH_TEST_IMAGE:-vpnkit-native-arch-lab}
expected=${VPNKIT_NATIVE_ARCH_TEST_IMAGE_ID:-sha256:2a081ef716ac1099601e5f0fef6a12a153549084998c9822492c452875985a9e}
name="vpnkit-native-arch-package-${UID}-$$-${RANDOM}"
tmp=$(mktemp -d)
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf -- "$tmp"; }
trap cleanup EXIT INT TERM
[[ $(docker image inspect "$image" --format '{{.Id}}') == "$expected" ]] || { echo 'unexpected Arch lab image'; exit 2; }
python3 "$root/packaging/arch/prepare-local-source.py" "$tmp/source"
# The only bind contains the curated public source snapshot, never the checkout.
docker run -d --pull never --name "$name" \
  --mount "type=bind,src=$tmp/source,dst=/input,readonly" \
  "$image" sleep infinity >/dev/null
# Copy only these public fixture sources, never checkout directories/state.
docker cp "$root/test/native-profile-installed-lab/main.go" "$name:/work/profile-main.go"
docker cp "$root/test/native-profile-installed-test.py" "$name:/work/profile-test.py"
docker cp "$root/test/compiled-tui-installed-test.py" "$name:/work/compiled-tui-test.py"
docker exec "$name" mkdir -p /work/portable-src/cmd/vpnkit-portable /work/portable-src/packaging/portable
docker cp "$root/cmd/vpnkit-portable/main.go" "$name:/work/portable-src/cmd/vpnkit-portable/main.go"
docker cp "$root/packaging/portable/build.py" "$name:/work/portable-src/packaging/portable/build.py"
# Network access is used only for frozen public Go/Bun build dependencies.
timeout --kill-after=5s 900s docker exec "$name" bash -lc '
set -Eeuo pipefail
cp -a /input/. /work/
chown -R builder:builder /work
runuser -u builder -- bash -lc "cd /work && makepkg --noconfirm --cleanbuild"
sources=(/work/src/local-vpn-kde-native-*/)
[[ ${#sources[@]} == 1 && -d ${sources[0]} ]] || { echo "unexpected extracted source directory layout"; exit 1; }
install -d -o builder -g builder "${sources[0]}/.build/profile-lab"
install -o builder -g builder -m644 /work/profile-main.go "${sources[0]}/.build/profile-lab/main.go"
runuser -u builder -- bash -c "cd ${sources[0]} && go build -o /work/profile-fixture ./.build/profile-lab"
'
docker network disconnect bridge "$name"
# No system manager is running: this verifies package filesystem/hooks and
# installed CLI layout, not KDE, Polkit prompts, or real systemd lifecycle.
timeout --kill-after=5s 180s docker exec "$name" bash -lc '
set -Eeuo pipefail
packages=(/work/local-vpn-kde-native-[0-9]*-x86_64.pkg.tar.zst)
[[ ${#packages[@]} == 1 ]]
sha256sum "${packages[0]}"
python /work/portable-src/packaging/portable/build.py "${packages[0]}" /work/vpnkit-portable
/work/vpnkit-portable --inspect > /work/portable-manifest.json
python -c '\''import json; m=json.load(open("/work/portable-manifest.json")); assert "bun" not in m["dependencies"] and not any(d.startswith("sing-box") for d in m["dependencies"])'\''
printf "y\ny\ny\n" | /work/vpnkit-portable --install
printf "portable_single_download_install=passed\n"
test -f /usr/lib/systemd/system/local-vpn-kde-network.service
test -f /usr/share/polkit-1/actions/org.local-vpn-kde.configure.policy
test -f /etc/netns/vpnkit-native/resolv.conf
grep -qx "nameserver 8.8.8.8" /etc/netns/vpnkit-native/resolv.conf
[[ $(stat -c "%u:%g:%a" /etc/netns/vpnkit-native/resolv.conf) == 0:0:644 ]]
test ! -e /usr/share/local-vpn-kde/.build
if runuser -u builder -- env HOME=/home/builder XDG_STATE_HOME=/home/builder/.local/state \
  /usr/bin/local-vpn-kde ui --status-json > /work/status.json; then
  printf "installed_first_launch=passed\n"
else
  result=$?
  if [[ ! -d /home/builder/.local/state/local-vpn-kde/vibe-vpn ]]; then
    printf "installed_first_launch=failed_missing_private_directories\n"
    runuser -u builder -- bash -c '\''umask 077; mkdir -p /home/builder/.local/state/local-vpn-kde/vibe-vpn'\''
    if runuser -u builder -- env HOME=/home/builder XDG_STATE_HOME=/home/builder/.local/state \
      /usr/bin/local-vpn-kde ui --status-json > /work/status.json; then
      printf "diagnostic_only_manually_created_directories=passed\n"
    fi
  fi
  exit "$result"
fi
python -c '\''import json; s=json.load(open("/work/status.json")); assert s["runtime"]=="native" and s["installed"] is True'\''
# Read-only status must not create state; interactive bridge startup must.
test ! -e /home/builder/.local/state/local-vpn-kde
runuser -u builder -- env HOME=/home/builder XDG_STATE_HOME=/home/builder/.local/state \
  /usr/bin/local-vpn-kde bridge </dev/null
[[ $(stat -c %a /home/builder/.local/state/local-vpn-kde) == 700 ]]
[[ $(stat -c %a /home/builder/.local/state/local-vpn-kde/vibe-vpn) == 700 ]]
VPNKIT_NATIVE_PACKAGE_LAB=1 /work/profile-fixture
VPNKIT_NATIVE_PACKAGE_LAB=1 python /work/profile-test.py
python /work/compiled-tui-test.py
# Simulate the marker left by a completed pre-hook and a later abort. The
# offline container cannot exercise actual service stopping.
test ! -e /var/lib/pacman/db.lck
(umask 077; printf "native-package-transaction-v1\n" > /run/local-vpn-kde-package-blocked)
install -d -m700 /var/lib/local-vpn-kde-install
(umask 077; printf "native-setup-incomplete-v1\n" > /var/lib/local-vpn-kde-install/setup-incomplete)
(umask 077; : > /var/lib/pacman/db.lck)
if /usr/bin/local-vpn-kde native-package --action recover; then
  echo "recovery incorrectly accepted an active package lock"; exit 1
fi
test -f /var/lib/pacman/db.lck
test -f /run/local-vpn-kde-package-blocked
# Only this test-created lock is removed, inside the disposable container.
rm /var/lib/pacman/db.lck
/usr/bin/local-vpn-kde native-package --action recover
test ! -e /run/local-vpn-kde-package-blocked
test -f /var/lib/local-vpn-kde-install/setup-incomplete
printf "arch_package_abort_recovery=passed\n"
# Local reinstallation exercises upgrade hooks without pretending this is a
# migration between released schema versions.
pacman -U --noconfirm "${packages[0]}"
printf retained > /etc/local-vpn-kde/package-lab-retain
pacman -R --noconfirm local-vpn-kde-native
test ! -e /usr/bin/local-vpn-kde
test ! -e /usr/libexec/local-vpn-kde/local-vpn-kde.bin
test ! -e /etc/netns/vpnkit-native/resolv.conf
test "$(</etc/local-vpn-kde/package-lab-retain)" == retained
test -d /home/builder/.local/state/local-vpn-kde
getent passwd local-vpn-kde >/dev/null
printf "arch_package_offline_lifecycle=passed\n"
'
# Optional LOCAL build artifact, copied only after the offline checks pass.
# Never copy generated profile fixtures or private runtime state.
if [[ -n ${VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR:-} ]]; then
  artifact_dir=$VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR
  mkdir -p -- "$artifact_dir"
  package_name=$(docker exec "$name" bash -c 'files=(/work/local-vpn-kde-native-[0-9]*-x86_64.pkg.tar.zst); [[ ${#files[@]} == 1 ]] && basename "${files[0]}"')
  [[ $package_name == local-vpn-kde-native-[0-9]*-x86_64.pkg.tar.zst && $package_name != */* ]]
  test ! -e "$artifact_dir/$package_name"
  docker cp "$name:/work/$package_name" "$artifact_dir/$package_name"
  test ! -e "$artifact_dir/vpnkit-portable"
  docker cp "$name:/work/vpnkit-portable" "$artifact_dir/vpnkit-portable"
  docker cp "$name:/work/vpnkit-portable.sha256" "$artifact_dir/vpnkit-portable.sha256"
  (cd "$artifact_dir" && sha256sum -- "$package_name" > "$package_name.sha256")
  printf "local_package_artifact=%s/%s\n" "$artifact_dir" "$package_name"
fi
