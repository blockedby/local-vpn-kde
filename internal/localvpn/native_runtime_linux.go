//go:build linux

package localvpn

import (
	"context"
	"io"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

const nativeRuntimePath = "/run/local-vpn-kde"

// NativeRuntimeConfig returns the fixed gateway layout installed for the
// dedicated native service account. It intentionally does not consult the
// process environment.
func NativeRuntimeConfig() ContainerConfig {
	return ContainerConfig{
		CIDR:       "10.90.0.0/24",
		Interface:  "sb-tun0",
		Peer:       "172.19.0.2",
		Table:      "101",
		Generation: nativeRuntimePath + "/sing-box-generation",
		Owner:      nativeRuntimePath + "/fail-closed-chain.owner",
		Runtime:    nativegateway.StateDir + "/sing-box/config.json",
		Source:     nativegateway.ConfigDir + "/sing-box/config.json",
		OpenVPN:    nativegateway.ConfigDir + "/openvpn/server.conf",
		Request:    nativeRuntimePath + "/restart-sing-box",
		Ack:        nativeRuntimePath + "/sing-box-generation.ack",
		Vibe:       nativegateway.ConfigDir + "/vibe-vpn/config.yaml",
		Binary:     nativePinnedRuntime + "/sing-box",
		IPv6:       "block",
	}
}

type nativeGatewayServeFunc func(context.Context, io.Writer, ContainerConfig) error

func serveNativeGateway(parent context.Context, output io.Writer, verify func() error, serve nativeGatewayServeFunc) error {
	if err := verify(); err != nil {
		return err
	}
	return serve(parent, output, NativeRuntimeConfig())
}

func nativeGatewayRuntimeOptions(c ContainerConfig) gatewayRuntimeOptions {
	fixedEnvironment := func() []string {
		return []string{
			"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
			"SINGBOX_RESTART_FILE=" + c.Request,
			"SINGBOX_GENERATION_FILE=" + c.Generation,
			"SINGBOX_ACK_FILE=" + c.Ack,
		}
	}
	return gatewayRuntimeOptions{
		helper:              nativePinnedRuntime + "/vibe-vpn",
		helperConfig:        c.Vibe,
		ownedChildrenHealth: true,
		onReady:             nativeReady,
		openVPN:             "/usr/sbin/openvpn",
		environment:         fixedEnvironment,
		bootstrapSetting:    func() string { return "" },
		bootstrapMaxNodes:   func() string { return "" },
		// Native bootstrap remains disabled and therefore needs no host log
		// directory. DNS failover remains renderer/service integration work;
		// enabling the container watcher would inherit host environment knobs.
	}
}

// ServeNativeGateway runs the shared gateway supervisor only after proving the
// process is already in the named native network namespace. It never enters a
// namespace and never accepts path or routing configuration from the host
// environment.
func ServeNativeGateway(parent context.Context, output io.Writer) error {
	return serveNativeGateway(parent, output, nativegateway.VerifyNamespace, func(ctx context.Context, output io.Writer, c ContainerConfig) error {
		return c.serveGateway(ctx, output, nativeGatewayRuntimeOptions(c))
	})
}
