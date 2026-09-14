#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
lifecycle="$root/scripts/vpnkit/vpnkit-local.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-retest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/secrets"
printf '1\n' >"$tmp/generation"

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
if [[ "$1" == inspect ]]; then
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
if [[ "$1" == exec* || "$*" == exec* ]]; then
  if [[ "$*" == *'test ! -e'* ]]; then
    [[ ! -e "$MOCK_REQUEST" ]]
    exit
  fi
  if [[ "$*" == *'cat '* ]]; then
    cat "$MOCK_GENERATION"
    exit 0
  fi
  if [[ "$*" == *'vibe-vpn --config'* ]]; then
    if [[ ${MOCK_RESTART_SUCCEEDS:-1} == 1 ]]; then
      next=$(( $(<"$MOCK_GENERATION") + 1 ))
      printf '%s\n' "$next" >"$MOCK_GENERATION"
      rm -f "$MOCK_REQUEST"
    fi
    exit 0
  fi
fi
exit 99
EOF
chmod +x "$tmp/bin/docker"
export PATH="$tmp/bin:$PATH"
export MOCK_GENERATION="$tmp/generation" MOCK_REQUEST="$tmp/request"
export MOCK_DOCKER_LOG="$tmp/docker.log" MOCK_WORKDIR="$root"
# Keep the durable lifecycle journal/lock inside this throwaway fixture; the
# stateful collision case intentionally retains its journal for fail-closed
# recovery and must not pollute the repository's ignored local runtime.
export VPNKIT_LOCAL_SECRETS_DIR="$tmp/secrets"
export VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false
export VPNKIT_LOCAL_RETEST_TIMEOUT_SECONDS=2
export VPNKIT_LOCAL_SINGBOX_GENERATION_FILE=/run/vpnkit/sing-box-generation
export VPNKIT_LOCAL_SINGBOX_RESTART_FILE=/run/vpnkit/restart-sing-box

# A successful pick is not accepted until the request has disappeared and the
# generation changed; the mock performs both transitions on the pick call.
output=$(bash "$lifecycle" retest select)
grep -Fxq 'vpnkit_local_retest_select=ok' <<<"$output"
grep -Fxq 'selection_details=redacted' <<<"$output"
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]]
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.lock")" == 600 ]]
[[ "$(stat -c '%h' "$tmp/secrets/state/lifecycle.lock")" == 1 ]]

# The terminal committed phase is exercised after retest has consumed a new
# generation. The next retest verifies that post-state before starting its own
# generation transition; it must never restore the pre-pick generation.
set +e
VPNKIT_LOCAL_TEST_FIXTURE=1 VPNKIT_LOCAL_LIFECYCLE_FAILPOINT=committed bash "$lifecycle" retest select >"$tmp/committed-retest.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || { echo "committed retest failpoint returned $rc" >&2; exit 1; }
[[ $(<"$tmp/generation") == 3 ]] || { echo 'committed retest did not leave the post-pick generation visible' >&2; exit 1; }
grep -Fqx 'phase=committed' "$tmp/secrets/state/lifecycle.journal"
grep -Fqx 'post_ready=yes' "$tmp/secrets/state/lifecycle.journal"
output=$(bash "$lifecycle" retest select)
grep -Fxq 'vpnkit_local_retest_select=ok' <<<"$output"
[[ $(<"$tmp/generation") == 4 ]]
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]]

# A completed pick with no runtime generation change must fail boundedly.
printf '1\n' >"$tmp/generation"
rm -f "$tmp/request"
if MOCK_RESTART_SUCCEEDS=0 bash "$lifecycle" retest select >/dev/null 2>&1; then
  echo 'retest accepted an unchanged runtime generation' >&2
  exit 1
fi

# A permitted project name is not enough to authorize a retest exec. Foreign
# and missing-owner labels must be rejected before the container is touched.
mkdir -p "$tmp/foreign-bin"
cat >"$tmp/foreign-bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"$MOCK_FOREIGN_DOCKER_LOG"
if [[ "${1:-}" == container || "${1:-}" == network || "${1:-}" == volume ]]; then exit 0; fi
if [[ "$*" == *'compose'*'ps -q vpnkit'* ]]; then printf 'foreign-cid\n'; exit 0; fi
if [[ "${1:-}" == inspect ]]; then
  joined="$*"
  if [[ "$joined" == *'State.Running'* ]]; then
    printf 'healthy\n'
  elif [[ "$joined" == *'{{.Name}}'* ]]; then
    if [[ "${MOCK_OWNER_MODE:-foreign}" == wrong-name ]]; then
      printf '/vpnkit-local-not-the-vpnkit-service\n'
    else
      printf '/vpnkit-local-vpnkit-1\n'
    fi
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project.working_dir'* ]]; then
    printf '%s\n' "$MOCK_WORKDIR"
  elif [[ "$joined" == *'Config.Labels'*'com.vpnkit.local.owner'* ]]; then
    [[ "${MOCK_OWNER_MODE:-foreign}" == missing ]] && printf '\n' || printf 'foreign-owner\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project'* ]]; then
    printf 'vpnkit-local\n'
  fi
  exit 0
