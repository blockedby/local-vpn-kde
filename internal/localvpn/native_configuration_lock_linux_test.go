//go:build linux

package localvpn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func nativeConfigurationLockTestRoot(t *testing.T) (string, uint32) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	return root, stat.Uid
}

func nativeConfigurationLockTestPath(root string) string {
	return filepath.Join(root, nativeConfigurationLockName)
}

func nativeConfigurationLockTestStat(t *testing.T, path string, follow bool) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	var err error
	if follow {
		err = unix.Stat(path, &stat)
	} else {
		err = unix.Lstat(path, &stat)
	}
	if err != nil {
		t.Fatal(err)
	}
	return stat
}

func TestAcquireNativeConfigurationLockCreatesPersistentProtectedInode(t *testing.T) {
	root, expectedUID := nativeConfigurationLockTestRoot(t)
	release, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID)
	if err != nil {
		t.Fatal(err)
	}
	path := nativeConfigurationLockTestPath(root)
	stat := nativeConfigurationLockTestStat(t, path, false)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != expectedUID || stat.Mode&07777 != 0600 || stat.Nlink != 1 {
		t.Fatalf("lock inode = mode %#o uid %d nlink %d", stat.Mode, stat.Uid, stat.Nlink)
	}

	release()
	release()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("normal release removed lock file: %v", err)
	}
	again, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestAcquireNativeConfigurationLockAllowsSharedAndBlocksExclusive(t *testing.T) {
	root, expectedUID := nativeConfigurationLockTestRoot(t)
	sharedOne, err := acquireNativeConfigurationLockAt(context.Background(), false, root, expectedUID)
	if err != nil {
		t.Fatal(err)
	}
	sharedTwo, err := acquireNativeConfigurationLockAt(context.Background(), false, root, expectedUID)
	if err != nil {
		sharedOne()
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	exclusiveDone := make(chan error, 1)
	go func() {
		release, err := acquireNativeConfigurationLockAt(ctx, true, root, expectedUID)
		if release != nil {
			release()
		}
		exclusiveDone <- err
	}()

	select {
	case err := <-exclusiveDone:
		sharedOne()
		sharedTwo()
		if err == nil {
			t.Fatal("exclusive lock was acquired while shared locks were held")
		}
		t.Fatalf("exclusive lock failed before shared release: %v", err)
	case <-time.After(3 * nativeConfigurationLockRetry):
	}

	sharedOne()
	sharedTwo()
	select {
	case err := <-exclusiveDone:
		if err != nil {
			t.Fatalf("exclusive lock did not acquire after shared release: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("exclusive lock did not become available after shared release")
	}
}

func TestAcquireNativeConfigurationLockCancellationClosesDescriptor(t *testing.T) {
	root, expectedUID := nativeConfigurationLockTestRoot(t)
	held, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		release func()
		err     error
	}, 1)
	go func() {
		release, err := acquireNativeConfigurationLockAt(ctx, true, root, expectedUID)
		result <- struct {
			release func()
			err     error
		}{release, err}
	}()
	time.Sleep(3 * nativeConfigurationLockRetry)
	cancel()
	select {
	case outcome := <-result:
		if outcome.release != nil {
			outcome.release()
		}
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", outcome.err)
		}
	case <-time.After(500 * time.Millisecond):
		held()
		t.Fatal("canceled lock acquisition did not return")
	}

	held()
	cleanup, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID)
	if err != nil {
		t.Fatalf("lock remained held after cancellation: %v", err)
	}
	cleanup()
}

func TestAcquireNativeConfigurationLockRejectsUnsafeExistingEntriesWithoutChangingThem(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root, expectedUID := nativeConfigurationLockTestRoot(t)
		path := nativeConfigurationLockTestPath(root)
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID); err == nil {
			t.Fatal("accepted symlink lock path")
		}
		link, err := os.Readlink(path)
		if err != nil || link != target {
			t.Fatalf("symlink changed or disappeared: link=%q err=%v", link, err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		root, expectedUID := nativeConfigurationLockTestRoot(t)
		path := nativeConfigurationLockTestPath(root)
		original := filepath.Join(root, "original")
		if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(original, path); err != nil {
			t.Fatal(err)
		}
		before := nativeConfigurationLockTestStat(t, path, false)
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID); err == nil {
			t.Fatal("accepted hard-linked lock path")
		}
		after := nativeConfigurationLockTestStat(t, path, false)
		if after.Dev != before.Dev || after.Ino != before.Ino || after.Nlink != 2 {
			t.Fatalf("hard link changed: before=%+v after=%+v", before, after)
		}
	})

	t.Run("world-writable", func(t *testing.T) {
		root, expectedUID := nativeConfigurationLockTestRoot(t)
		path := nativeConfigurationLockTestPath(root)
		if err := os.WriteFile(path, []byte("world-writable"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
		before := nativeConfigurationLockTestStat(t, path, false)
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID); err == nil {
			t.Fatal("accepted world-writable lock path")
		}
		after := nativeConfigurationLockTestStat(t, path, false)
		if after.Dev != before.Dev || after.Ino != before.Ino || after.Mode != before.Mode || after.Uid != before.Uid || after.Nlink != before.Nlink {
			t.Fatalf("world-writable entry changed: before=%+v after=%+v", before, after)
		}
	})

	t.Run("foreign-owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("foreign ownership setup requires root")
		}
		root, expectedUID := nativeConfigurationLockTestRoot(t)
		path := nativeConfigurationLockTestPath(root)
		if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
		foreignUID := 65534
		if expectedUID == uint32(foreignUID) {
			foreignUID = 65533
		}
		if err := os.Chown(path, foreignUID, foreignUID); err != nil {
			t.Skipf("foreign ownership setup unavailable: %v", err)
		}
		before := nativeConfigurationLockTestStat(t, path, false)
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, expectedUID); err == nil {
			t.Fatal("accepted foreign-owned lock path")
		}
		after := nativeConfigurationLockTestStat(t, path, false)
		if after.Dev != before.Dev || after.Ino != before.Ino || after.Mode != before.Mode || after.Uid != before.Uid || after.Nlink != before.Nlink {
			t.Fatalf("foreign entry changed: before=%+v after=%+v", before, after)
		}
	})
}

func TestAcquireNativeConfigurationLockRejectsUnsafeRootDescriptor(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		realRoot := t.TempDir()
		linkRoot := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(realRoot, linkRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, linkRoot, uint32(os.Geteuid())); err == nil {
			t.Fatal("accepted symlink root descriptor")
		}
	})

	t.Run("world-writable", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0777); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, uint32(os.Geteuid())); err == nil {
			t.Fatal("accepted world-writable root descriptor")
		}
	})

	t.Run("foreign-owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("foreign ownership setup requires root")
		}
		root := t.TempDir()
		foreignUID := 65534
		if err := os.Chown(root, foreignUID, foreignUID); err != nil {
			t.Skipf("foreign ownership setup unavailable: %v", err)
		}
		if _, err := acquireNativeConfigurationLockAt(context.Background(), true, root, 0); err == nil {
			t.Fatal("accepted foreign-owned root descriptor")
		}
	})
}

func TestAcquireNativeConfigurationLockProductionEntryPointRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production root guard is exercised by a non-root test runner")
	}
	if _, err := acquireNativeConfigurationLock(context.Background(), true); err == nil {
		t.Fatal("unprivileged process acquired production lock")
	}
}
