//go:build linux

package localvpn

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var (
	nativeDeliveryProfileOnce sync.Once
	nativeDeliveryProfile     []byte
	nativeDeliveryProfileErr  error
)

func testNativeDeliveryProfile(t *testing.T) []byte {
	t.Helper()
	nativeDeliveryProfileOnce.Do(func() {
		material, err := generatePKI(1)
		if err != nil {
			nativeDeliveryProfileErr = err
			return
		}
		nativeDeliveryProfile = []byte(fmt.Sprintf(`client
dev tun
proto udp
remote 127.0.0.1 21194
resolv-retry infinite
nobind
persist-key
persist-tun
remote-cert-tls server
auth SHA256
auth-nocache
data-ciphers AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC
data-ciphers-fallback AES-256-CBC
cipher AES-256-GCM
tun-mtu 1400
mssfix 1360
block-ipv6
key-direction 1
verb 3
<ca>
%s</ca>
<cert>
%s</cert>
<key>
%s</key>
<tls-auth>
%s</tls-auth>
`, material["ca.crt"], material["client.crt"], material["client.key"], material["ta.key"]))
	})
	if nativeDeliveryProfileErr != nil {
		t.Fatal(nativeDeliveryProfileErr)
	}
	return append([]byte(nil), nativeDeliveryProfile...)
}

type nativeDeliveryFixture struct {
	root, state, virtualState string
	uid, gid, privilegeUID    uint32
}

func newNativeDeliveryFixture(t *testing.T) nativeDeliveryFixture {
	t.Helper()
	root := t.TempDir()
	uid := uint32(os.Geteuid())
	gid := uint32(os.Getegid())
	ownerUID := uid
	if ownerUID == 0 {
		ownerUID = 12345
	}
	virtualState := "/home/test/.local/state/local-vpn-kde"
	state := filepath.Join(root, strings.TrimPrefix(virtualState, "/"))
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	for path := filepath.Dir(state); path != root; path = filepath.Dir(path) {
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
		if uid == 0 {
			if err := os.Chown(path, int(ownerUID), int(gid)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		if err := os.Chown(state, int(ownerUID), int(gid)); err != nil {
			t.Fatal(err)
		}
	}
	return nativeDeliveryFixture{root: root, state: state, virtualState: virtualState, uid: ownerUID, gid: gid, privilegeUID: uid}
}

func (f nativeDeliveryFixture) deliver(profile []byte) error {
	return deliverNativeClientProfile(f.uid, f.gid, f.virtualState, profile, f.root, f.privilegeUID)
}

func TestDeliverNativeClientProfileCreatesFixedPrivateDestination(t *testing.T) {
	f := newNativeDeliveryFixture(t)
	profile := testNativeDeliveryProfile(t)
	if err := f.deliver(profile); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(f.state, "openvpn/client", nativeClientProfileName)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(profile) {
		t.Fatal("delivered profile differs from input")
	}
	for _, path := range []string{filepath.Join(f.state, "openvpn"), filepath.Join(f.state, "openvpn/client")} {
		assertNativeDeliveryMetadata(t, path, f.uid, f.gid, syscall.S_IFDIR, 0700, 0)
	}
	assertNativeDeliveryMetadata(t, target, f.uid, f.gid, syscall.S_IFREG, 0600, 1)
}

func TestDeliverNativeClientProfileUpdatesExistingValidProfile(t *testing.T) {
	f := newNativeDeliveryFixture(t)
	first := append(testNativeDeliveryProfile(t), []byte("# first\n")...)
	second := append(testNativeDeliveryProfile(t), []byte("# second\n")...)
	if err := f.deliver(first); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.state, "openvpn/client", nativeClientProfileName)
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(second); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustReadNativeDeliveryFile(t, target)) != string(second) {
		t.Fatal("existing profile was not updated")
	}
	if os.SameFile(before, after) {
		t.Fatal("update did not atomically replace the profile inode")
	}
	assertNativeDeliveryMetadata(t, target, f.uid, f.gid, syscall.S_IFREG, 0600, 1)
}

