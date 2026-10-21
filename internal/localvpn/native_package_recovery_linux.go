//go:build linux

package localvpn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativePackageRecoveryTimeout  = 45 * time.Second
	nativePackageDatabaseLock     = "db.lck"
	nativePackageDatabaseOwnerUID = uint32(0)
	nativePackageDatabaseAnchor   = "/"
)

// RecoverNativePackageChange clears an inhibitor left by an aborted native
// package transaction. It never touches pacman's lock; recovery is allowed
// only after the package database has been inspected while holding the
// exclusive native configuration lock.
func RecoverNativePackageChange(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("native package recovery requires root")
	}

	bounded, cancel := nativePackageRecoveryContext(ctx)
	defer cancel()

	release, err := acquireNativeConfigurationLock(bounded, true)
	if err != nil {
		return err
	}
	defer release()

	return recoverNativePackageChangeLockedAt(bounded, nativePackageDatabaseAnchor, nativePackageDatabaseOwnerUID, completeNativePackageChangeLocked)
}

// nativePackageRecoveryContext covers lock acquisition, pacman inspection, and
// the validated finish as one operation. The at-seam below is intentionally
// unexported: production has no caller-supplied filesystem anchor or finish.
func nativePackageRecoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, nativePackageRecoveryTimeout)
}

// recoverNativePackageChangeAt is a test seam. configurationRoot and anchor
// stand in for /run and / respectively; production uses the fixed entry point
// above. It acquires the same exclusive configuration lock before inspecting
// the fixed relative pacman database path.
func recoverNativePackageChangeAt(ctx context.Context, configurationRoot, anchor string, expectedUID uint32, finish func(context.Context) error) error {
	bounded, cancel := nativePackageRecoveryContext(ctx)
	defer cancel()

	release, err := acquireNativeConfigurationLockAt(bounded, true, configurationRoot, expectedUID)
	if err != nil {
		return err
	}
	defer release()

	return recoverNativePackageChangeLockedAt(bounded, anchor, expectedUID, finish)
}

// Caller holds the exclusive native configuration lock. The anchor is only an
// internal test seam; the public recovery path passes "/" and UID 0.
func recoverNativePackageChangeLockedAt(ctx context.Context, anchor string, expectedUID uint32, finish func(context.Context) error) error {
	if finish == nil {
		return errors.New("native package recovery finish is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	databaseFD, err := openNativePackageDatabaseAt(anchor, expectedUID)
	if err != nil {
		return errors.New("cannot safely inspect pacman database")
	}
	defer unix.Close(databaseFD)

	active, err := nativePackageTransactionActiveAt(databaseFD)
	if err != nil {
		return err
	}
	if active {
		return errors.New("pacman package transaction is active")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := finish(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// openNativePackageDatabaseAt walks the fixed /var/lib/pacman path through
// pinned, no-follow descriptors. Every directory in the walk must be owned by
// the expected root UID and must not be group- or world-writable.
func openNativePackageDatabaseAt(anchor string, expectedUID uint32) (int, error) {
	if anchor == "" || !filepath.IsAbs(anchor) || filepath.Clean(anchor) != anchor {
		return -1, errors.New("native pacman database anchor is invalid")
	}

	fd, err := unix.Open(anchor, directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	if err := validateNativePackageDatabaseDirectory(fd, expectedUID); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}

	for _, part := range []string{"var", "lib", "pacman"} {
		next, err := unix.Openat(fd, part, directoryFlags, 0)
		if err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		if err := validateNativePackageDatabaseDirectory(next, expectedUID); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, err
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func validateNativePackageDatabaseDirectory(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return errors.New("native pacman database directory cannot be inspected")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != expectedUID || stat.Mode&0022 != 0 {
		return errors.New("native pacman database directory is unsafe")
	}
	return nil
}

// nativePackageTransactionActiveAt only reads metadata. Any entry at the
// fixed db.lck name blocks recovery, including symlinks and unexpected inode
// types; no package-manager file is opened, removed, or changed.
func nativePackageTransactionActiveAt(databaseFD int) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(databaseFD, nativePackageDatabaseLock, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("cannot safely inspect pacman transaction lock")
	}
	return true, nil
}
