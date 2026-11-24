//go:build linux

package localvpn

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

// NativeProfileStaleMessage is the fixed line emitted by package hooks. It
// carries no user names, paths or profile content.
const NativeProfileStaleMessage = "native_profile=stale: re-run native setup"

const nativeEnrolledGroup = "local-vpn-kde-users"

// ReportStaleNativeClientProfiles is an advisory post-install/upgrade check.
// Regenerating the client profile needs the subscription (it carries the
// generated client credentials), which only the interactive native setup has,
// so the hook only reports. It never fails, never touches NetworkManager and
// never prints profile content. Silent for fresh installs (no configuration or
// no profile) and for current profiles.
func ReportStaleNativeClientProfiles(output io.Writer) {
	if os.Geteuid() != 0 {
		return
	}
	if reportStaleNativeClientProfilesAt(nativegateway.ConfigDir+"/"+nativeConfigurationManifest, "/etc/group", nativeEnrolledMembers) {
		fmt.Fprintln(output, NativeProfileStaleMessage)
	}
}

func nativeEnrolledMembers(groupFile string) []*user.User {
	file, err := os.Open(groupFile)
	if err != nil {
		return nil
	}
	defer file.Close()
	var users []*user.User
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) != 4 || fields[0] != nativeEnrolledGroup {
			continue
		}
		for _, name := range strings.Split(fields[3], ",") {
			if name == "" {
				continue
			}
			if account, err := user.Lookup(name); err == nil {
				users = append(users, account)
			}
		}
	}
	return users
}

// reportStaleNativeClientProfilesAt returns true when any enrolled user's
// client profile is stale. Test seams: configuration manifest and group file.
func reportStaleNativeClientProfilesAt(configPath, groupFile string, members func(string) []*user.User) bool {
	if info, err := os.Stat(configPath); err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, account := range members(groupFile) {
		if staleNativeClientProfileOf(account) {
			return true
		}
	}
	return false
}

func staleNativeClientProfileOf(account *user.User) bool {
	var uid uint64
	if _, err := fmt.Sscanf(account.Uid, "%d", &uid); err != nil || uid == 0 || !filepath.IsAbs(account.HomeDir) || filepath.Clean(account.HomeDir) != account.HomeDir || account.HomeDir == "/" {
		return false
	}
	data, err := readNativeClientProfileAt("/", filepath.Join(account.HomeDir, ".local/state/local-vpn-kde"), uint32(uid))
	if err != nil {
		return false
	}
	return nativeProfileDataStale(data)
}

// readNativeClientProfileAt reads the profile through pinned, no-follow
// descriptors with the same ownership rules as delivery. Any anomaly means
// "not readable" and the profile is left to the user-side checks.
func readNativeClientProfileAt(anchor, statePath string, ownerUID uint32) ([]byte, error) {
	rootFD, err := unix.Open(anchor, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := openNativeProfileState(rootFD, statePath, ownerUID)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"openvpn", "client"} {
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
		if err := validateNativeProfilePrivateDirectory(fd, ownerUID); err != nil {
			unix.Close(fd)
			return nil, err
		}
	}
	defer unix.Close(fd)
	file, err := unix.Openat(fd, nativeClientProfileName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(file, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != ownerUID || st.Size > nativeClientProfileMaxSize {
		unix.Close(file)
		return nil, os.ErrInvalid
	}
	f := os.NewFile(uintptr(file), "profile")
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, nativeClientProfileMaxSize))
}
