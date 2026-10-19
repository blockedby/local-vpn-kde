//go:build linux

package nativegateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func testProofLocation(t *testing.T) namespaceProofLocation {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	return namespaceProofLocation{
		rootPath:     root,
		relativePath: "identity/namespace.json",
		ownerUID:     uint32(os.Geteuid()),
	}
}

func validTestProof() namespaceIdentity {
	return namespaceIdentity{
		Version:     namespaceIdentityVersion,
		ExpectedDev: 10,
		ExpectedIno: 20,
		InitialDev:  10,
		InitialIno:  30,
	}
}

func proofPath(location namespaceProofLocation) string {
	return filepath.Join(location.rootPath, filepath.FromSlash(location.relativePath))
}

func writeTestProof(t *testing.T, location namespaceProofLocation, data string, mode os.FileMode) {
	t.Helper()
	path := proofPath(location)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceIdentityProofRoundTrip(t *testing.T) {
	location := testProofLocation(t)
	want := validTestProof()
	if err := publishNamespaceIdentityProof(location, want); err != nil {
		t.Fatalf("publishNamespaceIdentityProof: %v", err)
	}
	got, err := readNamespaceIdentityProof(location)
	if err != nil {
		t.Fatalf("readNamespaceIdentityProof: %v", err)
	}
	if got != want {
		t.Fatalf("proof = %+v, want %+v", got, want)
	}
	info, err := os.Lstat(proofPath(location))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		t.Fatalf("proof mode = %v, want regular 0644", info.Mode())
	}
}

func TestVerifyIdentityProofRejectsStaleAndForeignIdentities(t *testing.T) {
	proof := validTestProof()
	expected := unix.Stat_t{Dev: proof.ExpectedDev, Ino: proof.ExpectedIno}
	foreign := unix.Stat_t{Dev: 99, Ino: 100}

	for _, tc := range []struct {
		name            string
		target, current unix.Stat_t
		proof           namespaceIdentity
		ok              bool
	}{
		{name: "valid", target: expected, current: expected, proof: proof, ok: true},
		{name: "stale target", target: foreign, current: expected, proof: proof},
		{name: "foreign current", target: expected, current: foreign, proof: proof},
		{name: "aliased identities", target: expected, current: expected, proof: namespaceIdentity{Version: 1, ExpectedDev: expected.Dev, ExpectedIno: expected.Ino, InitialDev: expected.Dev, InitialIno: expected.Ino}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyIdentityProof(tc.target, tc.current, tc.proof); (err == nil) != tc.ok {
				t.Fatalf("verifyIdentityProof error=%v, want success=%v", err, tc.ok)
			}
		})
	}
}

func TestReadNamespaceIdentityProofRejectsMissing(t *testing.T) {
	location := testProofLocation(t)
	if _, err := readNamespaceIdentityProof(location); err == nil {
		t.Fatal("readNamespaceIdentityProof unexpectedly accepted a missing proof")
	}
}

func TestReadNamespaceIdentityProofRejectsUnsafeMetadata(t *testing.T) {
	encoded, err := json.Marshal(validTestProof())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("mode", func(t *testing.T) {
		location := testProofLocation(t)
		writeTestProof(t, location, string(encoded), 0644)
		if err := os.Chmod(proofPath(location), 0664); err != nil {
			t.Fatal(err)
		}
		if _, err := readNamespaceIdentityProof(location); err == nil {
			t.Fatal("accepted group-writable proof")
		}
	})

	t.Run("forged ownership", func(t *testing.T) {
		location := testProofLocation(t)
		writeTestProof(t, location, string(encoded), 0644)
		fd, err := unix.Open(proofPath(location), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if _, err := validateProofFile(fd, location.ownerUID+1); err == nil {
			t.Fatal("accepted proof owned by a different uid")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		location := testProofLocation(t)
		path := proofPath(location)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(location.rootPath, "target.json")
		if err := os.WriteFile(target, encoded, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := readNamespaceIdentityProof(location); err == nil {
			t.Fatal("accepted symlink proof")
		}
		if err := publishNamespaceIdentityProof(location, validTestProof()); err == nil {
			t.Fatal("publisher replaced unsafe symlink proof")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		location := testProofLocation(t)
		writeTestProof(t, location, string(encoded), 0644)
		if err := os.Link(proofPath(location), filepath.Join(location.rootPath, "second-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := readNamespaceIdentityProof(location); err == nil {
			t.Fatal("accepted multiply-linked proof")
		}
	})

	t.Run("unsafe parent", func(t *testing.T) {
		location := testProofLocation(t)
		writeTestProof(t, location, string(encoded), 0644)
		if err := os.Chmod(filepath.Dir(proofPath(location)), 0775); err != nil {
			t.Fatal(err)
		}
		if _, err := readNamespaceIdentityProof(location); err == nil {
			t.Fatal("accepted proof from writable parent")
		}
	})
}

func TestReadNamespaceIdentityProofRejectsInvalidJSON(t *testing.T) {
	valid, err := json.Marshal(validTestProof())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"unknown field": strings.TrimSuffix(string(valid), "}") + `,"extra":true}`,
		"truncated":     string(valid[:len(valid)-1]),
		"multiple JSON": string(valid) + "\n" + string(valid),
		"oversized":     strings.Repeat(" ", namespaceIdentityMaxSize+1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			location := testProofLocation(t)
			writeTestProof(t, location, data, 0644)
			if _, err := readNamespaceIdentityProof(location); err == nil {
				t.Fatal("accepted invalid proof")
			}
		})
	}
}

func TestNamespaceIdentityValidationRejectsZeroAndEqualIdentities(t *testing.T) {
	zero := validTestProof()
	zero.ExpectedDev = 0
	equal := validTestProof()
	equal.InitialDev = equal.ExpectedDev
	equal.InitialIno = equal.ExpectedIno
	wrongVersion := validTestProof()
	wrongVersion.Version++
	for name, proof := range map[string]namespaceIdentity{
		"zero":          zero,
		"equal":         equal,
		"wrong version": wrongVersion,
	} {
		t.Run(name, func(t *testing.T) {
			if err := proof.validate(); err == nil {
				t.Fatal("invalid proof accepted")
			}
		})
	}
}
