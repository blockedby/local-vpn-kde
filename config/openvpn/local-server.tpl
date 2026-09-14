# Public-safe local-only OpenVPN server template.
# This keeps the full-tunnel MTU/MSS baseline while
# using an independent CA and server identity generated under secrets/vpnkit-local/.
port 1194
proto udp
dev tun0
topology subnet
server 10.89.0.0 255.255.255.0
# Keep tunnel packets below the path MTU used by the existing vpnkit server.
tun-mtu 1400
mssfix 1360
push "redirect-gateway def1 bypass-dhcp"
push "dhcp-option DNS {{OPENVPN_PUSH_DNS}}"
keepalive 10 120
persist-key
persist-tun
remote-cert-tls client
auth SHA256
data-ciphers AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC
data-ciphers-fallback AES-256-CBC
cipher AES-256-GCM
dh none
verb 3
ca /etc/openvpn/pki/ca.crt
cert /etc/openvpn/pki/server.crt
key /etc/openvpn/pki/server.key
tls-auth /etc/openvpn/pki/ta.key 0
key-direction 0
