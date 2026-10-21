# Combined speed/site batch validation — 2026-10-19

Candidate worktree/branch: `.worktrees/native-speed-batch`,
`perf/native-speed-batch`, based on `d324e11965488217858e78f3e25193c8fa9d7b75`.
The package below contains the frozen candidate working-tree snapshot; the Git
base alone does not identify the implementation. No stable approval or release,
paid CI, host installation, or host VPN changes are implied.

## Behavior and local checks

`servers speed-batch` runs fresh reachability, HTTPS website availability and
throughput in one supervised operation. Up to five workers prepare isolated
proxies; throughput downloads share one semaphore. Each reachable server reuses
one temporary sing-box for the website and throughput requests. Website failure
does not suppress throughput. Completed rows are durably published before their
completion event; cancellation preserves those rows and joins probe processes.
The three-second benchmark budget covers connection, SOCKS/TLS, headers and body;
ping, proxy startup, website checking and cleanup are additional preparation.

- `go test -race ./...`: PASS on frozen production code.
- Rebuilt `.build/local-vpn-kde.bin`; `bun run check`, `bun test`: PASS,
  74 tests and 346 assertions.
- `bash -n scripts/vpnkit/vpnkit-local.sh`: PASS.
- Existing host-mocked server bridge test: PASS, including speed-batch and
  old-backend detection. It does not replace the installed isolated checks.
- Backend race regressions cover one proxy per reachable node, concurrent
  preparation, serialized throughput, failed-site independence, unreachable-node
  skipping, cancellation while waiting for throughput, durable completed rows
  after cancellation, concurrent-catalog rejection and shared-stream validation.
- `go test -race ./cmd/vibe-vpn -run TestSpeedBatchSyntheticTiming -count=1 -v`:
  four artificial local nodes, each preparation 80 ms plus throughput 30 ms;
  serial 444 ms, pipeline 201 ms, ratio 0.45; maximum active downloads **1**.
  These figures measure scheduling with synthetic delays, not Internet speed.
- `VPNKIT_NATIVE_ARCH_TEST_ARTIFACT_DIR=/home/kcnc/code/temp/native-speed-batch/artifacts bash test/native-arch-package-build-test.sh`:
  PASS, isolated pinned Arch container, offline install/reinstall/removal,
  compiled TUI, portable installer and package-inhibitor recovery.

Package version:
`0.0.0.r81.gd324e1196548.snapshot.7ccd857134a7-1`

Artifact:
`local-vpn-kde-native-0.0.0.r81.gd324e1196548.snapshot.7ccd857134a7-1-x86_64.pkg.tar.zst`

SHA-256:
`dfd29ceccf71e56d1560275b498aa662dac278287fc4bc30f3f368ea9d85de73`

## Dedicated installed VM

Used only the existing `vpn-native-lab` guest on `nl-2-nvme`, through the dedicated
`vm-lab/ssh.sh` wrapper and lab passwordless sudo/Polkit authorization.
No real subscription, endpoint, key, generated configuration or private log was
copied to this report. Opaque IDs and private runtime hashes remain in the guest.

Before upgrade, `VPNKIT_NATIVE_VM_LAB=1 python3 test/native-speed-batch-installed-vm-test.py --baseline`
passed on installed `6773b70b908e` snapshot `973db22b7c06`: two distinct existing
working nodes, six successful old separate ping/site/speed requests, **14.271 s**
total, with running gateway/proxy PIDs, namespace, route and private runtime
configuration hashes unchanged. The candidate runs the same three measurements
against the same two opaque IDs and HTTPS website target.

Candidate hot-upgrade: PASS, transferred package SHA-256 matched the local
artifact, 30 HTTPS samples, zero failures. Running gateway/proxy process IDs,
namespace, route, active connections and pinned runtime binary hashes unchanged.
The package inhibitor cleared without reconnecting the guest VPN.

`VPNKIT_NATIVE_VM_LAB=1 python3 test/native-speed-batch-installed-vm-test.py`:
PASS on the installed candidate. Two successful nodes, no failed rows in the
comparison pass, same ping/site/speed work and same HTTPS target: **8.097 s**
combined versus **14.271 s** old separate calls (43.3% lower elapsed in this run).
This is one real-network comparison, not a guaranteed speedup or benchmark of
throughput quality.

Additional installed acceptance passed:

- Actual `start`, `ping`, `speed`, `complete` progress frames. Throughput start
  spacing covers the preceding server's reported download interval; local race
  tests separately establish maximum concurrent downloads one.
- A failed HTTPS website check still produces successful throughput evidence.
- Synthetic unreachable direct profile plus working existing node: one failed
  ping/speed row, one successful speed row, transport/batch remains successful.
- Cancel after the first actual durably persisted completion: cancellation
  classified correctly; saved row matches its streamed downloaded-byte count.
  The pass including work before cancellation took **3.758 s**; this figure is
  not cancellation latency. A subsequent combined probe succeeds.
- Selected server, main route, gateway/proxy PIDs, namespace inode and all three
  private runtime configuration hashes unchanged. Temporary synthetic profile
  removed and original active source restored in finally.

The guest remains connected through the same pre-upgrade running VPN. New
management binaries came from the candidate package; no live reconnect or
runtime binary replacement was performed.

Limitations: throughput serialization applies within the batch. Independent CLI
or TUI sessions and unrelated network traffic can still affect throughput. No KDE
visual or interactive Polkit-prompt test, no host deployment, and no stable tag.
