//go:build linux

package localvpn

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativeProfileTransactionReady      = "READY\n"
	nativeProfileTransactionCommitted  = "COMMITTED\n"
	nativeProfileTransactionRolledBack = "ROLLED_BACK\n"
	nativeProfileTransactionFailed     = "FAILED\n"
	nativeProfileStatePathMaxSize      = 4096
)

type nativeProfileTransaction interface {
	Commit() error
	Rollback() error
}

type nativeProfileFileSnapshot struct {
	dev, ino                      uint64
	size                          int64
	mode                          uint32
	uid, gid                      uint32
	nlink                         uint64
	mtimeSec, mtimeNsec           int64
	changeTimeSec, changeTimeNsec int64
	contentSum                    [sha256.Size]byte
}

type nativeProfileFileTransaction struct {
	rootFD, stateFD, openvpnFD, parentFD int
	oldFD, newFD                         int
	oldName                              string
	statePath                            string
	ownerUID                             uint32
	oldSnapshot, newSnapshot             nativeProfileFileSnapshot
	hadOld                               bool
	createdOpenVPN, createdClient        bool
	finished                             bool
}

func beginNativeProfileFileTransaction(statePath string, profile []byte, filesystemRoot string, ownerUID, ownerGID uint32) (*nativeProfileFileTransaction, error) {
	if ownerUID == 0 || uint32(os.Geteuid()) != ownerUID || uint32(os.Getegid()) != ownerGID {
		return nil, errors.New("native profile transaction requires the desktop identity")
	}
	if err := validateNativeClientStatePath(statePath); err != nil || len(statePath) > nativeProfileStatePathMaxSize {
		return nil, errors.New("native profile transaction state path is invalid")
	}
	if err := validateNativeClientProfile(profile); err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(filesystemRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("native profile transaction root is unavailable")
	}
	if err := validateNativeProfileAncestor(rootFD, ownerUID); err != nil {
		unix.Close(rootFD)
		return nil, errors.New("native profile transaction root is unsafe")
	}
	stateFD, err := openNativeProfileState(rootFD, statePath, ownerUID)
	if err != nil {
		unix.Close(rootFD)
		return nil, err
	}
	tx := &nativeProfileFileTransaction{rootFD: rootFD, stateFD: stateFD, openvpnFD: -1, parentFD: -1, oldFD: -1, newFD: -1, statePath: statePath, ownerUID: ownerUID}
	fail := func(err error) (*nativeProfileFileTransaction, error) {
		// Never remove a newly created directory merely because its old parent FD
		// still works: its canonical name must still identify the pinned directory.
		if cleanupErr := tx.cleanupCreatedDirectories(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		tx.close()
		return nil, err
	}

	tx.openvpnFD, tx.createdOpenVPN, err = openOrCreateNativeProfileChildTracked(stateFD, "openvpn", ownerUID, ownerGID)
	if err != nil {
		return fail(err)
	}
	tx.parentFD, tx.createdClient, err = openOrCreateNativeProfileChildTracked(tx.openvpnFD, "client", ownerUID, ownerGID)
	if err != nil {
		return fail(err)
	}
	tx.oldFD, tx.oldSnapshot, tx.hadOld, err = openNativeProfileFileSnapshot(tx.parentFD, nativeClientProfileName, ownerUID)
	if err != nil {
		return fail(err)
	}

	tempFD, tempName, snapshot, err := createNativeProfileTemp(tx.parentFD, ownerUID, ownerGID, profile)
	if err != nil {
		return fail(err)
	}
	tx.newFD, tx.newSnapshot = tempFD, snapshot
	if tx.hadOld {
		if err = unix.Renameat2(tx.parentFD, tempName, tx.parentFD, nativeClientProfileName, unix.RENAME_EXCHANGE); err != nil {
			unlinkNativeProfileIfSame(tx.parentFD, tempName, tempFD)
			return fail(errors.New("native profile transaction atomic stage failed"))
		}
		tx.oldName = tempName
		if !nativeProfileNameMatchesFD(tx.parentFD, tx.oldName, tx.oldFD) || !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) {
			if nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) && nativeProfileNameMatchesFD(tx.parentFD, tx.oldName, tx.oldFD) {
				_ = unix.Renameat2(tx.parentFD, tx.oldName, tx.parentFD, nativeClientProfileName, unix.RENAME_EXCHANGE)
			}
			return fail(errors.New("native profile transaction target changed concurrently"))
		}
	} else {
		if err = unix.Renameat2(tx.parentFD, tempName, tx.parentFD, nativeClientProfileName, unix.RENAME_NOREPLACE); err != nil {
			unlinkNativeProfileIfSame(tx.parentFD, tempName, tempFD)
			return fail(errors.New("native profile transaction atomic stage failed"))
		}
	}
	if err := unix.Fsync(tx.parentFD); err != nil {
		_ = tx.Rollback()
		tx.close()
		return nil, errors.New("native profile transaction staging sync failed")
	}
	if !tx.hierarchyMatches() || !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) || !nativeProfileSnapshotMatches(tx.newFD, tx.newSnapshot) {
		_ = tx.Rollback()
		tx.close()
		return nil, errors.New("native profile transaction staged state is uncertain")
	}
	return tx, nil
}

