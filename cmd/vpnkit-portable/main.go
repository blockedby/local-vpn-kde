//go:build linux

// vpnkit-portable is a single-download Arch installer. Pacman owns every
// installed file and runs the native package transaction guards.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed payload/*
var payload embed.FS

type manifest struct {
	Format       int      `json:"format"`
	Package      string   `json:"package"`
	SHA256       string   `json:"sha256"`
	Architecture string   `json:"architecture"`
	Revision     string   `json:"revision"`
	Dependencies []string `json:"dependencies"`
}

func loadPayload() (manifest, []byte, error) { return readPayload(payload) }

func readPayload(files fs.FS) (manifest, []byte, error) {
	var m manifest
	b, err := fs.ReadFile(files, "payload/manifest.json")
	if err != nil {
		return m, nil, errors.New("no payload: build with packaging/portable/build.py")
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, nil, err
	}
	if m.Format != 1 || m.Package != "application.pkg.tar.zst" || m.Architecture != "amd64" {
		return m, nil, errors.New("unsupported payload manifest")
	}
	b, err = fs.ReadFile(files, "payload/"+m.Package)
	if err != nil {
		return m, nil, err
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != m.SHA256 {
		return m, nil, errors.New("payload checksum mismatch")
	}
	return m, b, nil
}

func archHost(data string) bool {
	for _, line := range strings.Split(data, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || (name != "ID" && name != "ID_LIKE") {
			continue
		}
		for _, id := range strings.Fields(strings.Trim(value, "\"'")) {
			if id == "arch" {
				return true
			}
		}
	}
	return false
}

func install(m manifest, data []byte) error {
	if os.Geteuid() != 0 {
		return errors.New("install requires root; inspect the payload first with --inspect")
	}
	if runtime.GOARCH != m.Architecture {
		return errors.New("payload architecture does not match this computer")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil || !archHost(string(release)) {
		return errors.New("this installer supports Arch Linux and Arch derivatives only")
	}
	caller, err := installationCaller(os.Getenv("SUDO_UID"), os.Getenv("PKEXEC_UID"), user.LookupId)
	if err != nil {
		return err
	}
	if os.Getenv("SUDO_USER") != "" && os.Getenv("SUDO_USER") != caller {
		return errors.New("installation caller name does not match elevated UID")
	}
	// Use a private directory under the root-owned /run, never a user-selected
	// TMPDIR. Pacman handles dependency checking, locks, ownership and rollback
	// markers. Package signature policy remains pacman's policy.
	dir, err := os.MkdirTemp("/run", "local-vpn-kde-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := dir + "/" + m.Package
	if err = os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	// Keep pacman's interactive confirmation for both transactions. Do not
	// refresh repositories alone (-Sy), disable signature checks or retry.
	missing, err := missingDependencies(m.Dependencies, dependencyTest)
	if err != nil {
		return err
	}
	commands := [][]string{}
	if len(missing) > 0 {
		commands = append(commands, append([]string{"-S", "--needed", "--"}, missing...))
	}
	commands = append(commands, []string{"-U", "--", path})
	for _, args := range commands {
		cmd := exec.Command("/usr/bin/pacman", args...)
		cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LANG=C.UTF-8", "HOME=/root"}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = cmd.Run(); err != nil {
			return err
		}
	}
	if caller != "" {
		account, lookupErr := user.Lookup(caller)
		if lookupErr != nil || (os.Getenv("SUDO_USER") != "" && os.Getenv("SUDO_USER") != caller) {
			return errors.New("package installed, invoking desktop account could not be validated")
		}
		cmd := exec.Command("/usr/bin/usermod", "--append", "--groups", "local-vpn-kde-users", "--", caller)
		cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LANG=C.UTF-8"}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("package installed, user authorization failed: %w", err)
		}
		outcome, err := onboardDesktop(account, desktopCommand, os.Lstat)
		if err != nil {
			return fmt.Errorf("package installed; desktop setup incomplete: %w", err)
		}
		fmt.Fprintln(os.Stdout, outcome)
	} else {
		fmt.Fprintln(os.Stdout, "Package installed; no elevated desktop caller. Desktop enrollment and profile setup are deferred.")
	}
	return nil
}

const dependencyOutputLimit = 64 << 10

var dependencyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9@._+:-]*(?:[<>=]{1,2}[A-Za-z0-9@._+:-]+)?$`)

type dependencyOutput struct{ bytes.Buffer }

func (b *dependencyOutput) Write(data []byte) (int, error) {
	if len(data) > dependencyOutputLimit-b.Len() {
		return 0, errors.New("dependency test output exceeded limit")
	}
	return b.Buffer.Write(data)
}

func dependencyTest(args []string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pacman", args...)
	cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LANG=C.UTF-8", "LC_ALL=C", "HOME=/root"}
	output := &dependencyOutput{}
	cmd.Stdout = output
	err := cmd.Run()
	if err == nil {
		return output.Bytes(), 0, nil
	}
	var status *exec.ExitError
	if errors.As(err, &status) && status.ExitCode() == 127 {
		return output.Bytes(), 127, nil
	}
	return nil, -1, errors.New("pacman dependency test failed; no packages installed")
}

// Deptest honors installed providers and version constraints. Never turn
// arbitrary command output into package installation arguments.
func missingDependencies(deps []string, query func([]string) ([]byte, int, error)) ([]string, error) {
	allowed := map[string]bool{}
	for _, dep := range deps {
		if !dependencyPattern.MatchString(dep) {
			return nil, errors.New("invalid manifest dependency")
		}
		allowed[dep] = true
	}
	if len(deps) == 0 {
		return nil, nil
	}
	output, code, err := query(append([]string{"-T", "--"}, deps...))
	if err != nil {
		return nil, err
	}
	if len(output) > dependencyOutputLimit {
		return nil, errors.New("dependency test output exceeded limit")
	}
	if code == 0 && len(bytes.TrimSpace(output)) == 0 {
		return nil, nil
	}
	if code != 127 {
		return nil, errors.New("unexpected pacman dependency test result")
	}
	missing := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if !allowed[line] || seen[line] {
			return nil, errors.New("unexpected dependency test output")
		}
		seen[line] = true
		missing = append(missing, line)
	}
	if len(missing) == 0 {
		return nil, errors.New("empty unsatisfied dependency result")
	}
	return missing, nil
}

// Commands run with fresh account groups and a fixed user environment. Neither
// installer nor package hooks start the gateway or activate a connection.
func desktopCommand(account *user.User, args []string, session bool) error {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + account.HomeDir,
		"USER=" + account.Username, "LOGNAME=" + account.Username,
		"XDG_STATE_HOME=" + filepath.Join(account.HomeDir, ".local/state")}
	if session {
		runtime := "/run/user/" + account.Uid
		env = append(env, "XDG_RUNTIME_DIR="+runtime, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtime+"/bus")
	}
	argv := []string{"--user", account.Username, "--", "/usr/bin/env", "-i"}
	argv = append(argv, env...)
	argv = append(argv, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/runuser", argv...)
	cmd.Dir = account.HomeDir
	cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "HOME=/root"}
	// EOF initializes private state without launching an interactive interface.
	// Do not echo arbitrary backend errors, which can contain private paths.
	if err := cmd.Run(); err != nil {
		return errors.New("desktop initialization command failed")
	}
	return nil
}

func onboardDesktop(account *user.User, run func(*user.User, []string, bool) error, stat func(string) (fs.FileInfo, error)) (string, error) {
	if account == nil || account.Username == "" || account.Uid == "" || account.Uid == "0" || !filepath.IsAbs(account.HomeDir) || filepath.Clean(account.HomeDir) != account.HomeDir || account.HomeDir == "/" {
		return "", errors.New("invalid desktop account")
	}
	if err := run(account, []string{"/usr/bin/local-vpn-kde", "bridge"}, false); err != nil {
		return "", err
	}
	profile := filepath.Join(account.HomeDir, ".local/state/local-vpn-kde/openvpn/client/vpnkit-local.ovpn")
	info, err := stat(profile)
	if errors.Is(err, os.ErrNotExist) {
		return "Installed and enrolled. Open local-vpn-kde as your desktop user and add a server/profile; its NetworkManager profile is prepared on first connect. No VPN was connected.", nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("existing desktop profile is not a regular readable file")
	}
	bus, err := stat("/run/user/" + account.Uid + "/bus")
	if errors.Is(err, os.ErrNotExist) {
		return "Installed and enrolled; desktop profile import deferred until a logged-in desktop session. Open local-vpn-kde there; first connect prepares its profile. No VPN was connected.", nil
	}
	if err != nil || bus.Mode()&os.ModeSocket == 0 {
		return "", errors.New("desktop session bus unavailable")
	}
	args := []string{"/usr/bin/systemd-run", "--user", "--quiet", "--wait", "--collect", "--pipe",
		"--setenv=HOME=" + account.HomeDir, "--setenv=XDG_STATE_HOME=" + filepath.Join(account.HomeDir, ".local/state"),
		"/usr/bin/local-vpn-kde", "networkmanager", "--runtime", "native", "--action", "import", "--yes"}
	if err := run(account, args, true); err != nil {
		return "", errors.New("NetworkManager profile import failed in desktop session; open local-vpn-kde diagnostics")
	}
	return "Installed and enrolled; desktop NetworkManager profile prepared. No VPN was connected.", nil
}

// Root elevation authenticates the installation caller once. Never infer an
// account from HOME/USER or enroll users during a generic pacman transaction.
func installationCaller(sudoUID, pkexecUID string, lookup func(string) (*user.User, error)) (string, error) {
	if sudoUID != "" && pkexecUID != "" && sudoUID != pkexecUID {
		return "", errors.New("ambiguous installation caller")
	}
	uidText := sudoUID
	if uidText == "" {
		uidText = pkexecUID
	}
	if uidText == "" {
		return "", nil
	} // direct root install: setup enrolls later
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != uidText {
		return "", errors.New("invalid installation caller")
	}
	account, err := lookup(uidText)
	if err != nil || account.Uid != uidText || account.Username == "" {
		return "", errors.New("installation caller unavailable")
	}
	return account.Username, nil
}

func run(args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "--inspect" && args[0] != "--install") {
		return errors.New("usage: vpnkit-portable --inspect | --install (Arch x86_64; pacman confirms dependencies and package)")
	}
	m, data, err := loadPayload()
	if err != nil {
		return err
	}
	if args[0] == "--inspect" {
		return json.NewEncoder(out).Encode(m)
	}
	fmt.Fprintln(out, "Installing the embedded native package. Existing native package hooks guard upgrades; the invoking desktop user receives permission to manage this application without further password prompts. No VPN is connected by this installer.")
	return install(m, data)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
