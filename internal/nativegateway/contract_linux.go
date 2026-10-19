//go:build linux

// Package nativegateway implements the isolated, Docker-free backend for the
// KDE/NetworkManager OpenVPN connection. Network mutations belong exclusively
// to a privileged installed helper, never the desktop CLI process.
package nativegateway

const (
	Namespace      = "vpnkit-native"
	HostInterface  = "vpnkit-host"
	GuestInterface = "vpnkit-ns"
	TransitCIDR    = "172.30.89.0/30"
	HostAddress    = "172.30.89.1"
	GuestAddress   = "172.30.89.2"
	LoopbackPort   = 21194
	GatewayPort    = 1194
	NetworkUnit    = "local-vpn-kde-network.service"
	GatewayUnit    = "local-vpn-kde-gateway.service"
	ServiceUser    = "local-vpn-kde"
	LibexecDir     = "/usr/libexec/local-vpn-kde"
	UnitDirectory  = "/usr/lib/systemd/system"
	StateDir       = "/var/lib/local-vpn-kde"
	ConfigDir      = "/etc/local-vpn-kde"
)