func openOrCreateNativeProfileChildTracked(parentFD int, name string, ownerUID, ownerGID uint32) (int, bool, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		if validateNativeProfilePrivateDirectory(fd, ownerUID) != nil {
			unix.Close(fd)
			return -1, false, errors.New("existing native profile directory is unsafe")
		}
		return fd, false, nil
	}
	if !errors.Is(err, unix.ENOENT) || unix.Mkdirat(parentFD, name, 0700) != nil {
		return -1, false, errors.New("native profile directory creation failed")
	}
	fd, err = unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil || validateNativeProfilePrivateDirectory(fd, ownerUID) != nil {
		if fd >= 0 {
			unlinkNativeProfileDirectoryIfSame(parentFD, name, fd)
			unix.Close(fd)
		}
		return -1, false, errors.New("created native profile directory is unsafe")
	}
	if err := unix.Fsync(parentFD); err != nil {
		unlinkNativeProfileDirectoryIfSame(parentFD, name, fd)
		unix.Close(fd)
		return -1, false, errors.New("native profile parent sync failed")
	}
	return fd, true, nil
}

func createNativeProfileTemp(parentFD int, ownerUID, ownerGID uint32, profile []byte) (int, string, nativeProfileFileSnapshot, error) {
	var empty nativeProfileFileSnapshot
	var random [16]byte
	if _, err := io.ReadFull(randReader{}, random[:]); err != nil {
		return -1, "", empty, errors.New("native profile temporary name generation failed")
	}
	name := ".vpnkit-profile-" + encodeNativeProfileRandom(random[:]) + ".tmp"
	fd, err := unix.Openat(parentFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return -1, "", empty, errors.New("native profile temporary file creation failed")
	}
	if err := writeAllNativeProfile(fd, profile); err != nil || unix.Fchown(fd, int(ownerUID), int(ownerGID)) != nil || unix.Fchmod(fd, 0600) != nil || unix.Fsync(fd) != nil || validateNativeProfileFileFD(fd, ownerUID) != nil {
		unlinkNativeProfileIfSame(parentFD, name, fd)
		unix.Close(fd)
		return -1, "", empty, errors.New("native profile temporary file preparation failed")
	}
	snapshot, err := snapshotNativeProfileFile(fd)
	if err != nil {
		unlinkNativeProfileIfSame(parentFD, name, fd)
		unix.Close(fd)
		return -1, "", empty, errors.New("native profile temporary file snapshot failed")
	}
	return fd, name, snapshot, nil
}

// Small adapters keep randomness formatting shared with the delivery implementation.
type randReader struct{}

func (randReader) Read(p []byte) (int, error) { return unix.Getrandom(p, 0) }
func encodeNativeProfileRandom(p []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(p)*2)
	for i, b := range p {
		out[2*i], out[2*i+1] = digits[b>>4], digits[b&15]
	}
	return string(out)
}

