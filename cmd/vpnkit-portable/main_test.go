//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestArchHost(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"ID=arch\n", true}, {"ID=endeavouros\nID_LIKE=\"arch\"", true},
		{"ID=debian\n", false}, {"PRETTY_NAME=arch\n", false}, {"ID=archlinux-fake\n", false},
	} {
		if got := archHost(tc.s); got != tc.want {
			t.Errorf("%q: %v", tc.s, got)
		}
	}
}

func TestPayloadIntegrity(t *testing.T) {
	data := []byte("public package fixture")
	valid := manifest{Format: 1, Package: "application.pkg.tar.zst", Architecture: "amd64", SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	for _, name := range []string{"valid", "checksum", "path", "format", "architecture", "missing"} {
		t.Run(name, func(t *testing.T) {
			m := valid
			switch name {
			case "checksum":
				m.SHA256 = "wrong"
			case "path":
				m.Package = "../outside"
			case "format":
				m.Format = 2
			case "architecture":
				m.Architecture = "arm64"
			}
			encoded, _ := json.Marshal(m)
			files := fstest.MapFS{"payload/manifest.json": &fstest.MapFile{Data: encoded}, "payload/application.pkg.tar.zst": &fstest.MapFile{Data: data}}
			if name == "missing" {
				delete(files, "payload/application.pkg.tar.zst")
			}
			_, _, err := readPayload(files)
			if (err == nil) != (name == "valid") {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

func TestInstallationCaller(t *testing.T) {
	lookup := func(uid string) (*user.User, error) { return &user.User{Uid: uid, Username: "fixture-user"}, nil }
	for _, tc := range []struct {
		s, p  string
		valid bool
		name  string
	}{
		{"1000", "", true, "fixture-user"}, {"", "1000", true, "fixture-user"}, {"", "", true, ""},
		{"1000", "1001", false, ""}, {"0", "", false, ""}, {"-1", "", false, ""}, {"01000", "", false, ""},
	} {
		name, err := installationCaller(tc.s, tc.p, lookup)
		if (err == nil) != tc.valid || name != tc.name {
			t.Fatalf("%+v: %q %v", tc, name, err)
		}
	}
}

func TestDesktopOnboarding(t *testing.T) {
	account := &user.User{Uid: "1000", Username: "desktop", HomeDir: "/home/desktop"}
	for _, tc := range []struct {
		name                                 string
		profile, bus                         bool
		invalidProfile, initFail, importFail bool
		calls                                int
		wantError                            bool
		message                              string
	}{
		{name: "fresh install", calls: 1, message: "add a server/profile"},
		{name: "no session", profile: true, calls: 1, message: "logged-in desktop session"},
		{name: "configured desktop", profile: true, bus: true, calls: 2, message: "profile prepared"},
		{name: "bad profile", profile: true, invalidProfile: true, calls: 1, wantError: true},
		{name: "initialization failure", initFail: true, calls: 1, wantError: true},
		{name: "import failure", profile: true, bus: true, importFail: true, calls: 2, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var commands [][]string
			run := func(got *user.User, args []string, session bool) error {
				if got != account {
					t.Fatal("wrong account")
				}
				commands = append(commands, args)
				if session != (len(commands) == 2) {
					t.Fatal("wrong session context")
				}
				if tc.initFail || tc.importFail && session {
					return fmt.Errorf("fixture failure")
				}
				return nil
			}
			stat := func(path string) (fs.FileInfo, error) {
				mode := fs.FileMode(0600)
				name := "profile"
				if strings.HasSuffix(path, "/bus") {
					if path != "/run/user/1000/bus" {
						t.Fatal("wrong session bus")
					}
					if !tc.bus {
						return nil, os.ErrNotExist
					}
					mode = os.ModeSocket
					name = "bus"
				} else {
					if path != "/home/desktop/.local/state/local-vpn-kde/openvpn/client/vpnkit-local.ovpn" {
						t.Fatal("wrong user profile")
					}
					if !tc.profile {
						return nil, os.ErrNotExist
					}
					if tc.invalidProfile {
						mode = os.ModeSymlink
					}
				}
				return (fstest.MapFS{name: &fstest.MapFile{Mode: mode}}).Stat(name)
			}
			outcome, err := onboardDesktop(account, run, stat)
			if (err != nil) != tc.wantError || len(commands) != tc.calls || !strings.Contains(outcome, tc.message) {
				t.Fatalf("outcome=%q err=%v commands=%v", outcome, err, commands)
			}
			if len(commands) == 2 {
				want := []string{"/usr/bin/systemd-run", "--user", "--quiet", "--wait", "--collect", "--pipe", "--setenv=HOME=/home/desktop", "--setenv=XDG_STATE_HOME=/home/desktop/.local/state", "/usr/bin/local-vpn-kde", "networkmanager", "--runtime", "native", "--action", "import", "--yes"}
				if !reflect.DeepEqual(commands[1], want) {
					t.Fatalf("unexpected mutation: %v", commands[1])
				}
			}
		})
	}
}

func TestDesktopOnboardingRejectsInvalidAccount(t *testing.T) {
	for _, account := range []*user.User{nil, {Uid: "0", Username: "root", HomeDir: "/root"}, {Uid: "1000", Username: "desktop", HomeDir: "relative"}, {Uid: "1000", Username: "desktop", HomeDir: "/"}} {
		_, err := onboardDesktop(account, func(*user.User, []string, bool) error { t.Fatal("executed for invalid account"); return nil }, nil)
		if err == nil {
			t.Fatal("invalid account accepted")
		}
	}
}
