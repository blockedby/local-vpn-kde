package localvpn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const installedSubscriptionDirectory = "vibe-vpn"

// EnsureInstalledUserState creates the private directory tree required by an
// installed native TUI. It deliberately does not create or inspect any files.
func EnsureInstalledUserState(base string) error {
	uid := os.Getuid()
	euid := os.Geteuid()
	if uid == 0 || euid == 0 || uid != euid {
		return errors.New("installed user state requires a nonroot desktop uid")
	}
	return ensureInstalledUserStateAt(base, "/", uint32(uid))
}

// ensureInstalledUserStateAt is the filesystem-root and UID seam used by
// tests. Production always calls it with root "/" and the real user UID.
func ensureInstalledUserStateAt(base, root string, uid uint32) error {
	state, boundary, err := installedStatePath()
	if err != nil {
		return err
	}
	return ensureInstalledUserStatePaths(base, state, boundary, root, uid)
}

func ensureInstalledUserStatePaths(base, state, boundary, root string, uid uint32) error {
	for _, path := range []string{base, state, boundary, root} {
		if err := validateAbsoluteLayoutPath(path); err != nil {
			return fmt.Errorf("invalid installed state path: %w", err)
		}
		if filepath.Clean(path) != path {
			return errors.New("installed state path is not canonical")
		}
	}
	if state != filepath.Join(boundary, "local-vpn-kde") || base != state {
		return errors.New("installed application state path is fixed")
	}
	if !pathWithin(root, boundary) || !pathWithin(root, state) || !pathWithin(boundary, state) {
		return errors.New("installed state path escapes its user boundary")
	}

	fd, err := unix.Open(root, directoryFlags, 0)
	if err != nil {
		return fmt.Errorf("open installed state root: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	current := root
	currentPrivate := current == state
	if err := validateInstalledBootstrapDirectory(fd, uid, pathWithin(boundary, current), currentPrivate); err != nil {
		return fmt.Errorf("validate installed state root: %w", err)
	}

	for _, part := range relativeParts(root, state) {
		nextPath := filepath.Join(current, part)
		nextPrivate := nextPath == state
		nextUserOwned := pathWithin(boundary, nextPath)
		next, err := openOrCreateInstalledBootstrapDirectory(fd, part, uid, pathWithin(boundary, current), currentPrivate, nextUserOwned, nextPrivate)
		if err != nil {
			return fmt.Errorf("prepare installed state directory %s: %w", nextPath, err)
		}
		_ = unix.Close(fd)
		fd = next
		current = nextPath
		currentPrivate = nextPrivate
	}

	if err := validateInstalledBootstrapDirectory(fd, uid, true, true); err != nil {
		return fmt.Errorf("validate installed state directory: %w", err)
	}
	child, err := openOrCreateInstalledBootstrapDirectory(fd, installedSubscriptionDirectory, uid, true, true, true, true)
	if err != nil {
		return fmt.Errorf("prepare installed subscription directory: %w", err)
	}
	_ = unix.Close(child)
	return nil
}

func openOrCreateInstalledBootstrapDirectory(parentFD int, name string, uid uint32, parentUserOwned, parentPrivate, userOwned, private bool) (int, error) {
	if !component(name) {
		return -1, errors.New("invalid directory component")
	}
	if err := validateInstalledBootstrapDirectory(parentFD, uid, parentUserOwned, parentPrivate); err != nil {
		return -1, fmt.Errorf("unsafe parent directory: %w", err)
	}

	fd, err := unix.Openat(parentFD, name, directoryFlags, 0)
	created := false
	if errors.Is(err, unix.ENOENT) {
		// Revalidate the pinned parent immediately before creating anything.
		if err = validateInstalledBootstrapDirectory(parentFD, uid, parentUserOwned, parentPrivate); err != nil {
			return -1, fmt.Errorf("unsafe parent directory: %w", err)
		}
		if err = unix.Mkdirat(parentFD, name, 0700); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return -1, err
			}
		} else {
			created = true
		}
		fd, err = unix.Openat(parentFD, name, directoryFlags, 0)
	}
	if err != nil {
		return -1, err
	}

	if created {
		// A same-UID process can replace the name between mkdir and open.
		// Never chmod it: accept only an already private, user-owned inode.
		if err = validateInstalledBootstrapDirectory(fd, uid, true, true); err == nil {
			err = installedBootstrapDirectoryMatches(parentFD, name, fd)
		}
		if err == nil {
			err = validateInstalledBootstrapDirectory(parentFD, uid, parentUserOwned, parentPrivate)
		}
		if err == nil {
			err = validateInstalledBootstrapDirectory(fd, uid, userOwned, private)
		}
		if err == nil {
			err = installedBootstrapDirectoryMatches(parentFD, name, fd)
		}
	} else {
		err = validateInstalledBootstrapDirectory(fd, uid, userOwned, private)
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func installedBootstrapDirectoryMatches(parentFD int, name string, fd int) error {
	var held, named unix.Stat_t
	if err := unix.Fstat(fd, &held); err != nil {
		return err
	}
	if err := unix.Fstatat(parentFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFDIR || named.Mode&unix.S_IFMT != unix.S_IFDIR || held.Dev != named.Dev || held.Ino != named.Ino {
		return errors.New("directory identity changed")
	}
	return nil
}

func validateInstalledBootstrapDirectory(fd int, uid uint32, userOwned, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("not a directory")
	}
	if st.Mode&0022 != 0 {
		return errors.New("directory is writable by another user")
	}
	if userOwned {
		if st.Uid != uid {
			return errors.New("user directory has the wrong owner")
		}
	} else if st.Uid != 0 && st.Uid != uid {
		return errors.New("directory has an untrusted owner")
	}
	if private && (st.Uid != uid || st.Mode&0777 != 0700) {
		return errors.New("private directory must be owned by the user with mode 0700")
	}
	return nil
}
