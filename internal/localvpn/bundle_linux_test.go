package localvpn

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeCapabilityBundleRollback(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "networkmanager-uuid"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := directory(base, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dir)
	err = writeBundleChecked(dir, map[string][]byte{"networkmanager-uuid": []byte("new\n"), "networkmanager-state": []byte("new fingerprint\n")}, func() error { return errors.New("injected post-publication sync failure") })
	if err == nil {
		t.Fatal("failed bundle reported success")
	}
	got, err := os.ReadFile(filepath.Join(base, "networkmanager-uuid"))
	if err != nil || string(got) != "old\n" {
		t.Fatal("original capability was not restored")
	}
	if _, err = os.Stat(filepath.Join(base, "networkmanager-state")); !os.IsNotExist(err) {
		t.Fatal("new capability survived rollback")
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatal("rollback left staging files")
	}
}
