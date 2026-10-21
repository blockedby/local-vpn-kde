//go:build linux

package localvpn

import (
	"bufio"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	nativeClientProfileMaxSize = 1 << 20
	nativeClientProfileName    = "vpnkit-local.ovpn"
)

var nativeClientProfileDirectives = map[string]string{
	"client":                "",
	"dev":                   "tun",
	"proto":                 "udp",
	"remote":                "127.0.0.1 31194",
	"resolv-retry":          "infinite",
	"nobind":                "",
	"persist-key":           "",
	"persist-tun":           "",
	"remote-cert-tls":       "server",
	"auth":                  "SHA256",
	"auth-nocache":          "",
	"data-ciphers":          "AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC",
	"data-ciphers-fallback": "AES-256-CBC",
	"cipher":                "AES-256-GCM",
	"tun-mtu":               "1400",
	"mssfix":                "1360",
	"block-ipv6":            "",
	"key-direction":         "1",
	"verb":                  "3",
}

// DeliverNativeClientProfile installs profile at the sole supported relative
// destination, openvpn/client/vpnkit-local.ovpn. It must be invoked by root
// only after the parent operation authenticated the desktop user's UID,
// generated the profile from trusted assets, and validated statePath.
func DeliverNativeClientProfile(ownerUID, ownerGID uint32, statePath string, profile []byte) error {
	return deliverNativeClientProfile(ownerUID, ownerGID, statePath, profile, "/", 0)
}

// deliverNativeClientProfile is the private filesystem-root/privilege seam used
// by tests. Production always supplies the literal filesystem root and UID 0.
func deliverNativeClientProfile(ownerUID, ownerGID uint32, statePath string, profile []byte, tempRoot string, privilegeUID uint32) error {
	if uint32(os.Geteuid()) != privilegeUID {
		return errors.New("native client profile delivery requires the configured privileged identity")
	}
	if ownerUID == 0 {
		return errors.New("native client profile owner is invalid")
	}
	if err := validateNativeClientStatePath(statePath); err != nil {
		return err
	}
	if tempRoot == "" || !filepath.IsAbs(tempRoot) || filepath.Clean(tempRoot) != tempRoot {
		return errors.New("native client profile filesystem root is invalid")
	}
	if err := validateNativeClientProfile(profile); err != nil {
		return err
	}

	rootFD, err := unix.Open(tempRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("native client profile filesystem root is unavailable")
	}
	defer unix.Close(rootFD)
	if err := validateNativeProfileAncestor(rootFD, ownerUID); err != nil {
		return errors.New("native client profile filesystem root is unsafe")
	}

	stateFD, err := openNativeProfileState(rootFD, statePath, ownerUID)
	if err != nil {
		return err
	}
	defer unix.Close(stateFD)

	openvpnFD, err := openOrCreateNativeProfileChild(stateFD, "openvpn", ownerUID, ownerGID)
	if err != nil {
		return err
	}
	defer unix.Close(openvpnFD)
	clientFD, err := openOrCreateNativeProfileChild(openvpnFD, "client", ownerUID, ownerGID)
	if err != nil {
		return err
	}
	defer unix.Close(clientFD)

	return writeNativeClientProfile(clientFD, ownerUID, ownerGID, profile)
}

func validateNativeClientStatePath(statePath string) error {
	if statePath == "" || !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath || filepath.Base(statePath) != "local-vpn-kde" {
		return errors.New("native client profile state path is invalid")
	}
	return nil
}

func validateNativeProfileAncestor(fd int, ownerUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && st.Uid != ownerUID) {
		return errors.New("unsafe directory identity")
	}
	if st.Mode&0022 != 0 && !(st.Uid == 0 && st.Mode&unix.S_ISVTX != 0) {
		return errors.New("unsafe directory permissions")
	}
	return nil
}

func openNativeProfileState(rootFD int, statePath string, ownerUID uint32) (int, error) {
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return -1, errors.New("native client profile path traversal failed")
	}
	parts := strings.Split(strings.TrimPrefix(statePath, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return -1, errors.New("native client profile state path is invalid")
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			return -1, errors.New("native client profile state path is unavailable")
		}
		fd = next
		if i == len(parts)-1 {
			if err := validateNativeProfilePrivateDirectory(fd, ownerUID); err != nil {
				unix.Close(fd)
				return -1, errors.New("native client profile state directory is unsafe")
			}
		} else if err := validateNativeProfileAncestor(fd, ownerUID); err != nil {
			unix.Close(fd)
			return -1, errors.New("native client profile ancestor is unsafe")
		}
	}
	return fd, nil
}

func validateNativeProfilePrivateDirectory(fd int, ownerUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != ownerUID || st.Mode&07777 != 0700 {
		return errors.New("private directory metadata mismatch")
	}
	return nil
}

