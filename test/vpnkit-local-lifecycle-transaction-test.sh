#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
lifecycle="$root/scripts/vpnkit/vpnkit-local.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-transaction.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/secrets/state"
printf 'strict\n' >"$tmp/secrets/state/routing-policy"
chmod 600 "$tmp/secrets/state/routing-policy"
: >"$tmp/docker.log"

cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
case "${1:-}" in
  container|network|volume) exit 0 ;;
  compose)
    # The fixture has no running local container. This still answers every
    # read-only resource preflight so recovery can prove the empty state.
    exit 0
    ;;
  *) exit 99 ;;
esac
EOF
chmod +x "$tmp/bin/docker"

common_env=(
  "PATH=$tmp/bin:$PATH"
  "MOCK_DOCKER_LOG=$tmp/docker.log"
  "VPNKIT_LOCAL_SECRETS_DIR=$tmp/secrets"
  "VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local-transaction"
  "VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false"
  "VPNKIT_LOCAL_TEST_FIXTURE=1"
  "VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS=10"
)
run_lifecycle() {
  env "${common_env[@]}" "$lifecycle" "$@"
}
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

bash -n "$lifecycle"

test -f "$tmp/secrets/state/routing-policy"
run_lifecycle toggle mode >"$tmp/first.out"
[[ $(<"$tmp/secrets/state/routing-policy") == smart ]] || fail 'initial toggle did not commit smart policy'
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]] || fail 'successful toggle left a journal'

# Two toggles must serialize on the same persistent descriptor and therefore
# return to the original policy instead of racing on two stale reads.
printf 'strict\n' >"$tmp/secrets/state/routing-policy"
: >"$tmp/docker.log"
run_lifecycle toggle mode >"$tmp/concurrent-a.out" 2>&1 & a=$!
run_lifecycle toggle mode >"$tmp/concurrent-b.out" 2>&1 & b=$!
wait "$a" || fail 'first concurrent toggle failed'
wait "$b" || fail 'second concurrent toggle failed'
[[ $(<"$tmp/secrets/state/routing-policy") == strict ]] || fail 'concurrent toggles did not end at original policy'
[[ ! -e "$tmp/secrets/state/lifecycle.journal" ]] || fail 'concurrent toggles left a journal'
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.lock")" == 600 ]] || fail 'lifecycle lock mode is not 600'
[[ "$(stat -c '%h' "$tmp/secrets/state/lifecycle.lock")" == 1 ]] || fail 'lifecycle lock is hard-linked'
[[ "$(stat -c '%u' "$tmp/secrets/state/lifecycle.lock")" == "$(id -u)" ]] || fail 'lifecycle lock owner changed'

# A pre-existing lock must be rejected without changing its mode or touching
# the Docker boundary.
chmod 644 "$tmp/secrets/state/lifecycle.lock"
: >"$tmp/docker.log"
if env "${common_env[@]}" VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS=0 "$lifecycle" toggle mode >"$tmp/mode-reject.out" 2>&1; then
  fail 'mode-invalid lifecycle lock was accepted'
fi
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.lock")" == 644 ]] || fail 'mode-invalid lock was changed'
[[ ! -s "$tmp/docker.log" ]] || fail 'mode-invalid lock reached Docker'
chmod 600 "$tmp/secrets/state/lifecycle.lock"

printf 'lock-sentinel\n' >"$tmp/lock-target"
rm -f -- "$tmp/secrets/state/lifecycle.lock"
ln -s -- "$tmp/lock-target" "$tmp/secrets/state/lifecycle.lock"
: >"$tmp/docker.log"
if run_lifecycle toggle mode >"$tmp/lock-symlink.out" 2>&1; then
  fail 'symlink lifecycle lock was accepted'
fi
[[ $(<"$tmp/lock-target") == lock-sentinel ]] || fail 'lock symlink target changed'
[[ ! -s "$tmp/docker.log" ]] || fail 'lock symlink reached Docker'
rm -f -- "$tmp/secrets/state/lifecycle.lock"

# Journal links are rejected before recovery or a new mutation can begin.
printf 'journal-sentinel\n' >"$tmp/journal-target"
ln -s -- "$tmp/journal-target" "$tmp/secrets/state/lifecycle.journal"
: >"$tmp/docker.log"
if run_lifecycle toggle mode >"$tmp/journal-symlink.out" 2>&1; then
  fail 'symlink lifecycle journal was accepted'
fi
[[ $(<"$tmp/journal-target") == journal-sentinel ]] || fail 'journal symlink target changed'
[[ ! -s "$tmp/docker.log" ]] || fail 'journal symlink reached Docker'
rm -f -- "$tmp/secrets/state/lifecycle.journal"

