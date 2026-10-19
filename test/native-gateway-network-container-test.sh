#!/usr/bin/env bash
# Exercise the real native Controller and LinuxNetworkInspector in one disposable kernel lab.
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE:-vpnkit-local-lab-vpnkit}
expected_image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE_ID:-sha256:bf0e1ee7a974bd94a3059ecbd55bbecc17ada0e71f003b053025e2b653d95bd6}
name="vpnkit-native-gateway-lab-${UID}-$$-${RANDOM}"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-native-gateway-lab.XXXXXXXX")
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT INT TERM

command -v docker >/dev/null 2>&1 || {
  echo "blocker: docker is required for the disposable native gateway kernel lab" >&2
  exit 2
}
command -v go >/dev/null 2>&1 || {
  echo "blocker: Go is required to build the static native gateway lab binary" >&2
  exit 2
}
command -v timeout >/dev/null 2>&1 || {
  echo "blocker: host timeout is required to bound the disposable container" >&2
  exit 2
}

actual_image=$(docker image inspect "$image" --format '{{.Id}}' 2>/dev/null) || {
  echo "blocker: missing test image $image (build the existing local lab fixture first)" >&2
  exit 2
}
if [[ $actual_image != "$expected_image" ]]; then
  echo "blocker: test image $image is $actual_image, expected $expected_image" >&2
  exit 2
fi

(cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$tmp/native-gateway-lab" ./test/native-gateway-lab)
chmod 0555 "$tmp/native-gateway-lab"

# Only the static test binary is exposed to the container, and only read-only.
# The container has no network, host namespace mounts, repository mount, or
# container-runtime socket. Its privileged kernel mutations die with it.
timeout --signal=TERM --kill-after=5s 120s \
  docker run --rm --network none --privileged --pull never --name "$name" \
    --mount "type=bind,src=$tmp/native-gateway-lab,dst=/lab,readonly" \
    --entrypoint bash "$image" -lc '
set -Eeuo pipefail
for tool in ip nft sysctl timeout; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "blocker: required image tool is missing: $tool" >&2
    exit 2
  }
done
test -e /.dockerenv || {
  echo "blocker: Docker container marker /.dockerenv is absent" >&2
  exit 2
}

export VPNKIT_NATIVE_GATEWAY_KERNEL_LAB=1
[[ ${VPNKIT_NATIVE_GATEWAY_KERNEL_LAB:-} == 1 ]] || {
  echo "blocker: native gateway kernel lab opt-in is absent" >&2
  exit 2
}

uplink=lab-uplink
unrelated=lab-unrelated
cleanup_inner() {
  ip link del "$unrelated" >/dev/null 2>&1 || true
  ip link del "$uplink" >/dev/null 2>&1 || true
}
trap cleanup_inner EXIT INT TERM
ip link add "$uplink" type dummy
ip addr add 192.0.2.10/24 dev "$uplink"
ip link set "$uplink" up
ip route add default via 192.0.2.1 dev "$uplink"
ip link add "$unrelated" type dummy
ip addr add 203.0.113.10/24 dev "$unrelated"
ip link set "$unrelated" up

ip rule add pref 5260 from 192.168.240.0/24 lookup main
ip rule add pref 5261 to 192.168.240.0/24 lookup main
timeout --signal=TERM --kill-after=2s 105s /lab
ip rule show | grep -q "5260:.*192.168.240.0/24"
ip rule show | grep -q "5261:.*192.168.240.0/24"
'
