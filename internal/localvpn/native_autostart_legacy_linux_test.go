//go:build linux

package localvpn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeAutostartDoesNotAdoptEnabledLegacyIntent(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "autostart.json"), []byte(`{"gateway":true,"connect":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := Autostart{Base: base, Runtime: "native"}
	if _, err := a.Status(context.Background()); err == nil {
		t.Fatal("legacy Docker intent was adopted by native runtime")
	}
}
