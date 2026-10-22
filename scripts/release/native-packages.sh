#!/usr/bin/env bash
# Candidate artifacts only. Run on a clean checkout of the chosen revision.
# Never starts services or changes networking on the host.
set -Eeuo pipefail
[[ $# == 3 && $1 =~ ^(arch|debian)$ && $2 =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[1-9][0-9]*)?$ ]] || { echo 'Usage: native-packages.sh arch|debian VERSION NEW_OUTPUT' >&2; exit 2; }
distro=$1
if [[ $distro == arch ]]; then version=${2/-rc/rc}; else version=${2/-rc/~rc}; fi
root=$(cd "$(dirname "$0")/../.." && pwd -P)
output=$(realpath -m "$3")
# Candidate provenance must describe exactly the chosen Git revision.
[[ -z $(git -C "$root" status --porcelain --untracked-files=normal) ]] || {
  echo 'Commit source changes before building candidate artifacts.' >&2; exit 2;
}
[[ ! -e $output ]] || { echo 'Output already exists' >&2; exit 2; }
work=$(mktemp -d)
name="vpnkit-candidate-$distro-${UID}-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf -- "$work"; }
trap cleanup EXIT INT TERM
python3 "$root/packaging/native/prepare-source.py" "$version" "$work/source"
if [[ $distro == arch ]]; then
  image=${VPNKIT_NATIVE_ARCH_TEST_IMAGE:-vpnkit-native-arch-lab}
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker build -t "$image" -f "$root/test/native-arch-package.Dockerfile" "$root/packaging/arch"
  fi
else
  image=${VPNKIT_NATIVE_DEBIAN_TEST_IMAGE:-vpnkit-native-debian-lab}
  docker build -t "$image" "$root/packaging/debian"
fi
docker run -d --pull never --name "$name" --mount "type=bind,src=$work/source,dst=/input,readonly" "$image" sleep infinity >/dev/null
if [[ $distro == arch ]]; then
  docker exec "$name" bash -c 'set -Eeuo pipefail; cp -a /input/. /work/; chown -R builder:builder /work; runuser -u builder -- bash -c "cd /work && makepkg --noconfirm --cleanbuild"'
  docker network disconnect bridge "$name"
  docker exec "$name" bash -c 'set -Eeuo pipefail; packages=(/work/*.pkg.tar.zst); test "${#packages[@]}" = 1; pacman -U --noconfirm "${packages[0]}"; source=(/work/src/local-vpn-kde-native-*); python "${source[0]}/test/compiled-tui-installed-test.py"; pacman -U --noconfirm "${packages[0]}"; pacman -R --noconfirm local-vpn-kde-native; mkdir /artifacts; cp "${packages[0]}" /artifacts/'
else
  docker exec "$name" bash -c 'set -Eeuo pipefail; mkdir /work/source; tar -xzf /input/*.tar.gz -C /work/source --strip-components=1; cd /work/source; bash packaging/debian/build.sh "$1" /artifacts' _ "$version"
  docker network disconnect bridge "$name"
  docker exec -e VPNKIT_DEBIAN_PACKAGE_LAB=1 "$name" bash -c 'set -Eeuo pipefail; files=(/artifacts/*.deb); test "${#files[@]}" = 1; python3 /work/source/test/native-debian-package-installed-test.py "${files[0]}"'
fi
mkdir -p "$output"
docker cp "$name:/artifacts/." "$output/"
cp "$work/source/SOURCE.json" "$output/"
cp "$work/source/"*.tar.gz "$output/"
docker image inspect "$image" --format '{{.Id}}' > "$output/BUILDER.txt"
(cd "$output"; find . -maxdepth 1 -type f ! -name SHA256SUMS ! -name '*.sha256' -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
