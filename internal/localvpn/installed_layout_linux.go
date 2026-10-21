package localvpn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	InstalledAssetRoot = "/usr/share/local-vpn-kde"
	InstalledTUI       = "/usr/share/local-vpn-kde/scripts/vpnkit/tui/vpnkit-tui"
	InstalledCLI       = "/usr/libexec/local-vpn-kde/local-vpn-kde.bin"
)

// ApplicationLayout describes either the immutable native package layout or a
// checkout. State is always private mutable state; Assets is never used as a
// state directory.
type ApplicationLayout struct {
	Installed  bool
	Assets     string
	Executable string
	State      string
	Runtime    string
}

var installedAssetFiles = []string{
	"scripts/vpnkit/tui/vpnkit-tui",
	"scripts/vpnkit/vpnkit-native.sh",
	"config/openvpn/local-server.tpl",
	"config/openvpn/local-client.ovpn.template",
	"config/sing-box/config.tun.json.template",
}

// ResolveApplicationLayout selects installed mode only from the fixed native
// package executable path. Environment variables cannot opt a checkout into
// installed mode. This function validates paths but neither creates state nor
// reads any private state file.
func ResolveApplicationLayout(executable, repo, base, runtime string) (ApplicationLayout, error) {
	if err := safeLayoutInput(executable); err != nil || executable == "" {
		return ApplicationLayout{}, errors.New("invalid application executable")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return ApplicationLayout{}, fmt.Errorf("resolve application executable: %w", err)
	}
	executable = filepath.Clean(executable)
	if executable == InstalledCLI {
		return resolveInstalledLayout(repo, base, runtime)
	}
	return resolveCheckoutLayout(executable, repo, base, runtime)
}

func resolveInstalledLayout(repo, base, runtime string) (ApplicationLayout, error) {
	switch runtime {
	case "", "native":
		runtime = "native"
	case "docker":
		return ApplicationLayout{}, errors.New("installed application supports only the native runtime")
	default:
		return ApplicationLayout{}, errors.New("invalid installed runtime")
	}
	if err := safeLayoutInput(repo); err != nil {
		return ApplicationLayout{}, errors.New("invalid installed asset root")
	}
	if repo != "" && filepath.Clean(repo) != "." && filepath.Clean(repo) != InstalledAssetRoot {
		return ApplicationLayout{}, errors.New("installed application asset root is fixed")
	}

	state, boundary, err := installedStatePath()
	if err != nil {
		return ApplicationLayout{}, err
	}
	if err = validateInstalledStatePath(state, boundary, uint32(os.Getuid()), "/"); err != nil {
		return ApplicationLayout{}, fmt.Errorf("unsafe installed state path: %w", err)
	}

	switch base {
	case "", "secrets/vpnkit-local":
	case state:
	default:
		return ApplicationLayout{}, errors.New("installed application state path is fixed")
	}
	return ApplicationLayout{Installed: true, Assets: InstalledAssetRoot, Executable: InstalledCLI, State: state, Runtime: runtime}, nil
}

func resolveCheckoutLayout(executable, repo, base, runtime string) (ApplicationLayout, error) {
	for _, value := range []string{repo, base, runtime} {
		if err := safeLayoutInput(value); err != nil {
			return ApplicationLayout{}, errors.New("invalid checkout layout input")
		}
	}
	if runtime == "" {
		runtime = "docker"
	}
	if runtime != "docker" && runtime != "native" {
		return ApplicationLayout{}, errors.New("invalid runtime")
	}
	root, err := filepath.Abs(repo)
	if err != nil {
		return ApplicationLayout{}, fmt.Errorf("resolve checkout root: %w", err)
	}
	root = filepath.Clean(root)
	if base == "" {
		base = "secrets/vpnkit-local"
	}
	state, err := SecretRoot(root, base, false)
	if err != nil {
		return ApplicationLayout{}, err
	}
	return ApplicationLayout{Assets: root, Executable: executable, State: state, Runtime: runtime}, nil
}

func installedStatePath() (state, boundary string, err error) {
	boundary = os.Getenv("XDG_STATE_HOME")
	if boundary == "" {
		boundary, err = os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve user home: %w", err)
		}
		if err = validateAbsoluteLayoutPath(boundary); err != nil {
			return "", "", fmt.Errorf("invalid user home: %w", err)
		}
		boundary = filepath.Clean(boundary)
		boundary = filepath.Join(boundary, ".local", "state")
	} else {
		if err = validateAbsoluteLayoutPath(boundary); err != nil {
			return "", "", fmt.Errorf("invalid XDG_STATE_HOME: %w", err)
		}
		boundary = filepath.Clean(boundary)
	}
	return filepath.Join(boundary, "local-vpn-kde"), boundary, nil
}

func safeLayoutInput(value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("path contains a control character")
	}
	return nil
}

