//go:build linux

package nativeupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixturePackage = "fixture package bytes"

type fixture struct {
	t          *testing.T
	manager    string
	installed  string // version reported before install
	after      string // version reported after install
	latest     string
	metadata   map[string]string
	gate       string
	checksum   string
	installs   []string
	installCtx context.Context
	installErr error
	onInstall  func()
	phases     []string
	installer  *Installer
}

func newFixture(t *testing.T, manager string) *fixture {
	f := &fixture{t: t, manager: manager, installed: "0.4.3", after: "0.4.4", latest: "0.4.4"}
	if manager == "pacman" {
		f.metadata = map[string]string{"Name": PackageName, "Version": "0.4.4-1", "Architecture": "x86_64"}
		f.gate = PackageName + "\n"
	} else {
		f.metadata = map[string]string{"Package": PackageName, "Version": "0.4.4", "Architecture": "amd64"}
		f.gate = "Inst " + PackageName + " [0.4.3] (0.4.4 local)\nConf " + PackageName + " (0.4.4 local)\n"
	}
	sum := sha256.Sum256([]byte(fixturePackage))
	f.checksum = hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	f.installer = &Installer{
		Client: &Client{HTTP: server.Client(), APIBase: server.URL, DownloadBase: server.URL, UserAgent: "test"},
		Run:    f.run,
		Exists: func(path string) bool {
			return path == "/usr/bin/"+map[string]string{"pacman": "pacman", "dpkg": "dpkg-query"}[manager]
		},
		InstallPackage:    f.install,
		BeforeInstall:     func() {},
		WorkRoot:          work,
		Inhibitor:         filepath.Join(root, "blocked"),
		LockPath:          filepath.Join(root, "lock"),
		AllowNonRootOwner: true,
	}
	return f
}

func (f *fixture) asset() string {
	name, _ := AssetName(f.manager, Version{0, 4, 4})
	return name
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repos/" + Repository + "/releases/latest":
		fmt.Fprintf(w, `{"tag_name":"v%s","assets":[{"name":%q,"size":%d},{"name":"SHA256SUMS","size":100}]}`, f.latest, f.asset(), len(fixturePackage))
	case "/" + Repository + "/releases/download/v" + f.latest + "/SHA256SUMS":
		fmt.Fprintf(w, "%s  %s\n", f.checksum, f.asset())
	case "/" + Repository + "/releases/download/v" + f.latest + "/" + f.asset():
		fmt.Fprint(w, fixturePackage)
	default:
		http.NotFound(w, r)
	}
}

func (f *fixture) current() string {
	if len(f.installs) > 0 {
		return f.after
	}
	return f.installed
}

func (f *fixture) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	switch {
	case call == "/usr/bin/pacman -Qqo "+OwnedFile:
		return []byte(PackageName + "\n"), nil
	case call == "/usr/bin/pacman -Q "+PackageName:
		return []byte(PackageName + " " + f.current() + "-1\n"), nil
	case call == "/usr/bin/dpkg-query -S "+OwnedFile:
		return []byte(PackageName + ": " + OwnedFile + "\n"), nil
	case call == "/usr/bin/dpkg-query -W -f=${Version} "+PackageName:
		return []byte(f.current()), nil
	case strings.HasPrefix(call, "/usr/bin/pacman -Qip "), strings.HasPrefix(call, "/usr/bin/dpkg-deb -f "):
		var out strings.Builder
		for key, value := range f.metadata {
			if f.manager == "pacman" {
				fmt.Fprintf(&out, "%-15s : %s\n", key, value)
			} else {
				fmt.Fprintf(&out, "%s: %s\n", key, value)
			}
		}
		return []byte(out.String()), nil
	case strings.HasPrefix(call, "/usr/bin/pacman -U --print"), strings.HasPrefix(call, "/usr/bin/apt-get -s"):
		return []byte(f.gate), nil
	}
	f.t.Fatalf("unexpected command %q", call)
	return nil, nil
}

