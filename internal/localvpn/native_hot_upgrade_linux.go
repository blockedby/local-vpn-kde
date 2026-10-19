//go:build linux

package localvpn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"golang.org/x/sys/unix"
)

const nativePinnedRuntime = "/var/lib/local-vpn-kde-runtime"

// Bump whenever the worker protocol, private state or network journal becomes
// incompatible with a running supervisor from a previous package.
const nativeRuntimeABI = "native-runtime-v1"

func nativeRuntimeContract() []byte {
	h := sha256.New()
	for _, name := range []string{nativegateway.NetworkUnit, nativegateway.GatewayUnit} {
		io.WriteString(h, name)
		io.WriteString(h, nativegateway.UnitFiles()[name])
	}
	return []byte(nativeRuntimeABI + "\n" + hex.EncodeToString(h.Sum(nil)) + "\n")
}

// StageNativeRuntime is a root-only ExecStartPre. Snapshots outlive package file
// replacement, so subsequent proxy restarts use the same code as the supervisor.
func StageNativeRuntime(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("runtime staging requires root")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	blocked, err := nativePackageBlocked()
	if err != nil {
		return err
	}
	if blocked {
		return errors.New("package transaction blocks runtime staging")
	}
	if err := os.Mkdir(nativePinnedRuntime, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	fd, err := unix.Open(nativePinnedRuntime, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Mode&0022 != 0 {
		return errors.New("unsafe pinned runtime directory")
	}
	for _, name := range []string{"sing-box", "vibe-vpn"} {
		if err := copyRuntimeBinary(ctx, fd, filepath.Join(nativegateway.LibexecDir, name), name, 0); err != nil {
			return err
		}
	}
	return atomicWrite(fd, "contract", nativeRuntimeContract())
}

func copyRuntimeBinary(ctx context.Context, dst int, source, name string, expectedUID uint32) error {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), source)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != expectedUID || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Size <= 0 || st.Size > 256<<20 {
		return errors.New("unsafe runtime binary")
	}
	tmp := name + ".new"
	outfd, err := unix.Openat(dst, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0700)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dst, tmp, 0)
	out := os.NewFile(uintptr(outfd), tmp)
	defer out.Close()
	if _, err = io.Copy(out, io.LimitReader(f, 256<<20+1)); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = out.Chmod(0755); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = unix.Renameat(dst, tmp, dst, name); err != nil {
		return err
	}
	return unix.Fsync(dst)
}

// Upgrades inhibit mutating workers only for the package transaction. Existing
// packet forwarding, namespace, OpenVPN and the pinned proxy continue running.
func PrepareNativePackageUpgrade(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("package upgrade requires root")
	}
	ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	release, err := acquireNativeConfigurationLock(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	if err := checkRunningNativeContract(ctx); err != nil {
		return err
	}
	return setNativePackageBlock(true)
}

func checkRunningNativeContract(ctx context.Context) error {
	if _, err := os.Stat("/run/systemd/system"); os.IsNotExist(err) {
		return nil
	}
	state, err := (NativeService{}).queryUnit(ctx, nativegateway.GatewayUnit)
	if err != nil {
		return err
	}
	if state.ActiveState == "inactive" || state.ActiveState == "failed" {
		return nil
	}
	if state.ActiveState != "active" {
		return errors.New("wait for native service lifecycle before package upgrade")
	}
	fd, err := directory(nativePinnedRuntime, false, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := validateNativeServiceDirectory(fd, 0); err != nil {
		return err
	}
	data, err := readAt(fd, "contract")
	if err != nil {
		return err
	}
	if string(data) != string(nativeRuntimeContract()) {
		return errors.New("running native runtime is incompatible; explicit restart required")
	}
	return nil
}
