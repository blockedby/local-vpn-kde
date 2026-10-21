#!/usr/bin/env bash
# Explicitly exercise the native gateway with a privately supplied real subscription.
# This script is never called by the synthetic fixture or any automatic test target.
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage: test/native-gateway-subscription-container-test.sh [--env-file PATH] [--max-candidates N]

Runs the bounded, opt-in real-subscription native VPN lab in one disposable
Docker container. N defaults to 50 and may be 1..50. The production picker is
run in non-applying test mode with a 64 KiB byte cap and duration mode disabled.
The default environment file is repository-root/.env. VPNKIT_LAB_SUBSCRIPTION_URL
is extracted by test/native-subscription-env.py; the environment file itself is
never sourced or mounted.
EOF
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
env_file="$root/.env"
max_candidates=50
while (($#)); do
  case $1 in
    --env-file)
      (($# >= 2)) || { echo "error: --env-file requires a path" >&2; exit 2; }
      env_file=$2
      shift 2
      ;;
    --max-candidates)
      (($# >= 2)) || { echo "error: --max-candidates requires a value" >&2; exit 2; }
      [[ $2 =~ ^[0-9]+$ ]] && ((10#$2 >= 1 && 10#$2 <= 50)) || {
        echo "error: --max-candidates must be an integer from 1 through 50" >&2
        exit 2
      }
      max_candidates=$((10#$2))
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "error: unsupported argument (use --help)" >&2
      exit 2
      ;;
  esac
done

image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE:-vpnkit-local-lab-vpnkit}
expected_image=${VPNKIT_NATIVE_GATEWAY_TEST_IMAGE_ID:-sha256:bf0e1ee7a974bd94a3059ecbd55bbecc17ada0e71f003b053025e2b653d95bd6}
name="vpnkit-native-real-subscription-lab-${UID}-$$-${RANDOM}"
umask 077
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-native-real-subscription-lab.XXXXXXXX")
chmod 0700 "$tmp"
cleanup() {
  timeout --signal=KILL 5s docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for tool in docker go python3 timeout stat; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "blocker: required tool is unavailable" >&2
    exit 2
  }
done
helper="$root/test/native-subscription-env.py"
[[ -f $helper ]] || {
  echo "blocker: subscription environment extractor is unavailable" >&2
  exit 2
}

# The helper parses data rather than executing shell and writes only the selected
# value. Never source, print, mount, or pass the environment file to Docker.
python3 "$helper" "$env_file" "$tmp/sub_url"
[[ -f $tmp/sub_url && ! -L $tmp/sub_url ]] || {
  echo "blocker: subscription extraction did not create a regular file" >&2
  exit 2
}
[[ $(stat -c '%a' "$tmp/sub_url") == 600 ]] || {
  echo "blocker: extracted subscription file permissions are not 0600" >&2
  exit 2
}
subscription_size=$(stat -c '%s' "$tmp/sub_url")
((subscription_size > 0 && subscription_size <= 16384)) || {
  echo "blocker: extracted subscription value has an invalid size" >&2
  exit 2
}

actual_image=$(docker image inspect "$image" --format '{{.Id}}' 2>/dev/null) || {
  echo "blocker: pinned native VPN lab image is missing" >&2
  exit 2
}
[[ $actual_image == "$expected_image" ]] || {
  echo "blocker: native VPN lab image has an unexpected immutable ID" >&2
  exit 2
}

(
  cd "$root"
  CGO_ENABLED=0 go build -trimpath -o "$tmp/native-vpn-lab" ./test/native-vpn-lab
  CGO_ENABLED=0 go build -trimpath -o "$tmp/vibe-vpn" ./cmd/vibe-vpn
)
chmod 0555 "$tmp/native-vpn-lab" "$tmp/vibe-vpn"

# Only fresh binaries, public templates, and the single 0600 subscription-value
# file enter the container. There is no repository/.env mount, secret env/arg,
# host network/PID namespace, runtime socket, published port, or persisted log.
# Root needs privilege only to construct the isolated namespaces; the gateway is
# run as uid/gid 987 with NET_ADMIN/NET_RAW and no-new-privileges by the lab.
timeout --signal=TERM --kill-after=5s 590s \
  docker run --rm --network bridge --privileged --pull never --name "$name" \
    --env VPNKIT_NATIVE_GATEWAY_VPN_LAB=1 \
    --env VPNKIT_NATIVE_GATEWAY_REAL_SUBSCRIPTION_LAB=1 \
    --env VPNKIT_NATIVE_GATEWAY_VPN_LAB_MODE=service \
    --mount "type=bind,src=$tmp/native-vpn-lab,dst=/fixture/native-vpn-lab,readonly" \
    --mount "type=bind,src=$tmp/vibe-vpn,dst=/fixture/vibe-vpn,readonly" \
    --mount "type=bind,src=$tmp/sub_url,dst=/fixture/sub_url,readonly" \
    --mount "type=bind,src=$root/config/sing-box,dst=/fixture/config/sing-box,readonly" \
    --mount "type=bind,src=$root/config/openvpn,dst=/fixture/config/openvpn,readonly" \
    --entrypoint bash "$image" -lc '
set -Eeuo pipefail
for tool in ip nft sysctl iptables ip6tables openvpn sing-box pkill setpriv; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "blocker: required lab image tool is missing" >&2
    exit 2
  }
done
test -e /.dockerenv || { echo "blocker: Docker marker is absent" >&2; exit 2; }
[[ ${VPNKIT_NATIVE_GATEWAY_VPN_LAB:-} == 1 && ${VPNKIT_NATIVE_GATEWAY_REAL_SUBSCRIPTION_LAB:-} == 1 ]] || {
  echo "blocker: explicit real-subscription lab opt-ins are absent" >&2
  exit 2
}
exec timeout --signal=TERM --kill-after=3s 570s /fixture/native-vpn-lab --max-candidates "$1"
' bash "$max_candidates"
