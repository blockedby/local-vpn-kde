package localvpn

import (
	"errors"
	"io"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

// writeBundle publishes a small set of related private files under the caller's
// operation lock. All versions are staged first, and a failed publication
// restores only entries still pointing at this transaction's candidate inode.
func writeBundle(dir int, values map[string][]byte) error {
	return writeBundleChecked(dir, values, bundleHooks{})
}

type bundleHooks struct{ beforePublish, afterPublish func() error }

func bundleSnapshot(dir int, name string) ([]byte, *unix.Stat_t, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	stat, err := privateInode(fd)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
	if err != nil || len(data) > 16*1024*1024 {
		return nil, nil, errors.New("private bundle snapshot failed")
	}
	if !bundleOriginalMatches(dir, name, &stat) {
		return nil, nil, errors.New("private bundle changed while reading")
	}
	return data, &stat, nil
}

func bundleOriginalMatches(dir int, name string, previous *unix.Stat_t) bool {
	var current unix.Stat_t
	err := unix.Fstatat(dir, name, &current, unix.AT_SYMLINK_NOFOLLOW)
	if previous == nil {
		return errors.Is(err, unix.ENOENT)
	}
	return err == nil && current.Dev == previous.Dev && current.Ino == previous.Ino &&
		current.Size == previous.Size && current.Mode == previous.Mode && current.Uid == previous.Uid &&
		current.Nlink == 1 && current.Mtim == previous.Mtim && current.Ctim == previous.Ctim
}

func writeBundleChecked(dir int, values map[string][]byte, hooks bundleHooks) error {
	names := make([]string, 0, len(values))
	for name := range values {
		if !component(name) {
			return errors.New("invalid private bundle name")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	type change struct {
		name      string
		old, new  *privateStage
		original  *unix.Stat_t
		installed bool
	}
	changes := []*change{}
	defer func() {
		for _, c := range changes {
			if c.old != nil {
				c.old.close()
			}
			if c.new != nil {
				c.new.close()
			}
		}
	}()
	for _, name := range names {
		c := &change{name: name}
		changes = append(changes, c)
		previous, original, err := bundleSnapshot(dir, name)
		if err != nil {
			return err
		}
		c.original = original
		if original != nil {
			c.old, err = stagePrivate(dir, previous, original.Mode&0777)
			if err != nil {
				return err
			}
		}
		if values[name] != nil {
			c.new, err = stagePrivate(dir, values[name], 0600)
			if err != nil {
				return err
			}
		}
	}
	rollback := func() error {
		for i := len(changes) - 1; i >= 0; i-- {
			c := changes[i]
			if !c.installed {
				continue
			}
			if c.new != nil && !c.new.matches(c.name) {
				return errors.New("private bundle rollback identity changed")
			}
			if c.new == nil {
				var current unix.Stat_t
				if e := unix.Fstatat(dir, c.name, &current, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(e, unix.ENOENT) {
					return errors.New("private bundle removal changed during rollback")
				}
			}
			if c.old == nil {
				if err := unix.Unlinkat(dir, c.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
					return err
				}
			} else {
				if !c.old.matches(c.old.name) {
					return errors.New("private rollback stage changed")
				}
				if err := unix.Renameat(dir, c.old.name, dir, c.name); err != nil {
					return err
				}
			}
		}
		return unix.Fsync(dir)
	}
	if hooks.beforePublish != nil {
		if err := hooks.beforePublish(); err != nil {
			return err
		}
	}
	for _, c := range changes {
		var err error
		if !bundleOriginalMatches(dir, c.name, c.original) {
			err = errors.New("private bundle changed before publication")
		} else if c.new == nil {
			err = unix.Unlinkat(dir, c.name, 0)
			if errors.Is(err, unix.ENOENT) {
				err = nil
			}
		} else if !c.new.matches(c.new.name) {
			err = errors.New("private bundle candidate changed")
		} else {
			err = unix.Renameat(dir, c.new.name, dir, c.name)
		}
		if err != nil {
			if e := rollback(); e != nil {
				return errors.New("private bundle rollback incomplete")
			}
			return err
		}
		c.installed = true
	}
	var publishError error
	if hooks.afterPublish != nil {
		publishError = hooks.afterPublish()
	}
	if publishError == nil {
		publishError = unix.Fsync(dir)
	}
	if err := publishError; err != nil {
		if e := rollback(); e != nil {
			return errors.New("private bundle rollback incomplete")
		}
		return err
	}
	for _, c := range changes {
		if c.new != nil && !c.new.matches(c.name) {
			return errors.New("private bundle changed after publication")
		}
	}
	return nil
}
