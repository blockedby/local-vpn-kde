#!/usr/bin/env bash
# Mock-only contract tests for the tracked local installer and launcher.
# No sudo, Docker lifecycle, NetworkManager mutation, or host routing command
# is allowed to reach the real system from this test.
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
INSTALL_SOURCE="$ROOT/scripts/vpnkit/vpnkit-local-install.sh"
LAUNCHER_SOURCE="$ROOT/scripts/vpnkit/vpnkit-local-tui.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-install.XXXXXX")
trap 'rm -rf -- "$TMP"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

bash -n "$INSTALL_SOURCE" "$LAUNCHER_SOURCE" || fail 'new shell sources are not syntactically valid'
grep -Fq 'sudo -- "$UNDERLAY" install --yes' "$INSTALL_SOURCE" || fail 'installer does not use the bounded sudo underlay install'
grep -Fq 'VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false' "$INSTALL_SOURCE" || fail 'installer does not force container-only lifecycle start'
grep -Fq '"$NM_HELPER" import --yes' "$INSTALL_SOURCE" || fail 'installer does not import the owned NetworkManager profile'
grep -Fq 'profile_activation=manual' "$INSTALL_SOURCE" || fail 'installer does not expose manual activation'
! grep -Fq '"$NM_HELPER" connect' "$INSTALL_SOURCE" || fail 'installer contains an auto-connect path'

FAKE_REPO="$TMP/repo"
mkdir -p "$FAKE_REPO/scripts/vpnkit" "$FAKE_REPO/config" "$FAKE_REPO/secrets"
for path in \
  scripts/vpnkit/vpnkit-local-install.sh \
  scripts/vpnkit/vpnkit-local-tui.sh \
  scripts/vpnkit/vpnkit-local-path-guard.sh \
  scripts/vpnkit/vpnkit-local.sh \
  scripts/vpnkit/vpnkit-local-underlay-routing.sh \
  scripts/vpnkit/vpnkit-local-networkmanager.sh \
  scripts/vpnkit/vpnkit-local-assets.sh \
  scripts/vpnkit/vpnkit-render-local-kde-configs.sh \
  scripts/vpnkit/vpnkit-local-host-smoke.sh \
  scripts/vpnkit/vpnkit_local_kde_tui.py \
  config/vpnkit-local.env.example; do
  mkdir -p "$FAKE_REPO/$(dirname -- "$path")"
  cp -p "$ROOT/$path" "$FAKE_REPO/$path"
done
(
  cd "$FAKE_REPO"
  git init -q
)
chmod 700 "$FAKE_REPO/secrets"

MOCK_BIN="$TMP/bin"
MOCK_LOG="$TMP/mock.log"
mkdir -p "$MOCK_BIN"
: >"$MOCK_LOG"

cat >"$MOCK_BIN/docker" <<'EOF_DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"$MOCK_LOG"
if [[ "${1:-}" == compose && "${2:-}" == version ]]; then
  printf 'Docker Compose version v2.mock\n'
  exit 0
fi
printf 'unexpected docker operation\n' >&2
exit 91
EOF_DOCKER

cat >"$MOCK_BIN/sudo" <<'EOF_SUDO'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'sudo %s\n' "$*" >>"$MOCK_LOG"
printf 'sudo must not be invoked by dry-run\n' >&2
exit 92
EOF_SUDO

cat >"$MOCK_BIN/nmcli" <<'EOF_NMCLI'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'nmcli %s\n' "$*" >>"$MOCK_LOG"
if [[ "$*" == *'device status'* ]]; then
  printf '%s\n' 'enp42s0:ethernet:connected'
fi
EOF_NMCLI

cat >"$MOCK_BIN/ip" <<'EOF_IP'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'ip %s\n' "$*" >>"$MOCK_LOG"
case "$*" in
  '-4 rule show')
    cat <<'EOF_RULES'
0: from all lookup local
32766: from all lookup main
32767: from all lookup default
EOF_RULES
    ;;
  '-4 route show table all')
    cat <<'EOF_ROUTES'
