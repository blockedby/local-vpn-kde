#!/usr/bin/env bash
set -Eeuo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vpnkit-local-servers.XXXXXX")
cleanup() {
  if [[ -d "$tmp/state" ]]; then
    while IFS= read -r pid; do
      [[ "$pid" =~ ^[1-9][0-9]*$ ]] || continue
      kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    done < <(find "$tmp/state" -type f -name '*.state' -exec awk '{print $5}' {} \; 2>/dev/null || true)
  fi
  rm -rf -- "$tmp"
}
trap cleanup EXIT
mkdir -p "$tmp/bin" "$tmp/secrets/vibe-vpn" "$tmp/state"
: >"$tmp/docker.log"
printf 'owned\n' >"$tmp/resource-mode"

cat >"$tmp/fake-worker.py" <<'PY'
#!/usr/bin/env python3
import os
import pathlib
import signal
import subprocess
import sys
import time

parent_pid, child_pid, compensation, survivor = map(pathlib.Path, sys.argv[1:])

def on_term(_signum, _frame):
    compensation.touch()

signal.signal(signal.SIGTERM, on_term)
parent_pid.write_text(str(os.getpid()), encoding="ascii")
child_code = (
    "import os,pathlib,signal,sys,time; "
    "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
    "pathlib.Path(sys.argv[1]).write_text(str(os.getpid()), encoding='ascii'); "
    "time.sleep(0.8); pathlib.Path(sys.argv[2]).touch(); time.sleep(30)"
)
subprocess.Popen(
    [sys.executable, "-c", child_code, str(child_pid), str(survivor)],
    stdin=subprocess.DEVNULL,
    stdout=subprocess.DEVNULL,
    stderr=subprocess.DEVNULL,
)
while not child_pid.exists():
    time.sleep(0.005)
while True:
    time.sleep(1)
PY
chmod 700 "$tmp/fake-worker.py"

cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
{
  printf 'docker'
  printf ' <%s>' "$@"
  printf '\n'
} >>"$MOCK_DOCKER_LOG"
project=${VPNKIT_LOCAL_COMPOSE_PROJECT:-vpnkit-local-test-server}
mode=$(<"$MOCK_RESOURCE_MODE_FILE")
joined=" $* "
helper=/usr/local/bin/vibe-vpn-server-op-supervisor

group_live() {
  python3 - "$1" <<'PY'
import pathlib,sys
pgid=int(sys.argv[1])
for stat_path in pathlib.Path('/proc').glob('[0-9]*/stat'):
    try:
        raw=stat_path.read_text(encoding='ascii')
        tail=raw[raw.rfind(')')+2:].split()
        if tail[0] != 'Z' and int(tail[2]) == pgid and int(tail[3]) == pgid:
            raise SystemExit(0)
    except (OSError, ValueError, IndexError):
        pass
raise SystemExit(1)
PY
}
process_start() {
  python3 - "$1" <<'PY'
import pathlib,sys
raw=pathlib.Path('/proc',sys.argv[1],'stat').read_text(encoding='ascii')
tail=raw[raw.rfind(')')+2:].split()
print(tail[19])
PY
}

if [[ "${1:-}" == container && "${2:-}" == ls ]]; then
  if [[ "$joined" == *" id=owned-container "* ]]; then
    [[ "$mode" == absent ]] || printf 'owned-container\n'
  elif [[ "$joined" == *" label=com.docker.compose.project=$project "* ]]; then
    [[ "$mode" == absent ]] || { [[ "$mode" == replacement ]] && printf 'new-container\n' || printf 'owned-container\n'; }
  elif [[ "$joined" == *" name=^/${project}-vpnkit-1$ "* ]]; then
    [[ "$mode" == absent ]] || { [[ "$mode" == replacement ]] && printf 'new-container\n' || printf 'owned-container\n'; }
  fi
  exit 0
fi
if [[ "${1:-}" == network && "${2:-}" == ls ]]; then
  [[ "$mode" == absent ]] || printf 'owned-network\n'
  exit 0
fi
if [[ "${1:-}" == volume && "${2:-}" == ls ]]; then
  [[ "$mode" == absent ]] || printf 'owned-volume\n'
  exit 0
fi
if [[ "$joined" == *' compose '*' ps -q vpnkit '* ]]; then
  [[ "$mode" == absent ]] || { [[ "$mode" == replacement ]] && printf 'new-container\n' || printf 'owned-container\n'; }
  exit 0
