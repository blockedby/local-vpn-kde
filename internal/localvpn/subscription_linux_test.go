package localvpn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSubscriptionRollbackAndPrivacy(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			base := t.TempDir()
			if err := os.Chmod(base, 0700); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(base, "vibe-vpn")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "sub_url")
			if existing {
				if err := os.WriteFile(path, []byte("old-value\n"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			err := writeSubscription(base, "new-value", func() error { return errors.New("injected fsync failure") })
			if err == nil || err.Error() != "subscription update failed; prior state restored" {
				t.Fatalf("unexpected rollback result: %v", err)
			}
			if existing {
				data, e := os.ReadFile(path)
				if e != nil || string(data) != "old-value\n" {
					t.Fatal("original bytes not restored")
				}
				st, e := os.Stat(path)
				if e != nil || st.Mode().Perm() != 0400 {
					t.Fatal("original mode not restored")
				}
			} else {
				if _, e := os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("original absence not restored")
				}
			}
			stages, _ := filepath.Glob(filepath.Join(parent, "*.tmp"))
			if len(stages) > 0 {
				t.Fatal("staging files remain")
			}
			if err = WriteSubscription(base, "new-value"); err != nil {
				t.Fatal(err)
			}
			value, err := ReadSubscription(base)
			if err != nil || value != "new-value" || !SubscriptionConfigured(base) {
				t.Fatal("successful update not readable")
			}
		})
	}
}
func TestSubscriptionRejectsLinks(t *testing.T) {
	for _, link := range []func(string, string) error{os.Symlink, os.Link} {
		base := t.TempDir()
		if err := os.Chmod(base, 0700); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(base, "vibe-vpn")
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := link(outside, filepath.Join(parent, "sub_url")); err != nil {
			t.Fatal(err)
		}
		if err := WriteSubscription(base, "new"); err == nil {
			t.Fatal("linked subscription accepted")
		}
		data, err := os.ReadFile(outside)
		if err != nil || string(data) != "keep" {
			t.Fatal("outside file modified")
		}
	}
}
