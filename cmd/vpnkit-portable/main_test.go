//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/user"
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
