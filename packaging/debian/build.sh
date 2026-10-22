#!/usr/bin/env bash
# Run in the disposable Debian builder, with curated public source at /work.
set -Eeuo pipefail
[[ $# == 2 && $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+(~rc[1-9][0-9]*)?$ ]] || { echo 'Usage: build.sh VERSION OUTPUT' >&2; exit 2; }
version=$1
output=$(realpath -m "$2")
[[ $(dpkg --print-architecture) == amd64 ]]
root=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$root"
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT
export CGO_ENABLED=0 GOFLAGS='-buildvcs=false -trimpath'
for command in local-vpn-kde vibe-vpn vibe-vpn-server-op-supervisor; do
  target=$command; [[ $command != local-vpn-kde ]] || target=local-vpn-kde.bin
  go build -ldflags='-s -w' -o "$stage/usr/libexec/local-vpn-kde/$target" "./cmd/$command"
done
mkdir -p "$stage/usr/lib/systemd/system"
go run ./packaging/arch/generate-units "$stage/usr/lib/systemd/system"
(
  cd scripts/vpnkit/tui
  bun install --frozen-lockfile --production
  bun build --compile --target=bun-linux-x64-baseline --define 'process.env.VPNKIT_COMPILED_NATIVE="1"' index.ts --outfile "$stage/usr/share/local-vpn-kde/scripts/vpnkit/tui/vpnkit-tui"
)
install -Dm755 /usr/bin/sing-box "$stage/usr/libexec/local-vpn-kde/sing-box"
install -Dm755 packaging/arch/local-vpn-kde "$stage/usr/bin/local-vpn-kde"
install -Dm755 scripts/vpnkit/vpnkit-native.sh "$stage/usr/share/local-vpn-kde/scripts/vpnkit/vpnkit-native.sh"
install -Dm644 packaging/arch/native-netns-resolv.conf "$stage/etc/netns/vpnkit-native/resolv.conf"
install -Dm644 packaging/arch/local-vpn-kde-native.sysusers "$stage/usr/lib/sysusers.d/local-vpn-kde-native.conf"
sed -i 's|/usr/bin/nologin|/usr/sbin/nologin|' "$stage/usr/lib/sysusers.d/local-vpn-kde-native.conf"
install -Dm644 packaging/arch/local-vpn-kde-native.tmpfiles "$stage/usr/lib/tmpfiles.d/local-vpn-kde-native.conf"
install -Dm644 packaging/arch/49-local-vpn-kde.rules "$stage/usr/share/polkit-1/rules.d/49-local-vpn-kde.rules"
install -Dm644 packaging/arch/org.local-vpn-kde.configure.policy "$stage/usr/share/polkit-1/actions/org.local-vpn-kde.configure.policy"
mkdir -p "$stage/usr/share/local-vpn-kde/config"
cp -a --no-preserve=ownership config/openvpn config/sing-box "$stage/usr/share/local-vpn-kde/config/"
install -Dm644 /usr/share/licenses/sing-box/LICENSE "$stage/usr/share/doc/local-vpn-kde-native/sing-box.LICENSE"
# Bun includes its own license inventory in the executable.
bun --license > "$stage/usr/share/doc/local-vpn-kde-native/bun.LICENSE"
while IFS= read -r -d '' notice; do
  install -Dm644 "$notice" "$stage/usr/share/doc/local-vpn-kde-native/${notice#scripts/vpnkit/tui/}"
done < <(find scripts/vpnkit/tui/node_modules -type f -iname '*license*' -print0)
mkdir -p "$stage/DEBIAN"
for script in postinst prerm postrm; do install -m755 "packaging/debian/$script" "$stage/DEBIAN/$script"; done
printf '/etc/netns/vpnkit-native/resolv.conf\n' > "$stage/DEBIAN/conffiles"
# Compute shared-library ABI requirements on Debian, never reuse Arch ELFs.
mkdir -p "$stage/shlibdeps/debian"
printf 'Source: local-vpn-kde-native\nSection: net\nPriority: optional\nMaintainer: local-vpn-kde contributors <noreply@github.com>\n\nPackage: local-vpn-kde-native\nArchitecture: amd64\nDescription: Native VPN gateway\n' > "$stage/shlibdeps/debian/control"
libs=$(cd "$stage/shlibdeps" && dpkg-shlibdeps -O -e"$stage/usr/share/local-vpn-kde/scripts/vpnkit/tui/vpnkit-tui" -e"$stage/usr/libexec/local-vpn-kde/sing-box" | sed -n 's/^shlibs:Depends=//p')
rm -rf "$stage/shlibdeps"
cat > "$stage/DEBIAN/control" <<CONTROL
Package: local-vpn-kde-native
Version: $version
Architecture: amd64
Maintainer: local-vpn-kde contributors <noreply@github.com>
Section: net
Priority: optional
Depends: openvpn, network-manager, network-manager-openvpn, nftables, iptables, iproute2, polkitd, pkexec, passwd, util-linux, systemd, $libs
Homepage: https://github.com/blockedby/local-vpn-kde
Description: Native VPN gateway with an OpenTUI interface for KDE
 Includes the compiled interface, Go services and sing-box engine.
 NetworkManager provides the desktop VPN connection. No Docker is required.
CONTROL
mkdir -p "$output"
package="$output/local-vpn-kde-native_${version}_amd64.deb"
[[ ! -e $package ]]
dpkg-deb --root-owner-group --build "$stage" "$package"
(cd "$output" && sha256sum "$(basename "$package")" > "$(basename "$package").sha256")
