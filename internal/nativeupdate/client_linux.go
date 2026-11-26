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
	"syscall"

	"golang.org/x/sys/unix"
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

// Hooks connect the update command to the installed system. Running and
// Restart move a running VPN gateway onto the newly installed version; OpenApp
// replaces the process with the new interface. Nil hooks are never offered.
type Hooks struct {
	// Running reports an error when the state is unknown.
	Running func(ctx context.Context) (bool, error)
	// Restart reports false when the gateway turned out not to be running.
	Restart func(ctx context.Context) (bool, error)
	OpenApp func() error
}

const gatewayUnit = "local-vpn-kde-gateway.service"

// InstalledHooks act through the newly installed executables, so the new
// version's own lifecycle code and unit expectations do the work.
func InstalledHooks() Hooks {
	hooks := Hooks{
		Running: func(ctx context.Context) (bool, error) {
			out, err := ExecRunner(ctx, "/usr/bin/systemctl", "show", "--property=ActiveState", "--value", gatewayUnit)
			if err != nil {
				return false, fmt.Errorf("check the VPN gateway: %w", err)
			}
			switch state := strings.TrimSpace(string(out)); state {
			case "inactive", "":
				return false, nil
			default:
				return true, nil
			}
		},
		Restart: func(context.Context) (bool, error) {
			// The helper owns the transaction: it checks everything before
			// disconnecting, bounds each step and compensates on failure. It
			// runs in its own process group so a terminal Ctrl+C cannot stop it
			// between stopping and starting the gateway, and it is never killed.
			cmd := exec.Command(HelperPath, "native-desktop", "--action", "restart", "--yes")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stdout, stderr limitedOutput
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				if message := lastLine(stderr.Bytes()); message != "" {
					return false, errors.New(message)
				}
				return false, err
			}
			return !strings.Contains(string(stdout.Bytes()), "phase=gateway-not-running"), nil
		},
	}
	if _, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS); err == nil {
		hooks.OpenApp = func() error { return syscall.Exec(Launcher, []string{"kvpn"}, os.Environ()) }
	}
	return hooks
}

// Launcher opens the interface when started without arguments.
const Launcher = "/usr/bin/kvpn"

// lineReader lets a prompt give up on Ctrl+C even though the read itself
// cannot be interrupted: the reading goroutine just stays parked on stdin. It
// starts on the first prompt, so runs without prompts never touch stdin.
type lineReader struct {
	input io.Reader
	lines chan string
}

func (r *lineReader) start() {
	r.lines = make(chan string)
	go func() {
		defer close(r.lines)
		reader := bufio.NewReader(r.input)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				r.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
}

// read returns false at end of input or when ctx is cancelled.
func (r *lineReader) read(ctx context.Context) (string, bool) {
	if r.lines == nil {
		r.start()
	}
	select {
	case line, ok := <-r.lines:
		return line, ok
	case <-ctx.Done():
		return "", false
	}
}

func lastLine(data []byte) string {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// RestartCommand implements `kvpn restart`: it moves a running gateway onto the
// installed version and reconnects the VPN if it was connected.
func RestartCommand(ctx context.Context, stdout io.Writer, gateway Hooks) error {
	if os.Geteuid() == 0 {
		return errors.New("run the restart as your desktop user, not root")
	}
	if gateway.Restart == nil {
		return errors.New("restart is available only in the installed package")
	}
	fmt.Fprintln(stdout, "Restarting the VPN gateway (the VPN drops for a few seconds)…")
	restarted, err := gateway.Restart(ctx)
	if err != nil {
		return fmt.Errorf("restarting the VPN gateway failed: %w", err)
	}
	if !restarted {
		fmt.Fprintln(stdout, "The VPN gateway is not running. Nothing to restart; it starts on the installed version.")
		return nil
	}
	fmt.Fprintln(stdout, "Restarted the VPN gateway.")
	return nil
}

const usage = `Usage: kvpn update [--check] [--yes] [--restart]

Installs the latest release of the native package. The VPN stays connected.
If the VPN gateway is running, it offers to restart it on the new version;
the VPN then drops for a few seconds and reconnects.
  --check    only report whether an update is available
  --yes      install without asking (the gateway is not restarted)
  --restart  restart a running gateway after installing, without asking`

// RunCommand implements `kvpn update [--check] [--yes] [--restart]`.
func RunCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, checker *Checker, install func(ctx context.Context, version string, progress func(string)) (string, error), gateway Hooks) error {
	checkOnly, yes, restart := false, false, false
	for _, arg := range args {
		switch arg {
		case "--check":
			checkOnly = true
		case "--yes", "-y":
			yes = true
		case "--restart":
			restart = true
		case "--help", "-h":
			fmt.Fprintln(stdout, usage)
			return nil
		default:
			return fmt.Errorf("update accepts only --check, --yes and --restart, not %q", arg)
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
		fmt.Fprintln(stdout, "An update is available. Install it with: kvpn update")
		return nil
	}
	answers := &lineReader{input: stdin}
	ask := func(question string) bool {
		fmt.Fprintf(stdout, "%s [y/N] ", question)
		answer, ok := answers.read(ctx)
		if !ok {
			fmt.Fprintln(stdout)
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes"
	}
	// Interactive runs end by offering the new interface; Ctrl+C or end of
	// input simply quits.
	offerApp := func() error {
		if yes || gateway.OpenApp == nil {
			return nil
		}
		fmt.Fprint(stdout, "Press Enter to open kvpn, or Ctrl+C to quit. ")
		line, ok := answers.read(ctx)
		if !ok {
			fmt.Fprintln(stdout)
			return nil
		}
		if strings.TrimSpace(line) != "" {
			return nil
		}
		return gateway.OpenApp()
	}
	if !yes && !ask(fmt.Sprintf("Install %s now? The VPN stays connected.", result.Latest)) {
		fmt.Fprintln(stdout, "Not installed.")
		return nil
	}
	installed, err := install(ctx, result.Latest.String(), func(phase string) { fmt.Fprintln(stdout, PhaseText(phase)) })
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	running := false
	if gateway.Running != nil && gateway.Restart != nil {
		var err error
		if running, err = gateway.Running(ctx); err != nil {
			// Unknown is not "stopped": offer the restart; the helper decides.
			fmt.Fprintf(stdout, "Could not check the VPN gateway (%v).\n", err)
			running = true
		}
	}
	if !running {
		fmt.Fprintf(stdout, "Updated to %s.\n", installed)
		return offerApp()
	}
	if !restart && !yes {
		restart = ask(fmt.Sprintf("Updated to %s. The running VPN gateway is still the old version.\nRestart it now? The VPN drops for a few seconds and reconnects.", installed))
	}
	if !restart {
		fmt.Fprintf(stdout, "Updated to %s. The VPN gateway switches to the new version at its next restart (kvpn restart) or reboot.\n", installed)
		return offerApp()
	}
	fmt.Fprintln(stdout, "Restarting the VPN gateway…")
	restarted, err := gateway.Restart(ctx)
	if err != nil {
		return fmt.Errorf("updated to %s, but restarting the VPN gateway failed: %w", installed, err)
	}
	if !restarted {
		fmt.Fprintf(stdout, "Updated to %s. The VPN gateway is not running; it starts on the new version.\n", installed)
		return offerApp()
	}
	fmt.Fprintf(stdout, "Updated to %s and restarted the VPN gateway.\n", installed)
	return offerApp()
}
