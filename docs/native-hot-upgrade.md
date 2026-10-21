# Native package upgrades without disconnecting VPN

Compatible package upgrades now replace application files without stopping the
running gateway, OpenVPN or network namespace. Restart the TUI to load its new
version. The running gateway keeps its previous supervisor and private copies of
sing-box/vibe-vpn until an explicit service restart; this is a deferred runtime
upgrade, not migration of active sockets to a new engine.

A root-only systemd ExecStartPre snapshots the two child binaries into
`/var/lib/local-vpn-kde-runtime` (roughly 93 MiB extra disk space). The directory
is root-owned and not writable by the service user. Using disk rather than /run
also supports the dedicated 512 MiB VM. System dependency upgrades such as
OpenVPN, shared libraries or systemd itself are outside this guarantee.

Separate pacman hooks distinguish Upgrade from Remove. Upgrade takes the
configuration lock and installs the transaction inhibitor, preserving the data
plane. Existing privileged operations finish before file replacement; new ones
are refused until post-change validates assets, unit definitions and the running
runtime contract. Removal retains the existing stop-and-cleanup behavior.

The compatibility contract includes an explicit runtime ABI and a digest of
both unit definitions. Developers must bump the ABI for incompatible worker,
private-state or network-journal changes. A post-upgrade incompatibility leaves
running forwarding intact and keeps management inhibited. Pacman may already
have replaced files when a post hook fails: this is NOT automatic package
rollback. An administrator must stop the gateway, run `local-vpn-kde
native-package --action recover` as root, and restart explicitly. Do not remove
an inhibitor by hand. Failed transactions use the same bounded recovery path.

The first upgrade from packages predating this feature still invokes their old
stop-on-upgrade hook and requires one maintenance interruption. No host hook is
bypassed to avoid that transition. Subsequent compatible upgrades preserve the
connection. A package-managed snapshot is refreshed only at service startup;
security fixes to the running gateway/engine require an explicit restart.

## Dedicated lab acceptance

`VPNKIT_NATIVE_VM_LAB=1 python3 test/native-hot-upgrade-vm-test.py /tmp/candidate.pkg.tar.zst`
runs only in the existing `vpn-native-lab` guest on nl-2-nvme. It requires an
already connected native VPN, drives repeated HTTPS requests during pacman
installation, and compares gateway/child PIDs, namespace inode, active connection
UUIDs, route lookup and pinned binary hashes. It also checks that the transaction
inhibitor was cleared. Package diagnostics stay in a private guest log.

A passing request loop proves the measured upgrade did not interrupt those
requests; it is not proof that all long-lived TCP/UDP flows survive every type of
system upgrade. Stable runtime process and namespace identities provide the
additional evidence that this update does not tear down their sockets.
