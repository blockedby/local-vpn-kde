<div align="center">

# Local VPN KDE

**Your servers. Your subscriptions. One terminal interface.**

VPN for Linux and KDE, integrated with NetworkManager.<br>
Run natively without Docker, or use a dedicated Docker gateway.

[Русский](../README.md) · **English**

[Install](#install) · [First connection](#first-connection) · [Updates](#updates) · [Documentation](#documentation)

</div>

---

## Features

| Feature | What you can do |
| :--- | :--- |
| **Multiple profiles** | Keep subscriptions and individual server links, with a separate server list for each profile |
| **Your own server** | Add `vless://`, `hysteria2://` and `hy2://` links directly in the TUI |
| **Server checks** | Measure latency and download speed, and check a website of your choice |
| **Routing modes** | Switch between strict and smart routing from the interface |
| **Connection controls** | Start the gateway, connect and disconnect the VPN as separate actions |
| **Autostart** | Start the gateway at KDE login, optionally connecting the VPN |
| **Diagnostics** | Inspect service status and run checks in the same interface |

In the native runtime, choosing a profile changes the displayed server list. Applying a server and connecting the system VPN are separate actions.

## Install

Choose your runtime:

| | Native | Docker |
| :--- | :--- | :--- |
| **System** | Arch Linux / CachyOS, x86_64 | Ubuntu 24.04+, Debian 13+, Fedora, Arch / CachyOS, x86-64 |
| **Gateway** | System service, no Docker | Docker container |
| **Installation** | Prebuilt local bundle | Build from the repository |
| **Launch** | `kvpn` | `./run.sh` |

Both options target Linux with KDE and NetworkManager. Installation requires internet access and administrator privileges.

### Native · one sudo command

Your trusted bundle directory must contain `install-native.sh`, the executable `vpnkit-portable` and its checksum file. Verify the checksum, then install **from your regular desktop account**:

```bash
sha256sum -c vpnkit-portable.sha256
sudo bash ./install-native.sh
```

The installer adds missing system dependencies, installs the application and sets up user permissions. If configuration already exists, it prepares the NetworkManager profile. On a fresh installation, the profile is created after server setup, on first connection.

**Installation does not start or connect the VPN.** The target computer needs no Go, Bun or Docker. System components — NetworkManager, its OpenVPN plugin, OpenVPN, systemd, polkit and networking utilities — are installed through pacman.

Launch the interface **without sudo**:

```bash
kvpn
```

> Native bundles are currently built locally; these instructions do not assume a published release download. See [native installer](portable-installer.md) for bundle contents and build commands.

### Docker · from source

Install Git through your package manager if needed. Then run **without sudo**:

```bash
git clone https://github.com/blockedby/local-vpn-kde.git
cd local-vpn-kde
./install.sh
./run.sh
```

The installer requests administrator access when needed, installs build and runtime dependencies including Go, Bun, Docker Compose and OpenVPN, and prepares the KDE profile.

## First connection

The interface opens on a home screen with Connection («Подключение»), Servers («Серверы»), Measurements («Замеры»), Profiles («Профили»), Diagnostics («Диагностика») and Settings («Настройки») tiles. Use the arrow keys to choose a tile and `Enter` to open a section or perform the connection action; `Esc` returns from a section to home. In nested screens and menus `Esc` closes those first; in the server table it first clears a filter. A breadcrumb shows the path to the section; `Tab` / `Shift+Tab` cycle through Servers, Profiles, Diagnostics and Settings.

1. Press `c` and add the first subscription or an individual server link. Once installed native mode is configured, `c` opens Profiles («Профили»).
2. If the service is stopped, start it using the **Connection** tile or `s`. Open **Servers** (`v`, «Серверы»); refresh the list if needed (`r`).
3. Choose a row with the arrows and press `Enter`. The menu offers Select server («Выбрать сервер»), Ping and site («Ping и сайт»), Server speed («Скорость сервера»), Server details («Подробности сервера») and Rename («Переименовать»). Choose **Select server** and confirm: No («Нет») is selected by default, and Left/Right move between the buttons.
4. Return home and choose **Connection** or press `s` to connect the VPN.

**“Service ready” does not mean “VPN connected.”** The service can run checks while the system VPN connection remains off.

### Controls in the TUI

| Key | Action |
| :---: | :--- |
| `s` / `x` | Start the service or connect the VPN / disconnect the VPN, depending on state |
| `v` | Servers and connection checks |
| `c` | Profiles in installed native mode with a configured subscription; otherwise the subscription editor, if available |
| `m` | Strict / smart routing |
| `a` | Autostart settings |
| `d` | Diagnostics |
| `i` | Settings |
| `Tab` / `Shift+Tab` | Next / previous section among the four listed above |
| `/` | Filter displayed and original names and country code; `Enter` keeps it, `Esc` clears it |
| `p` / `t` | Ping and site / speed and site for shown servers |
| `T` / `b` | Test the speed of the row under the cursor / offer the fastest measured shown server for confirmation |
| `o` / `[` / `]` / `\` | Cycle sort (name, ping, speed, site, country) / choose column / reverse direction |
| `l` | Details for the row under the cursor |
| `:` / `Ctrl+P` | Open the shortcuts table with search focused |
| `j` / `k` | Activity / cancel operation |
| `X` | Remove the active profile after confirmation |
| `z` | Stop the service or Docker and the VPN after confirmation |
| `?` / `q` | Open keyboard shortcuts / exit the interface |

In the shortcuts table, `Tab` / `Shift+Tab` switches between this screen, global actions, and all actions. `/` focuses search. Select a row with `↑` / `↓`, run it with `Enter`, or close the table with `Esc`.

The status line names the selected server and the footer shows available keys. `a` opens autostart; it does not run ping. Checks can overlap or wait in a queue; `j` shows running, queued and finished operations. Available actions and shortcuts depend on the section and service state.

The server table shows name, two-letter country code, ping, speed, website access and status. The Country («Страна») column is hidden when the table is narrower than 60 characters. You can also choose a sort by clicking a column header.

To rename a server, press `Enter` on its row and choose Rename («Переименовать»). In the editor, `Enter` saves, `Esc` cancels, and an empty line restores the subscription name. A name can be up to 60 characters; surrounding and repeated spaces are removed. You can save up to 500 renamed servers. The local state file `server-names.json` holds server IDs and the chosen names; they survive subscription refresh and restart and are not removed when a profile is deleted. Details show the renamed server's original name.

Country lookup is offline, using the embedded [server-country](https://github.com/sapics/ip-location-db) dataset (public domain, PDDL 1.0); no server address is sent to a country lookup service. A known IP's code is available immediately; for a hostname it may appear after a successful ping, check or speed measurement. TCP ping uses the connected address; a proxy lookup with ambiguous or unknown addresses supplies no new code, and a previously determined code may remain. The embedded database is a snapshot, so a code can become outdated.

Autostart settings apply at the next KDE login. You can start only the gateway or also connect the VPN. Disabling autostart leaves the current connection running; the TUI does not open automatically at login. Docker may also restart an already-running gateway through its container restart policy.

### When another VPN is connected

Native can run alongside another VPN. Connecting native gives its routes priority for ordinary full-tunnel traffic; more-specific routes retain precedence. Other profiles are not modified. If routing cannot be verified, only the attempted native connection is rolled back.

A missing application-owned NetworkManager profile is created automatically. Foreign or modified profiles are not overwritten. See [VPN coexistence and profile protection](native-coinstall.md).

### Wi-Fi works after sleep but the VPN carries no traffic

If the native TUI shows **Шлюз без выхода в сеть**, first check that Wi-Fi or Ethernet has an IPv4 lease. A running native gateway repairs its private routes on uplink changes; **Connect** and **Refresh subscription and list** also check them. You do not need to disconnect another VPN or flush any host routes. If repair fails, inspect `journalctl -b -u local-vpn-kde-egress.service`.

Route repair is always on. To also connect the VPN after waking, enable **Settings → Автостарт после сна** in your desktop session. This separate option is off by default. It makes at most one connection attempt per wake, skipping competing tunnels. The result is recorded as `resume-connect` in the user's private `local-vpn-kde/diagnostics` state directory (including `other-vpn-active` for a skipped attempt).

## Updates

**Native (Arch or Debian package):** self-update is available from 0.4.4. Since 0.5.0 the command is only `kvpn`. On 0.4.4, which predates `kvpn`, update with `local-vpn-kde update`.

```bash
kvpn update                   # asks before installing, then offers to restart a running gateway
kvpn update --check           # only check
kvpn update --yes --restart   # install and restart a running gateway without asking
kvpn restart                  # restart the gateway later
```

The installation itself keeps the VPN connected. If the gateway is running, `kvpn update` then asks whether to restart it on the new version: the VPN drops for a few seconds and reconnects if it was connected. Declining (or `--yes` alone) keeps the old gateway running until you run `kvpn restart` or reboot.

In the TUI, open **Settings → [u] Check for updates**. After installation, **[g] Restart gateway** is offered if the gateway is running; **[R] Restart interface** reopens only the TUI. The updater takes the latest GitHub release, verifies the `SHA256SUMS` checksum and the package name, version and architecture, and installs it only if no other package changes. Members of `local-vpn-kde-users` need no password. Releases are not signed yet: the check catches corrupted or mixed-up downloads, not a replaced release on GitHub. Before 0.4.4, upgrade once by hand (`sudo pacman -U` with the release package).

Without a package, run the same installer from a new verified bundle:

```bash
sudo bash ./install-native.sh
```

Compatible application upgrades preserve the active connection. Reopen the TUI to load the new interface. The running VPN engine receives its new version when you explicitly restart the service. System dependency upgrades and migration from older packages without this support have separate limitations — see [upgrades without disconnecting](native-hot-upgrade.md).

**Docker:** from the repository directory:

```bash
git pull
./install.sh
```

## Documentation

| Topic | Guide |
| :--- | :--- |
| Native bundle contents, dependencies and build | [Portable installer](portable-installer.md) |
| Running alongside other VPNs | [Native coexistence](native-coinstall.md) |
| Updating a running application | [Hot upgrade](native-hot-upgrade.md) |
| Native architecture and source profiles | [Native runtime](native-runtime.md) |
| Installer and connectivity checks | [E2E matrix](native-e2e.md) |

Technical guides are primarily in English. Subscriptions, keys and generated configurations are private data; do not include them in Git or bug reports. Source installations keep private data in the ignored `secrets/` directory; the native package uses separate private user and service directories.

## Development and testing

Run local checks from the repository root:

```bash
go test -race ./...
mkdir -p .build
go build -o .build/local-vpn-kde.bin ./cmd/local-vpn-kde
cd scripts/vpnkit/tui
bun install --frozen-lockfile
bun run check
bun test
```

Validate installation and network changes in isolation:

- `test/native-arch-package-build-test.sh` — build and install the native package in a disposable Arch container.
- [Native E2E](native-e2e.md) — NetworkManager, profiles, routing and autostart checks in a dedicated VM.
- `test/vpnkit-local-install-container-test.sh --help` — Docker installer and connectivity checks, lab requirements and options.

Tests using real subscriptions receive them through supported environment variables, without saving them in the repository. Keep the host's working VPNs, DNS and routes out of the test environment.
