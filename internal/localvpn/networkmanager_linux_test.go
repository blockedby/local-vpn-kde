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
