//go:build linux

package nativeupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func checkerFor(f *fixture) *Checker {
	return &Checker{Client: f.installer.Client, Run: f.run, Exists: f.installer.Exists}
}

func TestRunCommand(t *testing.T) {
	for _, tc := range []struct {
		name, installed, input string
		args                   []string
		wantInstall            bool
		wantOutput             string
	}{
		{"up to date", "0.4.4", "", nil, false, "Up to date."},
		{"check only", "0.4.3", "", []string{"--check"}, false, "An update is available. Install it with: kvpn update"},
		{"declined", "0.4.3", "n\n", nil, false, "Not installed."},
		{"no answer", "0.4.3", "", nil, false, "Not installed."},
		{"confirmed", "0.4.3", "y\n", nil, true, "Updated to 0.4.4."},
		{"yes flag", "0.4.3", "", []string{"--yes"}, true, "Updated to 0.4.4."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "pacman")
			f.installed = tc.installed
			var requested string
			install := func(_ context.Context, version string, progress func(string)) (string, error) {
				requested = version
				progress("installing")
				return version, nil
			}
			var out bytes.Buffer
			if err := RunCommand(context.Background(), tc.args, strings.NewReader(tc.input), &out, checkerFor(f), install, Hooks{}); err != nil {
				t.Fatal(err)
			}
			if (requested != "") != tc.wantInstall || tc.wantInstall && requested != "0.4.4" {
				t.Fatalf("install requested %q", requested)
			}
			if !strings.Contains(out.String(), tc.wantOutput) || !strings.Contains(out.String(), "Latest:    0.4.4") {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestRunCommandOffersGatewayRestart(t *testing.T) {
	for _, tc := range []struct {
		name, input  string
		args         []string
		running      bool
		runningErr   error
		notRunning   bool
		restartErr   error
		wantRestart  bool
		wantOutput   string
		wantErr      string
		wantQuestion bool
	}{
		{name: "gateway stopped", input: "y\n", running: false, wantOutput: "Updated to 0.4.4.\n"},
		{name: "restart accepted", input: "y\ny\n", running: true, wantRestart: true, wantQuestion: true, wantOutput: "Updated to 0.4.4 and restarted the VPN gateway."},
		{name: "restart declined", input: "y\nn\n", running: true, wantQuestion: true, wantOutput: "switches to the new version at its next restart (kvpn restart) or reboot"},
		{name: "no answer", input: "y\n", running: true, wantQuestion: true, wantOutput: "switches to the new version at its next restart (kvpn restart) or reboot"},
		{name: "yes keeps gateway", args: []string{"--yes"}, running: true, wantOutput: "switches to the new version at its next restart (kvpn restart) or reboot"},
		{name: "restart flag", args: []string{"--yes", "--restart"}, running: true, wantRestart: true, wantOutput: "restarted the VPN gateway"},
		{name: "restart fails", args: []string{"--yes", "--restart"}, running: true, restartErr: errors.New("gateway start failed"), wantRestart: true, wantErr: "restarting the VPN gateway failed: gateway start failed"},
		{name: "unknown state still offers", input: "y\ny\n", runningErr: errors.New("bus timeout"), wantRestart: true, wantQuestion: true, wantOutput: "Could not check the VPN gateway (bus timeout)"},
		{name: "stopped before restart", args: []string{"--yes", "--restart"}, running: true, notRunning: true, wantRestart: true, wantOutput: "The VPN gateway is not running; it starts on the new version."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "pacman")
			f.installed = "0.4.3"
			install := func(_ context.Context, version string, _ func(string)) (string, error) { return version, nil }
			restarted := false
			gateway := Hooks{
				Running: func(context.Context) (bool, error) { return tc.running, tc.runningErr },
				Restart: func(context.Context) (bool, error) { restarted = true; return !tc.notRunning, tc.restartErr },
			}
			var out bytes.Buffer
			err := RunCommand(context.Background(), tc.args, strings.NewReader(tc.input), &out, checkerFor(f), install, gateway)
			if tc.wantErr != "" {
				expectError(t, err, tc.wantErr)
			} else if err != nil {
				t.Fatal(err)
			}
			if restarted != tc.wantRestart {
				t.Fatalf("restarted %v, output %q", restarted, out.String())
			}
			if strings.Contains(out.String(), "Restart it now?") != tc.wantQuestion || !strings.Contains(out.String(), tc.wantOutput) {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestRunCommandOffersToOpenApp(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		args        []string
		interrupt   bool
		wantOpen    bool
		wantPrompt  bool
	}{
		{name: "enter opens", input: "y\n\n", wantOpen: true, wantPrompt: true},
		{name: "other text quits", input: "y\nn\n", wantPrompt: true},
		{name: "unterminated answer counts", input: "y", wantPrompt: true},
		{name: "end of input quits", input: "y\n", wantPrompt: true},
		{name: "yes never asks", input: "\n", args: []string{"--yes"}},
		{name: "ctrl+c quits", input: "y\n", interrupt: true, wantPrompt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "pacman")
			f.installed = "0.4.3"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var input io.Reader = strings.NewReader(tc.input)
			if tc.interrupt {
				// Stdin stays open, as in a terminal; Ctrl+C cancels the context.
				reader, writer := io.Pipe()
				defer writer.Close()
				go func() { _, _ = io.WriteString(writer, tc.input) }()
				input = reader
			}
			install := func(_ context.Context, version string, _ func(string)) (string, error) {
				if tc.interrupt {
					cancel()
				}
				return version, nil
			}
			opened := false
			hooks := Hooks{Running: func(context.Context) (bool, error) { return false, nil }, Restart: func(context.Context) (bool, error) { return true, nil }, OpenApp: func() error { opened = true; return nil }}
			var out bytes.Buffer
			if err := RunCommand(ctx, tc.args, input, &out, checkerFor(f), install, hooks); err != nil {
				t.Fatal(err)
			}
			if opened != tc.wantOpen || strings.Contains(out.String(), "Press Enter to open kvpn") != tc.wantPrompt {
				t.Fatalf("opened %v output %q", opened, out.String())
			}
		})
	}
}

func TestRestartCommand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		running    bool
		err        error
		want       string
		wantCalled bool
	}{
		{"not running", false, nil, "Nothing to restart", true},
		{"restarted", true, nil, "Restarted the VPN gateway.", true},
		{"fails", true, errors.New("start failed"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			gateway := Hooks{Restart: func(context.Context) (bool, error) { called = true; return tc.running, tc.err }}
			var out bytes.Buffer
			err := RestartCommand(context.Background(), &out, gateway)
			if tc.err != nil {
				expectError(t, err, "restarting the VPN gateway failed: start failed")
			} else if err != nil {
				t.Fatal(err)
			}
			if called != tc.wantCalled || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("called %v output %q", called, out.String())
			}
		})
	}
}

