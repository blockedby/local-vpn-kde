//go:build linux

package localvpn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativeConfigurationLockPath    = "/run/local-vpn-kde-configuration.lock"
	nativeConfigurationLockRoot    = "/run"
	nativeConfigurationLockName    = "local-vpn-kde-configuration.lock"
	nativeConfigurationLockRetry   = 25 * time.Millisecond
	nativeConfigurationLockDirMode = 0022
)

// acquireNativeConfigurationLock serializes the privileged native
// configuration/package boundary. The shared form is reserved for the root
// native-network start path; callers must keep the returned release function
// until their whole operation, including its final selection, is complete.
func acquireNativeConfigurationLock(ctx context.Context, exclusive bool) (func(), error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("native configuration lock requires root")
	}
	return acquireNativeConfigurationLockAt(ctx, exclusive, nativeConfigurationLockRoot, 0)
}

// acquireNativeConfigurationLockAt is the filesystem-root and owner seam used
// by tests. Production always uses /run and UID 0 through
// acquireNativeConfigurationLock.
func acquireNativeConfigurationLockAt(ctx context.Context, exclusive bool, tempRoot string, expectedUID uint32) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tempRoot == "" || !filepath.IsAbs(tempRoot) || filepath.Clean(tempRoot) != tempRoot {
		return nil, errors.New("native configuration lock root is invalid")
	}

	parentFD, err := unix.Open(tempRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("native configuration lock root is unavailable: %w", err)
	}
	defer unix.Close(parentFD) // The lock descriptor pins the file after return.
	if err := validateNativeConfigurationLockRoot(parentFD, expectedUID); err != nil {
		return nil, err
	}

	lockFD, err := openNativeConfigurationLockFile(parentFD, expectedUID)
	if err != nil {
		return nil, err
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = unix.Close(lockFD)
		}
	}()

	operation := unix.LOCK_SH
	if exclusive {
		operation = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		err = unix.Flock(lockFD, operation|unix.LOCK_NB)
		if err == nil {
			// A cancellation can race the successful nonblocking flock. Do not
			// hand a lock to a caller whose operation has already been canceled.
			if contextErr := ctx.Err(); contextErr != nil {
				releaseNativeConfigurationLockFD(lockFD)
				closeOnFailure = false
				return nil, contextErr
			}
			if identityErr := verifyNativeConfigurationLockIdentity(parentFD, lockFD, expectedUID); identityErr != nil {
				releaseNativeConfigurationLockFD(lockFD)
				closeOnFailure = false
				return nil, identityErr
			}

			var once sync.Once
			release := func() {
				once.Do(func() {
					releaseNativeConfigurationLockFD(lockFD)
				})
			}
			closeOnFailure = false
			return release, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("native configuration lock acquisition failed: %w", err)
		}

		timer := time.NewTimer(nativeConfigurationLockRetry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateNativeConfigurationLockRoot(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("native configuration lock root cannot be inspected: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != expectedUID || stat.Mode&nativeConfigurationLockDirMode != 0 {
		return errors.New("native configuration lock root is unsafe")
	}
	return nil
}

func openNativeConfigurationLockFile(parentFD int, expectedUID uint32) (int, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	lockFD, err := unix.Openat(parentFD, nativeConfigurationLockName, flags|unix.O_CREAT|unix.O_EXCL, 0600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		// Never use O_CREAT for the existing-file path. O_NOFOLLOW makes a
		// pre-existing symlink fail closed rather than resolve outside /run.
		lockFD, err = unix.Openat(parentFD, nativeConfigurationLockName, flags, 0)
	}
	if err != nil {
		return -1, fmt.Errorf("native configuration lock file is unavailable: %w", err)
	}
	if created {
		// O_EXCL applies the requested mode subject to umask. Restore the
		// required exact mode before validating the newly created inode.
		if err := unix.Fchmod(lockFD, 0600); err != nil {
			_ = unix.Close(lockFD)
			return -1, fmt.Errorf("native configuration lock file cannot be protected: %w", err)
		}
	}
	if err := validateNativeConfigurationLockFileFD(lockFD, expectedUID); err != nil {
		_ = unix.Close(lockFD)
		return -1, err
	}
	if err := verifyNativeConfigurationLockIdentity(parentFD, lockFD, expectedUID); err != nil {
		_ = unix.Close(lockFD)
		return -1, err
	}
	return lockFD, nil
}

func validateNativeConfigurationLockFileFD(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("native configuration lock file cannot be inspected: %w", err)
	}
	return validateNativeConfigurationLockFileStat(&stat, expectedUID)
}

func validateNativeConfigurationLockFileStat(stat *unix.Stat_t, expectedUID uint32) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != expectedUID || stat.Mode&07777 != 0600 || stat.Nlink != 1 {
		return errors.New("native configuration lock file is unsafe")
	}
	return nil
}

func verifyNativeConfigurationLockIdentity(parentFD, lockFD int, expectedUID uint32) error {
	var held, current unix.Stat_t
	if err := unix.Fstat(lockFD, &held); err != nil {
		return fmt.Errorf("native configuration lock identity cannot be inspected: %w", err)
	}
	if err := unix.Fstatat(parentFD, nativeConfigurationLockName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("native configuration lock path cannot be inspected: %w", err)
	}
	if err := validateNativeConfigurationLockFileStat(&held, expectedUID); err != nil {
		return err
	}
	if err := validateNativeConfigurationLockFileStat(&current, expectedUID); err != nil {
		return err
	}
	if held.Dev != current.Dev || held.Ino != current.Ino {
		return errors.New("native configuration lock identity changed")
	}
	return nil
}

func releaseNativeConfigurationLockFD(fd int) {
	_ = unix.Flock(fd, unix.LOCK_UN)
	_ = unix.Close(fd)
}
