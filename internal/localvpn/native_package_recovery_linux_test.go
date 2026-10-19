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

type nativePackageRecoveryFixture struct {
	root string
	run  string
	db   string
	uid  uint32
}

func newNativePackageRecoveryFixture(t *testing.T) nativePackageRecoveryFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "run")
	db := filepath.Join(root, "var", "lib", "pacman")
	for _, path := range []string{run, db} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{root, run, filepath.Join(root, "var"), filepath.Join(root, "var", "lib"), db} {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}

	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	return nativePackageRecoveryFixture{root: root, run: run, db: db, uid: stat.Uid}
}

func (f nativePackageRecoveryFixture) recover(ctx context.Context, finish func(context.Context) error) error {
	return recoverNativePackageChangeAt(ctx, f.run, f.root, f.uid, finish)
}

func setNativePackageRecoveryMarker(t *testing.T, run string, blocked bool) {
	t.Helper()
	fd, err := unix.Open(run, directoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := setNativePackageBlockAt(fd, blocked); err != nil {
		t.Fatal(err)
	}
}

func nativePackageRecoveryMarker(t *testing.T, run string) bool {
	t.Helper()
	fd, err := unix.Open(run, directoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	blocked, err := nativePackageBlockedAt(fd)
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

func TestRecoverNativePackageChangeClearsAbortedTransactionAfterValidatedFinish(t *testing.T) {
	fixture := newNativePackageRecoveryFixture(t)
	setNativePackageRecoveryMarker(t, fixture.run, true)

	called := false
	err := fixture.recover(context.Background(), func(ctx context.Context) error {
		called = true
		if _, ok := ctx.Deadline(); !ok {
			t.Error("recovery finish did not receive a deadline")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		setNativePackageRecoveryMarker(t, fixture.run, false)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("validated finish was not invoked")
	}
	if nativePackageRecoveryMarker(t, fixture.run) {
		t.Fatal("aborted package marker was not cleared")
	}
	if _, err := os.Lstat(filepath.Join(fixture.db, nativePackageDatabaseLock)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery changed package lock state: %v", err)
	}
}

func TestRecoverNativePackageChangeRefusesExistingOrSymlinkPackageLock(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		fixture := newNativePackageRecoveryFixture(t)
		setNativePackageRecoveryMarker(t, fixture.run, true)
		lockPath := filepath.Join(fixture.db, nativePackageDatabaseLock)
		before := []byte("pacman-lock-data\n")
		if err := os.WriteFile(lockPath, before, 0600); err != nil {
			t.Fatal(err)
		}

		called := false
		err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("active package transaction was not refused: err=%v called=%v", err, called)
		}
		if after, readErr := os.ReadFile(lockPath); readErr != nil || string(after) != string(before) {
			t.Fatalf("package lock changed: data=%q err=%v", after, readErr)
		}
		if !nativePackageRecoveryMarker(t, fixture.run) {
			t.Fatal("package marker cleared while transaction was active")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		fixture := newNativePackageRecoveryFixture(t)
		setNativePackageRecoveryMarker(t, fixture.run, true)
		lockPath := filepath.Join(fixture.db, nativePackageDatabaseLock)
		target := filepath.Join(fixture.root, "pacman-lock-target")
		before := []byte("target-data\n")
		if err := os.WriteFile(target, before, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, lockPath); err != nil {
			t.Fatal(err)
		}

		called := false
		err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("symlink package lock was not refused: err=%v called=%v", err, called)
		}
		if link, readErr := os.Readlink(lockPath); readErr != nil || link != target {
			t.Fatalf("package lock symlink changed: link=%q err=%v", link, readErr)
		}
		if after, readErr := os.ReadFile(target); readErr != nil || string(after) != string(before) {
			t.Fatalf("symlink target changed: data=%q err=%v", after, readErr)
		}
		if !nativePackageRecoveryMarker(t, fixture.run) {
			t.Fatal("package marker cleared while transaction was active")
		}
	})
}

func TestRecoverNativePackageChangeRefusesUnsafePacmanParentsWithoutChangingThem(t *testing.T) {
	t.Run("missing-database", func(t *testing.T) {
		fixture := newNativePackageRecoveryFixture(t)
		if err := os.Remove(fixture.db); err != nil {
			t.Fatal(err)
		}
		called := false
		if err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		}); err == nil || called {
			t.Fatalf("missing pacman database was accepted: err=%v called=%v", err, called)
		}
	})

	t.Run("symlink-parent", func(t *testing.T) {
		fixture := newNativePackageRecoveryFixture(t)
		pacman := fixture.db
		if err := os.Remove(pacman); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(fixture.root, "pacman-target")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(target, "sentinel")
		if err := os.WriteFile(sentinel, []byte("unchanged\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, pacman); err != nil {
			t.Fatal(err)
		}

		called := false
		if err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		}); err == nil || called {
			t.Fatalf("symlink pacman parent was accepted: err=%v called=%v", err, called)
		}
		if link, readErr := os.Readlink(pacman); readErr != nil || link != target {
			t.Fatalf("pacman parent symlink changed: link=%q err=%v", link, readErr)
		}
		if data, readErr := os.ReadFile(sentinel); readErr != nil || string(data) != "unchanged\n" {
			t.Fatalf("symlink target changed: data=%q err=%v", data, readErr)
		}
	})

	t.Run("writable-parent", func(t *testing.T) {
		fixture := newNativePackageRecoveryFixture(t)
		parent := filepath.Join(fixture.root, "var", "lib")
		if err := os.Chmod(parent, 0777); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(parent)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		if err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		}); err == nil || called {
			t.Fatalf("writable pacman parent was accepted: err=%v called=%v", err, called)
		}
		after, err := os.Lstat(parent)
		if err != nil {
			t.Fatal(err)
		}
		if before.Mode() != after.Mode() || before.ModTime() != after.ModTime() {
			t.Fatalf("writable parent changed: before=%v after=%v", before, after)
		}
	})

	t.Run("foreign-parent", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("foreign ownership setup requires root")
		}
		fixture := newNativePackageRecoveryFixture(t)
		parent := filepath.Join(fixture.root, "var", "lib")
		foreignUID := 65534
		if fixture.uid == uint32(foreignUID) {
			foreignUID = 65533
		}
		if err := os.Chown(parent, foreignUID, foreignUID); err != nil {
			t.Skipf("foreign ownership setup unavailable: %v", err)
		}
		before := nativePackageRecoveryLstat(t, parent)
		called := false
		if err := fixture.recover(context.Background(), func(context.Context) error {
			called = true
			return nil
		}); err == nil || called {
			t.Fatalf("foreign pacman parent was accepted: err=%v called=%v", err, called)
		}
		after := nativePackageRecoveryLstat(t, parent)
		if before.Uid != after.Uid || before.Mode != after.Mode || before.Ino != after.Ino {
			t.Fatalf("foreign parent changed: before=%+v after=%+v", before, after)
		}
	})
}

