//go:build linux

package localvpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAssetsUseFixedServicePKIAndLocalEndpoint(t *testing.T) {
	template, err := filepath.Abs("../../config/openvpn")
	if err != nil {
		t.Fatal(err)
	}
	o := AssetOptions{Base: t.TempDir(), TemplateDir: template, Runtime: "native", Endpoint: "127.0.0.1", Port: 31194, DNS: "8.8.8.8", CertificateDays: 30}
	if err := Assets(o); err != nil {
		t.Fatal(err)
	}
	server, err := os.ReadFile(filepath.Join(o.Base, "rendered/openvpn/server.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(server), "/etc/openvpn/") || !strings.Contains(string(server), "key /etc/local-vpn-kde/openvpn/pki/server.key") {
		t.Fatal("native server does not use installed PKI paths")
	}
	profile, err := os.ReadFile(filepath.Join(o.Base, "openvpn/client/vpnkit-local.ovpn"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profile), "remote 127.0.0.1 31194") {
		t.Fatal("native profile does not use fixed loopback endpoint")
	}
	o.Base = t.TempDir()
	o.Port = 1194
	if err := Assets(o); err == nil {
		t.Fatal("wrong native endpoint port accepted")
	}
	entries, err := os.ReadDir(o.Base)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid native endpoint modified assets")
	}
}