# An unpublished transaction entry that is not a private directory is also a
# fail-closed boundary; orphan cleanup must never follow or silently ignore it.
mkdir -p "$tmp/secrets/state/lifecycle-transactions"
chmod 700 "$tmp/secrets/state/lifecycle-transactions"
printf 'orphan-sentinel\n' >"$tmp/orphan-target"
ln -s -- "$tmp/orphan-target" "$tmp/secrets/state/lifecycle-transactions/txn.bad"
: >"$tmp/docker.log"
if run_lifecycle toggle mode >"$tmp/orphan-symlink.out" 2>&1; then
  fail 'symlink unpublished transaction was accepted'
fi
[[ $(<"$tmp/orphan-target") == orphan-sentinel ]] || fail 'orphan symlink target changed'
[[ ! -s "$tmp/docker.log" ]] || fail 'orphan symlink reached Docker'
rm -f -- "$tmp/secrets/state/lifecycle-transactions/txn.bad"

run_failpoint() {
  env "${common_env[@]}" VPNKIT_LOCAL_LIFECYCLE_FAILPOINT="$1" "$lifecycle" toggle mode
}
assert_no_transaction_artifacts() {
  [[ ! -e "$tmp/secrets/state/lifecycle.journal" ]] || fail 'stale lifecycle journal remains'
  [[ -z "$(find -P "$tmp/secrets/state/lifecycle-transactions" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]] || fail 'stale lifecycle snapshot remains'
}

# SIGKILL bypasses every trap but leaves a durable phase. The next locked
# invocation must recover the exact pre-policy state before toggling.
printf 'strict\n' >"$tmp/secrets/state/routing-policy"
set +e
run_failpoint prepared >"$tmp/sigkill-prepared.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail "prepared failpoint returned $rc instead of SIGKILL status"
[[ -s "$tmp/secrets/state/lifecycle.journal" ]] || fail 'prepared SIGKILL lost its journal'
grep -Fqx 'phase=prepared' "$tmp/secrets/state/lifecycle.journal" || fail 'prepared SIGKILL journal phase was not durable'
! grep -Fq 'subscription.example.invalid' "$tmp/sigkill-prepared.out" || fail 'SIGKILL output leaked a subscription value'
run_lifecycle toggle mode >"$tmp/recovered-prepared.out"
[[ $(<"$tmp/secrets/state/routing-policy") == smart ]] || fail 'prepared journal recovery did not precede toggle'
assert_no_transaction_artifacts
run_lifecycle toggle mode >/dev/null
[[ $(<"$tmp/secrets/state/routing-policy") == strict ]] || fail 'policy did not return to strict after recovery check'

# A kill after the policy-commit phase is also recoverable and leaves the old
# policy bytes visible until the next invocation completes recovery.
set +e
run_failpoint policy-commit >"$tmp/sigkill-policy.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail "policy-commit failpoint returned $rc instead of SIGKILL status"
[[ $(<"$tmp/secrets/state/routing-policy") == strict ]] || fail 'policy changed before policy-commit failpoint'
grep -Fqx 'phase=policy-commit' "$tmp/secrets/state/lifecycle.journal" || fail 'policy-commit journal phase was not durable'
run_lifecycle toggle mode >/dev/null
[[ $(<"$tmp/secrets/state/routing-policy") == smart ]] || fail 'policy-commit journal did not recover before toggle'
assert_no_transaction_artifacts

# Journal mode is a fail-closed boundary and is never repaired implicitly.
set +e
run_failpoint policy-commit >/dev/null 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail 'second policy-commit failpoint did not SIGKILL'
chmod 644 "$tmp/secrets/state/lifecycle.journal"
if run_lifecycle toggle mode >"$tmp/journal-mode.out" 2>&1; then
  fail 'mode-invalid journal was accepted'
fi
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.journal")" == 644 ]] || fail 'mode-invalid journal was changed'
chmod 600 "$tmp/secrets/state/lifecycle.journal"
run_lifecycle toggle mode >/dev/null
assert_no_transaction_artifacts

# A SIGKILL immediately after the durable terminal commit must leave the
# candidate result visible. The next invocation verifies that post-state and
# cleans the terminal journal; it must not restore strict before toggling.
printf 'strict\n' >"$tmp/secrets/state/routing-policy"
set +e
run_failpoint committed >"$tmp/sigkill-committed.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail "committed failpoint returned $rc instead of SIGKILL status"
[[ $(<"$tmp/secrets/state/routing-policy") == smart ]] || fail 'committed failpoint did not leave candidate policy visible'
grep -Fqx 'phase=committed' "$tmp/secrets/state/lifecycle.journal" || fail 'committed SIGKILL journal phase was not durable'
grep -Fqx 'post_ready=yes' "$tmp/secrets/state/lifecycle.journal" || fail 'committed journal did not publish post-state readiness'
run_lifecycle toggle mode >/dev/null
[[ $(<"$tmp/secrets/state/routing-policy") == strict ]] || fail 'committed recovery did not verify candidate before next toggle'
assert_no_transaction_artifacts

