package profiles

import (
	"context"
	"encoding/json"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateStoreAndRedactedView(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "sub")
	os.WriteFile(legacy, []byte("https://example.invalid/private-token\nhttps://other.invalid/sub\n"), 0600)
	lock, e := state.AcquireLock(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	d, e := LoadLocked(dir, legacy)
	if e != nil || len(d.Items) != 2 {
		t.Fatal(e, d)
	}
	if e = d.Apply(Request{Action: "add", Name: "My VPS", Source: "hy2://private-pass@secret.invalid#VPS"}); e != nil {
		t.Fatal(e)
	}
	active := d.Active
	if active == d.Items[0].ID {
		t.Fatal("new profile not active")
	}
	if e = SaveLocked(context.Background(), dir, d); e != nil {
		t.Fatal(e)
	}
	d, e = LoadLocked(dir, legacy)
	if e != nil || len(d.Items) != 3 {
		t.Fatal(e)
	}
	b, _ := json.Marshal(d.Safe())
	for _, secret := range []string{"private-pass", "secret.invalid", "private-token", "https://", "hy2://"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("view leaked source")
		}
	}
	st, _ := os.Stat(filepath.Join(dir, File))
	if st.Mode().Perm() != 0600 {
		t.Fatal("not private")
	}
	if e = d.Apply(Request{Action: "remove", ID: active}); e != nil || len(d.Items) != 2 || d.Active == active {
		t.Fatal(e)
	}
}
func TestStoreRejectsSymlinkAndBadInput(t *testing.T) {
	for _, r := range []Request{{Action: "add", Name: "x", Source: "file:///etc/passwd"}, {Action: "add", Name: "x\n", Source: "https://example.invalid"}, {Action: "remove", ID: "../escape"}} {
		if r.Validate() == nil {
			t.Fatal("accepted unsafe input")
		}
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte(`{"version":1}`), 0600)
	os.Symlink(outside, filepath.Join(dir, File))
	lock, e := state.AcquireLock(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if _, e := LoadLocked(dir, ""); e == nil {
		t.Fatal("followed symlink")
	}
}
