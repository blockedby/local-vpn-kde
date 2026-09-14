#!/usr/bin/env bash
# Canonical normal-user launcher for the local issue-40 KDE TUI.
set -Eeuo pipefail
umask 077

(( EUID != 0 )) || { printf 'ERROR: run the local TUI as your normal desktop user, not as root\n' >&2; exit 3; }
command -v realpath >/dev/null 2>&1 || { printf 'ERROR: realpath is unavailable\n' >&2; exit 10; }
command -v stat >/dev/null 2>&1 || { printf 'ERROR: stat is unavailable\n' >&2; exit 10; }
command -v git >/dev/null 2>&1 || { printf 'ERROR: git is unavailable for canonical worktree discovery\n' >&2; exit 10; }
command -v python3 >/dev/null 2>&1 || { printf 'ERROR: python3 is unavailable\n' >&2; exit 10; }

path_has_symlink_component() {
  local path=$1 current=/ part
  local -a parts=()
  [[ "$path" == /* ]] || return 2
  IFS=/ read -r -a parts <<<"${path#/}"
  for part in "${parts[@]}"; do
    case "$part" in
      ''|.) continue ;;
      ..)
        current=${current%/*}
        [[ -n "$current" ]] || current=/
        continue
        ;;
    esac
    if [[ "$current" == / ]]; then current="/$part"; else current="$current/$part"; fi
    [[ -L "$current" ]] && return 0
  done
  return 1
}

requested=${BASH_SOURCE[0]}
case "$requested" in
  /*) ;;
  *) requested="$(pwd -P)/$requested" ;;
esac
if path_has_symlink_component "$requested"; then
  printf 'ERROR: TUI launcher path contains a symlink component\n' >&2
  exit 20
else
  path_rc=$?
  (( path_rc == 1 )) || { printf 'ERROR: TUI launcher path is not absolute\n' >&2; exit 20; }
fi
launcher=$(realpath -e -- "$requested" 2>/dev/null) || { printf 'ERROR: TUI launcher is unavailable\n' >&2; exit 20; }
launcher_dir=$(cd -- "$(dirname -- "$launcher")" && pwd -P) || { printf 'ERROR: TUI launcher directory is unavailable\n' >&2; exit 20; }
repo_root=$(git -C "$launcher_dir/../.." rev-parse --show-toplevel 2>/dev/null) || { printf 'ERROR: current directory is not a git worktree\n' >&2; exit 20; }
repo_root=$(realpath -e -- "$repo_root" 2>/dev/null) || { printf 'ERROR: git worktree root is unavailable\n' >&2; exit 20; }
[[ "$launcher_dir" == "$repo_root/scripts/vpnkit" ]] || { printf 'ERROR: TUI launcher is not the canonical scripts/vpnkit helper\n' >&2; exit 20; }

TUI="$repo_root/scripts/vpnkit/vpnkit_local_kde_tui.py"
LIFECYCLE="$repo_root/scripts/vpnkit/vpnkit-local.sh"
validate_source() {
  local path=$1 mode
  if path_has_symlink_component "$path"; then
    printf 'ERROR: canonical local TUI source contains a symlink component\n' >&2
    exit 20
  fi
  [[ -f "$path" && ! -L "$path" && -O "$path" ]] || { printf 'ERROR: canonical local TUI sources are unsafe\n' >&2; exit 20; }
  [[ "$(stat -c '%h' -- "$path" 2>/dev/null)" == 1 ]] || { printf 'ERROR: canonical local TUI sources must not be hard-linked\n' >&2; exit 20; }
  mode=$(stat -c '%a' -- "$path" 2>/dev/null) || { printf 'ERROR: canonical local TUI source mode is unavailable\n' >&2; exit 20; }
  [[ "${mode: -2}" != *[2367]* ]] || { printf 'ERROR: canonical local TUI source is writable by group or other\n' >&2; exit 20; }
}
for path in "$launcher" "$TUI" "$LIFECYCLE"; do validate_source "$path"; done
[[ -x "$LIFECYCLE" ]] || { printf 'ERROR: local lifecycle helper is not executable\n' >&2; exit 20; }

# Preserve the scriptable interface and an explicit recovery UI.
for argument in "$@"; do
  case "$argument" in
    --status-json|--test|--help|-h) exec python3 "$TUI" "$@" ;;
  esac
done
command -v bun >/dev/null 2>&1 || {
  printf 'ERROR: OpenTUI requires Bun >= 1.3. Install Bun, then run: cd scripts/vpnkit/tui && bun install --frozen-lockfile\n' >&2
  exit 10
}
for path in "$launcher_dir"/tui/{index,app,bridge,model}.ts; do validate_source "$path"; done
[[ -d "$launcher_dir/tui/node_modules/@opentui/core" ]] || {
  printf 'ERROR: install the interface dependencies first: cd scripts/vpnkit/tui && bun install --frozen-lockfile\n' >&2
  exit 10
}
exec bun "$launcher_dir/tui/index.ts" "$@"
