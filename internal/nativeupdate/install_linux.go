//go:build linux

package nativeupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	DefaultWorkRoot  = "/var/cache/local-vpn-kde/update"
	DefaultInhibitor = "/run/local-vpn-kde-package-blocked"
	DefaultLockPath  = "/run/local-vpn-kde-update.lock"
)

// Installer installs a newer release of the native package. It runs as root
// behind a fixed Polkit action; its only input is the version the user saw.
type Installer struct {
	Client            *Client
	Run               Runner
	Exists            func(string) bool
	InstallPackage    func(ctx context.Context, manager, path string) error
	BeforeInstall     func()
	WorkRoot          string
	Inhibitor         string
	LockPath          string
	AllowNonRootOwner bool // tests only
}

func (in *Installer) defaults() {
	if in.Client == nil {
		in.Client = NewClient("local-vpn-kde-updater")
	}
	if in.Run == nil {
		in.Run = ExecRunner
	}
	if in.Exists == nil {
		in.Exists = func(path string) bool { _, err := os.Stat(path); return err == nil }
	}
	if in.InstallPackage == nil {
		in.InstallPackage = realInstall
	}
	if in.BeforeInstall == nil {
		in.BeforeInstall = func() { signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE) }
	}
	if in.WorkRoot == "" {
		in.WorkRoot = DefaultWorkRoot
	}
	if in.Inhibitor == "" {
		in.Inhibitor = DefaultInhibitor
	}
	if in.LockPath == "" {
		in.LockPath = DefaultLockPath
	}
}

// Install downloads, verifies and installs exactly the requested version.
func (in *Installer) Install(ctx context.Context, version string, progress func(phase string)) (Installation, error) {
	in.defaults()
	if progress == nil {
		progress = func(string) {}
	}
	lock, err := os.OpenFile(in.LockPath, os.O_CREATE|os.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return Installation{}, fmt.Errorf("open update lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return Installation{}, errors.New("another update is already running")
		}
		return Installation{}, fmt.Errorf("lock update: %w", err)
	}

	progress("checking")
	want, err := ParseVersion(version)
	if err != nil {
		return Installation{}, err
	}
	installed, err := DetectInstallation(ctx, in.Run, in.Exists)
	if err != nil {
		return Installation{}, err
	}
	release, err := in.Client.Latest(ctx)
	if err != nil {
		return Installation{}, err
	}
	if release.Version != want {
		return Installation{}, fmt.Errorf("the latest release is now %s; check again", release.Version)
	}
	if !installed.Older(want.Major, want.Minor, want.Patch) {
		return Installation{}, fmt.Errorf("version %s is already installed", installed.Raw)
	}
	asset, err := AssetName(installed.Manager, want)
	if err != nil {
		return Installation{}, err
	}
	if _, ok := release.Assets[asset]; !ok {
		return Installation{}, fmt.Errorf("release %s has no %s package", want, installed.Manager)
	}

	dir, err := in.workDir()
	if err != nil {
		return Installation{}, err
	}
	defer os.RemoveAll(dir)

	progress("downloading")
	sums, err := in.Client.Checksums(ctx, release)
	if err != nil {
		return Installation{}, err
	}
	path := filepath.Join(dir, asset)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return Installation{}, err
	}
	err = in.Client.Download(ctx, release, asset, sums, file)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Installation{}, err
	}

	progress("verifying")
	if err := in.verifyMetadata(ctx, installed.Manager, path, want); err != nil {
		return Installation{}, err
	}
	// The download can take minutes. Re-check the installed version and the
	// planned transaction immediately before committing, so an administrator's
	// upgrade in the meantime is neither downgraded nor widened. The package
	// manager's own database lock excludes a transaction during the commit.
	current, err := DetectInstallation(ctx, in.Run, in.Exists)
	if err != nil {
		return Installation{}, err
	}
	if current.Manager != installed.Manager || !current.Older(want.Major, want.Minor, want.Patch) {
		return Installation{}, fmt.Errorf("the installed package changed to %s during the download; check again", current.Raw)
	}
	if err := in.verifyOnlyThisPackage(ctx, installed.Manager, path); err != nil {
		return Installation{}, err
	}
	// A marker left by an earlier failed upgrade needs the documented recovery,
	// not another upgrade whose hooks might clear it.
	if _, err := os.Lstat(in.Inhibitor); !errors.Is(err, os.ErrNotExist) {
		return Installation{}, errors.New("package management is blocked by an earlier unfinished upgrade; see docs/native-hot-upgrade.md (recovery)")
	}
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}

	progress("installing")
	in.BeforeInstall()
	installErr := in.InstallPackage(context.WithoutCancel(ctx), installed.Manager, path)
	after, err := DetectInstallation(context.WithoutCancel(ctx), in.Run, in.Exists)
	if err != nil {
		return Installation{}, fmt.Errorf("check installed version: %w", err)
	}
	if after.Major != want.Major || after.Minor != want.Minor || after.Patch != want.Patch {
		if installErr != nil {
			return Installation{}, fmt.Errorf("install %s: %w", asset, installErr)
		}
		return Installation{}, fmt.Errorf("installation did not complete; installed version is still %s", after.Raw)
	}
	if _, err := os.Lstat(in.Inhibitor); !errors.Is(err, os.ErrNotExist) {
		return Installation{}, errors.New("the update finished but package management is still blocked; see docs/native-hot-upgrade.md (recovery)")
	}
	if installErr != nil {
		return Installation{}, fmt.Errorf("version %s was installed but the package manager reported an error: %w", want, installErr)
	}
	progress("done")
	return after, nil
}

