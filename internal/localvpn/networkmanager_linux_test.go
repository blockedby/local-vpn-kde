package localvpn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNMLegacyMigrationFailurePreservesCapability(t *testing.T) {
	base := t.TempDir()
	profileDir := filepath.Join(base, "openvpn/client")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "vpnkit-local.ovpn"), []byte("client\ndev tun\nproto udp\nremote 127.0.0.1 21194\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n := NetworkManager{Base: base}
	fingerprint, err := n.profileFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	uuid := "12345678-1234-4234-8234-123456789abc"
	stateDir := filepath.Join(base, "state")
	if err = os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	uuidBytes := []byte(uuid + "\n")
	fingerprintBytes := []byte(fingerprint + "\n")
	for name, data := range map[string][]byte{"networkmanager-uuid": uuidBytes, "networkmanager-profile-fingerprint": fingerprintBytes} {
		if err = os.WriteFile(filepath.Join(stateDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mutations := 0
	n.command = func(_ context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch joined {
		case "-t -f NAME,UUID,TYPE connection show":
			return "vpnkit-local:" + uuid + ":vpn\n", nil
		case "-t -f connection.id,connection.uuid,connection.type,vpn.service-type,vpn.data connection show uuid " + uuid:
			return "vpnkit-local:" + uuid + ":vpn:org.freedesktop.NetworkManager.openvpn:remote = 127.0.0.1:21194\n", nil
		default:
			mutations++
			return "", errors.New("unexpected mutation")
		}
	}
	n.persist = func(int, map[string][]byte) error { return errors.New("injected native commit failure") }
	for _, action := range []string{"status", "import"} {
		var output bytes.Buffer
		if err = n.Run(context.Background(), action, true, &output); err == nil {
			t.Fatal("failed migration reported success")
		}
	}
	if mutations != 0 {
		t.Fatal("failed migration reached NetworkManager mutation")
	}
	for name, want := range map[string][]byte{"networkmanager-uuid": uuidBytes, "networkmanager-profile-fingerprint": fingerprintBytes} {
		got, e := os.ReadFile(filepath.Join(stateDir, name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("legacy ownership changed")
		}
	}
	if _, err = os.Stat(filepath.Join(stateDir, "networkmanager-state")); !os.IsNotExist(err) {
		t.Fatal("failed migration published ownership")
	}
}
func TestTerseInventoryPreservesEscapedNames(t *testing.T) {
	got := splitNMTerse(`work\:vpn:12345678-1234-4234-8234-123456789abc:vpn:Meta`)
	if len(got) != 4 || got[0] != "work:vpn" || !uuidPattern.MatchString(got[1]) {
		t.Fatal("escaped name shifted UUID/type fields")
	}
}

func TestNMRefreshRetainsNewProfileAfterOldDeletion(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost-delete-reply", true: "lost-inventory-reply"}[unreadable], func(t *testing.T) {
			base := t.TempDir()
			if err := os.MkdirAll(filepath.Join(base, "openvpn/client"), 0700); err != nil {
				t.Fatal(err)
			}
			n := NetworkManager{Base: base}
			if err := os.WriteFile(n.profilePath(), []byte("client\ndev tun\nproto udp\nremote 127.0.0.1 21194\n"), 0600); err != nil {
				t.Fatal(err)
			}
			old := "11111111-1111-4111-8111-111111111111"
			newID := "22222222-2222-4222-8222-222222222222"
			cap := nmCapability{UUID: old, Fingerprint: strings.Repeat("0", 64)}
			if err := n.writeCapability(cap); err != nil {
				t.Fatal(err)
			}
			names := map[string]string{old: "vpnkit-local"}
			deleted := false
			n.command = func(_ context.Context, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				switch {
				case joined == "-t -f NAME,UUID,TYPE connection show":
					if deleted && unreadable {
						return "", errors.New("inventory unavailable")
					}
					var out strings.Builder
					for id, name := range names {
						out.WriteString(name + ":" + id + ":vpn\n")
					}
					return out.String(), nil
				case strings.HasPrefix(joined, "connection import "):
					names[newID] = "imported"
					return newID, nil
				case strings.HasPrefix(joined, "connection modify uuid "):
					names[args[3]] = args[5]
					return "", nil
				case strings.HasPrefix(joined, "-t -f connection.id,"):
					id := args[len(args)-1]
					return names[id] + ":" + id + ":vpn:org.freedesktop.NetworkManager.openvpn:remote = 127.0.0.1:21194\n", nil
				case joined == "connection delete uuid "+old:
					delete(names, old)
					deleted = true
					return "", errors.New("reply lost after deletion")
				case joined == "connection delete uuid "+newID:
					delete(names, newID)
					return "", nil
				default:
					return "", errors.New("unexpected command")
				}
			}
			err := n.importProfile(context.Background(), cap, "owned")
			if (err != nil) != unreadable {
				t.Fatalf("unexpected refresh result: %v", err)
			}
			got, err := n.readCapability()
			fingerprint, _ := n.profileFingerprint()
			if err != nil || got.UUID != newID || got.Fingerprint != fingerprint || names[newID] != "vpnkit-local" {
				t.Fatal("cleanup uncertainty destroyed the newly committed profile")
			}
		})
	}
}

func TestNMPreviousInstallationMigration(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-uuid", "changed-profile", "active"} {
		t.Run(scenario, func(t *testing.T) {
			old := NetworkManager{Base: t.TempDir()}
			n := NetworkManager{Base: t.TempDir(), MigrationBase: old.Base}
			for _, manager := range []NetworkManager{old, n} {
				if err := os.MkdirAll(filepath.Dir(manager.profilePath()), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manager.profilePath(), []byte("client\ndev tun\nproto udp\nremote 127.0.0.1 21194\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id := "11111111-1111-4111-8111-111111111111"
			fingerprint, err := old.profileFingerprint()
			if err != nil {
				t.Fatal(err)
			}
			cap := nmCapability{UUID: id, Fingerprint: fingerprint}
			if err = old.writeCapability(cap); err != nil {
				t.Fatal(err)
			}
			if scenario == "changed-profile" {
				if err := os.WriteFile(old.profilePath(), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mutations := 0
			n.command = func(_ context.Context, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				switch joined {
				case "-t -f NAME,UUID,TYPE connection show":
					return "vpnkit-local:" + id + ":vpn\n", nil
				case "-t -f NAME,UUID,TYPE,DEVICE connection show --active":
					if scenario == "active" {
						return "vpnkit-local:" + id + ":vpn:tun0\n", nil
					}
					return "", nil
				case "-t -f connection.id,connection.uuid,connection.type,vpn.service-type,vpn.data connection show uuid " + id:
					if scenario == "wrong-uuid" {
						return "", errors.New("UUID absent")
					}
					return "vpnkit-local:" + id + ":vpn:org.freedesktop.NetworkManager.openvpn:remote = 127.0.0.1:21194\n", nil
				default:
					mutations++
					return "", errors.New("unexpected NetworkManager mutation")
				}
			}
			var out bytes.Buffer
			err = n.Run(context.Background(), "import", true, &out)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("unexpected migration result: %v", err)
			}
			got, err := n.readCapability()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "valid" && got != cap || scenario != "valid" && got.UUID != "" {
				t.Fatal("incorrect ownership migration")
			}
			if unchanged, err := old.readCapability(); err != nil || unchanged != cap {
				t.Fatal("previous installation was modified")
			}
			if mutations != 0 {
				t.Fatal("migration modified a NetworkManager profile")
			}
		})
	}
}
