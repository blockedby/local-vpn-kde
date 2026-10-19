package localvpn

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestEnsureInstalledUserStateFreshCreation(t *testing.T) {
	anchor, boundary, state := installedBootstrapTestPaths(t)
	if err := ensureInstalledUserStateAt(state, anchor, uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{filepath.Join(anchor, "xdg"), boundary, state, filepath.Join(state, installedSubscriptionDirectory)} {
		assertInstalledBootstrapPrivateDirectory(t, path, uint32(os.Getuid()))
	}

	want := map[string]bool{
		".":                                true,
		"xdg":                              true,
		"xdg/state":                        true,
		"xdg/state/local-vpn-kde":          true,
		"xdg/state/local-vpn-kde/vibe-vpn": true,
	}
	seen := make(map[string]bool)
	if err := filepath.WalkDir(anchor, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(anchor, path)
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			t.Fatalf("bootstrap created non-directory %q", relative)
		}
		seen[relative] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(want) {
		t.Fatalf("created directories = %#v, want %#v", seen, want)
	}
	for path := range want {
		if !seen[path] {
			t.Fatalf("missing created directory %q", path)
		}
	}
}

func TestEnsureInstalledUserStateIdempotentPreservesExistingMode(t *testing.T) {
	anchor, boundary, state := installedBootstrapTestPaths(t)
	uid := uint32(os.Getuid())
	if err := ensureInstalledUserStateAt(state, anchor, uid); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Dir(boundary)
	if err := os.Chmod(parent, 0750); err != nil {
		t.Fatal(err)
	}
	var before unix.Stat_t
	if err := unix.Lstat(parent, &before); err != nil {
		t.Fatal(err)
	}
	if err := ensureInstalledUserStateAt(state, anchor, uid); err != nil {
		t.Fatal(err)
	}
	var after unix.Stat_t
	if err := unix.Lstat(parent, &after); err != nil {
		t.Fatal(err)
	}
	if before.Uid != after.Uid || before.Mode != after.Mode || before.Ino != after.Ino || before.Dev != after.Dev {
		t.Fatalf("existing directory changed: before=%#v after=%#v", before, after)
	}
	assertInstalledBootstrapPrivateDirectory(t, state, uid)
	assertInstalledBootstrapPrivateDirectory(t, filepath.Join(state, installedSubscriptionDirectory), uid)
}

func TestEnsureInstalledUserStateRejectsUnsafePaths(t *testing.T) {
	t.Run("foreign base", func(t *testing.T) {
		anchor, boundary, state := installedBootstrapTestPaths(t)
		for _, base := range []string{filepath.Join(boundary, "other"), state + "/", boundary + "/../local-vpn-kde"} {
			if err := ensureInstalledUserStateAt(base, anchor, uint32(os.Getuid())); err == nil {
				t.Fatalf("foreign or noncanonical base %q was accepted", base)
			}
		}
	})

	t.Run("writable boundary", func(t *testing.T) {
		anchor, boundary, state := installedBootstrapTestPaths(t)
		if err := os.MkdirAll(boundary, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(boundary, 0777); err != nil {
			t.Fatal(err)
		}
		if err := ensureInstalledUserStateAt(state, anchor, uint32(os.Getuid())); err == nil {
			t.Fatal("writable existing boundary was accepted")
		}
		assertInstalledBootstrapMode(t, boundary, 0777)
		assertNotExists(t, state)
	})

	t.Run("world-writable tmp-like ancestor", func(t *testing.T) {
		anchor := t.TempDir()
		tmpLike := filepath.Join(anchor, "tmp")
		if err := os.Mkdir(tmpLike, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmpLike, 01777); err != nil {
			t.Fatal(err)
		}
		boundary := filepath.Join(tmpLike, "user", "state")
		t.Setenv("XDG_STATE_HOME", boundary)
		state := filepath.Join(boundary, "local-vpn-kde")
		if err := ensureInstalledUserStateAt(state, anchor, uint32(os.Getuid())); err == nil {
			t.Fatal("world-writable ancestor was accepted")
		}
		assertInstalledBootstrapMode(t, tmpLike, 0777)
		assertNotExists(t, state)
	})

	t.Run("symlink boundary", func(t *testing.T) {
		anchor, boundary, state := installedBootstrapTestPaths(t)
		if err := os.Mkdir(filepath.Join(anchor, "xdg"), 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(anchor, "target")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, boundary); err != nil {
			t.Fatal(err)
		}
		if err := ensureInstalledUserStateAt(state, anchor, uint32(os.Getuid())); err == nil {
			t.Fatal("symlinked existing boundary was accepted")
		}
		assertNotExists(t, state)
	})

	t.Run("unsafe existing state mode", func(t *testing.T) {
		anchor, _, state := installedBootstrapTestPaths(t)
		if err := os.MkdirAll(state, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(state, 0750); err != nil {
			t.Fatal(err)
		}
		if err := ensureInstalledUserStateAt(state, anchor, uint32(os.Getuid())); err == nil {
			t.Fatal("non-private existing state was accepted")
		}
		assertInstalledBootstrapMode(t, state, 0750)
	})

	t.Run("unsafe existing child mode", func(t *testing.T) {
		anchor, _, state := installedBootstrapTestPaths(t)
		uid := uint32(os.Getuid())
		if err := ensureInstalledUserStateAt(state, anchor, uid); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(state, installedSubscriptionDirectory)
		if err := os.Chmod(child, 0750); err != nil {
			t.Fatal(err)
		}
		if err := ensureInstalledUserStateAt(state, anchor, uid); err == nil {
			t.Fatal("non-private existing child was accepted")
		}
		assertInstalledBootstrapMode(t, child, 0750)
	})

	t.Run("unexpected owner uid", func(t *testing.T) {
		anchor, _, state := installedBootstrapTestPaths(t)
		uid := uint32(os.Getuid()) + 1
		if err := ensureInstalledUserStateAt(state, anchor, uid); err == nil {
			t.Fatal("directory owned by another uid was accepted")
		}
		assertNotExists(t, state)
	})
}

func TestEnsureInstalledUserStatePreservesForeignChild(t *testing.T) {
	anchor, _, state := installedBootstrapTestPaths(t)
	uid := uint32(os.Getuid())
	if err := ensureInstalledUserStateAt(state, anchor, uid); err != nil {
		t.Fatal(err)
	}

	foreign := filepath.Join(state, installedSubscriptionDirectory, "foreign-entry")
	content := []byte("fixture-owned content\n")
	if err := os.WriteFile(foreign, content, 0600); err != nil {
		t.Fatal(err)
	}
	var before unix.Stat_t
	if err := unix.Lstat(foreign, &before); err != nil {
		t.Fatal(err)
	}

	if err := ensureInstalledUserStateAt(state, anchor, uid); err != nil {
		t.Fatal(err)
	}
	var after unix.Stat_t
	if err := unix.Lstat(foreign, &after); err != nil {
		t.Fatal(err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Uid != after.Uid {
		t.Fatalf("foreign child changed: before=%#v after=%#v", before, after)
	}
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("foreign child content = %q, want %q", got, content)
	}
}

func installedBootstrapTestPaths(t *testing.T) (anchor, boundary, state string) {
	t.Helper()
	anchor = t.TempDir()
	boundary = filepath.Join(anchor, "xdg", "state")
	t.Setenv("XDG_STATE_HOME", boundary)
	state = filepath.Join(boundary, "local-vpn-kde")
	return anchor, boundary, state
}

func assertInstalledBootstrapPrivateDirectory(t *testing.T, path string, uid uint32) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uid || st.Mode&0777 != 0700 {
		t.Fatalf("directory %q is not private: mode=%#o uid=%d", path, st.Mode, st.Uid)
	}
}

func assertInstalledBootstrapMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode %q = %o, want %o", path, info.Mode().Perm(), want)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%q exists or could not be inspected: %v", path, err)
	}
}