fi
if [[ "${1:-}" == inspect ]]; then
  id=${*: -1}
  [[ "$mode" != absent ]] || exit 1
  case "$joined" in
    *'State.Running'*) [[ "$mode" == unhealthy ]] && printf 'unhealthy\n' || printf 'healthy\n' ;;
    *'{{.Name}}'*)
      case "$id" in
        owned-container|new-container) printf '/%s-vpnkit-1\n' "$project" ;;
        owned-network) printf '%s_vpnkit-local\n' "$project" ;;
        owned-volume) printf '%s_vpnkit-local-vibe-vpn-state\n' "$project" ;;
      esac
      ;;
    *'com.docker.compose.project.working_dir'*) printf '%s\n' "$MOCK_WORKDIR" ;;
    *'com.vpnkit.local.owner'*) [[ "$mode" == foreign ]] && printf 'foreign-owner\n' || printf 'local-lifecycle\n' ;;
    *'com.docker.compose.project"'*) printf '%s\n' "$project" ;;
    *'com.docker.compose.network'*) printf 'vpnkit-local\n' ;;
    *'com.docker.compose.volume'*) printf 'vpnkit-local-vibe-vpn-state\n' ;;
    *) exit 91 ;;
  esac
  exit 0
fi
if [[ "${1:-}" != exec ]]; then
  exit 98
fi
[[ "${2:-}" == owned-container || "${2:-}" == new-container ]] || exit 95
[[ "${3:-}" == "$helper" ]] || exit 96
command=${4:-}
token=${5:-}
[[ "$token" =~ ^op_[A-Za-z0-9_-]{43}$ ]] || exit 97
state="$MOCK_CONTAINER_STATE_DIR/$token.state"
case "$command" in
  prepare|run)
    operation=${6:-}
    server_id=${7:-}
    case "$operation:$#" in
      list:6|test-all:6|current:6) server_id= ;;
      check-batch:8) exit 2 ;;
      ping:7|select:7) [[ "$server_id" =~ ^srv_[A-Za-z0-9_-]{27}$ ]] || exit 92 ;;
      *) exit 93 ;;
    esac
    ;;
  cancel|verify) [[ $# -eq 5 ]] || exit 94 ;;
  *) exit 94 ;;
esac

case "$command" in
  prepare)
    [[ ! -e "$state" ]] || exit 40
    printf '%s %s %s prepared 0 0 0\n' "$token" "$operation" "${server_id:--}" >"$state"
    chmod 600 "$state"
    ;;
  run)
    read -r saved_token saved_operation saved_id status pid start client <"$state"
    [[ "$saved_token" == "$token" && "$saved_operation" == "$operation" && "$saved_id" == "${server_id:--}" && "$status" == prepared ]] || exit 41
    if [[ ! -e "$MOCK_LIFECYCLE_LOCK" ]] || flock -n "$MOCK_LIFECYCLE_LOCK" true 2>/dev/null; then
      printf 'exec-lock=free operation=%s\n' "$operation" >>"$MOCK_DOCKER_LOG"
    else
      printf 'exec-lock=held operation=%s\n' "$operation" >>"$MOCK_DOCKER_LOG"
    fi
    if [[ "${MOCK_EXEC_SLEEP:-0}" != 1 ]]; then
      if [[ "$operation" == list || "$operation" == test-all ]]; then
        printf '{"schema":"vibe-vpn.server-browser.v2","status":"ok","generation":4,"servers":[]}\n'
      else
        printf '{"schema":"vibe-vpn.server-browser.v2","status":"unavailable","generation":0}\n'
      fi
      rm -f -- "$state"
      exit 0
    fi
    parent_pid="$MOCK_CONTAINER_STATE_DIR/$token.parent-pid"
    child_pid="$MOCK_CONTAINER_STATE_DIR/$token.child-pid"
    compensation="$MOCK_CONTAINER_STATE_DIR/$token.compensated"
    survivor="$MOCK_CONTAINER_STATE_DIR/$token.survivor"
    setsid "$MOCK_WORKER_SCRIPT" "$parent_pid" "$child_pid" "$compensation" "$survivor" >/dev/null 2>"$MOCK_CONTAINER_STATE_DIR/private-worker-stderr" &
    worker_pid=$!
    worker_start=$(process_start "$worker_pid")
    printf '%s %s %s running %s %s %s\n' "$token" "$operation" "${server_id:--}" "$worker_pid" "$worker_start" "$$" >"$state"
    printf '%s\n' "$$" >"$MOCK_CONTAINER_STATE_DIR/$token.client-pid"
    wait "$worker_pid"
    ;;
  cancel)
    [[ -e "$state" ]] || exit 0
    read -r saved_token operation saved_id status pid start client <"$state"
    [[ "$saved_token" == "$token" ]] || exit 42
    if [[ "$status" == prepared ]]; then
      printf '%s %s %s closed 0 0 0\n' "$token" "$operation" "$saved_id" >"$state"
      exit 0
    fi
    [[ "$status" == running || "$status" == cancelling || "$status" == closed ]] || exit 43
    if [[ "$pid" != 0 ]] && group_live "$pid"; then
      current_start=$(process_start "$pid") || exit 44
      [[ "$current_start" == "$start" ]] || exit 45
      printf 'cancel token=%s pid=%s start=%s\n' "$token" "$pid" "$start" >>"$MOCK_DOCKER_LOG"
      printf '%s %s %s cancelling %s %s %s\n' "$token" "$operation" "$saved_id" "$pid" "$start" "$client" >"$state"
      kill -TERM -- "-$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        group_live "$pid" || break
        sleep 0.02
      done
      group_live "$pid" && kill -KILL -- "-$pid" 2>/dev/null || true
      for _ in {1..200}; do
        group_live "$pid" || break
        sleep 0.01
      done
      group_live "$pid" && exit 46
    fi
    printf '%s %s %s closed %s %s %s\n' "$token" "$operation" "$saved_id" "$pid" "$start" "$client" >"$state"
    ;;
  verify)
    [[ -e "$state" ]] || exit 0
    read -r saved_token operation saved_id status pid start client <"$state"
    [[ "$saved_token" == "$token" ]] || exit 42
    if [[ "$pid" != 0 ]] && group_live "$pid"; then
      exit 3
    fi
    [[ "$status" != prepared && "$status" != running && "$status" != cancelling ]] || exit 3
    rm -f -- "$state"
    ;;
