# Native package candidates for v0.2.0

The manual `native-packages.yml` workflow builds **Arch x86_64** `.pkg.tar.zst`
and **Debian 13 amd64** `.deb` candidates. It uploads GitHub Actions artifacts;
it does not create a tag, GitHub release, AUR entry or apt repository. This is
release preparation, not approval of a stable version. No CI run has been started
as part of preparing this workflow.

## Inputs and output

Select a full source commit SHA and a package version (default `0.2.0-rc1`). The
workflow checks out that exact SHA with no persisted Git credentials. Each matrix
job builds and validates its package in a disposable Docker container. Its only
trigger is `workflow_dispatch`; permission is `contents: read`.

The `0.2.0-rc1` input becomes `0.2.0rc1` on Arch and `0.2.0~rc1` on Debian, so the later `0.2.0` package sorts after the candidate.

Each artifact contains:

- the native package, including Go services, compiled Bun/OpenTUI frontend,
  sing-box, systemd/polkit integration and configuration templates;
- a curated public source archive and `SOURCE.json` with commit, version and
  source archive SHA-256;
- `BUILDER.txt` with the actual build-image ID, and `SHA256SUMS` for all payloads.

No private configuration, subscriptions, keys or runtime state are included.
Neither system Bun nor Docker is required on the target machine. OpenVPN,
NetworkManager and its OpenVPN plugin, systemd, polkit and networking utilities
remain system dependencies. Debian ELF dependencies are computed with
`dpkg-shlibdeps` in Debian; Arch executables are not repackaged as Debian binaries.

## Local preparation

Commit the source first; the runner rejects a dirty checkout. Docker and Python 3
must be available. Output paths must not exist:

```sh
bash scripts/release/native-packages.sh arch 0.2.0-rc1 /tmp/vpnkit-arch-candidate
bash scripts/release/native-packages.sh debian 0.2.0-rc1 /tmp/vpnkit-debian-candidate
```

The Arch runner uses the existing `vpnkit-native-arch-lab` image or builds it from
`test/native-arch-package.Dockerfile`. The Debian runner builds the dedicated
`packaging/debian/Dockerfile`: a pinned Debian base, checksum-pinned Bun 1.4.2 and
sing-box 1.14.1. Distribution repositories still supply build/system dependencies;
these builds are not claimed bit-for-bit reproducible. Actual image identity and
source checksums accompany the artifacts.

Containers use a curated public source mount, no host network mode, no privileged
mode, no Docker socket mount and no host configuration. Debian installation uses a separate clean runtime container to resolve dependencies
through apt, so build tools cannot mask missing runtime packages. The test
container network is detached before installed-package tests. Each runner removes its own container/staging on
exit. Existing host VPNs and application containers are untouched.

## Installing a tested candidate

After obtaining a trusted artifact and verifying `sha256sum -c SHA256SUMS`:

```sh
# Arch / CachyOS
sudo pacman -U ./local-vpn-kde-native-0.2.0rc1-1-x86_64.pkg.tar.zst

# Debian 13
sudo apt install ./local-vpn-kde-native_0.2.0~rc1_amd64.deb

# Both: launch as your desktop user
local-vpn-kde
```

The package-manager installation does not choose a desktop user, enroll one,
start a gateway or activate VPN. Initial profile setup in the TUI requests desktop
authorization and enrolls that user. The separate Arch [one-sudo bundle
installer](portable-installer.md) also performs enrollment; it is not a Debian
installer and is not included in these package-only candidate artifacts.

Debian maintainer scripts create service identities/directories, validate package
assets, preserve the runtime on compatible upgrades and stop only owned services
before removal. A failed pre-upgrade guard also fails dpkg's `failed-upgrade`
fallback. Private configuration and user profiles survive removal/purge.

Debian recovery uses POSIX locks on both dpkg `lock-frontend` and `lock`, held
throughout recovery. Existing idle lock files are normal. Recovery never removes
package-manager locks; it refuses held, missing or unsafe lock files.

## Coverage and remaining release gates

Local candidate checks cover package installation/reinstallation/removal, the
real compiled TUI and backend bridge without system Bun, user-owned state, and
Debian recovery with an active dpkg/frontend lock. Go race and OpenTUI checks run
locally. Arch/real-NetworkManager acceptance from PR #18 covers its existing
runtime; Debian package tests do **not** prove a full KDE login, Polkit prompt,
real systemd/VPN connection or uninterrupted live upgrade on Debian. Debian
13 desktop/VM acceptance is required before calling that platform stable.

Before publication: approve the final tested commit as stable, resolve the
project's unspecified redistribution license and bundled dependency obligations,
and explicitly authorize the release run. Candidate artifacts alone do not
fulfil these requirements. The existing `release.yml` still publishes the Docker
installer archive; promoting native candidate packages into a stable release is
a separate step. Do not publish this candidate workflow's artifacts as a stable
release without completing those checks.

Maintainer-script behavior follows the [Debian Policy installation lifecycle](https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html).