func TestDeliverNativeClientProfileRejectsUnsafeExistingTargetsWithoutMutation(t *testing.T) {
	profile := testNativeDeliveryProfile(t)
	tests := []struct {
		name  string
		setup func(*testing.T, nativeDeliveryFixture, string) func(*testing.T)
	}{
		{
			name: "mode",
			setup: func(t *testing.T, f nativeDeliveryFixture, target string) func(*testing.T) {
				writeNativeDeliveryTarget(t, target, f, 0644)
				return func(t *testing.T) {
					if string(mustReadNativeDeliveryFile(t, target)) != "sentinel" {
						t.Fatal("wrong-mode target content changed")
					}
					assertNativeDeliveryMetadata(t, target, f.uid, f.gid, syscall.S_IFREG, 0644, 1)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, f nativeDeliveryFixture, target string) func(*testing.T) {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("outside-sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					info, err := os.Lstat(target)
					if err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatal("target symlink was replaced")
					}
					if string(mustReadNativeDeliveryFile(t, outside)) != "outside-sentinel" {
						t.Fatal("symlink destination was modified")
					}
				}
			},
		},
		{
			name: "hardlink",
			setup: func(t *testing.T, f nativeDeliveryFixture, target string) func(*testing.T) {
				writeNativeDeliveryTarget(t, target, f, 0600)
				other := filepath.Join(filepath.Dir(target), "foreign-link")
				if err := os.Link(target, other); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					if string(mustReadNativeDeliveryFile(t, target)) != "sentinel" || string(mustReadNativeDeliveryFile(t, other)) != "sentinel" {
						t.Fatal("hard-linked target changed")
					}
					assertNativeDeliveryMetadata(t, target, f.uid, f.gid, syscall.S_IFREG, 0600, 2)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeDeliveryFixture(t)
			client := filepath.Join(f.state, "openvpn/client")
			if err := os.MkdirAll(client, 0700); err != nil {
				t.Fatal(err)
			}
			chownNativeDeliveryTree(t, filepath.Join(f.state, "openvpn"), f.uid, f.gid)
			target := filepath.Join(client, nativeClientProfileName)
			verify := tc.setup(t, f, target)
			if err := f.deliver(profile); err == nil {
				t.Fatal("unsafe existing target was accepted")
			}
			verify(t)
		})
	}
}

func TestDeliverNativeClientProfilePreservesUnsafeDirectoryModes(t *testing.T) {
	profile := testNativeDeliveryProfile(t)

	t.Run("state", func(t *testing.T) {
		f := newNativeDeliveryFixture(t)
		if err := os.Chmod(f.state, 0755); err != nil {
			t.Fatal(err)
		}
		if err := f.deliver(profile); err == nil {
			t.Fatal("non-private state directory was accepted")
		}
		assertNativeDeliveryMetadata(t, f.state, f.uid, f.gid, syscall.S_IFDIR, 0755, 0)
	})

	t.Run("ancestor", func(t *testing.T) {
		f := newNativeDeliveryFixture(t)
		ancestor := filepath.Join(f.root, "home/test")
		if err := os.Chmod(ancestor, 0770); err != nil {
			t.Fatal(err)
		}
		if err := f.deliver(profile); err == nil {
			t.Fatal("group-writable ancestor was accepted")
		}
		assertNativeDeliveryMetadata(t, ancestor, f.uid, f.gid, syscall.S_IFDIR, 0770, 0)
	})
}

func TestDeliverNativeClientProfileDoesNotFollowChildDirectorySymlink(t *testing.T) {
	f := newNativeDeliveryFixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, nativeClientProfileName)
	if err := os.WriteFile(sentinel, []byte("outside-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.state, "openvpn")); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(testNativeDeliveryProfile(t)); err == nil {
		t.Fatal("symlinked child directory was accepted")
	}
	if string(mustReadNativeDeliveryFile(t, sentinel)) != "outside-sentinel" {
		t.Fatal("write escaped the fixed destination")
	}
	if info, err := os.Lstat(filepath.Join(f.state, "openvpn")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("foreign child symlink was mutated")
	}
}

