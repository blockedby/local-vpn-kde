#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
lifecycle="$root/scripts/vpnkit/vpnkit-local.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-toggle.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/secrets/vibe-vpn"
printf 'https://subscription.example.invalid/test-only\n' >"$tmp/secrets/vibe-vpn/sub_url"
chmod 600 "$tmp/secrets/vibe-vpn/sub_url"
printf '0\n' >"$tmp/up-count"

cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"$MOCK_DOCKER_LOG"
if [[ "${1:-}" == container || "${1:-}" == network || "${1:-}" == volume ]]; then
  exit 0
fi
if [[ "$*" == *'compose'*'ps -q vpnkit'* ]]; then
  printf 'local-cid\n'
  exit 0
fi
if [[ "${1:-}" == inspect ]]; then
  joined="$*"
  if [[ "$joined" == *'State.Running'* ]]; then
    printf 'healthy\n'
  elif [[ "$joined" == *'{{.Name}}'* ]]; then
    printf '/vpnkit-local-vpnkit-1\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project.working_dir'* ]]; then
    printf '%s\n' "$MOCK_WORKDIR"
  elif [[ "$joined" == *'Config.Labels'*'com.vpnkit.local.owner'* ]]; then
    printf 'local-lifecycle\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project'* ]]; then
    printf 'vpnkit-local\n'
  fi
  exit 0
fi
if [[ "$*" == *'compose'*'up -d --pull never --force-recreate vpnkit'* ]]; then
  count=$(cat "$MOCK_UP_COUNT")
  count=$((count + 1))
  printf '%s\n' "$count" >"$MOCK_UP_COUNT"
  # First call is the candidate policy; rollback recreation succeeds.
  [[ "$count" -ne 1 ]]
  exit
fi
if [[ "$*" == *'compose'*'down --remove-orphans'* ]]; then
  exit 0
fi
exit 99
EOF
chmod +x "$tmp/bin/docker"
export PATH="$tmp/bin:$PATH" MOCK_UP_COUNT="$tmp/up-count"
export VPNKIT_LOCAL_SECRETS_DIR="$tmp/secrets"
export VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false
export MOCK_DOCKER_LOG="$tmp/docker.log" MOCK_WORKDIR="$root"
export VPNKIT_RULESET_SOURCE_MODE=local-fixture
export VPNKIT_LOCAL_POLICY=strict
export VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS=2

if bash "$lifecycle" toggle mode >"$tmp/output" 2>&1; then
  echo 'failed policy recreation unexpectedly succeeded' >&2
  exit 1
fi
[[ ! -e "$tmp/secrets/state/routing-policy" ]]
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]]
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.lock")" == 600 ]]
[[ "$(stat -c '%h' "$tmp/secrets/state/lifecycle.lock")" == 1 ]]
[[ $(<"$tmp/up-count") == 2 ]]
grep -Fq 'prior policy/runtime was restored' "$tmp/output"
! grep -Eiq 'test-only|subscription\.example' "$tmp/output"

# A stage chmod failure must leave the exact old policy inode bytes and mode;
# the candidate runtime is compensated without ever committing `smart`.
mkdir -p "$tmp/secrets/state"
printf 'strict\n' >"$tmp/secrets/state/routing-policy"
chmod 640 "$tmp/secrets/state/routing-policy"
old_hash=$(sha256sum "$tmp/secrets/state/routing-policy" | awk '{print $1}')
old_mode=$(stat -c '%a' "$tmp/secrets/state/routing-policy")
mkdir -p "$tmp/fail-bin"
cat >"$tmp/fail-bin/chmod" <<'EOF'
#!/usr/bin/env bash
for arg in "$@"; do
  [[ "$arg" == */.routing-policy.* ]] && exit 77
done
exec /bin/chmod "$@"
EOF
/bin/chmod +x "$tmp/fail-bin/chmod"
: >"$tmp/docker.log"
if PATH="$tmp/fail-bin:$PATH" "$lifecycle" toggle mode >"$tmp/chmod-failure.out" 2>&1; then
  echo 'toggle accepted an injected policy chmod failure' >&2
  exit 1
