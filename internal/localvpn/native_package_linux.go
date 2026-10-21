//go:build linux

package localvpn

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// PrepareNativePackageRemoval runs before package files are replaced/removed.
// Failure must abort the package transaction. It never deletes private data,
// profiles, accounts or user autostart preferences and never reconnects a VPN.
func PrepareNativePackageRemoval(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("native package transaction requires root")
	}
	if _, err := os.Stat("/run/systemd/system"); errors.Is(err, os.ErrNotExist) {
		return nil // Offline package root/container: no running system manager.
	} else if err != nil {
		return errors.New("cannot inspect system manager")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	release, err := acquireNativeConfigurationLock(lockCtx, true)
	if err != nil {
		return errors.New("package transaction blocked: native configuration is busy")
	}
	defer release()
	service := NativeService{}
	if err := prepareNativePackageRemoval(ctx, service.Stop, func(ctx context.Context) error {
		return RunNativeNetwork(ctx, "stop", io.Discard)
	}); err != nil {
		return err
	}
	return setNativePackageBlock(true)
}

func prepareNativePackageRemoval(ctx context.Context, stopService, stopNetwork func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return err
	}
	if err := stopService(bounded); err != nil {
		return errors.New("package transaction blocked: cannot safely stop native services")
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	if err := stopNetwork(bounded); err != nil {
		return errors.New("package transaction blocked: native network cleanup incomplete")
	}
	return nil
}