func TestYesNeverReadsStdin(t *testing.T) {
	f := newFixture(t, "pacman")
	f.installed = "0.4.3"
	install := func(_ context.Context, version string, _ func(string)) (string, error) { return version, nil }
	input := strings.NewReader("keep\n")
	hooks := Hooks{OpenApp: func() error { t.Fatal("must not open"); return nil }}
	if err := RunCommand(context.Background(), []string{"--yes"}, input, &bytes.Buffer{}, checkerFor(f), install, hooks); err != nil || input.Len() != len("keep\n") {
		t.Fatalf("stdin consumed: %v, %d left", err, input.Len())
	}
}

func TestRunCommandErrors(t *testing.T) {
	f := newFixture(t, "pacman")
	never := func(context.Context, string, func(string)) (string, error) {
		t.Fatal("install must not run")
		return "", nil
	}
	err := RunCommand(context.Background(), []string{"--force"}, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), never, Hooks{})
	expectError(t, err, "only --check, --yes and --restart")

	f.installed = "0.0.0.r278.gabc"
	err = RunCommand(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), never, Hooks{})
	expectError(t, err, "development build")

	checker := &Checker{Client: f.installer.Client, Run: f.run, Exists: func(string) bool { return false }}
	err = RunCommand(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, checker, never, Hooks{})
	expectError(t, err, "not from the Arch or Debian package")

	failing := func(context.Context, string, func(string)) (string, error) { return "", errors.New("offline") }
	f.installed = "0.4.3"
	err = RunCommand(context.Background(), []string{"--yes"}, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), failing, Hooks{})
	expectError(t, err, "update failed: offline")
}

// fakeHelper replaces pkexec with a shell script printing the given protocol output.
func fakeHelper(t *testing.T, script string, gotArgs *[]string) InstallOptions {
	return InstallOptions{Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*gotArgs = append([]string{name}, args...)
		return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; "+script)
	}}
}

func TestStartInstall(t *testing.T) {
	var args []string
	var phases []string
	options := fakeHelper(t, `printf '%s\n' '{"phase":"checking"}' 'noise' '{"phase":"installing"}' '{"ok":true,"version":"0.4.4"}'`, &args)
	version, err := StartInstall(context.Background(), "0.4.4", options, func(p string) { phases = append(phases, p) })
	if err != nil || version != "0.4.4" {
		t.Fatalf("version %q, err %v", version, err)
	}
	if strings.Join(phases, " ") != "checking installing" {
		t.Fatalf("phases %v", phases)
	}
	if strings.Join(args, " ") != "/usr/bin/pkexec --disable-internal-agent "+HelperPath+" native-update-install" {
		t.Fatalf("args %v", args)
	}

	options = fakeHelper(t, `echo '{"ok":false,"error":"checksum mismatch"}'; exit 1`, &args)
	options.TextAgent = true
	_, err = StartInstall(context.Background(), "0.4.4", options, nil)
	expectError(t, err, "checksum mismatch")
	if strings.Contains(strings.Join(args, " "), "--disable-internal-agent") {
		t.Fatal("text agent was disabled")
	}

	options = fakeHelper(t, `exit 126`, &args)
	_, err = StartInstall(context.Background(), "0.4.4", options, nil)
	expectError(t, err, "permission to install the update was not granted")

	options = fakeHelper(t, `exit 0`, &args)
	_, err = StartInstall(context.Background(), "0.4.4", options, nil)
	expectError(t, err, "without a result")
}