# A mismatched committed post-state is fail-closed. It retains the terminal
# journal and leaves the candidate visible instead of restoring strict.
set +e
run_failpoint committed >"$tmp/sigkill-committed-mismatch.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail 'committed mismatch setup did not SIGKILL'
post_state=$(find -P "$tmp/secrets/state/lifecycle-transactions" -name post-state -type f -print -quit)
[[ -n "$post_state" ]] || fail 'committed transaction did not persist post-state'
sed -i 's/^policy_value=smart$/policy_value=strict/' "$post_state"
set +e
run_lifecycle toggle mode >"$tmp/committed-mismatch.out" 2>&1
rc=$?
set -e
[[ "$rc" -ne 0 ]] || fail 'unverifiable committed post-state was accepted'
[[ $(<"$tmp/secrets/state/routing-policy") == smart ]] || fail 'unverifiable committed state was restored to pre-policy'
[[ -s "$tmp/secrets/state/lifecycle.journal" ]] || fail 'unverifiable committed journal was removed'
rm -rf -- "$tmp/secrets/state/lifecycle.journal" "$tmp/secrets/state/lifecycle-transactions"
printf 'strict\n' >"$tmp/secrets/state/routing-policy"

# Stop has its own terminal post-state (no selected container). Verify a
# committed stop before allowing the following stop to enter a new transaction.
set +e
env "${common_env[@]}" VPNKIT_LOCAL_LIFECYCLE_FAILPOINT=committed "$lifecycle" stop >"$tmp/sigkill-stop.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail "committed stop failpoint returned $rc instead of SIGKILL status"
grep -Fqx 'phase=committed' "$tmp/secrets/state/lifecycle.journal" || fail 'committed stop journal phase was not durable'
grep -Fqx 'post_ready=yes' "$tmp/secrets/state/lifecycle.journal" || fail 'committed stop journal lacked post-state'
run_lifecycle stop >/dev/null
assert_no_transaction_artifacts

# TERM during the pre-mutation Compose read is forwarded from the secure lock
# launcher to the locked child; its one-shot trap removes the preparing journal.
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
if [[ " $* " == *' compose '* && " $* " == *' ps -q vpnkit '* ]]; then sleep 5; exit 0; fi
case "${1:-}" in container|network|volume) exit 0 ;; *) exit 99 ;; esac
EOF
chmod +x "$tmp/bin/docker"
rm -f -- "$tmp/secrets/state/lifecycle.journal"
env "${common_env[@]}" "$lifecycle" toggle mode >"$tmp/term.out" 2>&1 & term_pid=$!
for _ in $(seq 1 100); do [[ -e "$tmp/secrets/state/lifecycle.journal" ]] && break; sleep 0.02; done
# Allow the locked child to install its traps after the durable publish before
# delivering the signal; the actual handler remains one-shot and bounded.
sleep 0.2
kill -TERM "$term_pid"
set +e
wait "$term_pid"
term_rc=$?
set -e
[[ "$term_rc" -eq 143 ]] || fail "TERM returned $term_rc instead of 143"
assert_no_transaction_artifacts
! grep -Fq 'subscription.example.invalid' "$tmp/term.out" || fail 'TERM output leaked a subscription value'

# Start followed by stop is serialized at the same outer lock. Launch stop
# only after start has durably published its snapshot; the final state must be
# stopped, never a half-started stack.
startstop_root="$tmp/startstop"
mkdir -p "$startstop_root/bin" "$startstop_root/secrets/vibe-vpn"
printf 'https://subscription.example.invalid/start-stop\n' >"$startstop_root/secrets/vibe-vpn/sub_url"
chmod 600 "$startstop_root/secrets/vibe-vpn/sub_url"
printf 'absent\n' >"$startstop_root/state"
cat >"$startstop_root/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
state=$(<"${MOCK_STARTSTOP_STATE:?}")
project=${MOCK_STARTSTOP_PROJECT:?}
joined="$*"
printf '%s %s\n' "$state" "$joined" >>"${MOCK_STARTSTOP_LOG:?}"
if [[ "${1:-}" == container || "${1:-}" == network || "${1:-}" == volume ]]; then
  if [[ "${1:-}" == container && "$state" == running && ( "$joined" == *"label=com.docker.compose.project=$project"* || "$joined" == *'name='* ) ]]; then
    printf 'startstop-cid\n'
  fi
  exit 0