func openNativeProfileFileSnapshot(parentFD int, name string, ownerUID uint32) (int, nativeProfileFileSnapshot, bool, error) {
	var empty nativeProfileFileSnapshot
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, empty, false, nil
	}
	if err != nil || validateNativeProfileFileFD(fd, ownerUID) != nil || !nativeProfileNameMatchesFD(parentFD, name, fd) {
		if fd >= 0 {
			unix.Close(fd)
		}
		return -1, empty, false, errors.New("existing native profile is unsafe")
	}
	snapshot, err := snapshotNativeProfileFile(fd)
	if err != nil {
		unix.Close(fd)
		return -1, empty, false, errors.New("existing native profile snapshot failed")
	}
	return fd, snapshot, true, nil
}

func snapshotNativeProfileFile(fd int) (nativeProfileFileSnapshot, error) {
	var before, after unix.Stat_t
	var snapshot nativeProfileFileSnapshot
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size < 0 || before.Size > nativeClientProfileMaxSize {
		return snapshot, errors.New("native profile snapshot metadata is invalid")
	}
	content := make([]byte, int(before.Size))
	for offset := 0; offset < len(content); {
		n, err := unix.Pread(fd, content[offset:], int64(offset))
		if err != nil || n == 0 {
			return snapshot, errors.New("native profile snapshot read failed")
		}
		offset += n
	}
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mode != after.Mode || before.Uid != after.Uid || before.Gid != after.Gid || before.Nlink != after.Nlink || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return snapshot, errors.New("native profile changed while snapshotting")
	}
	snapshot = nativeProfileFileSnapshot{
		dev: uint64(before.Dev), ino: uint64(before.Ino), size: before.Size, mode: before.Mode,
		uid: before.Uid, gid: before.Gid, nlink: uint64(before.Nlink),
		mtimeSec: before.Mtim.Sec, mtimeNsec: before.Mtim.Nsec,
		changeTimeSec: before.Ctim.Sec, changeTimeNsec: before.Ctim.Nsec,
		contentSum: sha256.Sum256(content),
	}
	return snapshot, nil
}

func nativeProfileSnapshotMatches(fd int, expected nativeProfileFileSnapshot) bool {
	actual, err := snapshotNativeProfileFile(fd)
	// Rename/exchange can update inode timestamps, so compare the durable
	// contract fields rather than timestamps. Timestamps are still checked
	// around each bounded read to reject a file changing while it is hashed.
	return err == nil && actual.dev == expected.dev && actual.ino == expected.ino &&
		actual.size == expected.size && actual.mode == expected.mode &&
		actual.uid == expected.uid && actual.gid == expected.gid &&
		actual.nlink == expected.nlink && actual.contentSum == expected.contentSum
}

func nativeProfileDirectoryNameMatchesFD(parentFD int, name string, fd int, ownerUID uint32) bool {
	var named unix.Stat_t
	return nativeProfileNameHasIdentity(parentFD, name, fd, &named) &&
		named.Mode&unix.S_IFMT == unix.S_IFDIR && validateNativeProfilePrivateDirectory(fd, ownerUID) == nil
}

func unlinkNativeProfileDirectoryIfSame(parentFD int, name string, fd int) {
	var named unix.Stat_t
	if nativeProfileNameHasIdentity(parentFD, name, fd, &named) && named.Mode&unix.S_IFMT == unix.S_IFDIR {
		_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
	}
}

func (tx *nativeProfileFileTransaction) stateNameMatchesFD() bool {
	fd, err := openNativeProfileState(tx.rootFD, tx.statePath, tx.ownerUID)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var named, held unix.Stat_t
	return unix.Fstat(fd, &named) == nil && unix.Fstat(tx.stateFD, &held) == nil && named.Dev == held.Dev && named.Ino == held.Ino
}

func (tx *nativeProfileFileTransaction) hierarchyMatches() bool {
	return tx.stateNameMatchesFD() &&
		nativeProfileDirectoryNameMatchesFD(tx.stateFD, "openvpn", tx.openvpnFD, tx.ownerUID) &&
		nativeProfileDirectoryNameMatchesFD(tx.openvpnFD, "client", tx.parentFD, tx.ownerUID)
}