func openOrCreateNativeProfileChild(parentFD int, name string, ownerUID, ownerGID uint32) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		if validateErr := validateNativeProfilePrivateDirectory(fd, ownerUID); validateErr != nil {
			unix.Close(fd)
			return -1, errors.New("existing native client profile directory is unsafe")
		}
		return fd, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, errors.New("native client profile directory is unavailable")
	}
	if err := unix.Mkdirat(parentFD, name, 0700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err == nil {
				if validateErr := validateNativeProfilePrivateDirectory(fd, ownerUID); validateErr == nil {
					return fd, nil
				}
				unix.Close(fd)
			}
		}
		return -1, errors.New("native client profile directory creation failed")
	}
	fd, err = unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errors.New("created native client profile directory is unavailable")
	}
	var created unix.Stat_t
	if err := unix.Fstat(fd, &created); err != nil || created.Mode&unix.S_IFMT != unix.S_IFDIR ||
		created.Uid != uint32(os.Geteuid()) || created.Mode&07777 != 0700 {
		unix.Close(fd)
		return -1, errors.New("created native client profile directory identity changed")
	}
	if err := unix.Fchown(fd, int(ownerUID), int(ownerGID)); err != nil {
		unix.Close(fd)
		return -1, errors.New("native client profile directory ownership failed")
	}
	if err := unix.Fchmod(fd, 0700); err != nil {
		unix.Close(fd)
		return -1, errors.New("native client profile directory permissions failed")
	}
	if err := validateNativeProfilePrivateDirectory(fd, ownerUID); err != nil {
		unix.Close(fd)
		return -1, errors.New("created native client profile directory is unsafe")
	}
	if err := unix.Fsync(fd); err != nil {
		unix.Close(fd)
		return -1, errors.New("native client profile directory sync failed")
	}
	if err := unix.Fsync(parentFD); err != nil {
		unix.Close(fd)
		return -1, errors.New("native client profile parent sync failed")
	}
	return fd, nil
}

func inspectNativeClientProfileTarget(parentFD int, ownerUID uint32) (int, bool, error) {
	fd, err := unix.Openat(parentFD, nativeClientProfileName, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, false, nil
	}
	if err != nil {
		return -1, false, errors.New("existing native client profile is unavailable")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != ownerUID || st.Mode&07777 != 0600 {
		unix.Close(fd)
		return -1, false, errors.New("existing native client profile is unsafe")
	}
	return fd, true, nil
}

func writeNativeClientProfile(parentFD int, ownerUID, ownerGID uint32, profile []byte) error {
	oldFD, exists, err := inspectNativeClientProfileTarget(parentFD, ownerUID)
	if err != nil {
		return err
	}
	if oldFD >= 0 {
		defer unix.Close(oldFD)
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("native client profile temporary name generation failed")
	}
	tempName := ".vpnkit-profile-" + hex.EncodeToString(random[:]) + ".tmp"
	tempFD, err := unix.Openat(parentFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("native client profile temporary file creation failed")
	}
	defer unix.Close(tempFD)
	tempAtName := true
	defer func() {
		if tempAtName {
			unlinkNativeProfileIfSame(parentFD, tempName, tempFD)
		}
	}()

	if err := writeAllNativeProfile(tempFD, profile); err != nil {
		return err
	}
	if err := unix.Fchown(tempFD, int(ownerUID), int(ownerGID)); err != nil {
		return errors.New("native client profile ownership failed")
	}
	if err := unix.Fchmod(tempFD, 0600); err != nil {
		return errors.New("native client profile permissions failed")
	}
	if err := unix.Fsync(tempFD); err != nil {
		return errors.New("native client profile data sync failed")
	}
	if err := validateNativeProfileFileFD(tempFD, ownerUID); err != nil || !nativeProfileNameMatchesFD(parentFD, tempName, tempFD) {
		return errors.New("native client profile temporary file is unsafe")
	}

	if !exists {
		if err := unix.Renameat2(parentFD, tempName, parentFD, nativeClientProfileName, unix.RENAME_NOREPLACE); err != nil {
			return errors.New("native client profile atomic install failed")
		}
		tempAtName = false
	} else {
		if err := unix.Renameat2(parentFD, tempName, parentFD, nativeClientProfileName, unix.RENAME_EXCHANGE); err != nil {
			return errors.New("native client profile atomic update failed")
		}
		if !nativeProfileNameMatchesFD(parentFD, tempName, oldFD) {
			// Restore both names only while the destination is still our held
			// temporary inode. Never unlink an object whose identity is unknown.
			if nativeProfileNameMatchesFD(parentFD, nativeClientProfileName, tempFD) {
				_ = unix.Renameat2(parentFD, tempName, parentFD, nativeClientProfileName, unix.RENAME_EXCHANGE)
			}
			return errors.New("native client profile target changed concurrently")
		}
		if err := unix.Unlinkat(parentFD, tempName, 0); err != nil {
			return errors.New("previous native client profile cleanup failed")
		}
		tempAtName = false
	}

	if !nativeProfileNameMatchesFD(parentFD, nativeClientProfileName, tempFD) {
		return errors.New("native client profile destination changed concurrently")
	}
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("native client profile directory sync failed")
	}
	return nil
}