default via 192.0.2.1 dev enp42s0 proto dhcp src 192.0.2.10 metric 100
192.0.2.0/24 dev enp42s0 proto kernel scope link src 192.0.2.10 metric 100
EOF_ROUTES
    ;;
  *)
    exit 0
    ;;
esac
EOF_IP

chmod 755 "$MOCK_BIN"/*
export MOCK_LOG
export PATH="$MOCK_BIN:$PATH"

# Help must not require a working Docker, NM, sudo, or route environment. The
# temporary copy also proves both launchers discover the canonical git root
# rather than using the caller's current directory.
"$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --help >"$TMP/install-help"
grep -Fq -- '--dry-run' "$TMP/install-help" || fail 'installer help omits dry-run'
grep -Fq -- 'rollback' "$TMP/install-help" || fail 'installer help omits rollback guidance'
"$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh" --help >"$TMP/tui-help"
grep -Fq -- '--status-json' "$TMP/tui-help" || fail 'canonical TUI launcher did not pass help through'
chmod 775 "$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh" --help >"$TMP/tui-mode.out" 2>&1; then
  fail 'group/other-writable launcher was accepted'
fi
chmod 755 "$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh"
mv "$FAKE_REPO/scripts/vpnkit/vpnkit_local_kde_tui.py" "$TMP/tui-source"
ln -s "$TMP/tui-source" "$FAKE_REPO/scripts/vpnkit/vpnkit_local_kde_tui.py"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh" --help >"$TMP/tui-link.out" 2>&1; then
  fail 'symlinked Python TUI source was accepted'
fi
rm -f -- "$FAKE_REPO/scripts/vpnkit/vpnkit_local_kde_tui.py"
mv "$TMP/tui-source" "$FAKE_REPO/scripts/vpnkit/vpnkit_local_kde_tui.py"
mkdir -p "$FAKE_REPO/secrets/vpnkit-local/vibe-vpn"
chmod 700 "$FAKE_REPO/secrets/vpnkit-local" "$FAKE_REPO/secrets/vpnkit-local/vibe-vpn"
tui_test_output=$("$FAKE_REPO/scripts/vpnkit/vpnkit-local-tui.sh" --test)
grep -Fq '"schema": 1' <<<"$tui_test_output" || fail 'canonical launcher did not reach the TUI test mode'

# Dry-run uses the tracked env example without creating the ignored local env
# file. The underlay plan is exercised through fake ip/nmcli only; sudo and
# all lifecycle mutation commands remain unreachable.
dry_output=$(
  "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run 2>"$TMP/dry.err"
) || fail 'dry-run preflight failed'
grep -Fq 'env_file=would-create-mode-0600' <<<"$dry_output" || fail 'dry-run did not describe the private env creation'
grep -Fq 'mutation=none' <<<"$dry_output" || fail 'dry-run did not report non-mutation'
grep -Fq 'underlay_plan=redacted' <<<"$dry_output" || fail 'dry-run omitted the redacted underlay plan'
[[ ! -e "$FAKE_REPO/config/vpnkit-local.local.env" && ! -L "$FAKE_REPO/config/vpnkit-local.local.env" ]] || fail 'dry-run created the env file'
! grep -Eq '^sudo |compose (up|down|build|start|stop)|connection (up|down|delete|modify|import)' "$MOCK_LOG" || fail 'dry-run reached a live mutation seam'

# A secret/profile sentinel is never emitted even when the canonical ignored
# tree already exists. Keep the sentinel out of diagnostics on failure too.
mkdir -p "$FAKE_REPO/secrets/vpnkit-local/vibe-vpn" "$FAKE_REPO/secrets/vpnkit-local/openvpn/client"
chmod 700 "$FAKE_REPO/secrets/vpnkit-local" "$FAKE_REPO/secrets/vpnkit-local/vibe-vpn" "$FAKE_REPO/secrets/vpnkit-local/openvpn" "$FAKE_REPO/secrets/vpnkit-local/openvpn/client"
printf '%s\n' 'subscription-secret-sentinel' >"$FAKE_REPO/secrets/vpnkit-local/vibe-vpn/sub_url"
printf '%s\n' 'profile-secret-sentinel' >"$FAKE_REPO/secrets/vpnkit-local/openvpn/client/vpnkit-local.ovpn"
chmod 600 "$FAKE_REPO/secrets/vpnkit-local/vibe-vpn/sub_url" "$FAKE_REPO/secrets/vpnkit-local/openvpn/client/vpnkit-local.ovpn"
secret_output=$(
  "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run 2>&1
) || fail 'dry-run with an existing safe local tree failed'
! grep -Fq 'subscription-secret-sentinel' <<<"$secret_output" || fail 'subscription content leaked'
! grep -Fq 'profile-secret-sentinel' <<<"$secret_output" || fail 'profile content leaked'

# Exercise the normal orchestration path against disposable fake underlay,
# lifecycle, NetworkManager, sudo, and Docker frontends. This proves the
# installer creates a private env, starts container-only, imports without an
# NM connect, and emits only bounded status values.
DEFAULT_REPO="$TMP/default-repo"
cp -a "$FAKE_REPO" "$DEFAULT_REPO"
cat >"$DEFAULT_REPO/scripts/vpnkit/vpnkit-local-underlay-routing.sh" <<'EOF_UNDERLAY'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'underlay %s\n' "$*" >>"$MOCK_LOG"
case "${1:-}" in
  plan)
    printf '%s\n' 'command=plan' 'mutation=none' 'physical_uplink_table=ready'
    ;;
  verify) printf '%s\n' 'verify=ok' ;;
  install) printf '%s\n' 'install=ok' ;;
  *) exit 2 ;;
esac
EOF_UNDERLAY
cat >"$DEFAULT_REPO/scripts/vpnkit/vpnkit-local.sh" <<'EOF_LIFECYCLE'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'lifecycle %s\n' "$*" >>"$MOCK_LOG"
case "${1:-}" in
  start)
    mkdir -p "$VPNKIT_LOCAL_SECRETS_DIR/openvpn/client"
    chmod 700 "$VPNKIT_LOCAL_SECRETS_DIR/openvpn" "$VPNKIT_LOCAL_SECRETS_DIR/openvpn/client"
    printf '%s\n' 'profile-secret-sentinel' >"$VPNKIT_LOCAL_SECRETS_DIR/openvpn/client/vpnkit-local.ovpn"
    chmod 600 "$VPNKIT_LOCAL_SECRETS_DIR/openvpn/client/vpnkit-local.ovpn"
    ;;
  status)
    printf '%s\n' '{"container":"healthy","routing_policy":"strict","subscription":"configured","networkmanager":{"configured":"not-managed","active":"not-managed"}}'
    ;;
  *) exit 2 ;;
esac
EOF_LIFECYCLE
cat >"$DEFAULT_REPO/scripts/vpnkit/vpnkit-local-networkmanager.sh" <<'EOF_NM_HELPER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'nm-helper %s\n' "$*" >>"$MOCK_LOG"
case "${1:-}" in
  import) ;;
  status)
    printf '%s\n' 'command=status' 'mutation=none' 'connection=vpnkit-local' 'configured=yes' 'active=no' 'ownership=owned' 'device=none' 'profile=ready' 'private_values=not_printed'
    ;;
  connect) exit 99 ;;
  *) exit 2 ;;
esac
EOF_NM_HELPER
cat >"$MOCK_BIN/sudo" <<'EOF_SUDO_MUTATION_MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'sudo %s\n' "$*" >>"$MOCK_LOG"
[[ "${1:-}" == -- ]] && shift
"$@"
EOF_SUDO_MUTATION_MOCK
chmod 755 "$DEFAULT_REPO/scripts/vpnkit/vpnkit-local-underlay-routing.sh" "$DEFAULT_REPO/scripts/vpnkit/vpnkit-local.sh" "$DEFAULT_REPO/scripts/vpnkit/vpnkit-local-networkmanager.sh" "$MOCK_BIN/sudo"

full_output=$("$DEFAULT_REPO/scripts/vpnkit/vpnkit-local-install.sh" 2>"$TMP/full.err") || {
  cat "$TMP/full.err" >&2
  fail 'mocked normal installer flow failed'
}
grep -Fq 'underlay_install=verified' <<<"$full_output" || fail 'normal flow omitted underlay verification'
grep -Fq 'container_start=complete' <<<"$full_output" || fail 'normal flow omitted container start'
grep -Fq 'profile_activation=manual' <<<"$full_output" || fail 'normal flow did not keep profile activation manual'
grep -Fq 'lifecycle_container=healthy' <<<"$full_output" || fail 'normal flow omitted redacted lifecycle status'
! grep -Fq 'subscription-secret-sentinel' <<<"$full_output" || fail 'normal flow leaked subscription content'
! grep -Fq 'profile-secret-sentinel' <<<"$full_output" || fail 'normal flow leaked profile content'
[[ -f "$DEFAULT_REPO/config/vpnkit-local.local.env" ]] || fail 'normal flow did not create local env'
[[ "$(stat -c '%a %h' -- "$DEFAULT_REPO/config/vpnkit-local.local.env")" == '600 1' ]] || fail 'normal env file permissions are unsafe'
[[ "$(stat -c '%a %h' -- "$DEFAULT_REPO/secrets/vpnkit-local/openvpn/client/vpnkit-local.ovpn")" == '600 1' ]] || fail 'normal profile permissions are unsafe'
grep -Fq 'sudo -- ' "$MOCK_LOG" || fail 'normal flow did not invoke sudo underlay install'
grep -Fq 'lifecycle start' "$MOCK_LOG" || fail 'normal flow did not invoke lifecycle start'
grep -Fq 'nm-helper import --yes' "$MOCK_LOG" || fail 'normal flow did not import NetworkManager profile'
! grep -Fq 'nm-helper connect' "$MOCK_LOG" || fail 'normal flow auto-connected NetworkManager profile'

# Existing env files are checked before parsing. Exercise symlink, mode, and
# hard-link rejection without touching the real repository's ignored config.
external="$TMP/external-env"
printf '%s\n' 'sentinel' >"$external"
ln -s "$external" "$FAKE_REPO/config/vpnkit-local.local.env"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run >"$TMP/link.out" 2>&1; then
  fail 'symlinked env file was accepted'
fi
rm -f -- "$FAKE_REPO/config/vpnkit-local.local.env"

cp -p "$FAKE_REPO/config/vpnkit-local.env.example" "$FAKE_REPO/config/vpnkit-local.local.env"
chmod 644 "$FAKE_REPO/config/vpnkit-local.local.env"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run >"$TMP/mode.out" 2>&1; then
  fail 'world-readable env file was accepted'
fi
rm -f -- "$FAKE_REPO/config/vpnkit-local.local.env"

cp -p "$FAKE_REPO/config/vpnkit-local.env.example" "$external"
ln "$external" "$FAKE_REPO/config/vpnkit-local.local.env"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run >"$TMP/link-count.out" 2>&1; then
  fail 'hard-linked env file was accepted'
fi
[[ "$(stat -c '%h' -- "$external")" == 2 ]] || fail 'hard-link fixture was not created'
rm -f -- "$FAKE_REPO/config/vpnkit-local.local.env"

# The env reader is an assignment parser, not a shell source. A command-like
# value must fail before the fake plan/frontends can run and must not execute.
unsafe_marker="$TMP/unsafe-env-command-ran"
cp -p "$FAKE_REPO/config/vpnkit-local.env.example" "$FAKE_REPO/config/vpnkit-local.local.env"
chmod 600 "$FAKE_REPO/config/vpnkit-local.local.env"
printf 'VPNKIT_LOCAL_POLICY=$(touch %q)\n' "$unsafe_marker" >>"$FAKE_REPO/config/vpnkit-local.local.env"
if "$FAKE_REPO/scripts/vpnkit/vpnkit-local-install.sh" --dry-run >"$TMP/unsafe-env.out" 2>&1; then
  fail 'command-like env assignment was accepted'
fi
[[ ! -e "$unsafe_marker" ]] || fail 'unsafe env assignment executed a command'
! grep -Fq 'should-not-run' "$TMP/unsafe-env.out" || fail 'unsafe env text was echoed'

printf 'vpnkit-local installer tests passed\n'
