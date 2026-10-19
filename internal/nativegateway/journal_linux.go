//go:build linux

package nativegateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	networkJournalDirectory = "/var/lib/local-vpn-kde-network"
	networkJournalFile      = "journal.json"
	networkJournalLock      = ".lock"

	maxNetworkJournalJSON = 64 * 1024
	journalLockPoll       = 10 * time.Millisecond
)

var (
	errFileJournalClosed        = errors.New("network journal is closed")
	errFileJournalUninitialized = errors.New("network journal is uninitialized")
)

// FileJournal is the durable JournalStore used by the privileged native
// network service. The directory and lock descriptors remain open for the
// lifetime of the object; every operation is therefore covered by the same
// inter-process mutation lock.
type FileJournal struct {
	mu          sync.RWMutex
	operations  chan struct{}
	dirfd       int
	lockfd      int
	expectedUID uint32
	closed      bool
	closeErr    error
}

var _ JournalStore = (*FileJournal)(nil)

// OpenNetworkJournal opens the fixed, root-owned native network journal. It
// does not accept a production path: the installed service has exactly one
// ownership journal location.
func OpenNetworkJournal(ctx context.Context) (*FileJournal, error) {
	ctx = journalContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("network journal requires effective UID 0")
	}
	return openNetworkJournalAt(ctx, networkJournalDirectory, 0, true)
}

// newFileJournalForTest is deliberately unexported. It is the test-only path
// seam for exercising the same descriptor and locking implementation without
// touching the installed root-owned location.
func newFileJournalForTest(ctx context.Context, directory string, expectedUID int) (*FileJournal, error) {
	uid, err := journalUID(expectedUID)
	if err != nil {
		return nil, err
	}
	return openNetworkJournalAtUID(ctx, directory, uid, false)
}

func openNetworkJournalAt(ctx context.Context, directory string, expectedUID int, create bool) (*FileJournal, error) {
	uid, err := journalUID(expectedUID)
	if err != nil {
		return nil, err
	}
	return openNetworkJournalAtUID(ctx, directory, uid, create)
}

func openNetworkJournalAtUID(ctx context.Context, directory string, expectedUID uint32, create bool) (*FileJournal, error) {
	ctx = journalContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dirfd, err := openJournalDirectory(ctx, directory, expectedUID, create)
	if err != nil {
		return nil, fmt.Errorf("open network journal directory: %w", err)
	}
	lockfd, err := openJournalLock(ctx, dirfd, expectedUID)
	if err != nil {
		_ = unix.Close(dirfd)
		return nil, fmt.Errorf("open network journal lock: %w", err)
	}
	return &FileJournal{
		operations:  make(chan struct{}, 1),
		dirfd:       dirfd,
		lockfd:      lockfd,
		expectedUID: expectedUID,
	}, nil
}

