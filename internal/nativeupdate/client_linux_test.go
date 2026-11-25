//go:build linux

package nativeupdate

import (
	"bytes"
	"context"
	"errors"
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
			if err := RunCommand(context.Background(), tc.args, strings.NewReader(tc.input), &out, checkerFor(f), install); err != nil {
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

func TestRunCommandErrors(t *testing.T) {
	f := newFixture(t, "pacman")
	never := func(context.Context, string, func(string)) (string, error) {
		t.Fatal("install must not run")
		return "", nil
	}
	err := RunCommand(context.Background(), []string{"--force"}, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), never)
	expectError(t, err, "only --check and --yes")

	f.installed = "0.0.0.r278.gabc"
	err = RunCommand(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), never)
	expectError(t, err, "development build")

	checker := &Checker{Client: f.installer.Client, Run: f.run, Exists: func(string) bool { return false }}
	err = RunCommand(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}, checker, never)
	expectError(t, err, "not from the Arch or Debian package")

	failing := func(context.Context, string, func(string)) (string, error) { return "", errors.New("offline") }
	f.installed = "0.4.3"
	err = RunCommand(context.Background(), []string{"--yes"}, strings.NewReader(""), &bytes.Buffer{}, checkerFor(f), failing)
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
