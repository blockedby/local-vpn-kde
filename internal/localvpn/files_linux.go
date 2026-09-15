package localvpn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const directoryFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func component(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

func directory(path string, create, owned bool) (int, error) {
	if !strings.HasPrefix(path, "/") {
		return -1, errors.New("absolute path required")
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return -1, errors.New("canonical path required")
		}
	}
	fd, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		next, e := childDirectory(fd, part, create, false)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	if err = checkDirectory(fd, owned); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func checkDirectory(fd int, owned bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || owned && st.Uid != uint32(os.Getuid()) {
		return errors.New("unsafe directory")
	}
	return nil
}

func childDirectory(parent int, name string, create, owned bool) (int, error) {
	if !component(name) {
		return -1, errors.New("invalid component")
	}
	fd, err := unix.Openat(parent, name, directoryFlags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err = unix.Mkdirat(parent, name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		fd, err = unix.Openat(parent, name, directoryFlags, 0)
	}
	if err != nil {
		return -1, err
	}
	if err = checkDirectory(fd, owned); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func readAt(parent int, name string) ([]byte, error) {
	if !component(name) {
		return nil, errors.New("invalid component")
	}
	// NONBLOCK prevents a hostile FIFO from hanging the reader before fstat.
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return nil, errors.New("unsafe input file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16*1024*1024+1))
	if len(data) > 16*1024*1024 {
		return nil, errors.New("input too large")
	}
	return data, err
}

func privateInode(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	if err == nil && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Getuid())) {
		err = errors.New("unsafe private file")
	}
	return st, err
}

func atomicWrite(parent int, name string, data []byte) error {
	if !component(name) {
		return errors.New("invalid output name")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	stage := ".vpnkit-render-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(parent, stage, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), stage)
	defer f.Close()
	renamed := false
	defer func() {
		if !renamed {
			var actual, held unix.Stat_t
			if unix.Fstat(fd, &held) == nil && unix.Fstatat(parent, stage, &actual, unix.AT_SYMLINK_NOFOLLOW) == nil && actual.Dev == held.Dev && actual.Ino == held.Ino {
				_ = unix.Unlinkat(parent, stage, 0)
			}
		}
	}()
	if _, err = privateInode(fd); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if _, err = privateInode(fd); err != nil {
		return err
	}
	if err = unix.Renameat(parent, stage, parent, name); err != nil {
		return err
	}
	renamed = true
	if err = unix.Fsync(parent); err != nil {
		return err
	}
	held, err := privateInode(fd)
	if err != nil {
		return err
	}
	var actual unix.Stat_t
	if err = unix.Fstatat(parent, name, &actual, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if actual.Dev != held.Dev || actual.Ino != held.Ino || actual.Nlink != 1 || actual.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("output replaced concurrently")
	}
	return nil
}

func lockFile(ctx context.Context, parent int, name string) (int, error) {
	if !component(name) {
		return -1, errors.New("invalid lock name")
	}
	fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return -1, err
	}
	if err = validatePrivateFile(fd); err != nil {
		unix.Close(fd)
		return -1, err
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var held, current unix.Stat_t
			if unix.Fstat(fd, &held) != nil || unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || held.Ino != current.Ino || held.Dev != current.Dev || held.Nlink != 1 {
				unix.Close(fd)
				return -1, errors.New("lock identity changed")
			}
			return fd, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			unix.Close(fd)
			return -1, err
		}
		select {
		case <-ctx.Done():
			unix.Close(fd)
			return -1, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
