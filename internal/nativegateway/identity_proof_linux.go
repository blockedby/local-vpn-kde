//go:build linux

package nativegateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	namespaceIdentityVersion = 1
	namespaceIdentityMaxSize = 4096
	namespaceIdentityPath    = "/run/local-vpn-kde-network/namespace.json"
)

type namespaceIdentity struct {
	Version     int    `json:"version"`
	ExpectedDev uint64 `json:"expectedDev"`
	ExpectedIno uint64 `json:"expectedIno"`
	InitialDev  uint64 `json:"initialDev"`
	InitialIno  uint64 `json:"initialIno"`
}

// namespaceProofLocation is an unexported test seam. Production callers always
// use productionNamespaceIdentityLocation, rooted at / and requiring uid 0.
type namespaceProofLocation struct {
	rootPath     string
	relativePath string
	ownerUID     uint32
}

func productionNamespaceIdentityLocation() namespaceProofLocation {
	return namespaceProofLocation{
		rootPath:     "/",
		relativePath: strings.TrimPrefix(namespaceIdentityPath, "/"),
		ownerUID:     0,
	}
}

func identityFromStats(expected, initial unix.Stat_t) namespaceIdentity {
	return namespaceIdentity{
		Version:     namespaceIdentityVersion,
		ExpectedDev: uint64(expected.Dev),
		ExpectedIno: uint64(expected.Ino),
		InitialDev:  uint64(initial.Dev),
		InitialIno:  uint64(initial.Ino),
	}
}

func (p namespaceIdentity) validate() error {
	if p.Version != namespaceIdentityVersion {
		return errors.New("unsupported namespace identity proof version")
	}
	if p.ExpectedDev == 0 || p.ExpectedIno == 0 || p.InitialDev == 0 || p.InitialIno == 0 {
		return errors.New("namespace identity proof contains a zero identity")
	}
	if p.ExpectedDev == p.InitialDev && p.ExpectedIno == p.InitialIno {
		return errors.New("namespace identity proof aliases the initial namespace")
	}
	return nil
}

func verifyIdentityProof(target, current unix.Stat_t, proof namespaceIdentity) error {
	if err := proof.validate(); err != nil {
		return err
	}
	expected := unix.Stat_t{Dev: proof.ExpectedDev, Ino: proof.ExpectedIno}
	initial := unix.Stat_t{Dev: proof.InitialDev, Ino: proof.InitialIno}
	if !sameNamespace(target, expected) || !sameNamespace(current, expected) {
		return fmt.Errorf("native gateway must run in namespace %s", Namespace)
	}
	if sameNamespace(current, initial) {
		return errors.New("refusing native gateway in the initial network namespace")
	}
	return nil
}

func readNamespaceIdentityProof(location namespaceProofLocation) (namespaceIdentity, error) {
	parent, base, err := openProofParent(location, false)
	if err != nil {
		return namespaceIdentity{}, err
	}
	defer unix.Close(parent)

	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return namespaceIdentity{}, errors.New("namespace identity proof unavailable")
	}
	file := os.NewFile(uintptr(fd), "namespace identity proof")
	if file == nil {
		unix.Close(fd)
		return namespaceIdentity{}, errors.New("namespace identity proof unavailable")
	}
	defer file.Close()

	before, err := validateProofFile(fd, location.ownerUID)
	if err != nil {
		return namespaceIdentity{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, namespaceIdentityMaxSize+1))
	if err != nil || len(data) == 0 || len(data) > namespaceIdentityMaxSize || int64(len(data)) != before.Size {
		return namespaceIdentity{}, errors.New("invalid namespace identity proof size")
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size {
		return namespaceIdentity{}, errors.New("namespace identity proof changed while reading")
	}

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var proof namespaceIdentity
	if err := decoder.Decode(&proof); err != nil {
		return namespaceIdentity{}, errors.New("invalid namespace identity proof")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return namespaceIdentity{}, errors.New("invalid trailing namespace identity proof data")
	}
	if err := proof.validate(); err != nil {
		return namespaceIdentity{}, err
	}
	return proof, nil
}