fi
[[ $(sha256sum "$tmp/secrets/state/routing-policy" | awk '{print $1}') == "$old_hash" ]]
[[ $(stat -c '%a' "$tmp/secrets/state/routing-policy") == "$old_mode" ]]
! compgen -G "$tmp/secrets/state/.routing-policy.*" >/dev/null
! grep -Eiq 'test-only|subscription\.example' "$tmp/chmod-failure.out"

# A final atomic rename failure also leaves the old bytes and mode unchanged.
cat >"$tmp/fail-bin/mv" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
target=${!#}
[[ "$target" == */state/routing-policy ]] && exit 78
exec /bin/mv "$@"
EOF
/bin/chmod +x "$tmp/fail-bin/mv"
: >"$tmp/docker.log"
if PATH="$tmp/fail-bin:$PATH" "$lifecycle" toggle mode >"$tmp/mv-failure.out" 2>&1; then
  echo 'toggle accepted an injected policy rename failure' >&2
  exit 1
fi
[[ $(sha256sum "$tmp/secrets/state/routing-policy" | awk '{print $1}') == "$old_hash" ]]
[[ $(stat -c '%a' "$tmp/secrets/state/routing-policy") == "$old_mode" ]]
! compgen -G "$tmp/secrets/state/.routing-policy.*" >/dev/null

# An allowed project name does not authorize a foreign or missing-owner
# container. The toggle must reject before either candidate or rollback
# Compose recreation.
mkdir -p "$tmp/foreign-bin"
cat >"$tmp/foreign-bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"$MOCK_DOCKER_LOG"
if [[ "${1:-}" == container || "${1:-}" == network || "${1:-}" == volume ]]; then exit 0; fi
if [[ "$*" == *'compose'*'ps -q vpnkit'* ]]; then printf 'foreign-cid\n'; exit 0; fi
if [[ "${1:-}" == inspect ]]; then
  joined="$*"
  if [[ "$joined" == *'State.Running'* ]]; then
    printf 'healthy\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project.working_dir'* ]]; then
    printf '%s\n' "$MOCK_WORKDIR"
  elif [[ "$joined" == *'Config.Labels'*'com.vpnkit.local.owner'* ]]; then
    [[ "${MOCK_OWNER_MODE:-foreign}" == missing ]] && printf '\n' || printf 'foreign-owner\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project'* ]]; then
    printf 'vpnkit-local\n'
  fi
  exit 0
fi
if [[ "$*" == *'compose'*'up '* || "$*" == *'compose'*'down '* ]]; then
  printf 'COMPOSE_MUTATION\n' >>"$MOCK_DOCKER_LOG"
fi
exit 0
EOF
chmod +x "$tmp/foreign-bin/docker"
for owner_mode in foreign missing; do
  : >"$tmp/foreign-docker.log"
  if PATH="$tmp/foreign-bin:$PATH" MOCK_DOCKER_LOG="$tmp/foreign-docker.log" \
      MOCK_OWNER_MODE="$owner_mode" MOCK_WORKDIR="$root" "$lifecycle" toggle mode >"$tmp/toggle-$owner_mode.out" 2>&1; then
    echo "unsafe toggle owner mode was accepted: $owner_mode" >&2
    exit 1
  fi
  ! grep -Fq COMPOSE_MUTATION "$tmp/foreign-docker.log"
done

# A policy symlink is rejected without following or replacing its target.
printf 'strict\n' >"$tmp/policy-target"
rm -f "$tmp/secrets/state/routing-policy"
ln -s "$tmp/policy-target" "$tmp/secrets/state/routing-policy"
: >"$tmp/docker.log"
if "$lifecycle" toggle mode >"$tmp/symlink.out" 2>&1; then
  echo 'toggle followed a policy symlink' >&2
  exit 1
fi
[[ $(<"$tmp/policy-target") == strict ]]
[[ ! -s "$tmp/docker.log" ]]
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]]
printf 'vpnkit local toggle transaction mock tests passed\n'