// Close releases the exclusive mutation lock and both held descriptors. It is
// safe to call more than once; a close waits for an operation already in
// progress before releasing the lock.
func (j *FileJournal) Close() error {
	if j == nil {
		return nil
	}
	if j.operations == nil {
		return nil
	}

	j.mu.Lock()
	if j.closed {
		err := j.closeErr
		j.mu.Unlock()
		return err
	}
	j.closed = true
	lockfd, dirfd := j.lockfd, j.dirfd
	j.lockfd, j.dirfd = -1, -1
	var closeErr error
	if lockfd >= 0 {
		if err := unix.Flock(lockfd, unix.LOCK_UN); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
		if err := unix.Close(lockfd); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if dirfd >= 0 {
		if err := unix.Close(dirfd); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	j.closeErr = closeErr
	j.mu.Unlock()
	return closeErr
}

func (j *FileJournal) beginOperation(ctx context.Context) (func(), error) {
	ctx = journalContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j == nil {
		return nil, errors.New("network journal is nil")
	}

	j.mu.RLock()
	if j.closed {
		j.mu.RUnlock()
		return nil, errFileJournalClosed
	}
	if j.operations == nil {
		j.mu.RUnlock()
		return nil, errFileJournalUninitialized
	}
	if err := validateJournalDirectory(j.dirfd, j.expectedUID); err != nil {
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal directory: %w", err)
	}
	if err := validateJournalLockFD(j.lockfd, j.expectedUID); err != nil {
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal lock: %w", err)
	}
	if err := verifyJournalPathIdentity(j.dirfd, networkJournalLock, j.lockfd); err != nil {
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal lock: %w", err)
	}
	select {
	case j.operations <- struct{}{}:
	case <-ctx.Done():
		j.mu.RUnlock()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-j.operations
		j.mu.RUnlock()
		return nil, err
	}
	if err := validateJournalDirectory(j.dirfd, j.expectedUID); err != nil {
		<-j.operations
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal directory: %w", err)
	}
	if err := validateJournalLockFD(j.lockfd, j.expectedUID); err != nil {
		<-j.operations
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal lock: %w", err)
	}
	if err := verifyJournalPathIdentity(j.dirfd, networkJournalLock, j.lockfd); err != nil {
		<-j.operations
		j.mu.RUnlock()
		return nil, fmt.Errorf("validate network journal lock: %w", err)
	}
	return func() {
		<-j.operations
		j.mu.RUnlock()
	}, nil
}

// Load implements JournalStore. A missing journal is not an error; every
// present journal must be a stable, private regular file containing exactly
// one known JSON object no larger than maxNetworkJournalJSON.
func (j *FileJournal) Load(ctx context.Context) (NetworkJournal, bool, error) {
	var zero NetworkJournal
	release, err := j.beginOperation(ctx)
	if err != nil {
		return zero, false, err
	}
	defer release()

	journal, exists, _, err := j.readJournal(ctx)
	return journal, exists, err
}

// Save implements JournalStore. It refuses to replace an unsafe or malformed
// existing journal, writes a random same-directory temporary inode, syncs the
// inode, atomically renames it, and syncs the containing directory.
func (j *FileJournal) Save(ctx context.Context, journal NetworkJournal) error {
	release, err := j.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = journalContext(ctx)

	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("marshal network journal: %w", err)
	}
	if len(data) == 0 || len(data) > maxNetworkJournalJSON {
		return errors.New("network journal JSON is too large")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Reading the old value before publishing prevents a malformed but
	// structurally safe file from being silently erased by a later Save.
	_, exists, before, err := j.readJournal(ctx)
	if err != nil {
		return fmt.Errorf("validate existing network journal: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	tempFD, tempName, err := createJournalTemp(ctx, j.dirfd, j.expectedUID)
	if err != nil {
		return fmt.Errorf("create network journal temporary file: %w", err)
	}
	removeTemp := true
	defer func() {
		if tempFD < 0 {
			return
		}
		if removeTemp {
			removeJournalTemp(j.dirfd, tempName, tempFD)
		}
		_ = unix.Close(tempFD)
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeJournalBytes(tempFD, data); err != nil {
		return fmt.Errorf("write network journal temporary file: %w", err)
	}
	if err := unix.Fchmod(tempFD, 0600); err != nil {
		return fmt.Errorf("set network journal temporary mode: %w", err)
	}
	if err := validateJournalFD(tempFD, j.expectedUID, int64(len(data))); err != nil {
		return fmt.Errorf("validate network journal temporary file: %w", err)
	}
	if err := unix.Fsync(tempFD); err != nil {
		return fmt.Errorf("sync network journal temporary file: %w", err)
	}
	if err := validateJournalFD(tempFD, j.expectedUID, int64(len(data))); err != nil {
		return fmt.Errorf("network journal temporary file changed: %w", err)
	}
	if err := verifyJournalPathIdentity(j.dirfd, tempName, tempFD); err != nil {
		return fmt.Errorf("network journal temporary path changed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Re-check the destination immediately before rename. The held lock makes
	// this a cooperative invariant; the check also prevents replacing a
	// symlink, hard link, or permission-unsafe file if one appeared meanwhile.
	after, afterExists, err := journalTargetStat(j.dirfd, networkJournalFile, j.expectedUID)
	if err != nil {
		return fmt.Errorf("validate network journal destination: %w", err)
	}
	if exists {
		if !afterExists || !sameJournalStat(before, after) {
			return errors.New("network journal changed during save")
		}
	} else if afterExists {
		return errors.New("network journal appeared during save")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat(j.dirfd, tempName, j.dirfd, networkJournalFile); err != nil {
		return fmt.Errorf("publish network journal: %w", err)
	}
	removeTemp = false
	if err := unix.Close(tempFD); err != nil {
		return fmt.Errorf("close network journal temporary file: %w", err)
	}
	tempFD = -1
	if err := unix.Fsync(j.dirfd); err != nil {
		return fmt.Errorf("sync network journal directory: %w", err)
	}

	published, err := statJournalPath(j.dirfd, networkJournalFile)
	if err != nil {
		return fmt.Errorf("verify published network journal: %w", err)
	}
	if err := validateJournalStat(&published, j.expectedUID, int64(len(data))); err != nil {
		return fmt.Errorf("verify published network journal: %w", err)
	}
	return nil
}

// Clear implements JournalStore. Missing state is already clear. A present
// state is first validated as a safe, valid JSON journal, then unlinked and
// the containing directory is synchronized before success is returned.
func (j *FileJournal) Clear(ctx context.Context) error {
	release, err := j.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = journalContext(ctx)

	_, exists, before, err := j.readJournal(ctx)
	if err != nil {
		return fmt.Errorf("validate network journal before clear: %w", err)
	}
	if !exists {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	after, stillExists, err := journalTargetStat(j.dirfd, networkJournalFile, j.expectedUID)
	if err != nil {
		return fmt.Errorf("validate network journal before unlink: %w", err)
	}
	if !stillExists || !sameJournalStat(before, after) {
		return errors.New("network journal changed during clear")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Unlinkat(j.dirfd, networkJournalFile, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("remove network journal: %w", err)
	}
	if err := unix.Fsync(j.dirfd); err != nil {
		return fmt.Errorf("sync network journal directory after clear: %w", err)
	}
	return nil
}

func (j *FileJournal) readJournal(ctx context.Context) (NetworkJournal, bool, unix.Stat_t, error) {
	var zero NetworkJournal
	if err := journalContext(ctx).Err(); err != nil {
		return zero, false, unix.Stat_t{}, err
	}

	fd, err := unix.Openat(j.dirfd, networkJournalFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return zero, false, unix.Stat_t{}, nil
	}
	if err != nil {
		return zero, false, unix.Stat_t{}, fmt.Errorf("open network journal: %w", err)
	}
	before := unix.Stat_t{}
	if err := unix.Fstat(fd, &before); err != nil {
		_ = unix.Close(fd)
		return zero, false, unix.Stat_t{}, fmt.Errorf("stat network journal: %w", err)
	}
	if err := validateJournalStat(&before, j.expectedUID, -1); err != nil {
		_ = unix.Close(fd)
		return zero, false, unix.Stat_t{}, err
	}
	if before.Size > maxNetworkJournalJSON {
		_ = unix.Close(fd)
		return zero, false, unix.Stat_t{}, errors.New("network journal is too large")
	}

	file := os.NewFile(uintptr(fd), networkJournalFile)
	if err := journalContext(ctx).Err(); err != nil {
		_ = file.Close()
		return zero, false, unix.Stat_t{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxNetworkJournalJSON+1))
	after := unix.Stat_t{}
	statErr := unix.Fstat(fd, &after)
	closeErr := file.Close()
	if readErr != nil {
		return zero, false, unix.Stat_t{}, fmt.Errorf("read network journal: %w", readErr)
	}
	if statErr != nil {
		return zero, false, unix.Stat_t{}, fmt.Errorf("stat network journal after read: %w", statErr)
	}
	if closeErr != nil {
		return zero, false, unix.Stat_t{}, fmt.Errorf("close network journal: %w", closeErr)
	}
	if err := journalContext(ctx).Err(); err != nil {
		return zero, false, unix.Stat_t{}, err
	}
	if err := validateJournalStat(&after, j.expectedUID, -1); err != nil {
		return zero, false, unix.Stat_t{}, errors.New("network journal changed during read")
	}
	if !sameJournalStat(before, after) || after.Size != int64(len(data)) || int64(len(data)) > maxNetworkJournalJSON {
		return zero, false, unix.Stat_t{}, errors.New("network journal changed during read")
	}
	pathStat, err := statJournalPath(j.dirfd, networkJournalFile)
	if err != nil || !sameJournalStat(after, pathStat) {
		if err == nil {
			err = errors.New("network journal path changed during read")
		}
		return zero, false, unix.Stat_t{}, err
	}

	journal, err := decodeNetworkJournal(data)
	if err != nil {
		return zero, false, unix.Stat_t{}, err
	}
	return journal, true, after, nil
}

func decodeNetworkJournal(data []byte) (NetworkJournal, error) {
	var journal NetworkJournal
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return journal, errors.New("network journal is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return journal, fmt.Errorf("decode network journal: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return journal, errors.New("network journal contains trailing JSON")
		}
		return journal, fmt.Errorf("network journal contains trailing data: %w", err)
	}
	return journal, nil
}

func createJournalTemp(ctx context.Context, dirfd int, expectedUID uint32) (int, string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		if err := journalContext(ctx).Err(); err != nil {
			return -1, "", err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return -1, "", err
		}
		name := "." + networkJournalFile + "." + hex.EncodeToString(random[:]) + ".tmp"
		fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return -1, "", err
		}
		if err := validateJournalFD(fd, expectedUID, 0); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(dirfd, name, 0)
			return -1, "", err
		}
		return fd, name, nil
	}
	return -1, "", errors.New("could not create unique network journal temporary file")
}

func writeJournalBytes(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func removeJournalTemp(dirfd int, name string, fd int) {
	var held, current unix.Stat_t
	if unix.Fstat(fd, &held) != nil || unix.Fstatat(dirfd, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return
	}
	if sameJournalStat(held, current) {
		_ = unix.Unlinkat(dirfd, name, 0)
	}
}

func journalTargetStat(dirfd int, name string, expectedUID uint32) (unix.Stat_t, bool, error) {
	stat, err := statJournalPath(dirfd, name)
	if errors.Is(err, unix.ENOENT) {
		return unix.Stat_t{}, false, nil
	}
	if err != nil {
		return unix.Stat_t{}, false, err
	}
	if err := validateJournalStat(&stat, expectedUID, -1); err != nil {
		return unix.Stat_t{}, false, err
	}
	return stat, true, nil
}

func statJournalPath(dirfd int, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	return stat, nil
}

func validateJournalFD(fd int, expectedUID uint32, expectedSize int64) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	return validateJournalStat(&stat, expectedUID, expectedSize)
}

func validateJournalLockFD(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	return validatePrivateRegularStat(&stat, expectedUID, -1, -1)
}

func validateJournalStat(stat *unix.Stat_t, expectedUID uint32, expectedSize int64) error {
	return validatePrivateRegularStat(stat, expectedUID, expectedSize, maxNetworkJournalJSON)
}

func validatePrivateRegularStat(stat *unix.Stat_t, expectedUID uint32, expectedSize, maxSize int64) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("network journal is not a regular file")
	}
	if stat.Uid != expectedUID {
		return errors.New("network journal has an unexpected owner")
	}
	if stat.Mode&07777 != 0600 {
		return errors.New("network journal is not mode 0600")
	}
	if stat.Nlink != 1 {
		return errors.New("network journal has multiple links")
	}
	if stat.Size < 0 || (maxSize >= 0 && stat.Size > maxSize) {
		return errors.New("network journal has an invalid size")
	}
	if expectedSize >= 0 && stat.Size != expectedSize {
		return errors.New("network journal has an unexpected size")
	}
	return nil
}

func sameJournalStat(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid && a.Nlink == b.Nlink && a.Size == b.Size
}

func openJournalLock(ctx context.Context, dirfd int, expectedUID uint32) (int, error) {
	fd, err := unix.Openat(dirfd, networkJournalLock, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return -1, err
	}
	if err := validateJournalLockFD(fd, expectedUID); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := verifyJournalPathIdentity(dirfd, networkJournalLock, fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}

	ticker := time.NewTicker(journalLockPoll)
	defer ticker.Stop()
	for {
		if err := journalContext(ctx).Err(); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if contextErr := journalContext(ctx).Err(); contextErr != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = unix.Close(fd)
				return -1, contextErr
			}
			if err := validateJournalLockFD(fd, expectedUID); err != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = unix.Close(fd)
				return -1, err
			}
			if err := verifyJournalPathIdentity(dirfd, networkJournalLock, fd); err != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = unix.Close(fd)
				return -1, err
			}
			return fd, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			_ = unix.Close(fd)
			return -1, err
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return -1, ctx.Err()
		case <-ticker.C:
		}
	}
}