esac
EOF
chmod 700 "$tmp/bin/docker"

lifecycle="$repo_root/scripts/vpnkit/vpnkit-local.sh"
export PATH="$tmp/bin:$PATH"
export MOCK_DOCKER_LOG="$tmp/docker.log"
export MOCK_WORKDIR="$repo_root"
export MOCK_LIFECYCLE_LOCK="$tmp/secrets/state/lifecycle.lock"
export MOCK_CONTAINER_STATE_DIR="$tmp/state"
export MOCK_RESOURCE_MODE_FILE="$tmp/resource-mode"
export MOCK_WORKER_SCRIPT="$tmp/fake-worker.py"
export VPNKIT_LOCAL_SECRETS_DIR="$tmp/secrets"
export VPNKIT_LOCAL_COMPOSE_PROJECT=vpnkit-local-test-server
export VPNKIT_LOCAL_MANAGE_NETWORKMANAGER=false
export VPNKIT_LOCAL_TEST_FIXTURE=1
# Operator settings must never redirect this fixture into live state.
export VPNKIT_LOCAL_ENV_FILE=/dev/null

bash -n "$lifecycle"
server_id="srv_$(printf 'A%.0s' {1..27})"

# Hostile IDs and generic/extra arguments are rejected before any Docker probe.
for args in \
  "servers ping srv_short" \
  "servers ping ${server_id};" \
  "servers select ../../private" \
  "servers list --json" \
  "servers exec docker ps" \
  "servers current extra"; do
  : >"$tmp/docker.log"
  if $lifecycle $args >"$tmp/out" 2>"$tmp/err"; then
    echo "unsafe server arguments were accepted" >&2
    exit 1
  fi
  [[ ! -s "$tmp/docker.log" ]]
done

# Every operation uses one opaque token and only fixed prepare/run/verify helper
# argv in the exact owned container. Mutations retain the outer lifecycle lock.
: >"$tmp/docker.log"
for args in \
  "servers list" \
  "servers test-all" \
  "servers ping $server_id" \
  "servers select $server_id" \
  "servers current"; do
  $lifecycle $args >"$tmp/out" 2>"$tmp/err"
  python3 - "$tmp/out" <<'PY'
import json,sys
with open(sys.argv[1], encoding='utf-8') as stream:
    value=json.load(stream)
assert value['schema']=='vibe-vpn.server-browser.v2'
assert value['status'] in {'ok','unavailable'}
PY
  [[ ! -s "$tmp/err" ]]
