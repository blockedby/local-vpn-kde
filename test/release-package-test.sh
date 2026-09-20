#!/usr/bin/env bash
set -Eeuo pipefail
helper=$(realpath "$(dirname "$0")/../scripts/release/package.sh")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/repo/cmd/local-vpn-kde" "$work/repo/secrets" "$work/repo/test"
cd "$work/repo"
git init -q
git config user.name 'Release fixture'
git config user.email 'release@example.invalid'
printf 'module fixture\n\ngo 1.22\n' > go.mod
printf 'package main\nimport "fmt"\nfunc main(){fmt.Println("tagged-release")}\n' > cmd/local-vpn-kde/main.go
printf '#!/bin/sh\necho installer\n' > install.sh
cp install.sh run.sh
chmod +x install.sh run.sh
printf 'private-fixture\n' > secrets/subscription
printf 'private-fixture\n' > test/private-fixture
printf 'fixture instructions\n' > README.md
git add .
git commit -qm fixture
git tag v1.2.3
commit=$(git rev-parse HEAD)
printf 'broken uncommitted source\n' > cmd/local-vpn-kde/main.go
bash "$helper" v1.2.3 "$work/out"
(cd "$work/out"; sha256sum -c SHA256SUMS)
tar -xzf "$work/out/local-vpn-kde-v1.2.3-linux-amd64.tar.gz" -C "$work"
release="$work/local-vpn-kde-v1.2.3-linux-amd64"
test "$("$release/.build/local-vpn-kde.bin")" = tagged-release
test -x "$release/install.sh"
test -x "$release/run.sh"
test ! -e "$release/secrets"
test ! -e "$release/test"
test "$(git -C "$release" rev-parse --show-toplevel)" = "$release"
test -z "$(git -C "$release" remote)"
test -z "$(find "$release/.git/objects" "$release/.git/refs" -type f -print -quit)"
test ! -e "$release/.git/hooks"
if git -C "$release" rev-parse --verify HEAD >/dev/null 2>&1; then echo 'Exported Git history' >&2; exit 1; fi
grep -Fx "Commit: $commit" "$release/RELEASE.txt" >/dev/null
if bash "$helper" v1.2.3 "$work/out" >/dev/null 2>&1; then echo 'Overwrote assets' >&2; exit 1; fi
if bash "$helper" 'v1.2.3;echo injected' "$work/invalid" >/dev/null 2>&1; then echo 'Accepted invalid tag' >&2; exit 1; fi
if bash "$helper" v9.9.9 "$work/missing" >/dev/null 2>&1; then echo 'Accepted absent tag' >&2; exit 1; fi
if [[ -n ${VPNKIT_RELEASE_TEST_IMAGE:-} ]]; then
  docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --entrypoint /release/.build/local-vpn-kde.bin -v "$release:/release:ro" "$VPNKIT_RELEASE_TEST_IMAGE" | grep -Fx tagged-release
fi
echo 'PASS: tagged source, binary, checksums, exclusions, permissions, overwrite and tag guards'
