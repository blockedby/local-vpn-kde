# Native and Docker installations on one workstation

Keep the Docker VPN running while installing/configuring the native package.
The package does not start the gateway or connect NetworkManager automatically.
The native gateway can start while the Docker VPN remains connected. Its
namespace traffic uses a dedicated physical-underlay table through source-scoped
rules at priorities 900/901, ahead of Docker rules at 998+. Host routes and the
active NetworkManager profile remain unchanged until explicit native connection.
Connecting native may keep another VPN connected: the owned native profile uses
IPv4 route metric 0, ahead of an ordinary competing full-tunnel route with a
higher metric. More-specific routes, including corporate network routes, retain
normal kernel precedence. No other VPN profile is modified or disconnected.
The Docker engine and unrelated containers may stay up.

Before reporting a successful native connection, the application proves the
owned TUN address, both native `/1` routes, and effective route lookup in both
IPv4 halves. A conflicting policy or equal-priority route can still prevent
connection. A failed activation or readiness check disconnects only the attempted
native UUID; the other VPN stays connected and its routes can take effect again.
An already-active native connection is verified without reconnecting it.
NetworkManager can translate OpenVPN's pushed `def1` option into one default
route, so native also provisions the explicit `/1` routes on its owned profile.
An older active profile without those routes needs a normal disconnect/reconnect;
installation and repeated start do not silently reroute a live connection.

First connect imports a missing native NetworkManager profile automatically.
A stale capability whose UUID no longer exists can be recreated when no profile
with the owned name collides. A proven, inactive owned profile can be refreshed
from the validated generated source. Foreign, renamed, invalid, or duplicate
profiles are refused; a name alone never authorizes adoption or deletion.
Unknown rules preceding 900, missing/ambiguous physical defaults, and reserved
resource collisions still block startup. This is not a claim of compatibility
with arbitrary third-party VPN firewalls.

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
is version 5 and records the boot identity. Supported package upgrade hooks
preserve the running gateway using its pinned runtime when available; older
preview hooks may stop their old gateway. Do not copy binaries over an active
service. Restarting/changing host firewalls during a native session
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
- Release-candidate checks are `go test -race ./...`, `bun run check`, and
  `bun test` in `scripts/vpnkit/tui`; record the exact revision and outcomes.
- Native connection regression tests exercise missing/stale-profile cold starts,
  foreign/tampered refusal, owned metric upgrades, both route halves, idempotent
  reconnect checks, and bounded rollback of a failed native activation.
- `test/native-nm-bootstrap-installed-vm-test.py` exercises installed profile
  bootstrap and controlled second-VPN coexistence in a disposable VM. Its peer
  verifies a local tunnel; the rival full-tunnel routes are a priority fixture,
  not a claim that this peer provides Internet egress.
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

The coexistence kernel lab also includes an active VPN default and Docker
underlay rules. It checks physical egress for the native namespace, unchanged
host route lookup during startup, and complete restoration after stop.

Authorization is enrolled once by the elevated portable installer (sudo/pkexec
caller) or the initial setup for a directly installed package. Polkit consults
current account membership, so enrollment and revocation work without restarting
the desktop session. Only native configuration/operations and start/stop of the
two fixed native units receive this permission; arbitrary system units do not.
