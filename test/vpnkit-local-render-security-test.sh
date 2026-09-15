#!/usr/bin/env bash
# Verify descriptor-relative renderer writes survive a deterministic same-UID race.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
RENDERER="$ROOT/scripts/vpnkit/vpnkit-render-local-kde-configs.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-render-security.XXXXXX")
trap 'rm -rf -- "$TMP"' EXIT

bash -n "$RENDERER"
(cd "$ROOT" && go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde)

# A normal direct fixture remains secret-free and produces every renderer
# output without requiring a subscription URL.
fixture="$TMP/fixture"
VPNKIT_LOCAL_TEST_FIXTURE=1 \
VPNKIT_LOCAL_SECRETS_DIR="$fixture" \
VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION=true \
VPNKIT_RULESET_SOURCE_MODE=local-fixture \
VPNKIT_SELECTED_OUTBOUND_MODE=direct-fixture \
  "$RENDERER" >"$TMP/fixture.out"
grep -Fxq 'secret_material=not_printed' "$TMP/fixture.out"
! grep -Eqi 'subscription-token|private-key|password=' "$TMP/fixture.out"
for output in \
  rendered/sing-box/config.json \
  rendered/sing-box/rule-sets/vpnkit-adblock.json \
  rendered/sing-box/rule-sets/vpnkit-dev-direct.json \
  rendered/sing-box/rule-sets/geoip-ru.json \
  rendered/sing-box/rule-sets/geosite-category-ru.json \
  rendered/vibe-vpn/config.yaml; do
  [[ -f "$fixture/$output" && ! -L "$fixture/$output" ]]
done
python3 - "$fixture/rendered/sing-box/config.json" <<'PY'
import json
import sys

config = json.load(open(sys.argv[1], encoding="utf-8"))
assert config["dns"]["final"] == "remote-dns"
assert config["route"]["final"] == "selected-native-out"
PY

# The native deterministic seam swaps held directories and final entries.
# It is test-only Go code, never an executable hook in the installed program.
(cd "$ROOT" && go test ./internal/localvpn -run '^TestRender' -count=1)

printf 'vpnkit local renderer security tests passed\n'
