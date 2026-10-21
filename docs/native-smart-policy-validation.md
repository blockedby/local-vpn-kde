# Native smart policy regression — 2026-10-19

The native template inherited a `direct-dns` local resolver, which could use
host systemd-resolved link IDs from inside the network namespace. Smart-mode
rule downloads consequently failed with `Link ... not known`. Native rendering
now uses an explicit UDP bootstrap resolver matching the namespace default.

The first isolated end-to-end run exposed a second issue: restarting the gateway
replaced OpenVPN while NetworkManager retained the old active tunnel. The proxy
SOCKS probe succeeded but host HTTPS failed. The desktop policy action now captures
the owned profile state, disconnects it before the policy change, and reconnects
it only if it was active and the resulting gateway is ready. This also applies
after a successful rollback. Other VPN profiles and initially disconnected native
profiles are not activated or disconnected. Mode changes still involve a brief
reconnection; they are not a zero-interruption engine reload.

Rollback now reports compensation failures, uses an uncanceled bounded context,
and clears the fixed gateway's systemd start limit before restarting. An inactive
or failed service can have its policy changed without implicitly starting it.

Validation:
- `go test -race ./...`: PASS.
- Bun typecheck and 71 tests: PASS before desktop reconnect change; final rerun
  recorded in the PR evidence.
- Isolated Arch install/reinstall/removal and compiled TUI checks: PASS.
- Dedicated `vpn-native-lab` guest on nl-2-nvme, installed package:
  `VPNKIT_NATIVE_VM_LAB=1 python3 test/native-policy-installed-vm-test.py`: PASS.
- Actual strict → smart → strict: gateway active and public HTTPS succeeds after
  each transition, explicit bootstrap DNS verified in generated smart config.
- Injected closed-loopback remote rule-set URL in the guest's public template:
  actual candidate startup fails, source returns to strict, gateway and tunneled
  HTTPS recover. Exact public template restored in finally; no production source,
  endpoints or credentials are stored in this report.
- Unit regressions: cancellation during candidate startup does not cancel rollback;
  rollback failures are surfaced; stopped service stays stopped; only a previously
  active owned NM profile is reconnected, including on candidate failure.

The original passing suites covered lifecycle/profile/upgrade paths but not this
policy transition. The new dedicated opt-in VM runner closes that specific gap.
