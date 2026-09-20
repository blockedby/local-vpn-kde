#!/usr/bin/env bash
# Build only the selected tag, never the working tree. Does not publish anything.
set -Eeuo pipefail
umask 022
[[ $# == 2 ]] || { echo 'Usage: package.sh vMAJOR.MINOR.PATCH OUTPUT_DIRECTORY' >&2; exit 2; }
tag=$1
[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Expected a stable semantic version tag.' >&2; exit 2; }
repo=$(git rev-parse --show-toplevel)
commit=$(git rev-parse --verify "refs/tags/$tag^{commit}")
output=$(realpath -m -- "$2")
name="local-vpn-kde-$tag-linux-amd64"
[[ ! -e $output/$name.tar.gz && ! -e $output/SHA256SUMS ]] || { echo 'Release assets already exist; refusing overwrite.' >&2; exit 2; }
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT
mkdir "$stage/$name"
paths=()
while IFS= read -r -d '' path; do
  case "$path" in
    *_test.go|*.test.ts|*/node_modules/*|*.local.env|*.log|*.key|*.pem|*/speed-demo*) continue ;;
    install.sh|run.sh|README.md|go.mod|go.sum|.dockerignore|.gitignore|compose.local.yaml|docker-compose.yml|cmd/*|internal/*|config/*|docker/*|scripts/vpnkit/*) paths+=("$path") ;;
  esac
done < <(git ls-tree -r --name-only -z "$commit")
[[ ${#paths[@]} -gt 0 ]] || { echo 'No application assets found.' >&2; exit 2; }
git archive "$commit" -- "${paths[@]}" | tar -xf - -C "$stage/$name"
[[ -z $(find "$stage/$name" -type l -print -quit) ]] || { echo 'Symlink in release source refused.' >&2; exit 2; }
(
  cd "$stage/$name"
  mkdir -p .build
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde
  # Legacy installers use Git only to discover the application root. Generate
  # empty metadata: never export repository history, hooks, or credentials.
  git -c init.defaultBranch=main init --quiet --template= .
  cat > RELEASE.txt <<INFO
Tag: $tag
Commit: $commit
Platform: linux-amd64

Extract the archive and run ./install.sh as your desktop user, then ./run.sh.
The installer installs dependencies and rebuilds the bundled application.
The empty .git directory is generated only for installer root discovery;
it contains no source history or remote. Do not use git pull to update this
archive installation. Download the next release and preserve your private
secrets/ directory when replacing application files.
INFO
)
# Normalize archive metadata; source and binary are both from the exact tag.
epoch=$(git show -s --format=%ct "$commit")
tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -C "$stage" -cf - "$name" | gzip -n > "$stage/$name.tar.gz"
mkdir -p -- "$output"
# noclobber protects concurrent callers too.
(set -o noclobber; cat "$stage/$name.tar.gz" > "$output/$name.tar.gz")
(cd "$output"; set -o noclobber; sha256sum "$name.tar.gz" > SHA256SUMS)
printf 'Packaged %s at %s\n' "$tag" "$commit"