func (tx *nativeProfileFileTransaction) cleanupCreatedDirectories() error {
	if tx.createdClient {
		if !tx.hierarchyMatches() {
			return errors.New("native profile transaction directory cleanup identity is uncertain")
		}
		if err := unix.Unlinkat(tx.openvpnFD, "client", unix.AT_REMOVEDIR); err != nil || unix.Fsync(tx.openvpnFD) != nil {
			return errors.New("native profile transaction client directory cleanup failed")
		}
	}
	if tx.createdOpenVPN {
		if !tx.stateNameMatchesFD() || !nativeProfileDirectoryNameMatchesFD(tx.stateFD, "openvpn", tx.openvpnFD, tx.ownerUID) {
			return errors.New("native profile transaction directory cleanup identity is uncertain")
		}
		if err := unix.Unlinkat(tx.stateFD, "openvpn", unix.AT_REMOVEDIR); err != nil || unix.Fsync(tx.stateFD) != nil {
			return errors.New("native profile transaction openvpn directory cleanup failed")
		}
	}
	return nil
}

func (tx *nativeProfileFileTransaction) Commit() error {
	if tx.finished {
		return errors.New("native profile transaction already finished")
	}
	if !tx.hierarchyMatches() {
		return errors.New("native profile transaction directory hierarchy changed concurrently")
	}
	if !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) || !nativeProfileSnapshotMatches(tx.newFD, tx.newSnapshot) {
		return errors.New("native profile transaction destination changed concurrently")
	}
	if tx.hadOld {
		if !nativeProfileNameMatchesFD(tx.parentFD, tx.oldName, tx.oldFD) || !nativeProfileSnapshotMatches(tx.oldFD, tx.oldSnapshot) {
			return errors.New("native profile transaction snapshot changed concurrently")
		}
		if err := unix.Unlinkat(tx.parentFD, tx.oldName, 0); err != nil {
			return errors.New("native profile transaction snapshot cleanup failed")
		}
	}
	if err := unix.Fsync(tx.parentFD); err != nil {
		return errors.New("native profile transaction commit sync failed")
	}
	if !tx.hierarchyMatches() || !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) || !nativeProfileSnapshotMatches(tx.newFD, tx.newSnapshot) {
		return errors.New("native profile transaction committed state is uncertain")
	}
	tx.finished = true
	tx.close()
	return nil
}

func (tx *nativeProfileFileTransaction) Rollback() error {
	if tx.finished {
		return errors.New("native profile transaction already finished")
	}
	if !tx.hierarchyMatches() {
		return errors.New("native profile transaction directory hierarchy changed concurrently")
	}
	if !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.newFD) || !nativeProfileSnapshotMatches(tx.newFD, tx.newSnapshot) {
		return errors.New("native profile transaction refuses to overwrite a changed destination")
	}
	if tx.hadOld {
		if !nativeProfileNameMatchesFD(tx.parentFD, tx.oldName, tx.oldFD) || !nativeProfileSnapshotMatches(tx.oldFD, tx.oldSnapshot) {
			return errors.New("native profile transaction refuses an altered snapshot")
		}
		if err := unix.Renameat2(tx.parentFD, tx.oldName, tx.parentFD, nativeClientProfileName, unix.RENAME_EXCHANGE); err != nil {
			return errors.New("native profile transaction rollback exchange failed")
		}
		if !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.oldFD) || !nativeProfileSnapshotMatches(tx.oldFD, tx.oldSnapshot) ||
			!nativeProfileNameMatchesFD(tx.parentFD, tx.oldName, tx.newFD) || !nativeProfileSnapshotMatches(tx.newFD, tx.newSnapshot) {
			return errors.New("native profile transaction rollback state is uncertain")
		}
		if err := unix.Unlinkat(tx.parentFD, tx.oldName, 0); err != nil {
			return errors.New("native profile transaction rollback cleanup failed")
		}
	} else if err := unix.Unlinkat(tx.parentFD, nativeClientProfileName, 0); err != nil {
		return errors.New("native profile transaction rollback removal failed")
	}
	if err := unix.Fsync(tx.parentFD); err != nil {
		return errors.New("native profile transaction rollback sync failed")
	}
	if !tx.hierarchyMatches() {
		return errors.New("native profile transaction rolled-back hierarchy is uncertain")
	}
	if tx.hadOld {
		if !nativeProfileNameMatchesFD(tx.parentFD, nativeClientProfileName, tx.oldFD) || !nativeProfileSnapshotMatches(tx.oldFD, tx.oldSnapshot) {
			return errors.New("native profile transaction restored state is uncertain")
		}
	} else {
		var st unix.Stat_t
		if err := unix.Fstatat(tx.parentFD, nativeClientProfileName, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
			return errors.New("native profile transaction rollback destination is uncertain")
		}
	}
	if err := tx.cleanupCreatedDirectories(); err != nil {
		return err
	}
	tx.finished = true
	tx.close()
	return nil
}

