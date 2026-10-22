<div align="center">

# Local VPN KDE

**Your servers. Your subscriptions. One terminal interface.**

VPN for Linux and KDE, integrated with NetworkManager.<br>
Run natively without Docker, or use a dedicated Docker gateway.

[Русский](README.md) · **English**

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
| **Launch** | `local-vpn-kde` | `./run.sh` |

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
local-vpn-kde
```

> Native bundles are currently built locally; these instructions do not assume a published release download. See [native installer](docs/portable-installer.md) for bundle contents and build commands.

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

1. Open **Profiles** (`c`, «Профили») and add a subscription or an individual server link.
2. Open **Servers** (`v`, «Серверы»). Refresh a new subscription (`r`), then select and apply a server.
3. Optionally check latency, website access or speed from the server list.
4. Return to the main screen. Start the service if it is stopped, then choose **Connect VPN** (`s`, «Подключить VPN»).

**“Service ready” does not mean “VPN connected.”** The service can run checks while the system VPN connection remains off.

### Controls in the TUI

| Key | Action |
| :---: | :--- |
| `s` / `x` | Start the service or connect the VPN / disconnect the VPN, depending on state |
| `v` | Servers and connection checks |
| `c` | Profiles: subscriptions and individual servers |
| `m` | Strict / smart routing |
| `a` | Autostart settings |
| `d` | Diagnostics |
| `z` | Stop the native service and its VPN |
| `Esc` / `q` | Go back / exit the interface |

Menus also support arrow keys, `Tab` and `Enter`. Available actions depend on the screen and service state.

Autostart settings apply at the next KDE login. You can start only the gateway or also connect the VPN. Disabling autostart leaves the current connection running; the TUI does not open automatically at login. Docker may also restart an already-running gateway through its container restart policy.

### When another VPN is connected

Native can run alongside another VPN. Connecting native gives its routes priority for ordinary full-tunnel traffic; more-specific routes retain precedence. Other profiles are not modified. If routing cannot be verified, only the attempted native connection is rolled back.

A missing application-owned NetworkManager profile is created automatically. Foreign or modified profiles are not overwritten. See [VPN coexistence and profile protection](docs/native-coinstall.md).

## Updates

**Native:** run the same installer from a new verified bundle:

```bash
sudo bash ./install-native.sh
```

Compatible application upgrades preserve the active connection. Reopen the TUI to load the new interface. The running VPN engine receives its new version when you explicitly restart the service. System dependency upgrades and migration from older packages without this support have separate limitations — see [upgrades without disconnecting](docs/native-hot-upgrade.md).

**Docker:** from the repository directory:

```bash
git pull
./install.sh
```

## Documentation

| Topic | Guide |
| :--- | :--- |
| Native bundle contents, dependencies and build | [Portable installer](docs/portable-installer.md) |
| Running alongside other VPNs | [Native coexistence](docs/native-coinstall.md) |
| Updating a running application | [Hot upgrade](docs/native-hot-upgrade.md) |
| Native architecture and source profiles | [Native runtime](docs/native-runtime.md) |
| Installer and connectivity checks | [E2E matrix](docs/native-e2e.md) |

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
- [Native E2E](docs/native-e2e.md) — NetworkManager, profiles, routing and autostart checks in a dedicated VM.
- `test/vpnkit-local-install-container-test.sh --help` — Docker installer and connectivity checks, lab requirements and options.

Tests using real subscriptions receive them through supported environment variables, without saving them in the repository. Keep the host's working VPNs, DNS and routes out of the test environment.
