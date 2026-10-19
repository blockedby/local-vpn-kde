# Profile refactor validation — 2026-10-19

Application revision tested: `d5c119dd3d4e13ae5cad3c5ca4fa3591d1924164`.
Parent: `f2e90bb931c8` on `feat/native-runtime`.
Child branch: `refactor/native-profiles`; review: https://github.com/blockedby/local-vpn-kde/pull/11
(base `feat/native-runtime`, not `main`). No merge, stable tag, release, or CI run.

## Change and regression scope

Profile requests share bounded, whole-frame JSON decoding across CLI, bridge,
and privileged native operations. The profile document owns validation and
cache refresh, including source identity checks after unlocked fetches. Failed,
empty, invalid, or oversized source responses preserve the previous cache and
mark only that source stale. Storage writes validate before replacing a private
file. Catalog construction and TUI profile catalog reset each have one path.

New tests cover request size/trailing data/unknown fields, failed reads,
source changes during refresh, per-source and total catalog bounds, failed
save preservation, migration, duplicate sources, profile limits, active-source
removal, and distinct identities for the same endpoint in separate profiles.
Two renderer tests cover failed profile opening and source deletion without
connecting or selecting a server. A separate bridge test covers process spawn
failure: Node emits `close` without `exit`, so cleanup must wait for `close`.

The conservative `SafeDisplayName` policy is unchanged: arbitrary single-token
labels, including `Hysteria2-05`, can still become `Server`.

## Local checks

- `go test -race ./...`: PASS, all packages.
- `go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde`: PASS; required by
  the Bun test exercising the real checkout bridge.
- `bun install --frozen-lockfile` in `scripts/vpnkit/tui`: PASS.
- `bun run check` and `bun test` there: PASS, 71 tests / 322 expectations.
- `go test -cover ./internal/profiles`: PASS, 90.3% statements, compared with
  77.8% for the parent revision. These percentages cover different source
  layouts and supplement the behavioral regression cases.
- `bash test/hysteria2-container-test.sh`: PASS in the pinned disposable image;
  Hysteria2 password/TLS/Salamander packet path and no direct fallback.
- `VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR=/tmp/vpnkit-refactor-d5c119d-artifacts
  bash test/native-arch-package-build-test.sh`: PASS. The existing runner builds
  a curated source snapshot in its pinned disposable Arch container, then
  disconnects its network for installed package, portable installer, profile
  fixtures, compiled TUI, abort recovery, reinstall, and uninstall checks.
- `git diff --check`: PASS.

The first Bun attempts in the fresh worktree had no `.build` bridge binary.
Its ENOENT exposed the cleanup hang above; the regression is fixed and the final
full Bun run used the freshly built binary. No timeout increase or test skip.

Package tested in the dedicated guest:
`local-vpn-kde-native-0.0.0.r77.gd5c119dd3d4e.snapshot.97337660d746-1-x86_64.pkg.tar.zst`.
SHA-256: `e574c01b6c0191325766010e60117f32ba630ea8e17506c8d99946d4a00a97be`.
This is a local test artifact, not a stable release. It contains the packaged
application, compiled TUI, sing-box, native service/configuration/installer assets;
normal native installation dependencies still apply.

## Dedicated nl-2-nvme lab

Used the existing `/opt/vpn-native-lab` QEMU/KVM guest through the existing
`vm-lab/ssh.sh` wrapper. Verified `/etc/hostname` is `vpn-native-lab`, kernel
`7.2.11-arch1-1`, 512 MiB configured / 448 MiB usable, one vCPU. QEMU stays in
its dedicated Docker bridge container, without host networking. Package SHA-256
was checked in the guest before `sudo pacman -U --noconfirm`.

- Guest execution of the cross-built `internal/profiles` test binary: PASS,
  including all request/cache/storage/migration boundaries.
- Guest execution of the cross-built CLI test binary with
  `-test.run 'TestProfiles|TestProfileCLI|TestFailedProfileRefresh'`: PASS,
  three tests covering runtime-preserving migration, cache retention, and
  rejection before state writes.
- Actual installed `/usr/bin/local-vpn-kde bridge` and Polkit/native worker:
  PASS for profile migration/list, add/open/remove of synthetic VLESS and
  Hysteria2 sources, direct-source refresh, separate `profile_id` boundaries,
  duplicate/unknown-field/trailing-frame rejection, and redacted profile views.
  Compared private current-node and sing-box config digests before and after:
  unchanged. Removed only test-created profiles and restored the original active
  source. No runtime server selection was sent by that profile test.
  Reusable opt-in runner: `VPNKIT_NATIVE_VM_LAB=1 python3
  test/native-profiles-installed-vm-test.py` inside this dedicated guest; it
  refuses both a missing opt-in and a different guest hostname.

## Lab upgrade prerequisite and limitations

The guest initially had the older r61/a29/e269 native package, not the parent
f2e90bb application. Its NM connection used the old `vpnkit-local` name and
`state/` capability. Current native code intentionally uses `vpnkit-native`
and `state-native/` to coexist with Docker installations. After the package hook
stopped the gateway, the old connection was still active; native status reported
its own profile unconfigured and `start` returned code 20. Direct guest HTTPS
followed the old dead TUN route, and setup selection failed closed.

Identified the old connection by its recorded legacy UUID and verified exact
connection name/type before disconnecting it, inside the dedicated guest only.
Direct guest HTTPS then passed. Native setup was retried through the existing
private-input lab helper; source URLs, credentials, endpoint addresses, raw
responses, generated profiles, and logs are not included here.

Final sequential live checks on the installed d5c119d application: PASS.

- Private source setup completed in 165 seconds; gateway inactive and the new
  native NM profile configured, without automatically connecting.
- `start`: healthy gateway, native NM active, public HTTPS through the tunnel.
- Explicit browser `servers/select` of a successfully benchmarked node still
  present in the refreshed catalog, followed by `servers/current`: exact opaque
  ID and `selected=true`; tunneled HTTPS passed again. Initial setup deliberately
  has no current browser selection until this explicit action; the old lifecycle
  harness initially ended with `servers/current=unavailable` for this reason.
- Existing `lifecycle-suite.py`: PASS after that explicit selection. Actual
  autostart enabled/enabled/disabled states matched gateway-only/gateway+connect/
  off settings. Disconnect/start/stop/start, HTTPS, 51-node refresh, and retained
  current selection all passed.
- Opt-in installed profile runner: PASS after lifecycle completion, including
  unchanged runtime digests and removal of test-created sources.
- Installed compiled TUI in a PTY: PASS for home, profiles, server table,
  110x30 and 70x18 terminal sizes, and clean shutdown. No TS source or node_modules
  was added to the installed application.

One intermediate profile check was mistakenly overlapped with lifecycle
stop/start and received a refresh failure while the gateway was stopping.
Its cleanup ran; the final profile/lifecycle/TUI checks above were sequential.
This was a harness scheduling error, not counted as a successful acceptance run.

Final guest state: healthy native gateway, connected native NM profile, strict
mode, autostart disabled, no failed systemd units. Guest memory was 448 MiB
usable / 256 MiB available, with 34 MiB swap used. The inactive legacy NM profile
was retained. No workstation VPN, resolver, routes, VPS host networking,
production containers, or Hysteria2 server were changed.

Graphical KDE/interactive Polkit and actual graphical-login autostart were not
retested. The guest uses its existing lab-only headless Polkit authorization.
QEMU/Docker NAT does not establish real Wi-Fi or workstation coexistence claims.
No matching open GitHub issue was found; this report and the child PR hold the
redacted evidence. Private lab inputs/replies remain outside tracked source.
