package localvpn

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxSubscription = 16384

func subscriptionParent(base string) (int, error) {
	root, err := directory(base, false, true)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	if err = privateDirectory(root); err != nil {
		return -1, err
	}
	parent, err := childDirectory(root, "vibe-vpn", false, true)
	if err != nil {
		return -1, err
	}
	if err = privateDirectory(parent); err != nil {
		unix.Close(parent)
		return -1, err
	}
	return parent, nil
}

func subscriptionFile(parent int) (*os.File, *unix.Stat_t, error) {
	fd, err := unix.Openat(parent, "sub_url", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), "sub_url")
	if err = validatePrivateFile(fd); err != nil {
		f.Close()
		return nil, nil, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, &stat, nil
}

func SubscriptionConfigured(base string) bool {
	parent, err := subscriptionParent(base)
	if err != nil {
		return false
	}
	defer unix.Close(parent)
	file, stat, err := subscriptionFile(parent)
	if file != nil {
		defer file.Close()
	}
	return err == nil && stat != nil && stat.Size > 0
}

func ReadSubscription(base string) (string, error) {
	parent, err := subscriptionParent(base)
	if err != nil {
		return "", err
	}
	defer unix.Close(parent)
	file, _, err := subscriptionFile(parent)
	if err != nil {
		return "", err
	}
	if file == nil {
		return "", nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSubscription+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxSubscription || !utf8.Valid(data) {
		return "", errors.New("invalid subscription")
	}
	return strings.TrimSpace(string(data)), nil
}

type privateStage struct {
	parent int
	name   string
	file   *os.File
}

func stagePrivate(parent int, data []byte, mode uint32) (*privateStage, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	name := ".subscription-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	stage := &privateStage{parent: parent, name: name, file: os.NewFile(uintptr(fd), name)}
	if _, err = stage.file.Write(data); err == nil {
		err = stage.file.Chmod(os.FileMode(mode))
	}
	if err == nil {
		err = stage.file.Sync()
	}
	if err == nil {
		err = validatePrivateFile(fd)
	}
	if err != nil {
		stage.close()
		return nil, err
	}
	return stage, nil
}
func (s *privateStage) matches(name string) bool {
	var held, actual unix.Stat_t
	return unix.Fstat(int(s.file.Fd()), &held) == nil && unix.Fstatat(s.parent, name, &actual, unix.AT_SYMLINK_NOFOLLOW) == nil && actual.Dev == held.Dev && actual.Ino == held.Ino && actual.Nlink == 1 && actual.Mode == held.Mode
}
func (s *privateStage) close() {
	if s.matches(s.name) {
		_ = unix.Unlinkat(s.parent, s.name, 0)
	}
	s.file.Close()
}

// WriteSubscription holds the persistent sibling lock through publication,
// durability checks, rollback, and stage cleanup. postRename is a private test
// seam used to prove compensation after a reported directory fsync failure.
func WriteSubscription(base, value string) error { return writeSubscription(base, value, nil) }
func writeSubscription(base, value string, postRename func() error) error {
	if len(value) > maxSubscription || strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("invalid subscription")
	}
	parent, err := subscriptionParent(base)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	lock, err := unix.Openat(parent, ".vibe-vpn.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(lock)
	if err = validatePrivateFile(lock); err != nil {
		return err
	}
	if err = unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("subscription busy")
	}
	defer unix.Flock(lock, unix.LOCK_UN)
	original, stat, err := subscriptionFile(parent)
	if err != nil {
		return err
	}
	var previous []byte
	mode := uint32(0600)
	if original != nil {
		defer original.Close()
		previous, err = io.ReadAll(io.LimitReader(original, maxSubscription+1))
		if err != nil {
			return err
		}
		if len(previous) > maxSubscription {
			return errors.New("subscription too large")
		}
		mode = stat.Mode & 0777
	}
	backup, err := stagePrivate(parent, previous, mode)
	if err != nil {
		return err
	}
	defer backup.close()
	stage, err := stagePrivate(parent, []byte(value), 0600)
	if err != nil {
		return err
	}
	defer stage.close()
	current, currentStat, err := subscriptionFile(parent)
	if current != nil {
		current.Close()
	}
	if err != nil {
		return err
	}
	if (stat == nil) != (currentStat == nil) {
		return errors.New("subscription changed")
	}
	if stat != nil && (stat.Ino != currentStat.Ino || stat.Dev != currentStat.Dev || stat.Size != currentStat.Size || stat.Mtim != currentStat.Mtim || stat.Ctim != currentStat.Ctim || stat.Mode != currentStat.Mode) {
		return errors.New("subscription changed")
	}
	if !stage.matches(stage.name) || !backup.matches(backup.name) {
		return errors.New("subscription staging changed")
	}
	if err = unix.Renameat(parent, stage.name, parent, "sub_url"); err != nil {
		return err
	}
	if postRename != nil {
		err = postRename()
	}
	if err == nil {
		err = unix.Fsync(parent)
	}
	if err == nil && !stage.matches("sub_url") {
		err = errors.New("subscription changed during publication")
	}
	if err == nil {
		return nil
	}
	// Never compensate over a third party's replacement.
	if !stage.matches("sub_url") {
		return errors.New("subscription rollback incomplete")
	}
	if stat == nil {
		err = unix.Unlinkat(parent, "sub_url", 0)
	} else {
		if !backup.matches(backup.name) {
			return errors.New("subscription rollback incomplete")
		}
		err = unix.Renameat(parent, backup.name, parent, "sub_url")
	}
	if err != nil {
		return errors.New("subscription rollback incomplete")
	}
	if err = unix.Fsync(parent); err != nil {
		return errors.New("subscription rollback incomplete")
	}
	return errors.New("subscription update failed; prior state restored")
}
