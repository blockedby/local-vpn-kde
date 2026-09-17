# Local VPN KDE

A Docker VPN gateway for KDE, with NetworkManager integration and a terminal UI. Choose servers, check ping and website availability, and measure download speed.

## Install

Linux x86-64 with KDE: Ubuntu 24.04+, Debian 13+, Fedora, or Arch/CachyOS. You need internet access and administrator privileges through sudo or Polkit.

```bash
git clone https://github.com/blockedby/local-vpn-kde.git
cd local-vpn-kde
./install.sh
./run.sh
```

Run as your regular user, **without sudo**. The installer requests administrator access when needed, installs dependencies (including Go, Bun, Docker Compose, and OpenVPN), builds the app, and configures the KDE profile. Install Git through your system package manager first if it is missing.

In the app, open **Subscription** (Подписка), paste your subscription URL, start the gateway, and select a server. Connect the VPN manually.

Use `./run.sh` for subsequent launches. To update, run `git pull` followed by `./install.sh`.

Subscriptions, keys, and logs stay in the gitignored `secrets/` directory.
