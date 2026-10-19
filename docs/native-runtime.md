# Docker-free KDE backend (in development)

**Not installable or ready for live VPN use yet.** Docker remains the default.
Do not install the unit templates manually: installation, desktop authorization,
and real systemd/NetworkManager integration have not passed acceptance.

## Architecture

KDE/NetworkManager retains the owned OpenVPN profile at `127.0.0.1:21194/udp`.
OpenVPN and sing-box run in the dedicated `vpnkit-native` network namespace,
not a host TUN or a proxy-only replacement. Transit is `172.30.89.0/30`.

The bounded root controller rejects foreign resources and network overlaps,
including Docker networks. Exact kernel identities and a durable, locked journal
control cleanup. A fixed nftables table translates the localhost endpoint.
Physical-uplink policy routing has an unreachable fallback to avoid following
the VPN default route. Namespace IPv6 is disabled. Cleanup deliberately leaves
shared host IPv4 forwarding enabled: exclusive ownership cannot be proved.

The gateway service is intended to run with only `CAP_NET_ADMIN` and
`CAP_NET_RAW`. Root initialization verifies that the target is not the initial
network namespace, applies fixed namespace sysctls, and publishes a protected
identity record. The restricted service validates that record and the current
namespace without needing access to `/proc/1/ns/net` or extra capabilities.
Systemd readiness is notified after owned-child and gateway network checks.

Binaries, configuration, and service definitions must be installed in protected
locations. A privileged service must never load executables from a mutable
checkout. The desktop service client accepts only the fixed managed units;
authorization denial is an error, not an automatic sudo escalation.

## Implemented interfaces

- `internal/nativegateway`: discovery, planning, kernel inspection, durable
  ownership journal, start/stop/status, namespace identity proof, unit templates.
- `native-network`, `native-namespace-init`, `native-serve`: fixed internal
  entrypoints; not general-purpose privileged command adapters.
- `render`, `assets`, `prepare --runtime native`: generate native inputs and the
  fixed localhost profile, **not** an installed gateway. Docker defaults remain.
- `native-desktop`: native service and owned NetworkManager lifecycle adapter.
- CLI bridge/OpenTUI `--runtime native`: experimental lifecycle integration;
  unsupported actions must fail closed, never fall back to Docker.

Automatic bootstrap selection and DNS failover remain disabled. The rendered
placeholder outbound is deliberately nonfunctional until a real server is
selected. Process startup is not evidence of internet or DNS readiness.

## Autostart

Native autostart wiring uses fixed system units and owned NetworkManager
operations, not Docker. Persisted runtime identity prevents enabled Docker
intent from silently becoming native intent, including legacy settings files.

Autostart is explicit and opt-in. Gateway startup and VPN autoconnect are
separate choices; autoconnect requires gateway startup. Disabling autostart does
not disconnect an active VPN. No implicit lingering or boot reconnect is allowed.
The real login/systemd/NetworkManager flow is still unverified.

## Evidence and remaining gates

Passed disposable labs cover durable journal close/reopen, repeated start/stop,
exact kernel cleanup/preservation, localhost UDP transit, and physical policy
routing. The root full-VPN fixture passes real OpenVPN/sing-box traffic to a
synthetic WAN HTTP server through pushed `/1` routes. HTTP fails after sing-box
loss while the client and `/1` routes remain active.

The restricted-service full-VPN fixture also passes, with UID/GID 987, only
NET_ADMIN/NET_RAW in all capability sets, and no-new-privileges. It uses
root-owned group-readable configuration and service-owned private state/runtime.
Namespace-identity and helper-config defects exposed by this fixture are fixed.
Both root and service modes passed the full fixture rerun.

Remaining acceptance includes:

- Real systemd unit hardening validation (beyond the passing capability fixture).
- Safe installation/removal, binary provenance, account/config permissions,
  and narrow desktop authorization.
- Native server selection, diagnostics, and full CLI/OpenTUI integration.
- Real NetworkManager/KDE and login/autostart integration.
- Uplink changes, DNS readiness, and failure behavior after OpenVPN exits and
  restores host routes. The fixture does **not** prove a host kill switch.
- Fresh Go race tests, Go bridge build, OpenTUI typecheck/tests, and both labs.

Containers are used only as disposable test isolation, not as a delivered
native runtime dependency. Never mutate the host VPN, resolver, routes, or
production containers for validation. No release or stable approval is implied.
