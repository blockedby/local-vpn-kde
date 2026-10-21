//go:build linux

package localvpn

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const nativeSetupInhibitorName = "setup-incomplete"
const nativeSetupInhibitorContents = "native-setup-incomplete-v1\n"

// All callers hold the root configuration lock (shared for reads, exclusive
// for changes). Unlike the package marker, this survives reboot: a killed
// setup process cannot make a potentially mismatched generation startable.
func nativeSetupIncomplete() (bool, error) {
	if os.Geteuid() != 0 {
		return true, errors.New("native setup state requires root")
	}
	return nativeSetupIncompleteAt("/", 0)
}
func setNativeSetupIncomplete(incomplete bool) error {
	if os.Geteuid() != 0 {
		return errors.New("native setup state requires root")
	}
	return setNativeSetupIncompleteAt("/", 0, incomplete)
}
func openNativeSetupInhibitorDirectory(anchor string, uid uint32, create bool) (int, error) {
	fd, err := unix.Open(anchor, directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	if err = validateProtectedDirectory(fd, uid, true, false); err != nil {
		unix.Close(fd)
		return -1, err
	}
	for _, part := range []string{"var", "lib", "local-vpn-kde-install"} {
		next, e := unix.Openat(fd, part, directoryFlags, 0)
		if errors.Is(e, unix.ENOENT) && create && part == "local-vpn-kde-install" {
			if e = unix.Mkdirat(fd, part, 0700); e == nil {
				if e = unix.Fsync(fd); e != nil {
					unix.Close(fd)
					return -1, e
				}
			} else if !errors.Is(e, unix.EEXIST) {
				unix.Close(fd)
				return -1, e
			}
			next, e = unix.Openat(fd, part, directoryFlags, 0)
		}
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
		if e = validateProtectedDirectory(fd, uid, true, part == "local-vpn-kde-install"); e != nil {
			unix.Close(fd)
			return -1, e
		}
	}
	return fd, nil
}
func readNativeSetupInhibitor(fd int, uid uint32) (bool, error) {
	raw, err := unix.Openat(fd, nativeSetupInhibitorName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	f := os.NewFile(uintptr(raw), nativeSetupInhibitorName)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(raw, &st); err != nil {
		return true, err
	}
	if st.Uid != uid || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 {
		return true, errors.New("unsafe native setup inhibitor")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(len(nativeSetupInhibitorContents)+1)))
	if err != nil || string(data) != nativeSetupInhibitorContents {
		return true, errors.New("invalid native setup inhibitor")
	}
	return true, nil
}
func nativeSetupIncompleteAt(anchor string, uid uint32) (bool, error) {
	fd, err := openNativeSetupInhibitorDirectory(anchor, uid, false)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer unix.Close(fd)
	return readNativeSetupInhibitor(fd, uid)
}
func setNativeSetupIncompleteAt(anchor string, uid uint32, incomplete bool) error {
	fd, err := openNativeSetupInhibitorDirectory(anchor, uid, incomplete)
	if errors.Is(err, unix.ENOENT) && !incomplete {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	exists, err := readNativeSetupInhibitor(fd, uid)
	if err != nil {
		return err
	}
	if incomplete {
		return atomicWrite(fd, nativeSetupInhibitorName, []byte(nativeSetupInhibitorContents))
	}
	if !exists {
		return nil
	}
	if err = unix.Unlinkat(fd, nativeSetupInhibitorName, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