func (tx *nativeProfileFileTransaction) close() {
	for _, fd := range []int{tx.oldFD, tx.newFD, tx.parentFD, tx.openvpnFD, tx.stateFD, tx.rootFD} {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
	tx.oldFD, tx.newFD, tx.parentFD, tx.openvpnFD, tx.stateFD, tx.rootFD = -1, -1, -1, -1, -1, -1
}

// RunNativeProfileTransaction is the fixed unprivileged CLI endpoint. It accepts
// no command-line options. stdin is a bounded binary request followed by one
// commit ('C') or rollback ('R') byte; stdout contains coarse protocol tokens only.
func RunNativeProfileTransaction(ctx context.Context, input io.Reader, output io.Writer) error {
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopSignals()
	if os.Geteuid() == 0 {
		return errors.New("native profile transaction refuses root")
	}
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0) != nil || os.Getppid() != parent {
		return errors.New("native profile transaction parent disappeared")
	}
	type request struct {
		reader  *bufio.Reader
		path    string
		profile []byte
		err     error
	}
	requests := make(chan request, 1)
	go func() {
		result := request{reader: bufio.NewReaderSize(input, 32<<10)}
		pathLength, err := readNativeProfileUint32(result.reader)
		if err != nil || pathLength == 0 || pathLength > nativeProfileStatePathMaxSize {
			result.err = errors.New("invalid native profile transaction request")
			requests <- result
			return
		}
		pathBytes := make([]byte, pathLength)
		if _, err = io.ReadFull(result.reader, pathBytes); err != nil {
			result.err = errors.New("invalid native profile transaction request")
			requests <- result
			return
		}
		profileLength, err := readNativeProfileUint32(result.reader)
		if err != nil || profileLength == 0 || profileLength > nativeClientProfileMaxSize {
			result.err = errors.New("invalid native profile transaction request")
			requests <- result
			return
		}
		result.path = string(pathBytes)
		result.profile = make([]byte, profileLength)
		if _, err = io.ReadFull(result.reader, result.profile); err != nil {
			result.err = errors.New("invalid native profile transaction request")
		}
		requests <- result
	}()
	var received request
	select {
	case received = <-requests:
	case <-ctx.Done():
		return errors.New("native profile transaction canceled")
	case <-time.After(10 * time.Second):
		return errors.New("native profile transaction input timed out")
	}
	if received.err != nil {
		return received.err
	}
	reader, profile := received.reader, received.profile
	tx, err := beginNativeProfileFileTransaction(received.path, profile, "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	for i := range profile {
		profile[i] = 0
	}
	if err != nil {
		_, _ = io.WriteString(output, nativeProfileTransactionFailed)
		return errors.New("native profile transaction staging failed")
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback()
		}
		// Rollback deliberately fails closed on uncertain state. Closing here is
		// unconditional so every helper error path still releases pinned FDs.
		tx.close()
	}()
	if _, err = io.WriteString(output, nativeProfileTransactionReady); err != nil {
		return errors.New("native profile transaction acknowledgment failed")
	}
	command := make(chan byte, 1)
	go func() {
		b, readErr := reader.ReadByte()
		if readErr == nil {
			command <- b
		}
		close(command)
	}()
	var action byte
	select {
	case action = <-command:
	case <-ctx.Done():
		return errors.New("native profile transaction canceled")
	case <-time.After(10 * time.Minute):
		return errors.New("native profile transaction timed out")
	}
	if action == 'C' {
		err = tx.Commit()
	} else if action == 'R' {
		err = tx.Rollback()
	} else {
		return errors.New("invalid native profile transaction command")
	}
	if err != nil {
		_, _ = io.WriteString(output, nativeProfileTransactionFailed)
		return err
	}
	finished = true
	token := nativeProfileTransactionRolledBack
	if action == 'C' {
		token = nativeProfileTransactionCommitted
	}
	if _, err := io.WriteString(output, token); err != nil {
		return errors.New("native profile transaction final acknowledgment failed")
	}
	return nil
}

