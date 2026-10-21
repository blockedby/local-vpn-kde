//go:build linux

package localvpn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeSetupInhibitorPersistsAndRejectsForeignState(t *testing.T) {
	root := t.TempDir()
	uid := uint32(os.Getuid())
	if err := os.MkdirAll(filepath.Join(root, "var/lib"), 0755); err != nil {
		t.Fatal(err)
	}
	if exists, err := nativeSetupIncompleteAt(root, uid); exists || err != nil {
		t.Fatal("unexpected initial inhibitor", err)
	}
	if err := setNativeSetupIncompleteAt(root, uid, true); err != nil {
		t.Fatal(err)
	}
	// Reopen independently, as a later service process or a reboot would do.
	if exists, err := nativeSetupIncompleteAt(root, uid); !exists || err != nil {
		t.Fatal("missing persistent inhibitor", err)
	}
	if err := setNativeSetupIncompleteAt(root, uid, false); err != nil {
		t.Fatal(err)
	}
	if err := setNativeSetupIncompleteAt(root, uid, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "var/lib/local-vpn-kde-install", nativeSetupInhibitorName)
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []bool{true, false} {
		if err := setNativeSetupIncompleteAt(root, uid, value); err == nil {
			t.Fatal("foreign inhibitor changed")
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "foreign" {
		t.Fatal("foreign data lost")
	}
}
func TestNativeSetupInhibitorRejectsUnsafeAncestor(t *testing.T) {
	for _, kind := range []string{"writable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			lib := filepath.Join(root, "var/lib")
			if err := os.MkdirAll(lib, 0755); err != nil {
				t.Fatal(err)
			}
			if kind == "writable" {
				if err := os.Chmod(lib, 0777); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(t.TempDir(), filepath.Join(lib, "local-vpn-kde-install")); err != nil {
					t.Fatal(err)
				}
			}
			if err := setNativeSetupIncompleteAt(root, uint32(os.Getuid()), true); err == nil {
				t.Fatal("unsafe ancestor accepted")
			}
		})
	}
}