fi
if [[ "${1:-}" == exec* || "$*" == *' exec '* ]]; then
  printf 'EXEC_MUTATION\n' >>"$MOCK_FOREIGN_DOCKER_LOG"
fi
exit 0
EOF
chmod +x "$tmp/foreign-bin/docker"
for owner_mode in foreign missing wrong-name; do
  : >"$tmp/foreign-docker.log"
  if PATH="$tmp/foreign-bin:$PATH" MOCK_FOREIGN_DOCKER_LOG="$tmp/foreign-docker.log" \
      MOCK_OWNER_MODE="$owner_mode" MOCK_WORKDIR="$root" "$lifecycle" retest select >"$tmp/foreign-$owner_mode.out" 2>&1; then
    echo "unsafe retest owner mode was accepted: $owner_mode" >&2
    exit 1
  fi
  ! grep -Fq EXEC_MUTATION "$tmp/foreign-docker.log"
done

# REV-001: after the request/generation preparation reads, a same-project,
# same-owner/same-workdir container can appear with an unexpected name. The
# final preflight must reject before the mutating pick exec.
stateful_bin="$tmp/stateful-bin"
mkdir -p "$stateful_bin"
printf 'initial\n' >"$tmp/docker-state"
cat >"$stateful_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
state=$(<"$MOCK_DOCKER_STATE")
joined="$*"
printf 'CALL phase=%s %s\n' "$state" "$joined" >>"$MOCK_DOCKER_LOG"
project=${MOCK_PROJECT:?}
if [[ "${1:-}" == container || "${1:-}" == network || "${1:-}" == volume ]]; then
  if [[ "${1:-}" == container && "${joined}" == *"label=com.docker.compose.project=$project"* ]]; then
    if [[ "$state" == injected ]]; then printf 'unexpected-container\n'; else printf 'owned-container\n'; fi
  fi
  exit 0
fi
if [[ "$joined" == *'compose'*'ps -q vpnkit'* ]]; then
  if [[ "$state" == injected ]]; then printf 'unexpected-container\n'; else printf 'owned-container\n'; fi
  exit 0
fi
if [[ "${1:-}" == inspect ]]; then
  id=${!#}
  if [[ "$joined" == *'State.Running'* ]]; then printf 'healthy\n'
  elif [[ "$joined" == *'{{.Name}}'* ]]; then
    if [[ "$state" == injected ]]; then printf '/%s-not-vpnkit\n' "$project"; else printf '/%s-vpnkit-1\n' "$project"; fi
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project.working_dir'* ]]; then printf '%s\n' "$MOCK_WORKDIR"
  elif [[ "$joined" == *'Config.Labels'*'com.vpnkit.local.owner'* ]]; then printf 'local-lifecycle\n'
  elif [[ "$joined" == *'Config.Labels'*'com.docker.compose.project'* ]]; then printf '%s\n' "$project"
  fi
  exit 0
fi
if [[ "${1:-}" == exec ]]; then
  if [[ "$state" == injected ]]; then printf 'EXEC_AFTER_INJECT\n' >>"$MOCK_DOCKER_LOG"; else printf 'EXEC_INITIAL\n' >>"$MOCK_DOCKER_LOG"; fi
  if [[ "$joined" == *'test ! -e'* ]]; then exit 0; fi
  if [[ "$joined" == *'cat '* ]]; then
    cat "$MOCK_GENERATION"
    printf 'injected\n' >"$MOCK_DOCKER_STATE"
    exit 0
  fi
  if [[ "$joined" == *'vibe-vpn --config'* ]]; then
    printf 'PICK_EXEC\n' >>"$MOCK_DOCKER_LOG"
    exit 0
  fi
fi
exit 99
EOF
chmod +x "$stateful_bin/docker"
: >"$tmp/stateful-docker.log"
if PATH="$stateful_bin:$PATH" MOCK_DOCKER_STATE="$tmp/docker-state" \
    MOCK_DOCKER_LOG="$tmp/stateful-docker.log" MOCK_PROJECT=vpnkit-local-retest-race \
    MOCK_WORKDIR="$root" VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local-retest-race \
    "$lifecycle" retest select >"$tmp/stateful.out" 2>&1; then
  echo 'post-preparation ownership collision was accepted by retest' >&2
  exit 1
fi
grep -Fq 'EXEC_INITIAL' "$tmp/stateful-docker.log"
! grep -Fq 'EXEC_AFTER_INJECT' "$tmp/stateful-docker.log"
! grep -Fq 'PICK_EXEC' "$tmp/stateful-docker.log"

printf 'vpnkit local retest generation mock tests passed\n'
