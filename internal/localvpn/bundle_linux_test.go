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
	err = writeBundleChecked(dir, map[string][]byte{"networkmanager-uuid": []byte("new\n"), "networkmanager-state": []byte("new fingerprint\n")}, bundleHooks{afterPublish: func() error { return errors.New("injected post-publication sync failure") }})
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

func TestBundlePreservesConcurrentChangesAndRollsBackOwnWrites(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "replaced"}[existing], func(t *testing.T) {
			base := t.TempDir()
			if existing {
				if err := os.WriteFile(filepath.Join(base, "z-state"), []byte("prior"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := directory(base, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(dir)
			err = writeBundleChecked(dir, map[string][]byte{"a-state": []byte("candidate"), "z-state": []byte("candidate")}, bundleHooks{beforePublish: func() error {
				return atomicWrite(dir, "z-state", []byte("concurrent edit"))
			}})
			if err == nil {
				t.Fatal("concurrent edit was overwritten")
			}
			got, err := os.ReadFile(filepath.Join(base, "z-state"))
			if err != nil || string(got) != "concurrent edit" {
				t.Fatal("rollback overwrote another writer")
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 1 {
				t.Fatal("partial publication or staging files survived")
			}
		})
	}
}
