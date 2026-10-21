//go:build linux

package localvpn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPublishNativeConfigurationInitialAndUpdate(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	first := nativeConfigurationTestBundle(1001, "first")
	publisher.serviceUID = 1002

	if err := publishNativeConfiguration(context.Background(), first, publisher); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	assertNativeConfigurationPublished(t, root, first, publisher)

	second := nativeConfigurationTestBundle(1001, "second")
	delete(second.Files, "sing-box/rule-sets/vpnkit-dev-direct.json")
	if err := publishNativeConfiguration(context.Background(), second, publisher); err != nil {
		t.Fatalf("update publish: %v", err)
	}
	assertNativeConfigurationPublished(t, root, second, publisher)
	if _, err := os.Stat(filepath.Join(root, nativeConfigurationTarget, "sing-box/rule-sets/vpnkit-dev-direct.json")); !os.IsNotExist(err) {
		t.Fatalf("removed optional file still exists: %v", err)
	}
	assertNoNativeConfigurationStages(t, root)
}

func TestPublishNativeConfigurationAcceptsProtectedEmptyTmpfilesTree(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	target := filepath.Join(root, nativeConfigurationTarget)
	for _, dir := range []string{"", "sing-box", "sing-box/rule-sets", "openvpn", "openvpn/pki"} {
		if err := os.Mkdir(filepath.Join(target, dir), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(target, dir), 0750); err != nil {
			t.Fatal(err)
		}
	}
	bundle := nativeConfigurationTestBundle(1001, "empty-tree")
	if err := publishNativeConfiguration(context.Background(), bundle, publisher); err != nil {
		t.Fatalf("publish over empty tree: %v", err)
	}
	assertNativeConfigurationPublished(t, root, bundle, publisher)
}

func TestPublishNativeConfigurationRejectsInvalidBundlesWithoutChangingPrior(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	original := nativeConfigurationTestBundle(1001, "original")
	if err := publishNativeConfiguration(context.Background(), original, publisher); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, nativeConfigurationTarget, "sing-box/config.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(NativeConfigurationBundle){
		"missing required": func(bundle NativeConfigurationBundle) { delete(bundle.Files, "openvpn/server.conf") },
		"unknown":          func(bundle NativeConfigurationBundle) { bundle.Files["other/payload"] = []byte("secret-marker") },
		"traversal":        func(bundle NativeConfigurationBundle) { bundle.Files["../server.key"] = []byte("secret-marker") },
		"invalid JSON": func(bundle NativeConfigurationBundle) {
			bundle.Files["sing-box/config.json"] = []byte(`{"secret-marker":`)
		},
		"oversize file": func(bundle NativeConfigurationBundle) {
			bundle.Files["vibe-vpn/sub_url"] = make([]byte, nativeConfigurationMaxFile+1)
		},
		"oversize total": func(bundle NativeConfigurationBundle) {
			large := make([]byte, nativeConfigurationMaxFile)
			bundle.Files["vibe-vpn/config.yaml"] = large
			bundle.Files["vibe-vpn/sub_url"] = large
			bundle.Files["openvpn/server.conf"] = large
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneNativeConfigurationBundle(nativeConfigurationTestBundle(1001, "candidate"))
			mutate(candidate)
			err := publishNativeConfiguration(context.Background(), candidate, publisher)
			if err == nil {
				t.Fatal("publish succeeded")
			}
			if strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("error exposed payload content: %v", err)
			}
			after, readErr := os.ReadFile(configPath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("prior tree changed: data=%q err=%v", after, readErr)
			}
			assertNoNativeConfigurationStages(t, root)
		})
	}
}