fi
if [[ "$joined" == *'compose'*'ps -q vpnkit'* ]]; then
  [[ "$state" == running ]] && printf 'startstop-cid\n'
  exit 0
fi
if [[ "${1:-}" == inspect ]]; then
  if [[ "$joined" == *State.Running* ]]; then printf 'healthy\n'
  elif [[ "$joined" == *'{{.Name}}'* ]]; then printf '/%s-vpnkit-1\n' "$project"
  elif [[ "$joined" == *working_dir* ]]; then printf '%s\n' "${MOCK_STARTSTOP_WORKDIR:?}"
  elif [[ "$joined" == *com.vpnkit.local.owner* ]]; then printf 'local-lifecycle\n'
  elif [[ "$joined" == *com.docker.compose.project* ]]; then printf '%s\n' "$project"
  fi
  exit 0
fi
if [[ "$joined" == *'compose'*' up -d --pull never vpnkit'* ]]; then
  sleep 0.2
  printf 'running\n' >"$MOCK_STARTSTOP_STATE"
  exit 0
fi
if [[ "$joined" == *'compose'*' down --remove-orphans'* ]]; then
  printf 'absent\n' >"$MOCK_STARTSTOP_STATE"
  exit 0
fi
exit 99
EOF
chmod +x "$startstop_root/bin/docker"
startstop_env=(
  "PATH=$startstop_root/bin:$PATH"
  "MOCK_STARTSTOP_STATE=$startstop_root/state"
  "MOCK_STARTSTOP_PROJECT=vpnkit-local-startstop"
  "MOCK_STARTSTOP_LOG=$startstop_root/docker.log"
  "MOCK_STARTSTOP_WORKDIR=$root"
  "VPNKIT_LOCAL_SECRETS_DIR=$startstop_root/secrets"
  "VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local-startstop"
  "VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false"
  "VPNKIT_LOCAL_TEST_FIXTURE=1"
  "VPNKIT_RULESET_SOURCE_MODE=local-fixture"
  "VPNKIT_SELECTED_OUTBOUND_MODE=direct-fixture"
  "VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION=true"
  "VPNKIT_LOCAL_BOOT_TIMEOUT_SECONDS=5"
  "VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS=10"
)
env "${startstop_env[@]}" "$lifecycle" start >"$startstop_root/start.out" 2>&1 & start_pid=$!
for _ in $(seq 1 200); do [[ -e "$startstop_root/secrets/state/lifecycle.journal" ]] && break; sleep 0.02; done
env "${startstop_env[@]}" "$lifecycle" stop >"$startstop_root/stop.out" 2>&1 & stop_pid=$!
wait "$start_pid" || { cat "$startstop_root/start.out" >&2; fail 'concurrent start failed'; }
wait "$stop_pid" || { cat "$startstop_root/stop.out" >&2; fail 'concurrent stop failed'; }
[[ $(<"$startstop_root/state") == absent ]] || fail 'concurrent start/stop did not end stopped'
[[ ! -e "$startstop_root/secrets/state/lifecycle.journal" ]] || fail 'concurrent start/stop left a journal'
! grep -Eiq 'start-stop|subscription\.example' "$startstop_root/start.out" "$startstop_root/stop.out" || fail 'start/stop output leaked private fixture values'

# Start also publishes a terminal healthy-container post-state before the
# committed failpoint. Recovery must verify that state, then the next stop may
# perform its own mutation.
set +e
env "${startstop_env[@]}" VPNKIT_LOCAL_LIFECYCLE_FAILPOINT=committed "$lifecycle" start >"$startstop_root/sigkill-start.out" 2>&1
rc=$?
set -e
[[ "$rc" -eq 137 ]] || fail "committed start failpoint returned $rc instead of SIGKILL status"
[[ $(<"$startstop_root/state") == running ]] || fail 'committed start did not leave the candidate runtime visible'
grep -Fqx 'phase=committed' "$startstop_root/secrets/state/lifecycle.journal" || fail 'committed start journal phase was not durable'
env "${startstop_env[@]}" "$lifecycle" stop >"$startstop_root/recover-start-stop.out" 2>&1 || {
  cat "$startstop_root/recover-start-stop.out" >&2
  fail 'committed start recovery did not permit the following stop'
}
[[ $(<"$startstop_root/state") == absent ]] || fail 'committed start recovery/stop did not end stopped'
[[ ! -e "$startstop_root/secrets/state/lifecycle.journal" ]] || fail 'committed start recovery left a journal'

printf 'vpnkit local lifecycle lock/journal transaction tests passed\n'
