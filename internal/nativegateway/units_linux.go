//go:build linux

package nativegateway

// These unit files are rendered for a future installer. Rendering is pure: it
// neither writes files nor asks systemd to load, enable, or start anything.
const networkUnitContents = `[Unit]
Description=Local VPN KDE native network namespace
After=NetworkManager.service network-online.target
Wants=network-online.target
Before=` + GatewayUnit + `

[Service]
Type=oneshot
User=root
Group=root
RuntimeDirectory=local-vpn-kde-network
RuntimeDirectoryMode=0755
RemainAfterExit=yes
ExecStart=` + LibexecDir + `/local-vpn-kde.bin native-network --action start
ExecStop=` + LibexecDir + `/local-vpn-kde.bin native-network --action stop
`

const gatewayUnitContents = `[Unit]
Description=Local VPN KDE native gateway
Requires=` + NetworkUnit + `
After=` + NetworkUnit + `
BindsTo=` + NetworkUnit + `

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=90s
User=` + ServiceUser + `
Group=` + ServiceUser + `
NetworkNamespacePath=/run/netns/` + Namespace + `
ExecStart=` + LibexecDir + `/local-vpn-kde.bin native-serve
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s

StateDirectory=local-vpn-kde
StateDirectoryMode=0700
RuntimeDirectory=local-vpn-kde
RuntimeDirectoryMode=0700
WorkingDirectory=` + StateDir + `
UMask=0077

CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
NoNewPrivileges=yes
SetLoginEnvironment=no

DevicePolicy=closed
DeviceAllow=/dev/net/tun rw
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ReadOnlyPaths=` + ConfigDir + `
ReadWritePaths=` + StateDir + ` /run/local-vpn-kde
`

// UnitFiles returns the fixed native service artifacts. Callers may decide
// when and where to install them; this function has no installation side
// effects and does not interpolate caller-controlled values.
func UnitFiles() map[string]string {
	return map[string]string{
		NetworkUnit: networkUnitContents,
		GatewayUnit: gatewayUnitContents,
	}
}
