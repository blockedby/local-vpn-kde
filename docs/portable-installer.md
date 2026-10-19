# Single-download native installer (Arch x86_64)

The local proof of concept delivers the complete native application in one
Linux executable. It embeds a regular `local-vpn-kde-native` Arch package and a
SHA-256 manifest. Run `./vpnkit-portable --inspect` without root to inspect the
package revision, checksum and remaining dependencies, then
`sudo ./vpnkit-portable --install`. Pacman asks for confirmation before installing
missing system dependencies and the embedded package. The installer enrolls
the invoking sudo/pkexec user for this application, so initial configuration
and subsequent TUI actions do not ask for another password. A direct root
installation without an invoking user leaves enrollment to the first setup. Keep the Arch system and
its repository databases current using the normal full-system upgrade workflow;
the installer deliberately does not perform a database-only refresh or retry.

This is an installer, not a zero-install VPN daemon. After installation use
`local-vpn-kde` as the desktop user. It installs root-owned files, systemd units,
sysusers/tmpfiles and polkit integration through pacman. Existing pre-transaction
hooks guard upgrades/removal and retain private state. Remove using
`sudo pacman -R local-vpn-kde-native`. Installation does not connect the VPN.
An existing setup must still be reviewed through the normal application flow.

## Included and required components

Included: three Go executables, a Bun-compiled OpenTUI 0.5.11 frontend with its
native code, sing-box copied from the version-bounded Arch build dependency,
OpenVPN/sing-box templates and rule fixtures, service/policy assets and dependency
license notices. There is no installed TypeScript source or `node_modules`, and
neither a system Bun nor a system sing-box is needed at runtime.

The package retains the namespace/veth/OpenVPN gateway. It requires the host's
NetworkManager and OpenVPN plugin, OpenVPN executable, polkit, systemd, iproute2,
nftables, iptables, account utilities and runtime C libraries. These are listed
in `--inspect` and resolved through pacman. Linux namespace/TUN/netfilter support
and a desktop authorization agent remain required. Bundling does not replace
system services or privileged authorization. It is Arch x86_64 only; other
Linux distributions and ARM need separate package/build/lab coverage.

## Build locally

Use the disposable, pinned Arch packaging lab; do not install onto the host:

```sh
VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR=/tmp/vpnkit-package-artifacts \
  bash test/native-arch-package-build-test.sh
```

The runner uses the curated public source snapshot, builds the package and
installer in a new container, disconnects that container from the network, then
checks installer installation, installed JSON status, profile/security fixtures,
compiled UI in a PTY, package recovery, reinstallation and removal. It does not
start a system manager or test a live KDE/polkit prompt/VPN connection. The
installed UI is tested after hiding the container's system Bun and sing-box.

To wrap an already-tested local package, on a build system with Python, bsdtar
and Go:

```sh
python3 packaging/portable/build.py /path/local-vpn-kde-native-VERSION-x86_64.pkg.tar.zst \
  /path/vpnkit-portable
(cd /path && sha256sum -c vpnkit-portable.sha256)
/path/vpnkit-portable --inspect
```

The wrapper copies only that package and the installer source into a temporary
build directory. It emits the executable and adjacent SHA-256 checksum; it does
not publish or mark a revision stable. `revision` is the package version,
including the curated public source snapshot digest, rather than a claim that
a dirty checkout exactly matches a Git commit. The embedded digest detects
payload corruption; authenticity still requires a trusted executable checksum
or signature obtained separately.

## Packaging constraints

Do not strip the standalone frontend: Arch's normal `strip` step truncates the
appended Bun payload, leaving a binary that starts Bun's CLI instead of the app.
The package explicitly disables stripping and debug-package extraction.
The compiled frontend uses a build-time installation layout; it does not derive
the backend path from Bun's virtual `import.meta.url`. Installed assets keep the
same trusted-root ownership and symlink protections as the native package.

The installer extracts only its exact embedded package into a private directory
under `/run` and calls `/usr/bin/pacman` with a fixed environment. It does not
extract arbitrary archive entries itself, override signature policy, bypass
package locks or execute helpers from a user-selected cache. It preserves
pacman's failure behavior and the application's interrupted-upgrade markers.
Temporary payload space and normal package installation space are both needed.

This is not yet a redistributable release. Resolve the project's currently
unspecified license and review corresponding-source/relinking obligations for
sing-box, Bun/JavaScriptCore and all bundled native dependencies before public
distribution. Included notices are useful evidence, not a substitute for that
review. Bundled runtime/security fixes must be delivered through new application
packages; updating the host's separate Bun or sing-box will not update them.

Official build references: [Bun standalone executables](https://bun.sh/docs/bundler/executables)
and [OpenTUI standalone executables](https://opentui.com/docs/reference/standalone-executables/).
The latter currently describes newer OpenTUI releases; the lab proves this
repository's pinned version rather than relying on that newer-version claim.
