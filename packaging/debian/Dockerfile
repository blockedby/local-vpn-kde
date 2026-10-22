# Disposable Debian 13 amd64 builder/runtime fixture. No host installation.
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl unzip golang-go python3 dpkg-dev binutils file \
    openvpn network-manager network-manager-openvpn nftables iptables iproute2 \
    polkitd pkexec passwd util-linux systemd libstdc++6 zlib1g libbrotli1 libzstd1 \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fL --retry 2 https://github.com/oven-sh/bun/releases/download/bun-v1.4.2/bun-linux-x64-baseline.zip -o /tmp/bun.zip \
 && echo 'c678040f14fe0440eb839d37cbd0ce4c051a32da72806ac97de6a6aab6bf728f  /tmp/bun.zip' | sha256sum -c - \
 && unzip -q /tmp/bun.zip -d /tmp/bun \
 && install -m755 /tmp/bun/bun-linux-x64-baseline/bun /usr/bin/bun \
 && rm -rf /tmp/bun /tmp/bun.zip
RUN curl -fL --retry 2 https://github.com/SagerNet/sing-box/releases/download/v1.14.1/sing-box-1.14.1-linux-amd64.tar.gz -o /tmp/sing-box.tar.gz \
 && echo '12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f  /tmp/sing-box.tar.gz' | sha256sum -c - \
 && mkdir /tmp/sing-box \
 && tar -xzf /tmp/sing-box.tar.gz -C /tmp/sing-box --strip-components=1 \
 && install -m755 /tmp/sing-box/sing-box /usr/bin/sing-box \
 && mkdir -p /usr/share/licenses/sing-box \
 && install -m644 /tmp/sing-box/LICENSE /usr/share/licenses/sing-box/LICENSE \
 && rm -rf /tmp/sing-box /tmp/sing-box.tar.gz
RUN useradd --create-home --shell /bin/bash builder
WORKDIR /work
