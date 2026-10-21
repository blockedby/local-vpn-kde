# Native network recovery after reboot

## Failure and repair

On 2026-10-20 the installed network service failed with `applied effect-1 is
absent`, preventing the gateway dependency and desktop autostart from starting.
The root-owned version 4 journal, written before the boot, still described 19
applied effects; the new kernel had no named native namespace.

Previous-boot logs show an earlier network stop refused to detach a namespace
that still had processes. Later starts encountered a missing connected route.
The subsequent reboot was requested normally through Plasma. The surviving
journal therefore demonstrates recovery after failed cleanup followed by a
normal reboot; it does not establish an abrupt power failure or the identity of
the process holding the namespace. The host's other active VPN was preserved.

The previous six-reboot acceptance test explicitly stopped the native runtime
before **every** reboot. Those tests proved saved autostart intent and session
activation, but always cleared the journal before boot and missed this failure.

Final product fix: `eaf7dae452b2ccc1b7a9004def6f0ba8e1630db8` (including
journal recovery `cd66a50`), based on
`perf/native-speed-batch` at `7eaafc0b97e7f22692e4eb36c36e8a4da66f35aa`.

- New version 5 journals contain the kernel boot identity. Missing applied
  resources during the same boot remain an error.
- Prior-boot and legacy version 4 recovery requires every exclusive effect in
  the complete plan to be absent, including effects beyond a recorded prefix.
  Remaining, conflicting or unverifiable resources prevent automatic recovery.
- Recovery probes the current physical uplink and validates a fresh host
  snapshot. It reads current shared IPv4 forwarding state instead of restoring
  a previous boot's value.
- The old journal is atomically replaced by current-boot intent before the first
  creation command. It is never blindly deleted. An intact live legacy journal
  migrates only after its immutable resource identities are verified.
- An absent namespace marker reaches the controller's cleanup proof. A present
  namespace still requires an empty process list before teardown.

The first installed candidate, `cd66a50`, still refused the preserved legacy
journal. Read-only classification showed only effects 7 and 8 (the dedicated
connected and default routes) were unverifiable. On a fresh kernel, iproute2
returns a nonzero status for a routing table that has never existed. The
inspector had treated that failed scoped query as unavailable evidence.

The follow-up `eaf7dae` falls back to a successful, parsed complete route dump
when the scoped query fails. It then filters the dedicated table normally.
Unrelated VPN routes do not imply owned state; conflicting or exact remaining
native routes retain their classification. Failed or malformed complete dumps
still block recovery. The guest journal was preserved between candidates so the
final candidate had to recover the original legacy state automatically.

## Local checks

Run from the fix checkout, on the product revision above:

```sh
go test -race ./...
mkdir -p .build
go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde
(cd scripts/vpnkit/tui && bun install --frozen-lockfile)
(cd scripts/vpnkit/tui && bun run check && bun test)
bash test/native-gateway-network-container-test.sh
git diff --check
```

All checks passed on `eaf7dae`. The disposable container exercised real kernel
routes, nftables, namespace inspection, journal persistence/reopening, repeated
start/stop and loopback UDP. It does not prove a complete VPN or systemd
lifecycle. OpenTUI: **74 passed, 0 failed**, including the real local Go
bridge test. Dependencies were initially absent in the new worktree; the frozen
install resolved that prerequisite without changing tracked files.

Focused Go regressions cover legacy/current-epoch compatibility; same-boot
missing effects; remaining, conflicting and unverifiable objects; reused kernel
identities; full-plan proof beyond an incomplete prefix; current forwarding
state; failures before intent replacement; resumption after intent publication;
durable intent/applied/removing/removed/relinquished phases; changed legacy
identities; current uplink discovery; failed probes; absent namespace cleanup;
and refusal to detach a namespace with holders.

## Installed lab acceptance

The package was built and validated in the existing disposable Arch runner:

```sh
VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR=/path/to/lab-artifacts \
  bash test/native-arch-package-build-test.sh
```

