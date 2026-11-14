<div align="center">

# Local VPN KDE

**Your servers. Your subscriptions. One terminal interface.**

VPN for Linux and KDE · NetworkManager · Native and Docker

[Русский](README.md) · **English**

</div>

![Main screen: VPN connection, smart routing and service controls](docs/images/home.png)

Subscriptions and individual **VLESS / Hysteria2** links, separate server lists, strict and smart routing, autostart and diagnostics — all in one TUI.

## Servers and profiles

Check latency, download speed and access to your chosen website before connecting.

![Server list with connectivity and speed results](docs/images/servers.png)

Keep your own VPS alongside multiple subscriptions. Selecting a profile changes the server list; connecting the VPN is a separate action.

![Subscription profiles and an individual Hysteria2 server](docs/images/profiles.png)

<sub>Captures of the actual interface with demonstration data. No private addresses or keys are used; measurements are illustrative. The TUI currently displays Russian labels.</sub>

## Install

Arch / Debian packaging for `v0.2.0`: [candidates and manual CI](docs/native-packages.md). Packages are not published yet.

### Native · Arch / CachyOS x86_64

From a trusted local bundle directory containing `vpnkit-portable`, its checksum and `install-native.sh`:

```bash
sha256sum -c vpnkit-portable.sha256
sudo bash ./install-native.sh
local-vpn-kde
```

Run the installer once with sudo, then launch the interface as your regular user. No Go, Bun or Docker is needed on this computer. Installation does not connect the VPN.

[Build a bundle and see what it contains →](docs/portable-installer.md)

### Docker · Linux x86-64 with KDE

Ubuntu 24.04+, Debian 13+, Fedora, Arch / CachyOS. Run without sudo — the installer requests privileges when needed:

```bash
git clone https://github.com/blockedby/local-vpn-kde.git
cd local-vpn-kde
./install.sh
./run.sh
```

## Get connected

Press `c` to set up the first subscription or link → **`v` Servers** → select a server → **`s` Connect VPN**. If the service is stopped, start it using the same key first. With a configured subscription, installed native mode uses `c` to open Profiles; you can also reach that tab with `Tab`.

`m` — routing mode · `a` — autostart · `d` — diagnostics · `x` — disconnect VPN.

The server table opens first. `Tab` cycles through Servers, Profiles, Diagnostics and Settings; the status line shows the connection, selected server and routing mode. `?` opens help, `:` or `Ctrl+P` searches commands, and `j` opens activity. Checks can overlap or wait in a queue; cancel a task from the activity panel.

In the table, `/` filters names, `Enter` keeps the filter and `Esc` clears it. `p` checks ping and website access and `t` checks speed and website access for the shown servers; `T` tests the row under the cursor and `b` offers the fastest measured shown server for confirmation. `o` cycles sort columns, `[` and `]` choose a column, and `\` reverses direction; clicking a header also sorts it. `l` opens server details, `i` opens settings, and `X` removes the active profile after confirmation. `a` opens autostart; it does not run ping.

## Learn more

[User guide and updates](docs/guide.en.md) · [VPN coexistence](docs/native-coinstall.md) · [Upgrades without disconnecting](docs/native-hot-upgrade.md) · [Testing](docs/native-e2e.md) · [Release process (RU)](docs/releases.md)
