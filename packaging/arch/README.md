# Arch local snapshot packaging

This directory is a bounded packaging deliverable for `local-vpn-kde-native`.
It is **not an AUR publication**. The current native worktree has no public,
immutable source archive, so AUR publication remains blocked until one exists.
Do not invent a branch, tag, or release URL in the meantime.

## Prepare a local source package

From the repository root, choose a new output directory outside the checkout:

```sh
python3 packaging/arch/prepare-local-source.py /tmp/local-vpn-kde-native-build
cd /tmp/local-vpn-kde-native-build
makepkg --syncdeps --cleanbuild
```

The preparation command refuses an existing output path. It selects regular
files only from a fixed public-code allowlist using Git's tracked plus
non-ignored-untracked inventory, rejects symlinks, writes a deterministic
Python-standard-library tarball, derives a development `pkgver`, and puts the
archive's actual SHA-256 in the generated `PKGBUILD`. It does not inspect
ignored files or broad-copy the worktree.

`makepkg` is expected to fetch Go modules and the frozen Bun production
lockfile dependencies in its isolated build environment. No host dependency
installation, service action, network mutation, or NetworkManager mutation is
performed by the preparation command.

## Package scope

- Builds `local-vpn-kde.bin`, `vibe-vpn`, and the server-operation supervisor
  into `/usr/libexec/local-vpn-kde`.
- Uses `/usr/bin/sing-box` through a fixed libexec symlink; Docker and the
  legacy `run.sh`/Docker setup entrypoint are not installed.
- Bundles the OpenTUI TypeScript, frozen lockfile, and Bun production
  `node_modules` under `/usr/share/local-vpn-kde`.
- Generates root system units at build time from
  `nativegateway.UnitFiles()`; the units have no `[Install]` section and are
  never enabled or started by package hooks.
- Creates service identities with sysusers. Membership in
  `local-vpn-kde-users` is optional and no real user is enrolled.
- Creates private config/state directories with tmpfiles. Runtime directories
  remain owned by each unit's `RuntimeDirectory=` directives.
- Before removal or upgrade, an ALPM `PreTransaction` / `AbortOnFail` hook
  invokes the application-owned verifier: stop gateway, then network, then
  finish identity-checked journal cleanup before binaries disappear. Failure
  aborts the package transaction. Upgrade does not automatically reconnect.
  Hooks only reload the systemd manager otherwise. Removal retains users, private configuration,
  state, journal data, and NetworkManager profiles; there is no purge mode.

Only `x86_64` is declared. The official Arch package evidence available while
this template was prepared exposed Bun for `x86_64`; an Arch/AUR Bun/OpenTUI
runtime dependency was not proven for `aarch64`.

## Known integration blocks

1. Installed paths are wired into the CLI, TUI bridge, and autostart. Actual
   packaged execution still needs container validation; legacy checkout files
   and Docker adapters are intentionally not installed.
2. The installed TUI subscription editor requests administrator authorization
   for private configuration generation and initial server selection, then
   imports the owned profile as the desktop user. This path is wired and has
   component tests, but packaged Polkit/NetworkManager integration remains
   unverified. Package hooks never generate credentials or import profiles.
3. `native-package --action pre-remove` and the aborting transaction hook
   exist, with sequencing unit tests. Live package-transaction validation is
   still required; offline roots skip system-manager operations.
4. Upstream currently has no repository license file or publishable license
   metadata. The local template records `custom:unlicensed`; licensing must be
   resolved before any public redistribution or AUR publication.
5. The package accepts `sing-box>=1.13` and `sing-box<1.15`. Synthetic full-VPN
   tests passed with Arch sing-box `1.14.1-2` and OpenVPN `2.7.7-1`, both as root
   and as a restricted service user (three service lifecycles). An opt-in real
   subscription test also passed: 20 candidates tested, 10 successful; the
   exact selected candidate transferred 65536 HTTPS bytes through the tunnel.
   This does not prove packaged onboarding, DNS, KDE or systemd integration.

Use `bash test/native-arch-package-build-test.sh` for disposable package checks.
The runner downloads public build dependencies, then disconnects the container
network before package installation/reinstallation/removal and installed-CLI
checks. It does not run a real system manager; this is not a desktop acceptance
or release-readiness test. Never use the real VPN host as a packaging lab.
Set `VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR` to an ignored local output directory
(e.g. `.build/native-arch-package`) to retain the tested development package and
its SHA-256 file. This is not publication or stable-version approval.

During removal/upgrade a root-owned `/run` inhibitor prevents a new start after
pre-transaction cleanup. Successful installation/upgrade clears it without
starting anything. If a package transaction is aborted, repair/verify the
package first; an administrator may then run the installed binary with
`native-package --action recover` to clear the owned inhibitor. Recovery takes
an exclusive configuration lock, refuses any existing pacman `db.lck`, and
validates installed assets/units and, where available, loaded systemd state.
It never removes pacman's lock. With no inhibitor, successful validation is an
idempotent no-op, not evidence that an abort occurred. `post-change` is reserved
for package hooks.
User autostart files remain dormant when the installed executable is absent.