func (in *Installer) workDir() (string, error) {
	if err := os.MkdirAll(in.WorkRoot, 0o700); err != nil {
		return "", fmt.Errorf("prepare download directory: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(in.WorkRoot, &st); err != nil {
		return "", fmt.Errorf("check download directory: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o077 != 0 || (!in.AllowNonRootOwner && st.Uid != 0) {
		return "", fmt.Errorf("refusing unsafe download directory %s", in.WorkRoot)
	}
	return os.MkdirTemp(in.WorkRoot, "update-")
}

func fields(output []byte, separator string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, separator)
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

func (in *Installer) verifyMetadata(ctx context.Context, manager, path string, want Version) error {
	var values map[string]string
	var expected [3][2]string
	switch manager {
	case "pacman":
		output, err := in.Run(ctx, "/usr/bin/pacman", "-Qip", path)
		if err != nil {
			return fmt.Errorf("read package metadata: %w", err)
		}
		values = fields(output, ":")
		expected = [3][2]string{{"Name", PackageName}, {"Version", want.String() + "-1"}, {"Architecture", "x86_64"}}
	case "dpkg":
		output, err := in.Run(ctx, "/usr/bin/dpkg-deb", "-f", path, "Package", "Version", "Architecture")
		if err != nil {
			return fmt.Errorf("read package metadata: %w", err)
		}
		values = fields(output, ":")
		expected = [3][2]string{{"Package", PackageName}, {"Version", want.String()}, {"Architecture", "amd64"}}
	default:
		return fmt.Errorf("unsupported package manager %q", manager)
	}
	for _, pair := range expected {
		if values[pair[0]] != pair[1] {
			return fmt.Errorf("downloaded package has %s %q, expected %q", strings.ToLower(pair[0]), values[pair[0]], pair[1])
		}
	}
	return nil
}

func (in *Installer) verifyOnlyThisPackage(ctx context.Context, manager, path string) error {
	var changed []string
	switch manager {
	case "pacman":
		output, err := in.Run(ctx, "/usr/bin/pacman", "-U", "--print", "--print-format", "%n", path)
		if err != nil {
			return fmt.Errorf("check package dependencies: %w", err)
		}
		for _, line := range strings.Split(string(output), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				changed = append(changed, line)
			}
		}
	case "dpkg":
		output, err := in.Run(ctx, "/usr/bin/apt-get", "-s", "--no-remove", "install", path)
		if err != nil {
			return fmt.Errorf("check package dependencies: %w", err)
		}
		for _, line := range strings.Split(string(output), "\n") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			switch parts[0] {
			case "Inst":
				changed = append(changed, parts[1])
			case "Conf":
				// Configuring this package is expected; configuring any other
				// (for example one left unconfigured) would run its scripts.
				if parts[1] != PackageName {
					changed = append(changed, "configure "+parts[1])
				}
			case "Remv", "Purg":
				changed = append(changed, "remove "+parts[1])
			}
		}
	default:
		return fmt.Errorf("unsupported package manager %q", manager)
	}
	if len(changed) != 1 || changed[0] != PackageName {
		var others []string
		for _, name := range changed {
			if name != PackageName {
				others = append(others, name)
			}
		}
		return fmt.Errorf("this update also changes other packages (%s); install it manually", strings.Join(others, ", "))
	}
	return nil
}