func (f *fixture) install(ctx context.Context, manager, path string) error {
	data, err := os.ReadFile(path)
	if err != nil || string(data) != fixturePackage || manager != f.manager {
		f.t.Fatalf("install got %q %v (%s)", data, err, manager)
	}
	f.installCtx = ctx
	f.installs = append(f.installs, path)
	if f.onInstall != nil {
		f.onInstall()
	}
	return f.installErr
}

func (f *fixture) runInstall() (Installation, error) {
	return f.runInstallContext(context.Background())
}

func (f *fixture) runInstallContext(ctx context.Context) (Installation, error) {
	return f.installer.Install(ctx, "0.4.4", func(phase string) { f.phases = append(f.phases, phase) })
}

func expectError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %v, want it to contain %q", err, contains)
	}
}

func assertWorkEmpty(t *testing.T, f *fixture) {
	t.Helper()
	entries, err := os.ReadDir(f.installer.WorkRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("download directory not cleaned: %v", entries)
	}
}

func TestInstallHappyPath(t *testing.T) {
	for _, manager := range []string{"pacman", "dpkg"} {
		t.Run(manager, func(t *testing.T) {
			f := newFixture(t, manager)
			got, err := f.runInstall()
			if err != nil {
				t.Fatal(err)
			}
			if got.Major != 0 || got.Minor != 4 || got.Patch != 4 || len(f.installs) != 1 {
				t.Fatalf("installed %+v after %d installs", got, len(f.installs))
			}
			if want := "checking downloading verifying installing done"; strings.Join(f.phases, " ") != want {
				t.Fatalf("phases %v", f.phases)
			}
			assertWorkEmpty(t, f)
		})
	}
}

func TestInstallRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*fixture)
	}{
		{"newer release appeared", "latest release is now 0.4.5", func(f *fixture) { f.latest = "0.4.5" }},
		{"already installed", "already installed", func(f *fixture) { f.installed = "0.4.4" }},
		{"downgrade", "already installed", func(f *fixture) { f.installed = "0.5.0" }},
		{"checksum mismatch", "checksum mismatch", func(f *fixture) { f.checksum = strings.Repeat("0", 64) }},
		{"wrong name", "name", func(f *fixture) { f.metadata["Name"] = "other" }},
		{"wrong version", "version", func(f *fixture) { f.metadata["Version"] = "0.4.5-1" }},
		{"wrong architecture", "architecture", func(f *fixture) { f.metadata["Architecture"] = "aarch64" }},
		{"extra dependency", "also changes other packages (sing-box)", func(f *fixture) { f.gate = "sing-box\n" + PackageName + "\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "pacman")
			tc.change(f)
			_, err := f.runInstall()
			expectError(t, err, tc.want)
			if len(f.installs) != 0 {
				t.Fatal("package was installed")
			}
			assertWorkEmpty(t, f)
		})
	}
}

func TestInstallDpkgGateRefusesRemovals(t *testing.T) {
	f := newFixture(t, "dpkg")
	f.gate += "Remv sing-box [1.13.0]\n"
	_, err := f.runInstall()
	expectError(t, err, "remove sing-box")
	if len(f.installs) != 0 {
		t.Fatal("package was installed")
	}
}

func TestInstallBusyLock(t *testing.T) {
	f := newFixture(t, "pacman")
	f.onInstall = func() {
		second := *f.installer
		_, err := second.Install(context.Background(), "0.4.4", nil)
		expectError(t, err, "another update is already running")
	}
	if _, err := f.runInstall(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRefusesUnsafeWorkRoot(t *testing.T) {
	t.Run("group readable", func(t *testing.T) {
		f := newFixture(t, "pacman")
		if err := os.MkdirAll(f.installer.WorkRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.installer.WorkRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := f.runInstall()
		expectError(t, err, "unsafe download directory")
	})
	t.Run("symlink", func(t *testing.T) {
		f := newFixture(t, "pacman")
		target := t.TempDir()
		if err := os.Symlink(target, f.installer.WorkRoot); err != nil {
			t.Fatal(err)
		}
		_, err := f.runInstall()
		expectError(t, err, "unsafe download directory")
	})
	t.Run("not owned by root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root")
		}
		f := newFixture(t, "pacman")
		f.installer.AllowNonRootOwner = false
		_, err := f.runInstall()
		expectError(t, err, "unsafe download directory")
	})
}

