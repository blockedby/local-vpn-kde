//go:build linux

package localvpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderNativeLayout(t *testing.T) {
	o := renderFixture(t)
	o.Runtime = "native"
	if err := Render(o); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"rendered/sing-box/config.json", "rendered/vibe-vpn/config.yaml"} {
		data, err := os.ReadFile(filepath.Join(o.Base, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range []string{"/etc/sing-box/", "/etc/vibe-vpn-input/", "/var/lib/vpnkit/", "/run/vpnkit/", "/usr/local/bin/sing-box"} {
			if strings.Contains(string(data), old) {
				t.Fatalf("%s retains container path %s", path, old)
			}
		}
		if !strings.Contains(string(data), "/etc/local-vpn-kde/") {
			t.Fatalf("%s lacks native paths", path)
		}
	}
}

func TestRenderRejectsUnknownRuntimeBeforeWriting(t *testing.T) {
	o := renderFixture(t)
	o.Runtime = "arbitrary"
	if err := Render(o); err == nil {
		t.Fatal("unknown runtime accepted")
	}
	entries, err := os.ReadDir(o.Base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("invalid runtime modified output tree")
	}
}
