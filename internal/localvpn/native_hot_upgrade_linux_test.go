//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"golang.org/x/sys/unix"
)

func TestNativeSingBoxSelectionAndStaging(t *testing.T) {
	original := selectNativeSingBox
	t.Cleanup(func() { selectNativeSingBox = original })
	for _, tt := range []struct {
		name   string
		choice nativegateway.SingBoxChoice
		output string
	}{
		{"bundled", nativegateway.SingBoxChoice{Path: nativegateway.BundledSingBox, Source: "bundled", Version: "1.14.4"}, "native_sing_box_selected=bundled version=1.14.4\n"},
		{"system", nativegateway.SingBoxChoice{Path: nativegateway.SystemSingBox, Source: "system", Version: "1.14.5", BundledProblem: "cannot-run"}, "native_sing_box_selected=system version=1.14.5 bundled=cannot-run\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			selectNativeSingBox = func(context.Context) (nativegateway.SingBoxChoice, error) { return tt.choice, nil }
			var output bytes.Buffer
			choice, err := nativeSingBoxSource(context.Background(), &output)
			if err != nil || output.Len() != 0 {
				t.Fatalf("selection: error=%v output=%q", err, output.String())
			}
			called := false
			err = stageNativeSingBox(context.Background(), 42, choice, &output, func(_ context.Context, fd int, source, name string, uid uint32) error {
				called = true
				if fd != 42 || source != tt.choice.Path || name != "sing-box" || uid != 0 {
					t.Errorf("copy arguments: fd=%d source=%q name=%q uid=%d", fd, source, name, uid)
				}
				return nil
			})
			if err != nil || !called || output.String() != tt.output {
				t.Fatalf("staging: error=%v called=%v output=%q", err, called, output.String())
			}
		})
	}
}

func TestNativeSingBoxUnavailable(t *testing.T) {
	original := selectNativeSingBox
	t.Cleanup(func() { selectNativeSingBox = original })
	selectNativeSingBox = func(context.Context) (nativegateway.SingBoxChoice, error) {
		return nativegateway.SingBoxChoice{}, nativegateway.ErrNoUsableSingBox
	}
	var output bytes.Buffer
	_, err := nativeSingBoxSource(context.Background(), &output)
	if !errors.Is(err, nativegateway.ErrNoUsableSingBox) {
		t.Fatalf("selection error: %v", err)
	}
	const want = "native_runtime_stage_failure=sing-box-unavailable: neither the bundled nor the system sing-box can run a supported version (1.13-1.14)\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRuntimeSnapshotSurvivesPackageReplacement(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "packaged")
	dst := filepath.Join(dir, "runtime")
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dst, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := os.WriteFile(src, []byte("old executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyRuntimeBinary(context.Background(), fd, src, "sing-box", uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new executable"), 0755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "sing-box"))
	if err != nil || string(b) != "old executable" {
		t.Fatalf("snapshot modified: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyRuntimeBinary(ctx, fd, src, "sing-box", uint32(os.Getuid())); err == nil {
		t.Fatal("canceled replacement accepted")
	}
	b, _ = os.ReadFile(filepath.Join(dst, "sing-box"))
	if string(b) != "old executable" {
		t.Fatal("cancellation destroyed live snapshot")
	}
	if err := os.WriteFile(filepath.Join(dst, "sing-box.new"), []byte("interrupted snapshot"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyRuntimeBinary(context.Background(), fd, src, "sing-box", uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dst, "sing-box"))
	if string(b) != "new executable" {
		t.Fatal("explicit restart did not refresh snapshot")
	}
}

func TestRuntimeSnapshotRejectsUnsafeInputs(t *testing.T) {
	for _, kind := range []string{"symlink", "writable", "foreign-owner", "destination-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source")
			os.WriteFile(src, []byte("fixture"), 0755)
			fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			uid := uint32(os.Getuid())
			switch kind {
			case "symlink":
				os.Symlink(src, src+".link")
				src += ".link"
			case "writable":
				os.Chmod(src, 0777)
			case "foreign-owner":
				uid++
			case "destination-symlink":
				os.Symlink(src, filepath.Join(dir, "target.new"))
			}
			if err := copyRuntimeBinary(context.Background(), fd, src, "target", uid); err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}