func TestDeliverNativeClientProfileRejectsInvalidAuthorityPathAndProfile(t *testing.T) {
	f := newNativeDeliveryFixture(t)
	profile := testNativeDeliveryProfile(t)

	if err := deliverNativeClientProfile(f.uid, f.gid, f.virtualState, profile, f.root, f.privilegeUID+1); err == nil {
		t.Fatal("wrong effective privilege identity was accepted")
	}
	for _, path := range []string{"relative/local-vpn-kde", "/home/test/../local-vpn-kde", "/home/test/other"} {
		if err := deliverNativeClientProfile(f.uid, f.gid, path, profile, f.root, f.privilegeUID); err == nil {
			t.Fatalf("invalid state path %q was accepted", path)
		}
	}
	if err := deliverNativeClientProfile(0, f.gid, f.virtualState, profile, f.root, f.privilegeUID); err == nil {
		t.Fatal("UID zero owner was accepted")
	}

	dangerous := append([]byte(nil), profile...)
	dangerous = append(dangerous, []byte("up /tmp/attacker\n")...)
	if err := f.deliver(dangerous); err == nil {
		t.Fatal("script directive was accepted")
	}
	wrongRemote := []byte(strings.Replace(string(profile), "remote 127.0.0.1 21194", "remote 127.0.0.1 1194", 1))
	if err := f.deliver(wrongRemote); err == nil {
		t.Fatal("wrong endpoint was accepted")
	}
	if err := f.deliver(make([]byte, nativeClientProfileMaxSize+1)); err == nil {
		t.Fatal("oversized profile was accepted")
	}
	if _, err := os.Stat(filepath.Join(f.state, "openvpn")); !os.IsNotExist(err) {
		t.Fatal("rejected input mutated the state tree")
	}
}

func TestDeliverNativeClientProfilePreservesForeignOwnedTarget(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("foreign ownership setup requires root")
	}
	f := newNativeDeliveryFixture(t)
	client := filepath.Join(f.state, "openvpn/client")
	if err := os.MkdirAll(client, 0700); err != nil {
		t.Fatal(err)
	}
	chownNativeDeliveryTree(t, filepath.Join(f.state, "openvpn"), f.uid, f.gid)
	target := filepath.Join(client, nativeClientProfileName)
	if err := os.WriteFile(target, []byte("foreign-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	foreignUID := f.uid + 1
	if err := os.Chown(target, int(foreignUID), int(f.gid)); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(testNativeDeliveryProfile(t)); err == nil {
		t.Fatal("foreign-owned target was accepted")
	}
	if string(mustReadNativeDeliveryFile(t, target)) != "foreign-sentinel" {
		t.Fatal("foreign-owned target changed")
	}
	assertNativeDeliveryMetadata(t, target, foreignUID, f.gid, syscall.S_IFREG, 0600, 1)
}

func writeNativeDeliveryTarget(t *testing.T, target string, f nativeDeliveryFixture, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(target, []byte("sentinel"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, mode); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(target, int(f.uid), int(f.gid)); err != nil {
			t.Fatal(err)
		}
	}
}

func chownNativeDeliveryTree(t *testing.T, root string, uid, gid uint32) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, int(uid), int(gid))
	}); err != nil {
		t.Fatal(err)
	}
}

func assertNativeDeliveryMetadata(t *testing.T, path string, uid, gid uint32, fileType uint32, mode uint32, links uint64) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	if uint32(st.Mode)&syscall.S_IFMT != fileType || uint32(st.Mode)&07777 != mode || st.Uid != uid || st.Gid != gid {
		t.Fatalf("unexpected metadata for %s: type=%#o mode=%#o uid=%d gid=%d", path, uint32(st.Mode)&syscall.S_IFMT, uint32(st.Mode)&07777, st.Uid, st.Gid)
	}
	if links != 0 && uint64(st.Nlink) != links {
		t.Fatalf("unexpected link count for %s: %d", path, st.Nlink)
	}
}

func mustReadNativeDeliveryFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
