//go:build linux

package localvpn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledBridgeFirstStatusDoesNotCreateState(t *testing.T) {
	base := filepath.Join(t.TempDir(), "missing", "local-vpn-kde")
	opts := BridgeOptions{Repo: InstalledAssetRoot, Base: base, Runtime: "native", Executable: filepath.Join(InstalledAssetRoot, "scripts/vpnkit/vpnkit-native.sh")}
	bridge, err := NewBridge(opts)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Status()["subscription"] != "not configured" {
		t.Fatal("fresh subscription incorrectly reported")
	}
	if _, err = os.Stat(filepath.Dir(base)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status construction mutated user directories")
	}
	opts.Runtime = "docker"
	if _, err = NewBridge(opts); err == nil {
		t.Fatal("checkout directory prerequisite weakened")
	}
}
func TestInstalledBridgeRejectsUnsafeExistingState(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBridge(BridgeOptions{Repo: InstalledAssetRoot, Base: base, Runtime: "native", Executable: filepath.Join(InstalledAssetRoot, "scripts/vpnkit/vpnkit-native.sh")}); err == nil {
		t.Fatal("unsafe existing state accepted")
	}
}