type tailBuffer struct{ data []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.data = append(t.data, p...)
	if len(t.data) > 2048 {
		t.data = append([]byte(nil), t.data[len(t.data)-2048:]...)
	}
	return len(p), nil
}

// realInstall commits the transaction. It is never killed: a deadline or a
// cancelled caller must not interrupt a package manager halfway through. The
// package manager runs in its own session, so a terminal Ctrl+C or hang-up
// does not reach it either.
func realInstall(_ context.Context, manager, path string) error {
	command, err := installCommand(manager, path)
	if err != nil {
		return err
	}
	var tail tailBuffer
	command.Stdout = &tail
	command.Stderr = &tail
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(tail.data))
	}
	return nil
}

func installCommand(manager, path string) (*exec.Cmd, error) {
	var command *exec.Cmd
	env := []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	switch manager {
	case "pacman":
		command = exec.Command("/usr/bin/pacman", "-U", "--noconfirm", path)
	case "dpkg":
		command = exec.Command("/usr/bin/apt-get", "install", "-y", "--no-remove", path)
		env = append(env, "DEBIAN_FRONTEND=noninteractive")
	default:
		return nil, fmt.Errorf("unsupported package manager %q", manager)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Env = env
	return command, nil
}

type installRunner interface {
	Install(ctx context.Context, version string, progress func(phase string)) (Installation, error)
}

// RunInstallHelper is the pkexec entry point for native-update-install.
func RunInstallHelper(ctx context.Context, in io.Reader, out io.Writer) error {
	return runInstallHelper(ctx, in, out, os.Geteuid(), &Installer{})
}

func runInstallHelper(ctx context.Context, in io.Reader, out io.Writer, euid int, installer installRunner) error {
	encoder := json.NewEncoder(out)
	emit := func(value any) { _ = encoder.Encode(value) } // the client may have gone away
	fail := func(err error) error {
		emit(map[string]any{"ok": false, "error": err.Error()})
		return err
	}
	if euid != 0 {
		return fail(errors.New("the update helper must run as root through pkexec"))
	}
	data, err := io.ReadAll(io.LimitReader(in, 4097))
	if err != nil {
		return fail(fmt.Errorf("read request: %w", err))
	}
	if len(data) > 4096 {
		return fail(errors.New("request is too large"))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request struct {
		Version string `json:"version"`
	}
	if err := decoder.Decode(&request); err != nil {
		return fail(fmt.Errorf("invalid request: %w", err))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fail(errors.New("invalid request: unexpected data after the request"))
	}
	installed, err := installer.Install(ctx, request.Version, func(phase string) { emit(map[string]string{"phase": phase}) })
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "version": fmt.Sprintf("%d.%d.%d", installed.Major, installed.Minor, installed.Patch)})
	return nil
}
