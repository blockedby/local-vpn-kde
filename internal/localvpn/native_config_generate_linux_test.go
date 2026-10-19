//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeGeneratedProductionEntryPointRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production entrypoint root guard is exercised by the non-root test runner")
	}
	if _, err := GenerateNativeConfiguration(context.Background(), "https://example.invalid/subscription", 4242); err == nil {
		t.Fatal("non-root production generation succeeded")
	}
}

func TestNativeGeneratedConfigurationUsesFixedNativePathsAndReturnsProfile(t *testing.T) {
	assetsRoot, tempParent := nativeGeneratedAssetFixture(t)
	const subscriptionURL = "https://example.invalid/subscription"

	generated, err := generateNativeConfigurationAt(context.Background(), subscriptionURL, 4242, assetsRoot, tempParent)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Bundle.OwnerUID != 4242 {
		t.Fatalf("bundle owner UID = %d, want 4242", generated.Bundle.OwnerUID)
	}
	for _, name := range nativeConfigurationRequiredFiles {
		if _, ok := generated.Bundle.Files[name]; !ok {
			t.Fatalf("required bundle file %q is missing", name)
		}
	}
	for _, name := range []string{
		"sing-box/rule-sets/vpnkit-adblock.json",
		"sing-box/rule-sets/vpnkit-dev-direct.json",
	} {
		if _, ok := generated.Bundle.Files[name]; !ok {
			t.Fatalf("rendered optional rule set %q is missing", name)
		}
	}
	for _, name := range []string{
		"openvpn/pki/ca.key",
		"openvpn/pki/client.crt",
		"openvpn/pki/client.key",
		"openvpn/client/vpnkit-local.ovpn",
	} {
		if _, ok := generated.Bundle.Files[name]; ok {
			t.Fatalf("private or separately delivered file %q was bundled", name)
		}
	}
	if !bytes.Equal(generated.Bundle.Files["vibe-vpn/sub_url"], []byte(subscriptionURL+"\n")) {
		t.Fatal("subscription file was not staged as the supplied URL")
	}

	config := generated.Bundle.Files["sing-box/config.json"]
	var decoded map[string]any
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatalf("native sing-box config is invalid: %v", err)
	}
	for _, oldPath := range []string{
		"/etc/sing-box/", "/etc/vibe-vpn-input/", "/var/lib/vpnkit/", "/run/vpnkit/", "/usr/local/bin/sing-box",
	} {
		for name, data := range generated.Bundle.Files {
			if strings.Contains(string(data), oldPath) {
				t.Fatalf("generated %q retains Docker path %q", name, oldPath)
			}
		}
	}
	if !strings.Contains(string(generated.Bundle.Files["vibe-vpn/config.yaml"]), "/etc/local-vpn-kde/") {
		t.Fatal("generated native configuration does not use installed paths")
	}
	if !strings.Contains(string(generated.Bundle.Files["openvpn/server.conf"]), "key /etc/local-vpn-kde/openvpn/pki/server.key") {
		t.Fatal("generated OpenVPN server does not use the native PKI path")
	}

	ca := nativeGeneratedCertificate(t, generated.Bundle.Files["openvpn/pki/ca.crt"])
	if !ca.IsCA {
		t.Fatal("generated CA certificate is not a CA")
	}
	server := nativeGeneratedCertificate(t, generated.Bundle.Files["openvpn/pki/server.crt"])
	if _, err := server.Verify(x509.VerifyOptions{Roots: x509.NewCertPool()}); err == nil {
		t.Fatal("server certificate unexpectedly verified without the generated CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("server certificate does not verify against generated CA: %v", err)
	}
	if _, err := x509.ParsePKCS1PrivateKey(nativeGeneratedPEM(t, generated.Bundle.Files["openvpn/pki/server.key"], "RSA PRIVATE KEY")); err != nil {
		t.Fatalf("generated server key is invalid: %v", err)
	}
	if remaining := int(server.NotAfter.Sub(server.NotBefore).Hours() / 24); remaining < 824 || remaining > 826 {
		t.Fatalf("generated certificate lifetime = %d days, want 825", remaining)
	}

	profile := string(generated.ClientProfile)
	if !strings.Contains(profile, "remote 127.0.0.1 31194") || !strings.Contains(profile, "<ca>\n") || !strings.Contains(profile, "<cert>\n") || !strings.Contains(profile, "<key>\n") || !strings.Contains(profile, "<tls-auth>\n") {
		t.Fatal("generated client profile is incomplete")
	}
	if !strings.Contains(profile, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatal("generated client profile does not contain its client key")
	}
	entries, err := os.ReadDir(tempParent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging directory was not cleaned up: %d entries", len(entries))
	}
}

