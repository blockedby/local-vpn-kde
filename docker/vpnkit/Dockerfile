FROM golang:1.24-bookworm AS vibe-vpn-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/vibe-vpn ./cmd/vibe-vpn \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/vibe-vpn-server-op-supervisor ./cmd/vibe-vpn-server-op-supervisor \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/local-vpn-kde ./cmd/local-vpn-kde

FROM debian:bookworm-slim
ARG SING_BOX_VERSION=1.13.11
ARG SING_BOX_SHA256=10ff037632165ca4f6472a0ec21393280ef5a33677e05bcde7fbcf6f9737637b
# bookworm-slim does not contain a CA bundle before ca-certificates is installed.
# Bootstrap the bundle from the pinned Debian-based builder so APT can use HTTPS.
COPY --from=vibe-vpn-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g; s|http://security.debian.org|https://security.debian.org|g' /etc/apt/sources.list.d/debian.sources \
 && apt-get -o Acquire::Retries=3 -o Acquire::https::Timeout=30 update \
 && apt-get -o Acquire::Retries=3 -o Acquire::https::Timeout=30 install -y --no-install-recommends ca-certificates curl iproute2 iptables nftables openvpn tcpdump bash procps tar util-linux \
 && curl -fsSL --retry 3 --connect-timeout 20 -o /tmp/sing-box.tar.gz "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/sing-box-${SING_BOX_VERSION}-linux-amd64.tar.gz" \
 && printf '%s  %s\n' "$SING_BOX_SHA256" /tmp/sing-box.tar.gz | sha256sum -c - \
 && tar -xzf /tmp/sing-box.tar.gz -C /tmp \
 && install -m 0755 "/tmp/sing-box-${SING_BOX_VERSION}-linux-amd64/sing-box" /usr/local/bin/sing-box \
 && rm -rf /var/lib/apt/lists/* /tmp/sing-box.tar.gz "/tmp/sing-box-${SING_BOX_VERSION}-linux-amd64"
COPY --from=vibe-vpn-builder /out/vibe-vpn /usr/local/bin/vibe-vpn
COPY --from=vibe-vpn-builder /out/vibe-vpn-server-op-supervisor /usr/local/bin/vibe-vpn-server-op-supervisor
COPY --from=vibe-vpn-builder /out/local-vpn-kde /usr/local/bin/local-vpn-kde
RUN install -d -m 0755 /etc/vibe-vpn /var/lib/vibe-vpn /var/log/vibe-vpn
ENTRYPOINT ["/usr/local/bin/local-vpn-kde", "container"]
