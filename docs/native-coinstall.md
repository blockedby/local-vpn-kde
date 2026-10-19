# Native and Docker installations on one workstation

Keep the Docker VPN running while installing/configuring the native package.
The package does not start the gateway or connect NetworkManager automatically.
Before starting the native gateway, stop/disconnect the Docker VPN using its
existing TUI. Native preflight still refuses a VPN default route; this change
supports separate installations and deliberate switching, not two competing
full-tunnel connections. The Docker engine and unrelated containers may stay up.

| Resource | Docker installation | Native installation |
|---|---|---|
| Local OpenVPN UDP endpoint | 127.0.0.1:21194 | 127.0.0.1:31194 |
| OpenVPN subnet | 10.89.0.0/24 | 10.90.0.0/24 |
| Underlay/transit | 172.30.89.0/24 | 172.30.90.0/30 |
| NetworkManager profile | vpnkit-local | vpnkit-native |
| User autostart unit | local-vpn-kde-autostart.service | local-vpn-kde-native-autostart.service |
| Profile capability directory under private state | state | state-native |

Native system units, /etc and /var/lib directories, user state, namespace and
nft table already have separate ownership from the checkout's Docker volumes
and secrets. Native import never adopts the Docker profile or its capability.
Disabling native autostart never modifies the Docker user unit.

Docker may leave an iptables FORWARD DROP policy even after its VPN container
stops. Native adds two narrow, comment-labelled rules for its own transit
address and physical interface; it does not change the policy or Docker chains.
The existing native nft table still restricts ingress to established return
traffic. Both rules participate in the ownership journal and are removed only
when their exact contents and uniqueness are verified. Network journal schema
is now version 3. The package's old-version pre-upgrade hook must stop the old
native gateway before replacing its binaries; do not copy binaries over an
active old service. Restarting/changing host firewalls during a native session
is outside the ownership transaction's concurrency guarantee.

For an earlier native preview using vpnkit-local/21194, stop it before upgrading
and rerun subscription setup from the new TUI. The old profile and old autostart
unit are intentionally not adopted or deleted because those names may belong
to the working Docker installation. Review any old autostart setting before
logging in; the new native unit remains independently opt-in.

## Validation

- Regression tests import native with an active Docker profile and retained
  legacy capability; Docker name/capability bytes remain unchanged.
- Native autostart creation preserves the Docker user unit byte-for-byte.
- Full `go test -race ./...` passes; OpenTUI check and 67 tests pass after merging
  the compiled UI installer into the native branch.
- `test/native-gateway-network-container-test.sh` runs the real journal,
  controller and inspector with FORWARD DROP and an unrelated rule. Start,
  stop and recovery pass, and the complete pre/post FORWARD rules are identical.
- `test/native-gateway-vpn-container-test.sh` passes both root and service-UID
  packet-path fixtures with FORWARD DROP; gateway loss remains fail-closed for
  a connected test client.
- `test/native-arch-package-build-test.sh` passes the single-installer/offline
  install, compiled UI PTY, profile fixtures, reinstall and uninstall checks.

These are isolated lab results, not proof of a live workstation cutover.
KDE authorization, local subscription setup and the eventual real switch must
be recorded separately. No stable release is implied.
