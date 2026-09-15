package localvpn

import (
	"errors"
	"sort"

	"golang.org/x/sys/unix"
)

// writeBundle publishes a small set of related private files under the caller's
// operation lock. All versions are staged first, and a failed publication
// restores only entries still pointing at this transaction's candidate inode.
func writeBundle(dir int, values map[string][]byte) error {
	return writeBundleChecked(dir, values, nil)
}
func writeBundleChecked(dir int, values map[string][]byte, afterPublish func() error) error {
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
		previous, err := readAt(dir, name)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		if err == nil {
			c.old, err = stagePrivate(dir, previous, 0600)
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
	for _, c := range changes {
		var err error
		if c.new == nil {
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
	if afterPublish != nil {
		publishError = afterPublish()
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