func verifyJournalPathIdentity(dirfd int, name string, fd int) error {
	var held, current unix.Stat_t
	if err := unix.Fstat(fd, &held); err != nil {
		return err
	}
	if err := unix.Fstatat(dirfd, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Dev != current.Dev || held.Ino != current.Ino || held.Nlink != 1 || current.Nlink != 1 {
		return errors.New("network journal lock identity changed")
	}
	return nil
}

func openJournalDirectory(ctx context.Context, directory string, expectedUID uint32, createFinal bool) (int, error) {
	parts, err := journalPathParts(directory)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := validateJournalAncestor(fd, expectedUID); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	for index, part := range parts {
		if err := journalContext(ctx).Err(); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		final := index == len(parts)-1
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		created := false
		if errors.Is(openErr, unix.ENOENT) && final && createFinal {
			if err := journalContext(ctx).Err(); err != nil {
				_ = unix.Close(fd)
				return -1, err
			}
			if err := unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, err
			}
			if err := journalContext(ctx).Err(); err != nil {
				_ = unix.Close(fd)
				return -1, err
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			created = openErr == nil
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, openErr
		}
		if final {
			if err := validateJournalDirectory(next, expectedUID); err != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, err
			}
		} else if err := validateJournalAncestor(next, expectedUID); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, err
		}
		if created {
			if err := unix.Fsync(fd); err != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, err
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func journalPathParts(directory string) ([]string, error) {
	if !strings.HasPrefix(directory, "/") {
		return nil, errors.New("network journal directory must be absolute")
	}
	parts := make([]string, 0, 8)
	for _, part := range strings.Split(directory, "/") {
		if part == "" {
			continue
		}
		if part == "." || part == ".." || strings.Contains(part, "\x00") {
			return nil, errors.New("network journal directory is not canonical")
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, errors.New("network journal directory is invalid")
	}
	return parts, nil
}

func validateJournalAncestor(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("network journal path component is not a directory")
	}
	if stat.Uid != expectedUID && stat.Uid != 0 {
		return errors.New("network journal path component has an unexpected owner")
	}
	// A root-owned sticky temporary directory is safe for the private test
	// constructor (Go's TempDir uses /tmp); ordinary group/world-writable
	// ancestors are never accepted.
	if stat.Mode&0022 != 0 && !(stat.Uid == 0 && stat.Mode&01000 != 0) {
		return errors.New("network journal path component is writable by group or world")
	}
	return nil
}

func validateJournalDirectory(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("network journal directory is not a directory")
	}
	if stat.Uid != expectedUID {
		return errors.New("network journal directory has an unexpected owner")
	}
	if stat.Mode&07777 != 0700 {
		return errors.New("network journal directory is not mode 0700")
	}
	return nil
}

func journalUID(uid int) (uint32, error) {
	if uid < 0 || uint64(uid) > uint64(^uint32(0)) {
		return 0, errors.New("invalid expected journal UID")
	}
	return uint32(uid), nil
}

func journalContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
