#!/usr/bin/env bash
# Verify descriptor-relative renderer writes survive a deterministic same-UID race.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
RENDERER="$ROOT/scripts/vpnkit/vpnkit-render-local-kde-configs.sh"
WRITER="$ROOT/scripts/vpnkit/vpnkit-render-local-kde-secure.py"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-render-security.XXXXXX")
trap 'rm -rf -- "$TMP"' EXIT

bash -n "$RENDERER"
python3 - "$WRITER" <<'PY'
import pathlib
import sys

compile(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"), sys.argv[1], "exec")
PY

grep -Fq 'O_DIRECTORY' "$WRITER"
grep -Fq 'O_NOFOLLOW' "$WRITER"
grep -Fq 'O_CREAT' "$WRITER"
grep -Fq 'O_EXCL' "$WRITER"
grep -Fq 'os.fchmod' "$WRITER"
grep -Fq 'os.fsync' "$WRITER"
grep -Fq 'st_nlink != 1' "$WRITER"
grep -Fq 'os.replace' "$WRITER"
grep -Fq 'src_dir_fd=dir_fd' "$WRITER"
grep -Fq 'dst_dir_fd=dir_fd' "$WRITER"

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

# The hook runs after the shell guard and after all destination directory FDs
# are held. It moves both final output directories behind the renderer's path
# and replaces those path entries with symlinks to an outside sentinel tree.
# A successful renderer must write only through the held old directories; a
# safe failure is also accepted, but the outside sentinel may never change.
race_base="$TMP/race-base"
outside="$TMP/outside"
mkdir -p "$race_base/vibe-vpn" "$outside/sing-box" "$outside/vibe-vpn"
printf 'subscription fixture\n' >"$race_base/vibe-vpn/sub_url"
printf 'outside sing-box sentinel\n' >"$outside/sing-box/sentinel"
printf 'outside ruleset sentinel\n' >"$outside/sing-box/ruleset-sentinel"
printf 'outside vibe sentinel\n' >"$outside/vibe-vpn/sentinel"
outside_sing_mode=$(stat -c '%a' "$outside/sing-box")
outside_vibe_mode=$(stat -c '%a' "$outside/vibe-vpn")
cat >"$TMP/race-hook.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
base=$VPNKIT_LOCAL_SECRETS_DIR
outside=$VPNKIT_LOCAL_RENDER_RACE_OUTSIDE
printf 'hook-ran\n' >"$VPNKIT_LOCAL_RENDER_HOOK_MARKER"
mv -- "$base/rendered/sing-box" "$base/rendered/sing-box-held"
ln -s -- "$outside/sing-box" "$base/rendered/sing-box"
mv -- "$base/rendered/vibe-vpn" "$base/rendered/vibe-vpn-held"
ln -s -- "$outside/vibe-vpn" "$base/rendered/vibe-vpn"
# Install final symlinks in the already-held directories too. renameat must
# replace these entries, not follow them into the outside tree.
ln -s -- "$outside/sing-box/sentinel" "$base/rendered/sing-box-held/config.json"
ln -- "$outside/sing-box/ruleset-sentinel" "$base/rendered/sing-box-held/rule-sets/vpnkit-adblock.json"
ln -s -- "$outside/vibe-vpn/sentinel" "$base/rendered/vibe-vpn-held/config.yaml"
EOF
chmod 700 "$TMP/race-hook.sh"
export VPNKIT_LOCAL_RENDER_RACE_HOOK="$TMP/race-hook.sh"
export VPNKIT_LOCAL_RENDER_RACE_OUTSIDE="$outside"
export VPNKIT_LOCAL_RENDER_HOOK_MARKER="$TMP/hook.marker"
export VPNKIT_LOCAL_TEST_FIXTURE=1
export VPNKIT_LOCAL_SECRETS_DIR="$race_base"
export VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION=false
export VPNKIT_RULESET_SOURCE_MODE=local-fixture
export VPNKIT_SELECTED_OUTBOUND_MODE=direct-fixture
if "$RENDERER" >"$TMP/race.out" 2>&1; then
  race_rc=0
else
  race_rc=$?
fi
[[ -f "$VPNKIT_LOCAL_RENDER_HOOK_MARKER" ]]
cmp -s "$outside/sing-box/sentinel" <(printf 'outside sing-box sentinel\n')
cmp -s "$outside/sing-box/ruleset-sentinel" <(printf 'outside ruleset sentinel\n')
cmp -s "$outside/vibe-vpn/sentinel" <(printf 'outside vibe sentinel\n')
[[ "$(stat -c '%a' "$outside/sing-box")" == "$outside_sing_mode" ]]
[[ "$(stat -c '%a' "$outside/vibe-vpn")" == "$outside_vibe_mode" ]]
[[ -L "$race_base/rendered/sing-box" && -L "$race_base/rendered/vibe-vpn" ]]
if [[ "$race_rc" -eq 0 ]]; then
  [[ -f "$race_base/rendered/sing-box-held/config.json" ]]
  [[ -f "$race_base/rendered/sing-box-held/rule-sets/vpnkit-adblock.json" ]]
  [[ -f "$race_base/rendered/vibe-vpn-held/config.yaml" ]]
  ! find "$race_base/rendered" -name '.vpnkit-render-*.tmp' -print -quit | grep -q .
fi

printf 'vpnkit local renderer security tests passed\n'
