//go:build linux

package nativegateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelectSingBox(t *testing.T) {
	tests := []struct {
		name, bundled, system, missing, failed string
		want                                   SingBoxChoice
		wantErr                                bool
	}{
		{name: "bundled newer", bundled: "1.14.5", system: "1.13.9", want: SingBoxChoice{Path: BundledSingBox, Source: "bundled", Version: "1.14.5"}},
		{name: "system newer", bundled: "1.14.3", system: "1.14.4", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4"}},
		{name: "equal prefers bundled", bundled: "1.14.4", system: "1.14.4", want: SingBoxChoice{Path: BundledSingBox, Source: "bundled", Version: "1.14.4"}},
		{name: "bundled cannot run", failed: BundledSingBox, system: "1.14.4", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4", BundledProblem: "cannot-run"}},
		{name: "bundled missing", missing: BundledSingBox, system: "1.14.4", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4", BundledProblem: "missing"}},
		{name: "system unsupported", bundled: "1.14.9", system: "1.15.0", want: SingBoxChoice{Path: BundledSingBox, Source: "bundled", Version: "1.14.9"}},
		{name: "bundled unsupported", bundled: "1.12.9", system: "1.13.0", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.13.0", BundledProblem: "unsupported-version"}},
		{name: "release beats prerelease", bundled: "1.14.4-beta.1", system: "1.14.4", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4"}},
		{name: "numeric prerelease ordering", bundled: "1.14.4-beta.2", system: "1.14.4-beta.10", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4-beta.10"}},
		{name: "newer prerelease beats older release", bundled: "1.14.3", system: "1.14.4-beta.1", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.4-beta.1"}},
		{name: "upper bound prerelease supported", bundled: "1.14.9", system: "1.15.0-beta.1", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.15.0-beta.1"}},
		{name: "lower bound prerelease unsupported", bundled: "1.13.0-beta.1", system: "1.13.0", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.13.0", BundledProblem: "unsupported-version"}},
		{name: "garbage output", bundled: "garbage", system: "1.14.1", want: SingBoxChoice{Path: SystemSingBox, Source: "system", Version: "1.14.1", BundledProblem: "cannot-run"}},
		{name: "both unusable", bundled: "garbage", system: "1.15.0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stat := func(path string) error {
				if path == tt.missing {
					return os.ErrNotExist
				}
				return nil
			}
			probe := func(_ context.Context, path string) (string, error) {
				if path == tt.missing {
					t.Fatal("missing candidate was executed")
				}
				if path == tt.failed {
					return "", errors.New("error loading shared libraries")
				}
				v := tt.system
				if path == BundledSingBox {
					v = tt.bundled
				}
				if v == "garbage" {
					return v, nil
				}
				return "sing-box version " + v + "\nextra details\n", nil
			}
			got, err := selectSingBox(context.Background(), probe, stat)
			if tt.wantErr {
				if !errors.Is(err, ErrNoUsableSingBox) {
					t.Fatalf("got error %v, want ErrNoUsableSingBox", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestEffectiveSingBoxBinCache(t *testing.T) {
	oldNow, oldSelector := singBoxNow, singBoxSelector
	t.Cleanup(func() {
		singBoxNow, singBoxSelector = oldNow, oldSelector
		singBoxCacheValid = false
	})
	singBoxCacheValid = false
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	singBoxNow = func() time.Time { return now }
	calls := 0
	singBoxSelector = func(context.Context) (SingBoxChoice, error) {
		calls++
		if calls == 2 {
			return SingBoxChoice{}, ErrNoUsableSingBox
		}
		return SingBoxChoice{Path: SystemSingBox}, nil
	}
	if got := EffectiveSingBoxBin("/custom/sing-box"); got != "/custom/sing-box" || calls != 0 {
		t.Fatalf("passthrough: got %q, %d calls", got, calls)
	}
	if got := EffectiveSingBoxBin(BundledSingBox); got != SystemSingBox || calls != 1 {
		t.Fatalf("initial: got %q, %d calls", got, calls)
	}
	now = now.Add(29 * time.Second)
	if got := EffectiveSingBoxBin(BundledSingBox); got != SystemSingBox || calls != 1 {
		t.Fatalf("cached: got %q, %d calls", got, calls)
	}
	now = now.Add(time.Second)
	if got := EffectiveSingBoxBin(BundledSingBox); got != BundledSingBox || calls != 2 {
		t.Fatalf("expired: got %q, %d calls", got, calls)
	}
	if got := EffectiveSingBoxBin(BundledSingBox); got != BundledSingBox || calls != 2 {
		t.Fatalf("cached error: got %q, %d calls", got, calls)
	}
}

func TestProbeSingBox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sing-box")
	script := "#!/bin/sh\n[ \"$1\" = version ] || exit 1\n[ \"$PATH\" = /usr/sbin:/usr/bin:/sbin:/bin ] || exit 1\nprintf 'sing-box version 1.14.4\\n'\nprintf 'ignored stderr' >&2\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := probeSingBox(context.Background(), path)
	if err != nil || got != "sing-box version 1.14.4\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	var output cappedOutput
	if n, err := output.Write([]byte(strings.Repeat("x", 5000))); n != 5000 || err != nil || output.Len() != 4096 || !output.overflow {
		t.Fatalf("cap: n=%d err=%v len=%d overflow=%v", n, err, output.Len(), output.overflow)
	}
}