Passed: offline package lifecycle/abort recovery, embedded portable installer,
installed profile transactions, and compiled TUI launch without Bun or
`node_modules`.

Artifact: `local-vpn-kde-native-0.0.0.r84.geaf7dae452b2.snapshot.cc857f339de6-1-x86_64.pkg.tar.zst`.
SHA-256: `11606c5cf25b488493a24ddd215df9da4022fdfbdef9e49583f010e3f87752f7`.

In the disposable guest, the installed `eaf7dae` package recovered its preserved
version 4 journal automatically at graphical-session activation. The resulting version 5
journal matched the current kernel boot, the desktop had the native TUN route,
and HTTPS succeeded. Protected source hashes were unchanged. No
journal was manually cleared between the failed and corrected candidates.

All **12 required cases passed**, with this execution provenance:

| Case, in both strict and smart modes | Accepted cases |
| --- | ---: |
| Disabled, normal reboot | 2 |
| Active service-only, normal reboot / abrupt QMP reset | 4 |
| Active connected runtime, normal reboot / abrupt QMP reset | 4 |
| Test-owned namespace holder, cleanup refusal, normal reboot | 2 |

The first execution produced 10 successful ordinary-case records in
`active-reboot-matrix.log`, then stopped at the holder test's incorrect
assumption that the outer desktop Stop command must return nonzero. After
checking actual systemd failure and preserved holder identity instead, the
focused two-case holder rerun exited 0 (`held-namespace-reboot-matrix.log`).
This is combined case coverage, **not** a claim that one full 12-case invocation
exited 0. `combined-12-case-validation.json` records the exact provenance and
independent final restoration.

```sh
VPNKIT_NATIVE_VM_LAB=1 python3 test/native-autostart-installed-vm-test.py \
  --ssh /path/to/vm-lab/ssh.sh --qmp-host dedicated-lab-hypervisor \
  --qmp-control-socket /path/to/existing-lab-ssh.sock
VPNKIT_NATIVE_VM_LAB=1 python3 test/native-autostart-installed-vm-test.py \
  --ssh /path/to/vm-lab/ssh.sh --case-group holder
```

Each accepted case checked a changed kernel boot ID, persisted mode/config,
expected gateway/desktop routes, and HTTPS. Enabled cases also checked a version
5 journal bound to the current boot and namespace SOCKS HTTPS. The holder cases
proved the same test-owned process remained in the namespace after refused
cleanup, then verified automatic recovery after normal reboot. Four resets
were abrupt; eight ordinary enabled-runtime cases kept the gateway active at
the reboot/reset boundary. Holder cases intentionally attempted shutdown first.

Independent cleanup exited 0: strict mode, active gateway, connected desktop VPN
and HTTPS, saved autostart off, linger disabled, test session/holder unit and
metadata removed, private profile/configuration hashes unchanged, and current
boot version 5 journal. The guest is disposable; no test reboots the workstation.
Public runners contain no subscription, endpoint, key or private log content.

## Limits

Automatic recovery deliberately refuses same-boot partial network loss and
remaining or conflicting ownership state. It does not kill unknown namespace
holders, repair another program's routes, or bypass the namespace process guard.
The earlier holder's identity has not been proven.

The holder cases also exposed a separate status-propagation limitation:
`native-desktop stop` returned exit 0 while the network unit's actual `ExecStop`
exited 20 and systemd recorded `Result=exit-code`. The preserved holder and
network unit failure are checked directly by the runner. This reboot-recovery
change does not fix that CLI stop-result reporting; it remains tracked in
[issue 14](https://github.com/blockedby/local-vpn-kde/issues/14).

The headless guest models graphical-session activation using a dedicated target
that depends on `graphical-session.target`. KDE display-manager login and visible
Polkit dialogs remain manual checks. The real desktop has enabled saved
autostart intent and retains the historical failed login result until a future
autostart run. That history is not reset to make the UI appear successful. VM
session-boundary success does not prove the real desktop's next login. No stable
approval, tag, release or host reboot is part of this validation.