func nativePackageRecoveryLstat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}

func TestRecoverNativePackageChangeChecksPackageLockAfterExclusiveConfigurationLock(t *testing.T) {
	fixture := newNativePackageRecoveryFixture(t)
	held, err := acquireNativeConfigurationLockAt(context.Background(), true, fixture.run, fixture.uid)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	called := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		result <- fixture.recover(ctx, func(context.Context) error {
			called <- struct{}{}
			return nil
		})
	}()

	// Keep the configuration lock held long enough for the recovery goroutine to
	// be waiting on it. The package lock is added only after that point.
	time.Sleep(8 * nativeConfigurationLockRetry)
	lockPath := filepath.Join(fixture.db, nativePackageDatabaseLock)
	if err := os.WriteFile(lockPath, []byte("created-under-lock\n"), 0600); err != nil {
		held()
		t.Fatal(err)
	}
	held()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("recovery cleared the marker after a package lock appeared")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not finish after configuration lock release")
	}
	select {
	case <-called:
		t.Fatal("validated finish ran while package transaction was active")
	default:
	}
}

func TestRecoverNativePackageChangePassesCancellationToFinish(t *testing.T) {
	fixture := newNativePackageRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := fixture.recover(ctx, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("recovery deadline was not bounded: %v", elapsed)
	}
}
