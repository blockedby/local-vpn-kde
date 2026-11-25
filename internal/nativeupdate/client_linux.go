//go:build linux

package nativeupdate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// HelperPath is the fixed executable behind the org.local-vpn-kde.update action.
const HelperPath = "/usr/libexec/local-vpn-kde/local-vpn-kde.bin"

// CheckResult compares the installed package with the latest release.
type CheckResult struct {
	Installed Installation
	Latest    Version
	Available bool
}

// InstalledVersion formats the installed upstream version without pkgrel.
func (r CheckResult) InstalledVersion() string {
	return fmt.Sprintf("%d.%d.%d", r.Installed.Major, r.Installed.Minor, r.Installed.Patch)
}

// Checker is the unprivileged half: it reads package state and release metadata.
type Checker struct {
	Client *Client
	Run    Runner
	Exists func(string) bool
}

func (c *Checker) Check(ctx context.Context) (CheckResult, error) {
	if c.Client == nil {
		c.Client = NewClient("local-vpn-kde-updater")
	}
	if c.Run == nil {
		c.Run = ExecRunner
	}
	if c.Exists == nil {
		c.Exists = func(path string) bool { _, err := os.Stat(path); return err == nil }
	}
	installed, err := DetectInstallation(ctx, c.Run, c.Exists)
	if err != nil {
		return CheckResult{}, err
	}
	release, err := c.Client.Latest(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	v := release.Version
	return CheckResult{Installed: installed, Latest: v, Available: installed.Older(v.Major, v.Minor, v.Patch)}, nil
}

// PhaseText describes a helper phase for people.
func PhaseText(phase string) string {
	switch phase {
	case "checking":
		return "Checking the release…"
	case "downloading":
		return "Downloading…"
	case "verifying":
		return "Verifying the package…"
	case "installing":
		return "Installing (the VPN stays connected)…"
	case "done":
		return "Installed."
	}
	return phase
}

// InstallOptions controls how the privileged helper is started.
type InstallOptions struct {
	// TextAgent lets pkexec ask for a password in the terminal. The TUI
	// disables it because a prompt would corrupt its screen.
	TextAgent bool
	Command   func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// StartInstall runs the helper through pkexec and reports its phases.
func StartInstall(ctx context.Context, version string, options InstallOptions, progress func(phase string)) (string, error) {
	if os.Geteuid() == 0 {
		return "", errors.New("run the update as your desktop user; it asks for permission itself")
	}
	if progress == nil {
		progress = func(string) {}
	}
	command := options.Command
	if command == nil {
		command = exec.CommandContext
	}
	args := []string{HelperPath, "native-update-install"}
	if !options.TextAgent {
		args = append([]string{"--disable-internal-agent"}, args...)
	}
	cmd := command(ctx, "/usr/bin/pkexec", args...)
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start update helper: %w", err)
	}
	request, _ := json.Marshal(map[string]string{"version": version})
	_, _ = stdin.Write(append(request, '\n'))
	stdin.Close()

	var final struct {
		Phase   string `json:"phase"`
		OK      *bool  `json:"ok"`
		Version string `json:"version"`
		Error   string `json:"error"`
	}
	result := final
	scanner := bufio.NewScanner(io.LimitReader(stdout, 1<<20))
	for scanner.Scan() {
		line := final
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		if line.Phase != "" {
			progress(line.Phase)
		}
		if line.OK != nil {
			result = line
		}
	}
	waitErr := cmd.Wait()
	if result.OK != nil {
		if *result.OK {
			return result.Version, nil
		}
		return "", errors.New(result.Error)
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) && (exit.ExitCode() == 126 || exit.ExitCode() == 127) {
		return "", errors.New("permission to install the update was not granted")
	}
	if waitErr != nil {
		return "", fmt.Errorf("update helper failed: %w", waitErr)
	}
	return "", errors.New("update helper ended without a result")
}

// RunCommand implements `local-vpn-kde update [--check] [--yes]`.
func RunCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, checker *Checker, install func(ctx context.Context, version string, progress func(string)) (string, error)) error {
	checkOnly, yes := false, false
	for _, arg := range args {
		switch arg {
		case "--check":
			checkOnly = true
		case "--yes", "-y":
			yes = true
		case "--help", "-h":
			fmt.Fprintln(stdout, "Usage: local-vpn-kde update [--check] [--yes]\n\nInstalls the latest release of the native package. The VPN stays connected.\n  --check  only report whether an update is available\n  --yes    install without asking")
			return nil
		default:
			return fmt.Errorf("update accepts only --check and --yes, not %q", arg)
		}
	}
	if os.Geteuid() == 0 {
		return errors.New("run the update as your desktop user, not root; it asks for permission itself")
	}
	result, err := checker.Check(ctx)
	if errors.Is(err, ErrNotPackaged) {
		return errors.New("this installation is not from the Arch or Debian package; update it the way you installed it")
	}
	if errors.Is(err, ErrDevelopmentBuild) {
		return fmt.Errorf("this is a development build; install a release package by hand (%w)", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed: %s\nLatest:    %s\n", result.InstalledVersion(), result.Latest)
	if !result.Available {
		fmt.Fprintln(stdout, "Up to date.")
		return nil
	}
	if checkOnly {
		fmt.Fprintln(stdout, "An update is available. Install it with: local-vpn-kde update")
		return nil
	}
	if !yes {
		fmt.Fprintf(stdout, "Install %s now? The VPN stays connected. [y/N] ", result.Latest)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Not installed.")
			return nil
		}
	}
	installed, err := install(ctx, result.Latest.String(), func(phase string) { fmt.Fprintln(stdout, PhaseText(phase)) })
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	fmt.Fprintf(stdout, "Updated to %s. Reopen the app to use it. The VPN gateway switches to the new version at its next restart or reboot.\n", installed)
	return nil
}
