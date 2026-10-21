# Native acceptance evidence — 2026-10-19

Issue: [#14](https://github.com/blockedby/local-vpn-kde/issues/14).
PR: [#13](https://github.com/blockedby/local-vpn-kde/pull/13), base
`feat/native-runtime`.

Application/test revision: `6773b70b908e`, including native startup correction
`7407d00` and smart routing correction `0c1d288`.

Package:
`local-vpn-kde-native-0.0.0.r80.g6773b70b908e.snapshot.973db22b7c06-1-x86_64.pkg.tar.zst`

SHA-256: `d10d45e90d58d8e1c9b4e24d570cc538a2dcad0a63742437071898b3744db5da`.

## Local and container checks

- `go test -race ./...`: PASS.
- `bun run check` and `bun test` in `scripts/vpnkit/tui`: PASS, 74 tests,
  346 assertions. Input/rendering uses a deterministic backend.
- `bash test/native-arch-package-build-test.sh`: PASS. Portable install, first
  launch, compiled PTY without Bun/node_modules, transaction recovery, offline
  reinstall/removal. Package above exported only after acceptance passed.
- `bash test/native-gateway-network-container-test.sh`: PASS. Actual kernel
  routes/nftables, namespace operations, journal reopening, repeated start/stop.
- `bash test/native-gateway-vpn-container-test.sh`: PASS in root and restricted
  service modes. The initial run exposed an outdated fixture: it omitted the
  production `StageNativeRuntime` step. The fixture now executes that same step.
- `bash test/hysteria2-container-test.sh`: PASS, including TLS/options,
  Salamander obfuscation and failure without direct fallback.
- `python3 packaging/arch/test_packaging.py` and
  `python3 test/native-subscription-env-test.py`: PASS, 3 and 5 tests respectively.
- Optional external-subscription container runner was attempted without its
  required private env file and refused to run; it is not counted as passed.
  Installed subscription refresh and actual internet/protocol checks below use
  the dedicated guest instead.

## Installed application

Exclusive dedicated 512 MB QEMU guest `vpn-native-lab` on nl-2-nvme, accessed
through the existing SSH wrapper. Production containers and host routing were
not modified by the lab. The first reboot exposed `-no-reboot` in the QEMU
launcher: removed it only from the disposable lab and rebuilt that container,
retaining its guest disk. Infrastructure-affected attempts are excluded.

Command (paths shortened; no source values included):

```sh
VPNKIT_NATIVE_VM_LAB=1 python3 -u test/native-installed-e2e.py \
  --ssh /path/to/vm-lab/ssh.sh \
  --package /path/to/the-package-above.pkg.tar.zst \
  --server-file /private/alternate-hysteria2-link
```

The suite verifies actual installed bridge operations and network traffic:

- Hot upgrade: 27 HTTPS samples, zero failures, unchanged gateway/child process
  identities, namespace, active connections, route and pinned runtime hashes.
- Strict → smart → strict with working HTTPS; injected actual rule download
  failure restores strict mode and tunneled connectivity.
- Direct profiles, separate lists, duplicate/invalid requests, bridge session
  persistence, failed source isolation; generated/live config and selection
  hashes unchanged by browsing and refresh.
- Trusted local HTTPS subscription: replace VLESS fixture with Hysteria2 fixture;
  HTTP 503, invalid content and empty content retain the last-good cache; a valid
  response clears stale state. Temporary CA and profile removed.
- Service-only, connect, disconnect, stop, repeated actions, failed backend
  startup and recovery, diagnostics, ping/availability/batch/speed, and cancel
  after an installed batch worker announces its start. Connectivity assertions
  include a tunnel route, not only an active status label.
- Actual configured VLESS → authorized Hysteria2 → original VLESS: verify live
  runtime protocol, selected opaque ID, desktop tunnel route and HTTPS. Restore
  original profiles and selection; source bytes are never written to evidence.
- Six actual guest reboots: all PASS on the final package, with a changed kernel
  boot ID each time, saved mode/configuration, expected gateway/NM/routes,
  desktop HTTPS in all six cases, and namespace SOCKS HTTPS in all four enabled
  cases. Matrix: strict/off, strict/service, strict/connect, smart/off,
  smart/service, smart/connect. The full sequential command exited 0 with
  `installed_native_e2e=passed; reboot=passed`.

Cleanup restored the original strict VLESS connection, autostart disabled and
linger disabled; removed the synthetic session target and temporary source copy.
Raw run output is retained locally in `/tmp/native-full-e2e-installed.log`.
The later evidence commit changes only this report and prints the installed
package version after upgrade in the orchestrator; application code is unchanged.

An initial lifecycle run found a real product bug: repeating `start` while the
owned VPN was active attempted duplicate NM activation and returned code 20.
The fix accepts an existing connection only after ownership, other-VPN, TUN
mapping and full-route checks. Recovery of a non-ready gateway first disconnects
its stale owned tunnel. New regressions and independent review cover both paths.

## Limits

KDE display-manager login and visible Polkit prompts need a graphical desktop.
The headless reboot test activates a test target depending on the actual
`graphical-session.target`; it does not claim to automate KDE login. Compiled
PTY coverage proves launch/packaging, while the broader TUI flows use a mock
backend. No stable release or paid CI was run. Continuous HTTPS sampling is not
a proof that every possible long-lived connection survives an update.