func validateAbsoluteLayoutPath(path string) error {
	if err := safeLayoutInput(path); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return errors.New("absolute path required")
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == "." || part == ".." {
			return errors.New("noncanonical path")
		}
	}
	return nil
}

// validateInstalledStatePath walks only existing directories with no-follow
// descriptors. anchor is a test seam; production always supplies "/".
func validateInstalledStatePath(state, boundary string, uid uint32, anchor string) error {
	if err := validateAbsoluteLayoutPath(state); err != nil {
		return err
	}
	if err := validateAbsoluteLayoutPath(boundary); err != nil {
		return err
	}
	state = filepath.Clean(state)
	boundary = filepath.Clean(boundary)
	anchor = filepath.Clean(anchor)
	if !pathWithin(anchor, state) || !pathWithin(anchor, boundary) || !pathWithin(boundary, state) {
		return errors.New("state path escapes its user boundary")
	}

	fd, err := unix.Open(anchor, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = validateProtectedDirectory(fd, uid, anchor == boundary, anchor == state); err != nil {
		return err
	}
	current := anchor
	for _, part := range relativeParts(anchor, state) {
		next, openErr := unix.Openat(fd, part, directoryFlags, 0)
		if errors.Is(openErr, unix.ENOENT) {
			return nil
		}
		if openErr != nil {
			return openErr
		}
		unix.Close(fd)
		fd = next
		current = filepath.Join(current, part)
		if err = validateProtectedDirectory(fd, uid, current == boundary || pathWithin(boundary, current), current == state); err != nil {
			return err
		}
	}
	return nil
}

func validateProtectedDirectory(fd int, uid uint32, userOwned, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 {
		return errors.New("directory is writable by another user")
	}
	if userOwned {
		if st.Uid != uid {
			return errors.New("user state directory has the wrong owner")
		}
	} else if st.Uid != 0 && st.Uid != uid {
		return errors.New("directory has an untrusted owner")
	}
	if private && (st.Uid != uid || st.Mode&0777 != 0700) {
		return errors.New("state directory must be owned by the user with mode 0700")
	}
	return nil
}

// ValidateInstalledAssets verifies the small fixed set of native package entry
// points and templates without reading their contents. It accepts no caller
// paths and never validates checkout assets.
func ValidateInstalledAssets() error {
	return validateInstalledAssetsAt(InstalledAssetRoot, InstalledCLI, 0, "/")
}

// validateInstalledAssetsAt is an internal test seam. All production arguments
// are fixed by ValidateInstalledAssets.
func validateInstalledAssetsAt(assetRoot, cli string, expectedUID uint32, anchor string) error {
	if err := validateTrustedRegularFile(cli, expectedUID, anchor, true); err != nil {
		return fmt.Errorf("validate installed CLI: %w", err)
	}
	for _, relative := range installedAssetFiles {
		executable := relative == "scripts/vpnkit/vpnkit-native.sh" || relative == "scripts/vpnkit/tui/vpnkit-tui"
		if err := validateTrustedRegularFile(filepath.Join(assetRoot, relative), expectedUID, anchor, executable); err != nil {
			return fmt.Errorf("validate installed asset %s: %w", relative, err)
		}
	}
	return nil
}

func validateTrustedRegularFile(path string, expectedUID uint32, anchor string, executable bool) error {
	if err := validateAbsoluteLayoutPath(path); err != nil {
		return err
	}
	anchor = filepath.Clean(anchor)
	path = filepath.Clean(path)
	if !pathWithin(anchor, path) || path == anchor {
		return errors.New("asset escapes trusted root")
	}
	fd, err := unix.Open(anchor, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = validateTrustedAssetDirectory(fd, expectedUID); err != nil {
		return err
	}
	parts := relativeParts(anchor, path)
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, directoryFlags, 0)
		if openErr != nil {
			return openErr
		}
		unix.Close(fd)
		fd = next
		if err = validateTrustedAssetDirectory(fd, expectedUID); err != nil {
			return err
		}
	}
	file, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer unix.Close(file)
	var st unix.Stat_t
	if err = unix.Fstat(file, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != expectedUID || st.Mode&0022 != 0 {
		return errors.New("unsafe installed file")
	}
	if executable && st.Mode&0111 == 0 {
		return errors.New("installed executable is not executable")
	}
	return nil
}

func validateTrustedAssetDirectory(fd int, expectedUID uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != expectedUID || st.Mode&0022 != 0 {
		return errors.New("unsafe installed directory")
	}
	return nil
}

func pathWithin(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator)) || root == "/" && filepath.IsAbs(path)
}

func relativeParts(root, path string) []string {
	relative := strings.TrimPrefix(path, root)
	relative = strings.TrimPrefix(relative, string(filepath.Separator))
	if relative == "" {
		return nil
	}
	return strings.Split(relative, string(filepath.Separator))
}
