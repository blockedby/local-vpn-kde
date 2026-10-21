//go:build linux

package localvpn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInstalledAutostartUsesFixedExecutableAndGraphicalSession(t *testing.T) {
	// Installed paths intentionally reject world-writable ancestors such as /tmp.
	parent, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", parent)
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateHome := filepath.Join(home, "state $literal%value")
	t.Setenv("XDG_STATE_HOME", stateHome)
	base := filepath.Join(stateHome, "local-vpn-kde")
	a := Autostart{Repo: InstalledAssetRoot, Base: base, Installed: true}
	unit, err := a.unit()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{InstalledCLI, "--runtime \"native\"", "--secrets-dir ", "WantedBy=graphical-session.target", "PartOf=graphical-session.target", "ConditionPathIsExecutable=" + InstalledCLI} {
		if !strings.Contains(unit, want) {
			t.Fatalf("missing unit contract %q", want)
		}
	}
	wantEnvironment := "Environment=" + strconv.Quote(strings.ReplaceAll("XDG_STATE_HOME="+stateHome, "%", "%%")) + "\n"
	if !strings.Contains(unit, wantEnvironment) {
		t.Fatal("custom XDG state path is not carried to the user manager")
	}
	for _, bad := range []string{".build/", "WantedBy=default.target", "--runtime \"docker\""} {
		if strings.Contains(unit, bad) {
			t.Fatalf("unexpected unit contract %q", bad)
		}
	}
	a.Base = filepath.Join(home, "other")
	if _, err := a.unit(); err == nil {
		t.Fatal("arbitrary installed state accepted")
	}
}
