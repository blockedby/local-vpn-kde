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

Optional: open **Autostart** (Автозапуск) to start the gateway at KDE login and optionally connect the VPN. Settings apply on the next login; disabling autostart leaves the current connection running. The terminal UI does not open automatically. Docker may also restart an already-running gateway through its existing restart policy.

Use `./run.sh` for subsequent launches. To update, run `git pull` followed by `./install.sh`.

Subscriptions, keys, and logs stay in the gitignored `secrets/` directory.

## Container smoke test

Run `test/vpnkit-local-install-container-test.sh` to test the real installer on a clean Debian container without Go or Bun, then check the subscription, ping, speed, website access, and VPN connection. It uses a separate Docker daemon and temporary network rules through the physical uplink; it does not connect the host VPN.

Requires Docker, the `local-vpn-kde:latest` image, and an existing physical underlay routing table (default `51840`). Supply `VPNKIT_LAB_SUBSCRIPTION_URL` or `VPNKIT_LAB_SUBSCRIPTION_FILE`, or use the saved subscription. Run with `--help` for network overrides. Private logs stay in `secrets/install-lab/`. The smoke test tries the selected server first, with up to five candidates. KDE desktop interaction is not covered.