func TestNativeGeneratedConfigurationRejectsUnsafeInputsWithoutWrites(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "missing", url: ""},
		{name: "wrong scheme", url: "http://example.invalid/subscription"},
		{name: "userinfo", url: "https://user:secret-marker@example.invalid/subscription"},
		{name: "fragment", url: "https://example.invalid/subscription#secret-marker"},
		{name: "newline", url: "https://example.invalid/secret-marker\n"},
		{name: "control", url: "https://example.invalid/secret-marker\x00"},
		{name: "too long", url: "https://example.invalid/" + strings.Repeat("a", nativeGenerationMaxSubscriptionURL)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			assetsRoot, tempParent := nativeGeneratedAssetFixture(t)
			_, err := generateNativeConfigurationAt(context.Background(), test.url, 4242, assetsRoot, tempParent)
			if err == nil {
				t.Fatal("unsafe subscription URL was accepted")
			}
			if strings.Contains(err.Error(), "secret-marker") {
				t.Fatal("subscription URL was exposed in the error")
			}
			nativeGeneratedAssertEmptyDirectory(t, tempParent)
		})
	}

	t.Run("missing installed template", func(t *testing.T) {
		assetsRoot, tempParent := nativeGeneratedAssetFixture(t)
		missing := filepath.Join(assetsRoot, "config/openvpn/local-server.tpl")
		if err := os.Remove(missing); err != nil {
			t.Fatal(err)
		}
		if _, err := generateNativeConfigurationAt(context.Background(), "https://example.invalid/subscription", 4242, assetsRoot, tempParent); err == nil {
			t.Fatal("generation succeeded with a missing installed template")
		}
		nativeGeneratedAssertEmptyDirectory(t, tempParent)
	})

	t.Run("unsafe installed template", func(t *testing.T) {
		assetsRoot, tempParent := nativeGeneratedAssetFixture(t)
		unsafe := filepath.Join(assetsRoot, "config/openvpn/local-server.tpl")
		if err := os.Chmod(unsafe, 0666); err != nil {
			t.Fatal(err)
		}
		if _, err := generateNativeConfigurationAt(context.Background(), "https://example.invalid/subscription", 4242, assetsRoot, tempParent); err == nil {
			t.Fatal("generation succeeded with an unsafe installed template")
		}
		nativeGeneratedAssertEmptyDirectory(t, tempParent)
	})
}

func TestNativeGeneratedConfigurationCleansUpOnCancellation(t *testing.T) {
	assetsRoot, tempParent := nativeGeneratedAssetFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := generateNativeConfigurationAt(ctx, "https://example.invalid/subscription", 4242, assetsRoot, tempParent); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled generation error = %v, want context.Canceled", err)
	}
	nativeGeneratedAssertEmptyDirectory(t, tempParent)

	assetsRoot, tempParent = nativeGeneratedAssetFixture(t)
	cancelAfterStage := &nativeGeneratedCancelContext{Context: context.Background(), cancelAt: 6}
	if _, err := generateNativeConfigurationAt(cancelAfterStage, "https://example.invalid/subscription", 4242, assetsRoot, tempParent); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-stage cancellation error = %v, want context.Canceled", err)
	}
	nativeGeneratedAssertEmptyDirectory(t, tempParent)
}

func nativeGeneratedAssetFixture(t *testing.T) (string, string) {
	t.Helper()
	assetRoot := filepath.Join(t.TempDir(), "assets")
	if err := os.Mkdir(assetRoot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range installedAssetFiles {
		source := []byte("compiled TUI fixture\n")
		if relative != "scripts/vpnkit/tui/vpnkit-tui" {
			var err error
			source, err = os.ReadFile(filepath.Join("..", "..", relative))
			if err != nil {
				t.Fatalf("read public fixture %s: %v", relative, err)
			}
		}
		destination := filepath.Join(assetRoot, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if relative == "scripts/vpnkit/vpnkit-native.sh" || relative == "scripts/vpnkit/tui/vpnkit-tui" {
			mode = 0700
		}
		if err := os.WriteFile(destination, source, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range []string{
		"config/sing-box/rule-sets/vpnkit-adblock.json",
		"config/sing-box/rule-sets/vpnkit-dev-direct.json",
	} {
		source, err := os.ReadFile(filepath.Join("..", "..", relative))
		if err != nil {
			t.Fatalf("read public rule fixture %s: %v", relative, err)
		}
		destination := filepath.Join(assetRoot, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, source, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return assetRoot, t.TempDir()
}

func nativeGeneratedCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, rest := pem.Decode(data)
	if block == nil || len(rest) != 0 || block.Type != "CERTIFICATE" {
		t.Fatal("generated certificate PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("generated certificate is invalid: %v", err)
	}
	return certificate
}

func nativeGeneratedPEM(t *testing.T, data []byte, kind string) []byte {
	t.Helper()
	block, rest := pem.Decode(data)
	if block == nil || len(rest) != 0 || block.Type != kind {
		t.Fatalf("generated %s PEM is invalid", kind)
	}
	return block.Bytes
}

func nativeGeneratedAssertEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging parent retained %d entries", len(entries))
	}
}

type nativeGeneratedCancelContext struct {
	context.Context
	calls    int
	cancelAt int
}

func (c *nativeGeneratedCancelContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