done
grep -Fq 'exec-lock=free operation=list' "$tmp/docker.log"
grep -Fq 'exec-lock=free operation=current' "$tmp/docker.log"
grep -Fq 'exec-lock=held operation=test-all' "$tmp/docker.log"
grep -Fq 'exec-lock=held operation=ping' "$tmp/docker.log"
grep -Fq 'exec-lock=held operation=select' "$tmp/docker.log"
grep -Eq 'docker <exec> <owned-container> </usr/local/bin/vibe-vpn-server-op-supervisor> <prepare> <op_[A-Za-z0-9_-]{43}> <ping> <srv_' "$tmp/docker.log"
grep -Eq 'docker <exec> <owned-container> </usr/local/bin/vibe-vpn-server-op-supervisor> <run> <op_[A-Za-z0-9_-]{43}> <select> <srv_' "$tmp/docker.log"
! grep -Fq '/usr/local/bin/vibe-vpn> <--config>' "$tmp/docker.log"
[[ "$(stat -c '%a' "$tmp/secrets/state/lifecycle.lock")" == 600 ]]
[[ "$(stat -c '%h' "$tmp/secrets/state/lifecycle.lock")" == 1 ]]

# A pre-batch supervisor must produce an actionable version error.
$lifecycle servers check-batch "$server_id" https://example.com/ >"$tmp/out" 2>"$tmp/err"
python3 - "$tmp/out" <<'PYCODE'
import json,sys
assert json.load(open(sys.argv[1]))['status'] == 'backend-outdated'
PYCODE
[[ ! -s "$tmp/err" ]]

# Absent, unhealthy, and foreign containers return one fixed safe object and
# never cross the helper exec boundary.
for mode in absent unhealthy foreign; do
  printf '%s\n' "$mode" >"$tmp/resource-mode"
  : >"$tmp/docker.log"
  MOCK_RESOURCE_MODE=$mode $lifecycle servers list >"$tmp/out" 2>"$tmp/err"
  python3 - "$tmp/out" <<'PY'
import json,sys
with open(sys.argv[1], encoding='utf-8') as stream:
    value=json.load(stream)
assert value == {'schema':'vibe-vpn.server-browser.v2','status':'unavailable','generation':0}
PY
  ! grep -Fq 'docker <exec>' "$tmp/docker.log"
  [[ ! -s "$tmp/err" ]]
done
printf 'owned\n' >"$tmp/resource-mode"

# A busy lifecycle lock blocks mutation before Docker while reads remain exact.
python3 - "$MOCK_LIFECYCLE_LOCK" "$tmp/lock-ready" <<'PY' &
import fcntl,os,sys,time
fd=os.open(sys.argv[1], os.O_RDWR)
fcntl.flock(fd, fcntl.LOCK_EX)
open(sys.argv[2], 'w').close()
time.sleep(3)
PY
lock_pid=$!
for _ in {1..50}; do [[ -e "$tmp/lock-ready" ]] && break; sleep 0.02; done
[[ -e "$tmp/lock-ready" ]]
: >"$tmp/docker.log"
if VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS=0 $lifecycle servers select "$server_id" >"$tmp/out" 2>"$tmp/err"; then
  echo 'server selection ignored a busy lifecycle lock' >&2
  exit 1
fi
[[ ! -s "$tmp/docker.log" ]]
VPNKIT_LOCAL_LIFECYCLE_LOCK_WAIT_SECONDS=0 $lifecycle servers current >"$tmp/out" 2>"$tmp/err"
grep -Eq 'docker <exec> <owned-container> </usr/local/bin/vibe-vpn-server-op-supervisor> <run> <op_[A-Za-z0-9_-]{43}> <current>' "$tmp/docker.log"
kill "$lock_pid" 2>/dev/null || true
wait "$lock_pid" 2>/dev/null || true

# Fake daemon proof: killing only the Docker exec client does not kill the
# daemon-owned worker. The exact token helper cancel performs TERM->KILL/drain.
old_token=$(python3 -c 'import secrets; print("op_"+secrets.token_urlsafe(32))')
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor prepare "$old_token" select "$server_id"
MOCK_EXEC_SLEEP=1 docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor run "$old_token" select "$server_id" >"$tmp/direct.out" 2>"$tmp/direct.err" &
direct_client=$!
for _ in {1..200}; do [[ -s "$tmp/state/$old_token.parent-pid" && -s "$tmp/state/$old_token.child-pid" ]] && break; sleep 0.01; done
[[ -s "$tmp/state/$old_token.parent-pid" && -s "$tmp/state/$old_token.child-pid" ]]
direct_worker=$(<"$tmp/state/$old_token.parent-pid")
kill -TERM "$direct_client" 2>/dev/null || true
wait "$direct_client" 2>/dev/null || true
kill -0 "$direct_worker"
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor cancel "$old_token"
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor verify "$old_token"
! kill -0 "$direct_worker" 2>/dev/null || [[ "$(awk '{print $3}' "/proc/$direct_worker/stat" 2>/dev/null || true)" == Z ]]
[[ -e "$tmp/state/$old_token.compensated" ]]
sleep 0.85
[[ ! -e "$tmp/state/$old_token.survivor" ]]
[[ ! -s "$tmp/direct.out" && ! -s "$tmp/direct.err" ]]