func publishNamespaceIdentityProof(location namespaceProofLocation, proof namespaceIdentity) error {
	if err := proof.validate(); err != nil {
		return err
	}
	parent, base, err := openProofParent(location, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)

	if err := rejectUnsafeExistingProof(parent, base, location.ownerUID); err != nil {
		return err
	}
	data, err := json.Marshal(proof)
	if err != nil {
		return errors.New("cannot encode namespace identity proof")
	}
	data = append(data, '\n')
	if len(data) > namespaceIdentityMaxSize {
		return errors.New("namespace identity proof is too large")
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("cannot create namespace identity proof")
	}
	temp := ".namespace.json." + hex.EncodeToString(random[:])
	fd, err := unix.Openat(parent, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot create namespace identity proof")
	}
	keep := false
	defer func() {
		unix.Close(fd)
		if !keep {
			_ = unix.Unlinkat(parent, temp, 0)
		}
	}()
	if err := unix.Fchmod(fd, 0644); err != nil {
		return errors.New("cannot set namespace identity proof permissions")
	}
	if err := writeAll(fd, data); err != nil {
		return errors.New("cannot write namespace identity proof")
	}
	if _, err := validateProofFile(fd, location.ownerUID); err != nil {
		return errors.New("cannot create safely owned namespace identity proof")
	}
	if err := unix.Fsync(fd); err != nil {
		return errors.New("cannot sync namespace identity proof")
	}
	if err := unix.Renameat(parent, temp, parent, base); err != nil {
		return errors.New("cannot publish namespace identity proof")
	}
	keep = true
	if err := unix.Fsync(parent); err != nil {
		return errors.New("cannot sync namespace identity proof directory")
	}
	return nil
}

func openProofParent(location namespaceProofLocation, create bool) (int, string, error) {
	parts := strings.Split(location.relativePath, "/")
	if location.rootPath == "" || len(parts) < 2 {
		return -1, "", errors.New("invalid namespace identity proof path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, "", errors.New("invalid namespace identity proof path")
		}
	}
	fd, err := unix.Open(location.rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", errors.New("namespace identity proof root unavailable")
	}
	if err := validateProtectedDirectory(fd, location.ownerUID, false); err != nil {
		unix.Close(fd)
		return -1, "", err
	}
	for i, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && create && i == len(parts)-2 && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, part, 0755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				unix.Close(fd)
				return -1, "", errors.New("cannot create namespace identity proof directory")
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(fd)
		if openErr != nil {
			return -1, "", errors.New("namespace identity proof directory unavailable")
		}
		if err := validateProtectedDirectory(next, location.ownerUID, i == len(parts)-2); err != nil {
			unix.Close(next)
			return -1, "", err
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func validateProtectedDirectory(fd int, ownerUID uint32, parent bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return errors.New("namespace identity proof directory identity unavailable")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != ownerUID || st.Mode&0022 != 0 {
		return errors.New("unsafe namespace identity proof directory")
	}
	if parent && st.Mode&0777 != 0755 {
		return errors.New("namespace identity proof directory must be mode 0755")
	}
	return nil
}

func validateProofFile(fd int, ownerUID uint32) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, errors.New("namespace identity proof identity unavailable")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != ownerUID || st.Mode&0777 != 0644 || st.Nlink != 1 {
		return st, errors.New("unsafe namespace identity proof")
	}
	if st.Size <= 0 || st.Size > namespaceIdentityMaxSize {
		return st, errors.New("invalid namespace identity proof size")
	}
	return st, nil
}

func rejectUnsafeExistingProof(parent int, base string, ownerUID uint32) error {
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return errors.New("unsafe existing namespace identity proof")
	}
	defer unix.Close(fd)
	if _, err := validateProofFile(fd, ownerUID); err != nil {
		return fmt.Errorf("unsafe existing namespace identity proof: %w", err)
	}
	return nil
}

func writeAll(fd int, data []byte) error {
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
