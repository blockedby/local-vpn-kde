# Portable installer validation — 2026-10-19

Related GitHub issue: none assigned. Per task-owner instruction this redacted
report records evidence without creating an issue or PR solely for reporting.
No stable approval, tag, release, paid CI, host installation or VPN reconnection
was performed. The worktree remains isolated from the parent native-runtime
worktree and live VM/desktop testing.

## Tested revisions

- Application/installer implementation: `fb2394e`.
- Parent namespace DNS fix cherry-picked without conflict: `f737b05`
  (original `e269040`).
- Final package/PTY acceptance commit: `d05cb344ea398988fa029c5625a8e00d10572061`.
- Final curated public application source digest: `5da1e05f6cc9`.
- Package: `0.0.0.r64.gd05cb344ea39.snapshot.5da1e05f6cc9-1`.

## Commands and outcomes

`go test -race ./...` passed across all packages at `f737b05`; the next commit
only strengthened the Python PTY acceptance test. `python3 -m unittest discover
-s packaging/arch -p 'test_*.py'` passed (3 tests). Installer Go tests cover
manifest format/path/architecture rejection and package checksum/missing-file
failures.

`go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde`, then `bun run check`
and `bun test` in `scripts/vpnkit/tui`: passed, 67 tests / 299 assertions.
An initial Bun run without the checkout backend binary timed out in the existing
actual-Go-bridge integration test. Building its required backend and rerunning
resolved it; it was not hidden by increasing the test timeout.

```sh
VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR=/tmp/vpnkit-portable-verified-sol \
  bash test/native-arch-package-build-test.sh
```

Passed at `d05cb34`. The runner used pinned image
`sha256:2a081ef716ac1099601e5f0fef6a12a153549084998c9822492c452875985a9e`,
Bun 1.4.2 and the frozen OpenTUI 0.5.11 production dependencies. Build downloads
were public dependencies only. Runtime tests ran after disconnecting the
new disposable container from the network. The runner removed its container.
A separate exploratory build container was also removed.

Observed success markers:

- `portable_single_download_install=passed` — Go executable embedded the complete
  package, verified its digest and installed through interactive pacman.
- `installed_first_launch=passed` — installed JSON status selected native mode
  without creating mutable user state.
- `native_profile_installed_lab=pass` and
  `native_profile_installed_transaction_lab=pass` — existing installed profile
  publication/ownership/transaction tests.
- `compiled_tui_without_bun_or_node_modules=passed` — demo and real native UI
  rendered in a PTY, with installed TS/node_modules absent and system Bun and
  sing-box hidden. Real UI spawned the exact fixed Go bridge path, verified
  through its descendant process command line; Ctrl-C drained and exited cleanly.
  The bundled sing-box executable also ran successfully.
- `arch_package_abort_recovery=passed` — active package lock rejected recovery,
  interrupted transaction recovery retained setup-incomplete state.
- `arch_package_offline_lifecycle=passed` — reinstall and uninstall ran existing
  pre/post hooks, removed application files and preserved private state/accounts.

The first exploratory package exposed a real failure: makepkg `strip` truncated
Bun's appended payload. Explicit `!strip`/`!debug` fixed it; the installed PTY
acceptance now catches that regression.

## Local artifacts

Only public build assets are retained under `/tmp/vpnkit-portable-verified-sol`;
these paths are local evidence, not published download URLs.

- `vpnkit-portable`: 70,500,512 bytes (~67.2 MiB).
- Executable SHA-256:
  `8605b0e6558c1c20a459fc64e60050d5320893e60c09b7ca255a91c147610bf5`.
- Embedded package SHA-256:
  `8a355e7582b3acbf7e5ff8d677b9a6bbdbb37bbff60d0c1248a35916ab3d0106`.
- Adjacent executable/package SHA-256 files included.

## Material limits

This proves Arch x86_64 package installation and frontend/runtime packaging.
The lab has no booted system manager or KDE session: real systemd lifecycle,
polkit desktop authorization, live VPN egress, fail-closed routing and coexistence
with another VPN were not exercised by this packaging task. The task owner
handles VM connectivity independently. System dependencies were already present
in the pinned image; downloading/installing missing dependencies on a fresh
machine was not simulated. Pacman performed its normal dependency checks.

No cross-distribution, ARM, minimum-glibc or older-CPU compatibility claim is
made. The app still requires system NetworkManager/OpenVPN/polkit/systemd,
network tools and kernel functionality. Runtime library dependencies remain
explicit. Licensing/source/relinking review is still required before public
redistribution. Included third-party notices do not grant the missing project
license or establish release approval.