func TestPublishNativeConfigurationPreservesTamperedOrForeignTree(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"foreign file": func(t *testing.T, target string) {
			if err := os.WriteFile(filepath.Join(target, "foreign"), []byte("keep-me"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, target string) {
			name := filepath.Join(target, "openvpn/server.conf")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../vibe-vpn/sub_url", name); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, target string) {
			name := filepath.Join(target, "openvpn/server.conf")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(target, "vibe-vpn/sub_url"), name); err != nil {
				t.Fatal(err)
			}
		},
		"digest": func(t *testing.T, target string) {
			name := filepath.Join(target, "vibe-vpn/sub_url")
			if err := os.WriteFile(name, []byte("tampered"), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(name, 0640); err != nil {
				t.Fatal(err)
			}
		},
		"manifest owner": func(t *testing.T, target string) {
			name := filepath.Join(target, nativeConfigurationManifest)
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var manifest nativeConfigurationManifestData
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.OwnerUID++
			data, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(name, 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			root, publisher := newNativeConfigurationTestPublisher(t)
			original := nativeConfigurationTestBundle(1001, "original")
			if err := publishNativeConfiguration(context.Background(), original, publisher); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, nativeConfigurationTarget)
			tamper(t, target)
			if err := publishNativeConfiguration(context.Background(), nativeConfigurationTestBundle(1001, "replacement"), publisher); err == nil {
				t.Fatal("publish accepted unsafe existing tree")
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("existing tree was removed: %v", err)
			}
			if name == "foreign file" {
				data, err := os.ReadFile(filepath.Join(target, "foreign"))
				if err != nil || string(data) != "keep-me" {
					t.Fatalf("foreign content was not preserved: %q, %v", data, err)
				}
			}
			assertNoNativeConfigurationStages(t, root)
		})
	}
}

func TestPublishNativeConfigurationCancellationLeavesPriorIntact(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	original := nativeConfigurationTestBundle(1001, "old")
	if err := publishNativeConfiguration(context.Background(), original, publisher); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, nativeConfigurationTarget, "sing-box/config.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &nativeConfigurationTestContext{Context: context.Background(), third: func() error { return context.Canceled }}
	if err := publishNativeConfiguration(ctx, nativeConfigurationTestBundle(1001, "new"), publisher); err == nil {
		t.Fatal("canceled publish succeeded")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("prior tree changed after cancellation: %q, %v", after, err)
	}
	assertNoNativeConfigurationStages(t, root)
}

func TestPublishNativeConfigurationReportsCommittedStateWhenCleanupFails(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	original := nativeConfigurationTestBundle(1001, "old")
	if err := publishNativeConfiguration(context.Background(), original, publisher); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, nativeConfigurationTarget)
	ctx := &nativeConfigurationTestContext{Context: context.Background(), third: func() error {
		return os.WriteFile(filepath.Join(target, "foreign-after-verification"), []byte("preserve"), 0600)
	}}
	replacement := nativeConfigurationTestBundle(1001, "new")
	err := publishNativeConfiguration(ctx, replacement, publisher)
	var committed *NativeConfigurationCommittedError
	if err == nil || !strings.Contains(err.Error(), "was committed") || !errors.As(err, &committed) {
		t.Fatalf("cleanup failure did not report typed committed state: %v", err)
	}
	assertNativeConfigurationPublished(t, root, replacement, publisher)
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	preserved := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".local-vpn-kde.new-") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(root, entry.Name(), "foreign-after-verification"))
		if readErr == nil && string(data) == "preserve" {
			preserved = true
		}
	}
	if !preserved {
		t.Fatal("foreign content from previous tree was not preserved for manual cleanup")
	}
}

func TestPublishNativeConfigurationNoReplaceFailurePreservesInterferingTarget(t *testing.T) {
	root, publisher := newNativeConfigurationTestPublisher(t)
	target := filepath.Join(root, nativeConfigurationTarget)
	ctx := &nativeConfigurationTestContext{Context: context.Background(), third: func() error {
		if err := os.Mkdir(target, 0750); err != nil {
			return err
		}
		return nil
	}}
	if err := publishNativeConfiguration(ctx, nativeConfigurationTestBundle(1001, "candidate"), publisher); err == nil {
		t.Fatal("publish unexpectedly replaced an intervening target")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("intervening target was changed: entries=%v err=%v", entries, err)
	}
	assertNoNativeConfigurationStages(t, root)
}

func TestPrepareNativeConfigurationRejectsPrivilegedOwners(t *testing.T) {
	bundle := nativeConfigurationTestBundle(1001, "owner")
	bundle.OwnerUID = 0
	if _, _, err := prepareNativeConfigurationBundle(bundle, 1002); err == nil {
		t.Fatal("root owner accepted")
	}
	bundle.OwnerUID = 1002
	if _, _, err := prepareNativeConfigurationBundle(bundle, 1002); err == nil {
		t.Fatal("service owner accepted")
	}
}