# Stale cancellation is isolated from a newer exact token.
new_token=$(python3 -c 'import secrets; print("op_"+secrets.token_urlsafe(32))')
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor prepare "$new_token" ping "$server_id"
MOCK_EXEC_SLEEP=1 docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor run "$new_token" ping "$server_id" >/dev/null 2>/dev/null &
new_client=$!
for _ in {1..200}; do [[ -s "$tmp/state/$new_token.parent-pid" ]] && break; sleep 0.01; done
new_worker=$(<"$tmp/state/$new_token.parent-pid")
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor cancel "$old_token"
kill -0 "$new_worker"
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor cancel "$new_token"
docker exec owned-container /usr/local/bin/vibe-vpn-server-op-supervisor verify "$new_token"
wait "$new_client" 2>/dev/null || true

# End-to-end host signal cancellation: the wrapper re-proves the captured
# original container after a Compose replacement, invokes exact token cancel,
# and does not return until the in-container TERM-resistant tree is drained.
: >"$tmp/docker.log"
MOCK_EXEC_SLEEP=1 VPNKIT_TUI_SUPERVISED=1 $lifecycle servers select "$server_id" >"$tmp/out" 2>"$tmp/err" &
operation_pid=$!
for _ in {1..300}; do
  active_state=$(find "$tmp/state" -maxdepth 1 -name 'op_*.state' -type f -print -quit)
  [[ -n "$active_state" ]] && read -r active_token _ _ active_status active_worker active_start active_client <"$active_state"
  [[ "${active_status:-}" == running ]] && break
  sleep 0.01
done
[[ "${active_status:-}" == running ]]
printf 'replacement\n' >"$tmp/resource-mode"
started=$(python3 -c 'import time; print(time.monotonic())')
kill -TERM "$operation_pid"
for _ in {1..500}; do
  kill -0 "$operation_pid" 2>/dev/null || break
  sleep 0.01
done
if kill -0 "$operation_pid" 2>/dev/null; then
  echo 'canceled server lifecycle shell returned before closure' >&2
  exit 1
fi
wait "$operation_pid" 2>/dev/null || true
elapsed=$(python3 - "$started" <<'PY'
import sys,time
print(time.monotonic()-float(sys.argv[1]))
PY
)
python3 - "$elapsed" <<'PY'
import sys
assert float(sys.argv[1]) >= 0.15, sys.argv[1]
PY
grep -Eq "cancel token=$active_token pid=$active_worker start=$active_start" "$tmp/docker.log"
grep -Fq "docker <exec> <owned-container> </usr/local/bin/vibe-vpn-server-op-supervisor> <cancel> <$active_token>" "$tmp/docker.log"
! grep -Fq 'docker <exec> <new-container> </usr/local/bin/vibe-vpn-server-op-supervisor> <cancel>' "$tmp/docker.log"
! kill -0 "$active_worker" 2>/dev/null || [[ "$(awk '{print $3}' "/proc/$active_worker/stat" 2>/dev/null || true)" == Z ]]
[[ -e "$tmp/state/$active_token.compensated" ]]
sleep 0.85
[[ ! -e "$tmp/state/$active_token.survivor" ]]
[[ ! -s "$tmp/out" && ! -s "$tmp/err" ]]
! grep -Eqi 'prod|production|vpnkit-vibe-vpn-state' "$tmp/docker.log"

printf 'vpnkit local server host bridge tests passed\n'

# Backend-only start must leave pending recovery state untouched, before Docker.
printf 'fixture-pending-recovery\n' > "$tmp/secrets/state/lifecycle.journal"
: > "$tmp/docker.log"
if $lifecycle backend start >"$tmp/out" 2>"$tmp/err"; then
  echo 'pending recovery unexpectedly accepted' >&2
  exit 1
fi
grep -Fq 'lifecycle recovery required before backend-only start' "$tmp/err"
grep -Fxq 'fixture-pending-recovery' "$tmp/secrets/state/lifecycle.journal"
[[ ! -s "$tmp/docker.log" ]]
