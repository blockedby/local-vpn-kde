# Disposable packaging/runtime laboratory, never a delivered dependency.
FROM archlinux:base
RUN pacman -Syu --noconfirm --needed \
    base-devel go bun sing-box openvpn networkmanager networkmanager-openvpn \
    nftables iptables-nft iproute2 polkit systemd curl python \
 && pacman -Scc --noconfirm \
 && useradd --create-home --shell /bin/bash builder
# Build as builder; only explicit container-local package transactions use root.
WORKDIR /work