func readNativeProfileUint32(r io.Reader) (uint32, error) {
	var b [4]byte
	_, err := io.ReadFull(r, b[:])
	return binary.BigEndian.Uint32(b[:]), err
}
func writeNativeProfileUint32(w io.Writer, value uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], value)
	_, err := w.Write(b[:])
	return err
}

type nativeProfileChildTransaction struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	done    bool
}

func startNativeProfileTransaction(ctx context.Context, uid, gid uint32, statePath string, profile []byte) (nativeProfileTransaction, error) {
	command := exec.CommandContext(ctx, "/usr/bin/setpriv", "--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--reuid", strconv.FormatUint(uint64(uid), 10), "--regid", strconv.FormatUint(uint64(gid), 10), "--clear-groups", "--", InstalledCLI, "native-profile-transaction")
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/", "LANG=C.UTF-8"}
	command.Dir = "/"
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 5 * time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("native profile transaction pipe unavailable")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, errors.New("native profile transaction pipe unavailable")
	}
	if err = command.Start(); err != nil {
		stdin.Close()
		return nil, errors.New("native profile transaction could not start")
	}
	tx := &nativeProfileChildTransaction{command: command, input: stdin, output: bufio.NewReaderSize(stdout, 64)}
	failed := true
	defer func() {
		if failed {
			_ = tx.abort()
		}
	}()
	if len(statePath) == 0 || len(statePath) > nativeProfileStatePathMaxSize || len(profile) == 0 || len(profile) > nativeClientProfileMaxSize {
		return nil, errors.New("native profile transaction input is invalid")
	}
	if writeNativeProfileUint32(stdin, uint32(len(statePath))) != nil || writeAll(stdin, []byte(statePath)) != nil || writeNativeProfileUint32(stdin, uint32(len(profile))) != nil || writeAll(stdin, profile) != nil {
		return nil, errors.New("native profile transaction request failed")
	}
	token, err := tx.output.ReadString('\n')
	if err != nil || token != nativeProfileTransactionReady {
		return nil, errors.New("native profile transaction staging failed")
	}
	failed = false
	return tx, nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func (tx *nativeProfileChildTransaction) Commit() error {
	return tx.finish('C', nativeProfileTransactionCommitted)
}
func (tx *nativeProfileChildTransaction) Rollback() error {
	return tx.finish('R', nativeProfileTransactionRolledBack)
}
func (tx *nativeProfileChildTransaction) finish(action byte, expected string) error {
	if tx.done {
		return errors.New("native profile transaction already finished")
	}
	tx.done = true
	if _, err := tx.input.Write([]byte{action}); err != nil {
		return tx.abort()
	}
	_ = tx.input.Close()
	token, readErr := tx.output.ReadString('\n')
	waitErr := tx.command.Wait()
	if readErr != nil || token != expected || waitErr != nil {
		return errors.New("native profile transaction completion failed")
	}
	return nil
}
func (tx *nativeProfileChildTransaction) abort() error {
	if tx.input != nil {
		_ = tx.input.Close()
	}
	if tx.command.Process != nil {
		_ = tx.command.Process.Signal(syscall.SIGTERM)
	}
	_ = tx.command.Wait()
	tx.done = true
	return errors.New("native profile transaction aborted")
}