func TestInstallPostChecks(t *testing.T) {
	t.Run("version unchanged", func(t *testing.T) {
		f := newFixture(t, "pacman")
		f.after = "0.4.3"
		_, err := f.runInstall()
		expectError(t, err, "installed version is still 0.4.3-1")
		assertWorkEmpty(t, f)
	})
	t.Run("package manager failed", func(t *testing.T) {
		f := newFixture(t, "pacman")
		f.after = "0.4.3"
		f.installErr = errors.New("exit status 1: conflicting files")
		_, err := f.runInstall()
		expectError(t, err, "conflicting files")
	})
	t.Run("inhibitor left", func(t *testing.T) {
		f := newFixture(t, "pacman")
		f.onInstall = func() {
			if err := os.WriteFile(f.installer.Inhibitor, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := f.runInstall()
		expectError(t, err, "package management is still blocked")
	})
}

func TestInstallSurvivesCallerCancellation(t *testing.T) {
	f := newFixture(t, "pacman")
	ctx, cancel := context.WithCancel(context.Background())
	f.installer.BeforeInstall = cancel
	if _, err := f.runInstallContext(ctx); err != nil {
		t.Fatal(err)
	}
	if f.installCtx.Err() != nil {
		t.Fatal("install context was cancelled with the caller")
	}
}

type fakeInstaller struct {
	version string
	err     error
}

func (f *fakeInstaller) Install(_ context.Context, version string, progress func(string)) (Installation, error) {
	f.version = version
	progress("checking")
	if f.err != nil {
		return Installation{}, f.err
	}
	progress("done")
	return Installation{Major: 0, Minor: 4, Patch: 4}, nil
}

func decodeLines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		lines = append(lines, value)
	}
	return lines
}

func TestRunInstallHelperProtocol(t *testing.T) {
	var out bytes.Buffer
	installer := &fakeInstaller{}
	if err := runInstallHelper(context.Background(), strings.NewReader(`{"version":"0.4.4"}`+"\n"), &out, 0, installer); err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, &out)
	if installer.version != "0.4.4" || lines[0]["phase"] != "checking" || lines[len(lines)-1]["ok"] != true || lines[len(lines)-1]["version"] != "0.4.4" {
		t.Fatalf("unexpected output %v", lines)
	}

	out.Reset()
	err := runInstallHelper(context.Background(), strings.NewReader(`{"version":"0.4.4"}`), &out, 0, &fakeInstaller{err: errors.New("offline")})
	expectError(t, err, "offline")
	if lines := decodeLines(t, &out); lines[len(lines)-1]["ok"] != false || lines[len(lines)-1]["error"] != "offline" {
		t.Fatalf("unexpected failure output %v", lines)
	}
}

func TestRunInstallHelperRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		euid              int
	}{
		{"not root", `{"version":"0.4.4"}`, "must run as root", 1000},
		{"malformed", `{"version":`, "invalid request", 0},
		{"unknown field", `{"version":"0.4.4","url":"https://example.com"}`, "invalid request", 0},
		{"trailing data", `{"version":"0.4.4"} {"version":"0.4.5"}`, "unexpected data", 0},
		{"too large", `{"version":"` + strings.Repeat("1", 5000) + `"}`, "too large", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installer := &fakeInstaller{}
			var out bytes.Buffer
			err := runInstallHelper(context.Background(), strings.NewReader(tc.input), &out, tc.euid, installer)
			expectError(t, err, tc.want)
			if installer.version != "" {
				t.Fatal("installer ran")
			}
			if lines := decodeLines(t, &out); lines[len(lines)-1]["ok"] != false {
				t.Fatalf("unexpected output %v", lines)
			}
		})
	}
}
