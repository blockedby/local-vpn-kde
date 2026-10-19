//go:build linux

package localvpn

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

func TestNativeRuntimeConfigIsFixed(t *testing.T) {
	t.Setenv("OVPN_CIDR", "192.0.2.0/24")
	t.Setenv("SINGBOX_CONFIG", "/tmp/host-controlled.json")
	t.Setenv("VPNKIT_IPV6_POLICY", "allow")

	got := NativeRuntimeConfig()
	want := ContainerConfig{
		CIDR:       "10.89.0.0/24",
		Interface:  "sb-tun0",
		Peer:       "172.19.0.2",
		Table:      "101",
		Generation: "/run/local-vpn-kde/sing-box-generation",
		Owner:      "/run/local-vpn-kde/fail-closed-chain.owner",
		Runtime:    "/var/lib/local-vpn-kde/sing-box/config.json",
		Source:     "/etc/local-vpn-kde/sing-box/config.json",
		OpenVPN:    "/etc/local-vpn-kde/openvpn/server.conf",
		Request:    "/run/local-vpn-kde/restart-sing-box",
		Ack:        "/run/local-vpn-kde/sing-box-generation.ack",
		Vibe:       "/etc/local-vpn-kde/vibe-vpn/config.yaml",
		Binary:     "/usr/libexec/local-vpn-kde/sing-box",
		IPv6:       "block",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NativeRuntimeConfig() = %#v, want %#v", got, want)
	}
	if got.Binary != nativegateway.LibexecDir+"/sing-box" {
		t.Fatal("native sing-box binary is not rooted in the fixed libexec directory")
	}

	options := nativeGatewayRuntimeOptions(got)
	if !options.ownedChildrenHealth {
		t.Fatal("native readiness must use owned children, not global pgrep")
	}
	if options.helper != nativegateway.LibexecDir+"/vibe-vpn" {
		t.Fatalf("native helper = %q", options.helper)
	}
	if options.helperConfig != got.Vibe {
		t.Fatal("native config synchronization must use its explicit installed helper config")
	}
	if options.openVPN != "/usr/sbin/openvpn" {
		t.Fatalf("native OpenVPN binary = %q", options.openVPN)
	}
	wantEnvironment := []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"SINGBOX_RESTART_FILE=/run/local-vpn-kde/restart-sing-box",
		"SINGBOX_GENERATION_FILE=/run/local-vpn-kde/sing-box-generation",
		"SINGBOX_ACK_FILE=/run/local-vpn-kde/sing-box-generation.ack",
	}
	if environment := options.environment(); !reflect.DeepEqual(environment, wantEnvironment) {
		t.Fatalf("native environment = %#v, want %#v", environment, wantEnvironment)
	}
	if options.bootstrapSetting() != "" || options.watchDNS != nil || options.setRestartEnvironment {
		t.Fatal("native runtime inherited an environment-controlled supervisor feature")
	}
}

func TestServeNativeGatewayRejectsMissingNamespaceBeforeRuntime(t *testing.T) {
	missing := errors.New("native network namespace unavailable")
	called := false
	err := serveNativeGateway(context.Background(), io.Discard, func() error { return missing }, func(context.Context, io.Writer, ContainerConfig) error {
		called = true
		return nil
	})
	if !errors.Is(err, missing) {
		t.Fatalf("error = %v, want %v", err, missing)
	}
	if called {
		t.Fatal("gateway runtime called after namespace verification failed")
	}
}

func TestServeNativeGatewayVerifiesBeforeConstructingRuntime(t *testing.T) {
	verified := false
	err := serveNativeGateway(context.Background(), io.Discard, func() error {
		verified = true
		return nil
	}, func(_ context.Context, _ io.Writer, config ContainerConfig) error {
		if !verified {
			t.Fatal("gateway runtime called before namespace verification")
		}
		if config != NativeRuntimeConfig() {
			t.Fatalf("gateway runtime config = %#v", config)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