func writeAllNativeProfile(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return errors.New("native client profile write failed")
		}
		if n == 0 {
			return errors.New("native client profile write made no progress")
		}
		data = data[n:]
	}
	return nil
}

func validateNativeProfileFileFD(fd int, ownerUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != ownerUID || st.Mode&07777 != 0600 {
		return errors.New("private file metadata mismatch")
	}
	return nil
}

func nativeProfileNameMatchesFD(parentFD int, name string, fd int) bool {
	var named unix.Stat_t
	return nativeProfileNameHasIdentity(parentFD, name, fd, &named) &&
		named.Mode&unix.S_IFMT == unix.S_IFREG && named.Nlink == 1
}

func nativeProfileNameHasIdentity(parentFD int, name string, fd int, named *unix.Stat_t) bool {
	var held unix.Stat_t
	return unix.Fstatat(parentFD, name, named, unix.AT_SYMLINK_NOFOLLOW) == nil &&
		unix.Fstat(fd, &held) == nil && named.Dev == held.Dev && named.Ino == held.Ino
}

func unlinkNativeProfileIfSame(parentFD int, name string, fd int) {
	var named unix.Stat_t
	if nativeProfileNameHasIdentity(parentFD, name, fd, &named) {
		_ = unix.Unlinkat(parentFD, name, 0)
	}
}

func validateNativeClientProfile(profile []byte) error {
	if len(profile) == 0 || len(profile) > nativeClientProfileMaxSize || strings.IndexByte(string(profile), 0) >= 0 {
		return errors.New("native client profile size or encoding is invalid")
	}
	seen := make(map[string]bool, len(nativeClientProfileDirectives))
	blocks := map[string][]string{}
	blockOrder := []string{"ca", "cert", "key", "tls-auth"}
	blockIndex := 0
	active := ""

	scanner := bufio.NewScanner(strings.NewReader(string(profile)))
	scanner.Buffer(make([]byte, 4096), nativeClientProfileMaxSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if active != "" {
			if line == "</"+active+">" {
				active = ""
				blockIndex++
				continue
			}
			if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
				return errors.New("native client profile inline block is invalid")
			}
			blocks[blockOrder[blockIndex]] = append(blocks[blockOrder[blockIndex]], line)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if blockIndex < len(blockOrder) && line == "<"+blockOrder[blockIndex]+">" {
			active = blockOrder[blockIndex]
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		expected, ok := nativeClientProfileDirectives[name]
		if !ok || seen[name] || strings.Join(fields[1:], " ") != expected {
			return errors.New("native client profile contains an unsupported directive")
		}
		seen[name] = true
	}
	if scanner.Err() != nil || active != "" || blockIndex != len(blockOrder) {
		return errors.New("native client profile structure is invalid")
	}
	for name := range nativeClientProfileDirectives {
		if !seen[name] {
			return errors.New("native client profile is incomplete")
		}
	}
	if !validNativeProfileCertificateBlock(blocks["ca"]) || !validNativeProfileCertificateBlock(blocks["cert"]) ||
		!validNativeProfileKeyBlock(blocks["key"]) || !validNativeProfileTLSAuthBlock(blocks["tls-auth"]) {
		return errors.New("native client profile inline material is invalid")
	}
	return nil
}

func validNativeProfileCertificateBlock(lines []string) bool {
	data := []byte(strings.Join(lines, "\n") + "\n")
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return false
	}
	_, err := x509.ParseCertificate(block.Bytes)
	return err == nil
}

func validNativeProfileKeyBlock(lines []string) bool {
	data := []byte(strings.Join(lines, "\n") + "\n")
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return false
	}
	_, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	return err == nil
}

func validNativeProfileTLSAuthBlock(lines []string) bool {
	if len(lines) < 3 || lines[0] != "#" || lines[1] != "# 2048 bit OpenVPN static key" || lines[2] != "#" {
		return false
	}
	lines = lines[3:]
	if len(lines) != 18 || lines[0] != "-----BEGIN OpenVPN Static key V1-----" || lines[len(lines)-1] != "-----END OpenVPN Static key V1-----" {
		return false
	}
	for _, line := range lines[1 : len(lines)-1] {
		if len(line) != 32 {
			return false
		}
		if _, err := hex.DecodeString(line); err != nil {
			return false
		}
	}
	return true
}
