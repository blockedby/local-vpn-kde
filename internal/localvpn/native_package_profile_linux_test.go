//go:build linux

package localvpn

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func staleProfileFixture(t *testing.T, remote string, withConfig bool) (config string, members func(string) []*user.User) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(home, ".local/state/local-vpn-kde")
	if remote != "" {
		client := filepath.Join(state, "openvpn/client")
		if err := os.MkdirAll(client, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{state, filepath.Join(state, "openvpn"), client} {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		profile := "client\nremote " + remote + "\n<key>\nremote 9.9.9.9 1\n</key>\n"
		if err := os.WriteFile(filepath.Join(client, "vpnkit-local.ovpn"), []byte(profile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config = filepath.Join(root, "config.json")
	if withConfig {
		if err := os.WriteFile(config, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	account := &user.User{Uid: fmt.Sprint(os.Getuid()), Username: "x", HomeDir: home}
	return config, func(string) []*user.User { return []*user.User{account} }
}

func TestReportStaleNativeClientProfile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("fixture requires a non-root owner")
	}
	for _, tc := range []struct {
		name, remote string
		config       bool
		want         bool
	}{
		{"docker leftover", "127.0.0.1 21194", true, true},
		{"correct native profile", nativeClientRemote, true, false},
		{"no profile", "", true, false},
		{"no configuration", "127.0.0.1 21194", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, members := staleProfileFixture(t, tc.remote, tc.config)
			if got := reportStaleNativeClientProfilesAt(config, "unused", members); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestStaleNativeClientProfileRejectsUnsafeFiles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("fixture requires a non-root owner")
	}
	config, members := staleProfileFixture(t, "127.0.0.1 21194", true)
	account := members("")[0]
	file := filepath.Join(account.HomeDir, ".local/state/local-vpn-kde/openvpn/client/vpnkit-local.ovpn")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", file); err != nil {
		t.Fatal(err)
	}
	if reportStaleNativeClientProfilesAt(config, "unused", members) {
		t.Fatal("symlinked profile must not be read")
	}
}

func TestReportStaleNativeClientProfilesOutputIsFixed(t *testing.T) {
	var out bytes.Buffer
	ReportStaleNativeClientProfiles(&out) // non-root: silent
	if os.Geteuid() != 0 && out.Len() != 0 {
		t.Fatal("unexpected output")
	}
	if NativeProfileStaleMessage != "native_profile=stale: re-run native setup" {
		t.Fatal("message changed")
	}
}

func TestNativeEnrolledMembersParsesGroupFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "group")
	if err := os.WriteFile(file, []byte("wheel:x:10:root\nlocal-vpn-kde-users:x:900:nosuchuser-xyz,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := nativeEnrolledMembers(file); len(got) != 0 {
		t.Fatalf("unknown users must be skipped: %v", got)
	}
}
