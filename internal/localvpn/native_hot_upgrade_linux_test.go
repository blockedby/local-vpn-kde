//go:build linux

package localvpn

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

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
