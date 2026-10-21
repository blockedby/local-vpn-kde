#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=vpnkit-local-lab-vpnkit
expected=sha256:bf0e1ee7a974bd94a3059ecbd55bbecc17ada0e71f003b053025e2b653d95bd6
[[ $(docker image inspect "$image" --format '{{.Id}}') == "$expected" ]]
tmp=$(mktemp -d)
name="vpnkit-hy2-lab-${UID}-$$"
cleanup(){ docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf -- "$tmp"; }
trap cleanup EXIT INT TERM
(cd "$root" && CGO_ENABLED=0 go build -o "$tmp/lab" ./test/hysteria2-lab)
network=none
extra=()
if [[ -n ${VPNKIT_LAB_SOURCE_FILE:-} ]]; then
  [[ -f $VPNKIT_LAB_SOURCE_FILE && ! -L $VPNKIT_LAB_SOURCE_FILE ]]
  [[ $(stat -c '%a:%u' "$VPNKIT_LAB_SOURCE_FILE") == "600:$UID" ]]
  network=bridge
  extra=(--env VPNKIT_LAB_SOURCE_FILE=/private-source --mount "type=bind,src=$VPNKIT_LAB_SOURCE_FILE,dst=/private-source,readonly")
fi
timeout 50s docker run --rm --name "$name" --user "$UID" --network "$network" "${extra[@]}" --cap-drop ALL --security-opt no-new-privileges --pull never --env VPNKIT_HY2_LAB=1 --mount "type=bind,src=$tmp/lab,dst=/lab,readonly" --entrypoint /lab "$image"
