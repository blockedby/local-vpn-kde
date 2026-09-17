FROM debian:13
ENV container=docker
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv dbus sudo iproute2 ca-certificates git curl python3 udev && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash tester \
    && echo 'tester ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tester \
    && chmod 440 /etc/sudoers.d/tester
# Docker supplies eth0 and its default route; no DHCP wait is needed here.
RUN ln -s /dev/null /etc/systemd/system/NetworkManager-wait-online.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
