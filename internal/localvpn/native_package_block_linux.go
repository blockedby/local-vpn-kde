//go:build linux

package localvpn

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const nativePackageBlockName = "local-vpn-kde-package-blocked"
const nativePackageBlockContents = "native-package-transaction-v1\n"

// Caller holds the root configuration lock. A persistent-in-/run marker closes
// the gap between the pre-transaction hook's exit and package file replacement.
func nativePackageBlocked() (bool, error) {
	fd, err := directory("/run", false, false)
	if err != nil {
		return true, err
	}
	defer unix.Close(fd)
	return nativePackageBlockedAt(fd)
}
func nativePackageBlockedAt(fd int) (bool, error) {
	data, err := readAt(fd, nativePackageBlockName)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil || string(data) != nativePackageBlockContents {
		return true, errors.New("native package transaction marker is invalid")
	}
	return true, nil
}
func setNativePackageBlockAt(fd int, blocked bool) error {
	exists, err := nativePackageBlockedAt(fd)
	if err != nil {
		return err
	}
	if blocked {
		return atomicWrite(fd, nativePackageBlockName, []byte(nativePackageBlockContents))
	}
	if !exists {
		return nil
	}
	if err = unix.Unlinkat(fd, nativePackageBlockName, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
func setNativePackageBlock(blocked bool) error {
	fd, err := directory("/run", false, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return setNativePackageBlockAt(fd, blocked)
}

// CompleteNativePackageChange is called only after installation/upgrade. It
// clears the start inhibitor without enabling, starting or reconnecting anything.
func CompleteNativePackageChange(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("native package transaction requires root")
	}
	release, err := acquireNativeConfigurationLock(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	return completeNativePackageChangeLocked(ctx)
}

// Caller holds the exclusive configuration lock.
func completeNativePackageChangeLocked(ctx context.Context) error {
	var err error
	if err = ValidateInstalledAssets(); err != nil {
		return errors.New("cannot clear package inhibitor: installed assets invalid")
	}
	if err = (NativeService{}).verifyInstalledUnits(ctx); err != nil {
		return errors.New("cannot clear package inhibitor: installed units invalid")
	}
	if _, err = os.Stat("/run/systemd/system"); err == nil {
		if err = (NativeService{}).mutationPreflight(ctx); err != nil {
			return errors.New("cannot clear package inhibitor: system manager requires attention")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect system manager")
	}
	return setNativePackageBlock(false)
}
