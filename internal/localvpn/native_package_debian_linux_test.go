//go:build linux

package localvpn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDebianRecoveryLockSafety(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "symlink", "writable", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			db := filepath.Join(root, "var/lib/dpkg")
			if err := os.MkdirAll(db, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"lock-frontend", "lock"} {
				if err := os.WriteFile(filepath.Join(db, name), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			lock := filepath.Join(db, "lock")
			switch kind {
			case "missing":
				os.Remove(lock)
			case "symlink":
				os.Remove(lock)
				os.Symlink("lock-frontend", lock)
			case "writable":
				os.Chmod(lock, 0666)
			case "hardlink":
				os.Link(lock, filepath.Join(db, "extra"))
			case "directory":
				os.Remove(lock)
				os.Mkdir(lock, 0700)
			}
			called := false
			err := recoverDebianPackageChangeAt(context.Background(), root, uint32(os.Getuid()), func(context.Context) error { called = true; return nil })
			if kind == "valid" && (err != nil || !called) {
				t.Fatalf("valid locks rejected: %v", err)
			}
			if kind != "valid" && (err == nil || called) {
				t.Fatalf("unsafe lock accepted: %s", kind)
			}
			data, readErr := os.ReadFile(filepath.Join(db, "lock-frontend"))
			if readErr != nil || string(data) != "preserve" {
				t.Fatal("lock modified")
			}
		})
	}
}
