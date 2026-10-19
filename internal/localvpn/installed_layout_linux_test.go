package localvpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveApplicationLayoutInstalledReadOnlyXDG(t *testing.T) {
	xdg := "/home/local-vpn-kde-layout-test-user/.state"
	t.Setenv("XDG_STATE_HOME", xdg)

	layout, err := ResolveApplicationLayout(InstalledCLI, "", "secrets/vpnkit-local", "")
	if err != nil {
		t.Fatal(err)
	}
	want := ApplicationLayout{
		Installed:  true,
		Assets:     InstalledAssetRoot,
		Executable: InstalledCLI,
		State:      filepath.Join(xdg, "local-vpn-kde"),
		Runtime:    "native",
	}
	if layout != want {
		t.Fatalf("layout = %#v, want %#v", layout, want)
	}
	if _, err := os.Lstat(xdg); !os.IsNotExist(err) {
		t.Fatalf("resolver created or encountered test state: %v", err)
	}

	layout, err = ResolveApplicationLayout(InstalledCLI, ".", want.State, "native")
	if err != nil {
		t.Fatal(err)
	}
	if layout != want {
		t.Fatalf("explicit installed layout = %#v, want %#v", layout, want)
	}
}

func TestResolveApplicationLayoutInstalledRejectsOverrides(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/home/local-vpn-kde-layout-rejection/.state")
	state := "/home/local-vpn-kde-layout-rejection/.state/local-vpn-kde"
	tests := []struct {
		name, repo, base, runtime string
	}{
		{name: "foreign repo", repo: "/opt/local-vpn-kde"},
		{name: "foreign state", base: "/var/lib/local-vpn-kde"},
		{name: "nearby state", base: state + "-other"},
		{name: "docker", runtime: "docker"},
		{name: "unknown runtime", runtime: "podman"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveApplicationLayout(InstalledCLI, test.repo, test.base, test.runtime); err == nil {
				t.Fatal("unsafe installed override was accepted")
			}
		})
	}
}

func TestResolveApplicationLayoutInstalledRejectsUnsafeXDG(t *testing.T) {
	for _, value := range []string{"relative/state", "/home/user/../other", "/home/user\n/state"} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", value)
			if _, err := ResolveApplicationLayout(InstalledCLI, "", "", ""); err == nil {
				t.Fatalf("unsafe XDG_STATE_HOME %q was accepted", value)
			}
		})
	}
	if err := validateAbsoluteLayoutPath("/home/user\x00/state"); err == nil {
		t.Fatal("NUL-containing path was accepted")
	}
}

func TestResolveApplicationLayoutCheckoutPreservesSecretRoot(t *testing.T) {
	repo := t.TempDir()
	executable := filepath.Join(repo, ".build", "local-vpn-kde.bin")
	layout, err := ResolveApplicationLayout(executable, repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if layout.Installed || layout.Assets != repo || layout.Executable != executable || layout.State != filepath.Join(repo, "secrets/vpnkit-local") || layout.Runtime != "docker" {
		t.Fatalf("unexpected checkout layout: %#v", layout)
	}
	if _, err = ResolveApplicationLayout(executable, repo, "/etc/local-vpn-kde", "native"); err == nil {
		t.Fatal("checkout escaped SecretRoot restrictions")
	}
}

func TestValidateInstalledStatePathPermissionsAndLinks(t *testing.T) {
	uid := uint32(os.Getuid())
	t.Run("valid existing state", func(t *testing.T) {
		anchor := t.TempDir()
		boundary := filepath.Join(anchor, "xdg")
		state := filepath.Join(boundary, "local-vpn-kde")
		if err := os.MkdirAll(state, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(boundary, 0755); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledStatePath(state, boundary, uid, anchor); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("state must be private", func(t *testing.T) {
		anchor := t.TempDir()
		boundary := filepath.Join(anchor, "xdg")
		state := filepath.Join(boundary, "local-vpn-kde")
		if err := os.MkdirAll(state, 0755); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledStatePath(state, boundary, uid, anchor); err == nil {
			t.Fatal("non-private state directory was accepted")
		}
	})
	t.Run("writable parent", func(t *testing.T) {
		anchor := t.TempDir()
		boundary := filepath.Join(anchor, "xdg")
		if err := os.Mkdir(boundary, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(boundary, 0777); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledStatePath(filepath.Join(boundary, "local-vpn-kde"), boundary, uid, anchor); err == nil {
			t.Fatal("writable state parent was accepted")
		}
	})
	t.Run("symlink component", func(t *testing.T) {
		anchor := t.TempDir()
		target := filepath.Join(anchor, "target")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		boundary := filepath.Join(anchor, "xdg")
		if err := os.Symlink(target, boundary); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledStatePath(filepath.Join(boundary, "local-vpn-kde"), boundary, uid, anchor); err == nil {
			t.Fatal("symlinked state parent was accepted")
		}
	})
}

func TestValidateInstalledAssetsAt(t *testing.T) {
	anchor := t.TempDir()
	assetRoot := filepath.Join(anchor, "share", "local-vpn-kde")
	cli := filepath.Join(anchor, "libexec", "local-vpn-kde", "local-vpn-kde.bin")
	writeInstalledAssetFixture(t, assetRoot, cli)
	if err := validateInstalledAssetsAt(assetRoot, cli, uint32(os.Getuid()), anchor); err != nil {
		t.Fatal(err)
	}

	t.Run("symlink required file", func(t *testing.T) {
		fixture := t.TempDir()
		assets := filepath.Join(fixture, "share", "local-vpn-kde")
		binary := filepath.Join(fixture, "libexec", "local-vpn-kde", "local-vpn-kde.bin")
		writeInstalledAssetFixture(t, assets, binary)
		entry := filepath.Join(assets, "scripts/vpnkit/tui/index.ts")
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(assets, "scripts/vpnkit/tui/package.json"), entry); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledAssetsAt(assets, binary, uint32(os.Getuid()), fixture); err == nil {
			t.Fatal("symlinked installed asset was accepted")
		}
	})

	t.Run("writable required file", func(t *testing.T) {
		fixture := t.TempDir()
		assets := filepath.Join(fixture, "share", "local-vpn-kde")
		binary := filepath.Join(fixture, "libexec", "local-vpn-kde", "local-vpn-kde.bin")
		writeInstalledAssetFixture(t, assets, binary)
		if err := os.Chmod(filepath.Join(assets, "scripts/vpnkit/tui/package.json"), 0666); err != nil {
			t.Fatal(err)
		}
		if err := validateInstalledAssetsAt(assets, binary, uint32(os.Getuid()), fixture); err == nil {
			t.Fatal("writable installed asset was accepted")
		}
	})
}

func writeInstalledAssetFixture(t *testing.T, assetRoot, cli string) {
	t.Helper()
	for _, path := range append([]string{cli}, installedAssetFiles...) {
		if path != cli {
			path = filepath.Join(assetRoot, path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if path == cli || strings.HasSuffix(path, "vpnkit-native.sh") {
			mode = 0755
		}
		if err := os.WriteFile(path, []byte("fixture\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
}
