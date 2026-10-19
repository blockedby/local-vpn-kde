#!/usr/bin/env bash
# Exercise the full native gateway VPN packet path in one disposable kernel lab.
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE:-vpnkit-local-lab-vpnkit}
expected_image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE_ID:-sha256:bf0e1ee7a974bd94a3059ecbd55bbecc17ada0e71f003b053025e2b653d95bd6}
name_prefix="vpnkit-native-vpn-lab-${UID}-$$-${RANDOM}"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-native-vpn-lab.XXXXXXXX")
cleanup() {
  docker rm -f "${name_prefix}-root" "${name_prefix}-service" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT INT TERM

for tool in docker go timeout; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "blocker: $tool is required for the disposable native VPN lab" >&2
    exit 2
  }
done

actual_image=$(docker image inspect "$image" --format '{{.Id}}' 2>/dev/null) || {
  echo "blocker: missing test image $image (build the existing local lab fixture first)" >&2
  exit 2
}
if [[ $actual_image != "$expected_image" ]]; then
  echo "blocker: test image $image has an unexpected immutable ID" >&2
  exit 2
fi

(
  cd "$root"
  CGO_ENABLED=0 go build -trimpath -o "$tmp/native-vpn-lab" ./test/native-vpn-lab
  CGO_ENABLED=0 go build -trimpath -o "$tmp/vibe-vpn" ./cmd/vibe-vpn
)
chmod 0555 "$tmp/native-vpn-lab" "$tmp/vibe-vpn"

# Expose only freshly built fixture binaries and public templates. There is no
# repository mount, host network/PID namespace, host runtime socket, or secret
# bind. Generated PKI and controlled diagnostics remain in each disposable
# container filesystem. The container orchestrator needs broad privileges to
# construct namespaces; service mode drops the VPN supervisor to the exact
# production capability contract after root performs namespace initialization.
run_fixture() {
  local mode=$1
  local name="${name_prefix}-${mode}"
  echo "fixture_mode=$mode"
  timeout --signal=TERM --kill-after=5s 120s \
    docker run --rm --network none --privileged --pull never --name "$name" \
      --env "VPNKIT_NATIVE_GATEWAY_VPN_LAB_MODE=$mode" \
      --mount "type=bind,src=$tmp/native-vpn-lab,dst=/fixture/native-vpn-lab,readonly" \
      --mount "type=bind,src=$tmp/vibe-vpn,dst=/fixture/vibe-vpn,readonly" \
      --mount "type=bind,src=$root/config/sing-box,dst=/fixture/config/sing-box,readonly" \
      --mount "type=bind,src=$root/config/openvpn,dst=/fixture/config/openvpn,readonly" \
      --entrypoint bash "$image" -lc '
set -Eeuo pipefail
for tool in ip nft sysctl iptables ip6tables openvpn sing-box curl pkill ss; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "blocker: required image tool is missing: $tool" >&2
    exit 2
  }
done
if [[ ${VPNKIT_NATIVE_GATEWAY_VPN_LAB_MODE:-} == service ]] && ! command -v setpriv >/dev/null 2>&1; then
  echo "blocker: service fixture requires image tool: setpriv" >&2
  exit 2
fi
test -e /.dockerenv || {
  echo "blocker: Docker container marker is absent" >&2
  exit 2
}
export VPNKIT_NATIVE_GATEWAY_VPN_LAB=1
[[ ${VPNKIT_NATIVE_GATEWAY_VPN_LAB:-} == 1 ]] || {
  echo "blocker: native VPN lab opt-in is absent" >&2
  exit 2
}
exec timeout --signal=TERM --kill-after=3s 105s /fixture/native-vpn-lab
'
}

run_fixture root
run_fixture service
