# Hot upgrade validation — 2026-10-19

Application revision: `623b252c83b8`, parent `2a31e6c` on `feat/native-runtime`.
Isolated worktree/branch: `.worktrees/hot-upgrade`, `feat/native-hot-upgrade`.
No stable tag, release, paid CI, host installation or host VPN changes.

## Local checks

- `go test -race ./...`: PASS on final application code.
- Rebuilt `.build/local-vpn-kde.bin`; `bun run check`, `bun test`: PASS, 71 tests.
- `python3 packaging/arch/test_packaging.py`: PASS, three tests.
- Snapshot regression tests prove package source replacement leaves pinned bytes
  intact, cancellation preserves the previous snapshot, explicit staging refreshes
  it, and crash-left temporaries recover. Symlink, writable, foreign-owned and
  unsafe destination inputs are rejected.
- `bash test/native-arch-package-build-test.sh`: PASS for the final package;
  offline install/reinstall/removal, compiled TUI, portable installer and inhibitor
  recovery. The container has no systemd, so live behavior is tested separately.

Final package:
`local-vpn-kde-native-0.0.0.r78.g623b252c83b8.snapshot.10d91dad5d57-1-x86_64.pkg.tar.zst`

SHA-256:
`9ea2c2f203de9a90e9029bbf51ef9b70a621fe55227c108f2f452556ec24a0bc`

## nl-2-nvme dedicated 512 MiB guest

Used only the existing `vpn-native-lab` QEMU guest through vm-lab/ssh.sh.
The initial transition from the legacy package used its normal cold upgrade hook.
A first experimental /run binary snapshot exceeded the guest's 90 MiB tmpfs;
staging failed safely and no hot-upgrade result is claimed for that version.
The implementation now stores snapshots on disk. Removed only the temporary
snapshot created by this experiment.

After bootstrapping the disk-backed version and confirming native VPN connected:

1. Same-version live reinstall: PASS, 43 HTTPS samples, zero failures. Gateway and
   child PIDs, namespace inode, active connection UUIDs, route lookup and pinned
   binary hashes all unchanged; inhibitor cleared.
2. Deliberately mismatched runtime contract in guest: post-change refused,
   inhibitor retained, gateway PID unchanged. Restored the exact contract and ran
   normal `native-package --action recover`; inhibitor cleared without restart.
3. Actual upgrade to final 623b252 package: PASS, 26 HTTPS samples, zero failures;
   all runtime identities/routes unchanged. Local and guest package SHA matched.
4. Confirmed running supervisor executable hash differs from the installed new
   executable: this exercised new management with an old running supervisor.
5. `VPNKIT_NATIVE_VM_LAB=1 python3 test/native-profiles-installed-vm-test.py`:
   PASS after upgrade, including Hysteria2/VLESS, strict input, runtime preservation,
   and cleanup of test-created profiles.

One transfer initially exhausted guest /tmp because earlier test packages were
retained. Pacman rejected the truncated archive before installation. Removed only
those test archives, retransferred, verified SHA, then ran the passing final test.

Limitations: request sampling plus unchanged process identities is not a universal
proof of uninterrupted arbitrary long-lived streams. No GUI authorization test,
no host deployment, no sing-box/OpenVPN binary-version replacement test, and no
automatic package rollback. Runtime security fixes need explicit service restart;
compatible TUI/management updates take effect on new launches. See
[native-hot-upgrade.md](native-hot-upgrade.md) for the compatibility contract and
first-install transition.
