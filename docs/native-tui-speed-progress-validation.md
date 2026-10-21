# Native TUI speed progress and runtime identity

## Failure

On 2026-10-21, “Скорость + сайт” stopped at “Проверено: 0/1” with the
management-service-unavailable message. A ping result remained visible, but the
site and speed result did not complete. After another status poll, the same
interface offered to start Docker despite having identified a native install.

The Go speed batch emits `start`, `ping`, `speed`, and `complete` frames. Its
`CheckStream` validates and forwards `speed`, and the app renderer already
understood that stage. However, the TypeScript `Bridge` wire validator accepted
only `start`, `ping`, and `complete`. A valid `speed` frame therefore triggered
`Bridge.fail()`, rejecting pending requests and permanently closing the logical
transport. This did not require the Go child or native gateway to die.

The next status poll failed through that closed transport. The app cleared its
whole status, including known runtime identity; its legacy fallback selected
Docker labels. Native start/stop action-label overrides already existed. The
misleading Docker screen came from losing runtime identity, not from a native
measurement being routed through Docker.

## Fix and local verification

Product revision: `162f5aea5a86daca7821f9b3795566d0c54b36c9`.

- `289a3ce`: accept the known `speed` stage, requiring a successful ping and a
  determined site result. Unknown stages and incomplete evidence remain errors.
- `162f5ae`: preserve a previously known native/docker runtime and installation
  identity after failed status polling; reset service, VPN and NetworkManager
  readiness to unknown. An initial failure does not invent native identity.

Native measurements retain the existing installed route: fixed native adapter
→ `native-operation-client` → Polkit worker → native network namespace →
unprivileged server-operation supervisor. No Docker routing change was made.

```sh
go test -race ./...
(cd scripts/vpnkit/tui && bun run check && bun test)
```

Passed: full Go race suite on unchanged Go code; OpenTUI typecheck; **79 tests,
0 failures, 374 assertions**.

The transport regression runs the real Go bridge and `CheckStream`, forwards all
four stages into the real TypeScript `Bridge`, receives the final catalog, then
performs another status request. Restoring the old stage whitelist makes this
regression fail with `backend-unavailable`. Its lifecycle adapter is an inert
fixture, using the shared adapter override; it invokes neither Docker nor
privileged networking and does not claim installed-native coverage.

Render regressions open the native server screen, lose two successive status
polls, and verify native labels, unavailable status, no Docker offer, and no
stale connected state. Separate held-action tests check native and Docker
start/stop labels. Existing deterministic-backend UI tests bypassed the wire
validator, while earlier installed Go-bridge tests bypassed the compiled TUI;
that coverage gap allowed the protocol mismatch to escape.

## Installed acceptance

The first candidate harness completed the batch without losing the bridge, but
reported a failed ping. Its profile-selection wait was insufficient: the generic
“Добавить профиль” screen appeared before asynchronous `profiles/list` finished,
so the numeric selection key could be ignored. Those runs did not establish
that the intended temporary profile was measured. The corrected harness waits
for the unique fixture label and verifies the selected profile header and server
row before submitting the batch. These earlier measurement results are not
accepted as either positive coverage or evidence of a new product defect.

The corrected, untraced installed runner passed in the Docker-free
`vpn-native-lab` guest using the real compiled TUI, installed Go bridge, Polkit
worker and real authorized one-server profile:

- Speed and site completed, rendered the real speed stage, and allowed another
  diagnostic action.
- An unreachable loopback site failed while throughput remained positive;
  another diagnostic action still worked.
- Cancellation during the real speed stage completed and allowed another action.
- Terminating the owned bridge child preserved native identity, cleared readiness
  to unknown and displayed no Docker offer.

Run inside the dedicated configured guest, with QA-only `pyte` installed:

```sh
VPNKIT_NATIVE_VM_LAB=1 python3 test/native-speed-batch-tui-installed-vm-test.py
```

The runner requires its explicit guest hostname and Docker absence guards. It
creates and removes a temporary profile, compares the original profile list,
active source/configuration hashes and gateway PID, and leaves the runtime
unchanged. Private terminal frames and source data are never public evidence.

Tested package:
`local-vpn-kde-native-0.0.0.r87.g162f5aea5a86.snapshot.78d6d901e47f-1-x86_64.pkg.tar.zst`.
SHA-256: `fdfe5f2284a7bc635c4fda119c44ce659d5bf76c68aab7cf09cf598a8b758615`.
The sanitized local evidence is `compiled-ui-acceptance.log`; the earlier
diagnostic run also passed all four cases. The installed baseline reproduced the
old speed-stage transport failure. These disposable guest checks do not reboot
or reconnect the host VPN.
