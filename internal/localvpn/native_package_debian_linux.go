//go:build linux

package localvpn

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
)

// dpkg lock files persist between transactions. Hold POSIX locks (not flock)
// throughout recovery; never unlink or replace a package-manager lock file.
func recoverDebianPackageChangeAt(ctx context.Context, anchor string, uid uint32, finish func(context.Context) error) error {
	db, err := openPackageDatabaseAt(anchor, uid, "dpkg")
	if err != nil {
		return errors.New("cannot safely inspect dpkg database")
	}
	defer unix.Close(db)
	var locks []int
	defer func() {
		for _, fd := range locks {
			_ = unix.Close(fd)
		}
	}()
	for _, name := range []string{"lock-frontend", "lock"} {
		fd, err := unix.Openat(db, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return errors.New("cannot safely open dpkg transaction lock")
		}
		locks = append(locks, fd)
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uid || st.Mode&0022 != 0 || st.Nlink != 1 {
			return errors.New("unsafe dpkg transaction lock")
		}
		lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
		if unix.FcntlFlock(uintptr(fd), unix.F_SETLK, &lock) != nil {
			return errors.New("dpkg package transaction is active or lock unavailable")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := finish(ctx); err != nil {
		return err
	}
	return ctx.Err()
}
