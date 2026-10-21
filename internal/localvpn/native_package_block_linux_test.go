//go:build linux

package localvpn

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativePackageInhibitorLifecycleAndForeignPreservation(t *testing.T) {
	root := t.TempDir()
	fd, err := directory(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if blocked, err := nativePackageBlockedAt(fd); err != nil || blocked {
		t.Fatal("unexpected initial inhibitor")
	}
	if err = setNativePackageBlockAt(fd, true); err != nil {
		t.Fatal(err)
	}
	if blocked, err := nativePackageBlockedAt(fd); err != nil || !blocked {
		t.Fatal("inhibitor missing")
	}
	if err = setNativePackageBlockAt(fd, false); err != nil {
		t.Fatal(err)
	}
	if err = setNativePackageBlockAt(fd, false); err != nil {
		t.Fatal("clear is not idempotent")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("absent-marker completion must not invent recovery state")
	}
	path := filepath.Join(root, nativePackageBlockName)
	if err = os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []bool{false, true} {
		if err = setNativePackageBlockAt(fd, blocked); err == nil {
			t.Fatal("foreign marker replaced")
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "foreign" {
		t.Fatal("foreign marker damaged")
	}
}