func nativeConfigurationTestBundle(owner uint32, marker string) NativeConfigurationBundle {
	return NativeConfigurationBundle{OwnerUID: owner, Files: map[string][]byte{
		"sing-box/config.json":                        []byte(`{"route":{"final":"` + marker + `"}}`),
		"sing-box/rule-sets/vpnkit-adblock.json":      []byte(`{"version":1,"rules":[]}`),
		"sing-box/rule-sets/vpnkit-dev-direct.json":   []byte(`{"version":1,"rules":[]}`),
		"sing-box/rule-sets/geoip-ru.json":            []byte(`{"version":1,"rules":[]}`),
		"sing-box/rule-sets/geosite-category-ru.json": []byte(`{"version":1,"rules":[]}`),
		"vibe-vpn/config.yaml":                        []byte("mode: " + marker + "\n"),
		"vibe-vpn/sub_url":                            []byte("https://example.invalid/" + marker + "\n"),
		"openvpn/server.conf":                         []byte("dev tun # " + marker + "\n"),
		"openvpn/pki/ca.crt":                          []byte("CA-" + marker),
		"openvpn/pki/server.crt":                      []byte("CERT-" + marker),
		"openvpn/pki/server.key":                      []byte("KEY-" + marker),
		"openvpn/pki/ta.key":                          []byte("TA-" + marker),
	}}
}

func cloneNativeConfigurationBundle(bundle NativeConfigurationBundle) NativeConfigurationBundle {
	clone := NativeConfigurationBundle{OwnerUID: bundle.OwnerUID, Files: make(map[string][]byte, len(bundle.Files))}
	for name, data := range bundle.Files {
		clone.Files[name] = append([]byte(nil), data...)
	}
	return clone
}

func newNativeConfigurationTestPublisher(t *testing.T) (string, nativeConfigurationPublisher) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	return root, nativeConfigurationPublisher{rootDir: root, rootUID: uid, serviceUID: 1002, serviceGID: gid}
}

func assertNativeConfigurationPublished(t *testing.T, root string, bundle NativeConfigurationBundle, publisher nativeConfigurationPublisher) {
	t.Helper()
	target := filepath.Join(root, nativeConfigurationTarget)
	for name, want := range bundle.Files {
		full := filepath.Join(target, filepath.FromSlash(name))
		got, err := os.ReadFile(full)
		if err != nil || string(got) != string(want) {
			t.Fatalf("published %s mismatch: %q, %v", name, got, err)
		}
		assertNativeConfigurationModeAndOwnership(t, full, 0640, publisher)
	}
	manifestPath := filepath.Join(target, nativeConfigurationManifest)
	assertNativeConfigurationModeAndOwnership(t, manifestPath, 0600, publisher)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest nativeConfigurationManifestData
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != nativeConfigurationVersion || manifest.Layout != nativeConfigurationLayout || manifest.OwnerUID != bundle.OwnerUID {
		t.Fatalf("unexpected manifest identity: %+v", manifest)
	}
	for name, contents := range bundle.Files {
		sum := sha256.Sum256(contents)
		if manifest.Files[name] != hex.EncodeToString(sum[:]) {
			t.Fatalf("manifest digest for %s = %q", name, manifest.Files[name])
		}
	}
	for _, dir := range []string{"", "sing-box", "sing-box/rule-sets", "vibe-vpn", "openvpn", "openvpn/pki"} {
		assertNativeConfigurationModeAndOwnership(t, filepath.Join(target, dir), 0750, publisher)
	}
}

func assertNativeConfigurationModeAndOwnership(t *testing.T, name string, mode os.FileMode, publisher nativeConfigurationPublisher) {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", name, info.Mode().Perm(), mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != publisher.rootUID || stat.Gid != publisher.serviceGID {
		t.Fatalf("%s ownership is not %d:%d", name, publisher.rootUID, publisher.serviceGID)
	}
}

type nativeConfigurationTestContext struct {
	context.Context
	calls int
	third func() error
}

func (c *nativeConfigurationTestContext) Err() error {
	c.calls++
	if c.calls == 3 {
		return c.third()
	}
	return nil
}

func assertNoNativeConfigurationStages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".local-vpn-kde.new-") {
			t.Fatalf("staging tree was left behind: %s", entry.Name())
		}
	}
}
