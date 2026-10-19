# Native acceptance tests

Run destructive system/network scenarios only in the dedicated disposable
`vpn-native-lab` guest. It needs a configured working native server, an active
native connection, passwordless lab sudo and lab-only Polkit authorization.
Reserve the guest exclusively: lifecycle, policy, subscription and reboot tests
must not run concurrently. The SSH wrapper accepts a single remote command.
QEMU must allow reboot: remove `-no-reboot` from the lab launcher.

```sh
go test -race ./...
(cd scripts/vpnkit/tui && bun run check && bun test)
VPNKIT_NATIVE_VM_LAB=1 python3 test/native-installed-e2e.py \
  --ssh /path/to/vm-lab/ssh.sh \
  --package /path/to/candidate.pkg.tar.zst \
  --server-file /private/alternate-hysteria2-link
```

The optional package is installed under continuous HTTPS sampling. Without it,
the runner tests the currently installed package and prints its version.
`--skip-reboot` explicitly marks reboot coverage skipped; do not use that result
as evidence of autostart after reboot. The runner exits on the first failure.
Credentials stay in the guest's existing private configuration. Test sources use
documentation addresses. No real subscription is needed in the repository.
The optional alternate server file must be owned by the invoking user with mode
0600. The runner copies it privately into the guest and removes that copy after
the switch test. Omitting it explicitly skips real alternate-server switching.

| Layer | Runner | Checks |
| --- | --- | --- |
| Installed policy | `native-policy-installed-vm-test.py` | Strict/smart/strict, real rule download failure, restored policy and HTTPS |
| Installed profiles | `native-profiles-installed-vm-test.py` | Direct VLESS/Hysteria2 add/open/remove, independent lists, persistence, malformed payloads, failed-source isolation, runtime unchanged |
| Installed subscription | `native-subscription-cache-installed-vm-test.py` | Trusted fixture HTTPS, successful replacement, HTTP 503/invalid/empty source retention, recovery; removes its CA/profile |
| Installed lifecycle | `native-lifecycle-installed-vm-test.py` | Service-only/start/disconnect/stop, repeat actions, current server selection, ping/availability/batch/speed, cancellation, diagnostics, invalid requests |
| Real server switch | `native-server-switch-installed-vm-test.py` | Add authorized alternate Hysteria2 profile, select and verify tunneled HTTPS, return to original profile/server, remove temporary profile |
| Real guest reboot | `native-autostart-installed-vm-test.py` | Six reboots: strict/smart × disabled/service-only/connect; changed kernel boot ID, saved configuration, gateway proxy traffic, desktop route and HTTPS |
| Installed upgrade | `native-hot-upgrade-vm-test.py` | Continuous HTTPS during package replacement, unchanged processes/namespace/routes, cleared inhibitor |
| TUI input/rendering | `scripts/vpnkit/tui/app.test.ts` | Key navigation, confirmations, profile/server flows, cancellation, failed mode transition/retry, diagnostic retry; deterministic backend |
| Packaging / compiled PTY | `native-arch-package-build-test.sh` | Offline install/reinstall/removal/recovery, permissions, compiled TUI launch without Bun/node_modules |
| Isolated packet paths | `native-gateway-vpn-container-test.sh`, `hysteria2-container-test.sh` | Native gateway under production capability contract; Hysteria2 link, TLS/obfuscation and no direct fallback |

Additional isolated network coverage is provided by
`native-gateway-network-container-test.sh` (journal recovery, real kernel routes,
nftables, namespace inspection and repeated start/stop). The optional
`native-gateway-subscription-container-test.sh --env-file /private/lab.env`
requires `VPNKIT_LAB_SUBSCRIPTION_URL` in that private file. It is deliberately
not included in the default suite and fails closed when the source is absent.

The reboot runner activates a dedicated test session target that depends on
`graphical-session.target` after a real guest boot. This verifies the actual
installed autostart unit at the session boundary, but does not exercise a KDE
display manager. It restores saved autostart, mode, connection and linger intent.
KDE GUI/Polkit dialogs remain manual checks. The compiled PTY test proves package
startup, while broad TUI flow tests use a deterministic backend; neither alone
proves a real network transition.

Test scripts use `finally` cleanup, but a killed process or inaccessible guest
can leave test state. Restore the disposable guest before rerunning. Do not run
these scripts against a workstation or production VPN.
