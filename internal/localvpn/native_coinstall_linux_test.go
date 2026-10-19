package localvpn

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeImportPreservesActiveDockerProfileAndCapability(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	docker := NetworkManager{Base: base}
	native := NetworkManager{Base: base, Native: true}
	old := "11111111-1111-4111-8111-111111111111"
	fresh := "22222222-2222-4222-8222-222222222222"
	if err := os.MkdirAll(filepath.Dir(native.profilePath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native.profilePath(), []byte("client\ndev tun\nproto udp\nremote 127.0.0.1 31194\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cap := nmCapability{UUID: old, Fingerprint: strings.Repeat("a", 64)}
	if err := docker.writeCapability(cap); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{old: "vpnkit-local"}
	native.command = func(_ context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "-t -f NAME,UUID,TYPE connection show":
			var out strings.Builder
			for id, name := range names {
				fmt.Fprintf(&out, "%s:%s:vpn\n", name, id)
			}
			return out.String(), nil
		case joined == "-t -f NAME,UUID,TYPE,DEVICE connection show --active":
			return "vpnkit-local:" + old + ":vpn:tun0\n", nil
		case strings.HasPrefix(joined, "connection import "):
			names[fresh] = "vpnkit-local"
			return fresh, nil
		case strings.HasPrefix(joined, "connection modify uuid "):
			if args[3] != fresh {
				t.Fatal("mutated Docker profile", args)
			}
			names[fresh] = args[5]
			return "", nil
		case strings.HasPrefix(joined, "-t -f connection.id,"):
			id := args[len(args)-1]
			return names[id] + ":" + id + ":vpn:org.freedesktop.NetworkManager.openvpn:remote = 127.0.0.1:31194\n", nil
		default:
			t.Fatalf("unexpected command: %s", joined)
			return "", nil
		}
	}
	var out bytes.Buffer
	if err := native.Run(ctx, "import", true, &out); err != nil {
		t.Fatal(err)
	}
	if names[old] != "vpnkit-local" || names[fresh] != "vpnkit-native" {
		t.Fatal(names)
	}
	got, err := docker.readCapability()
	if err != nil || got != cap {
		t.Fatal("Docker capability changed", got, err)
	}
	got, err = native.readCapability()
	if err != nil || got.UUID != fresh {
		t.Fatal("native capability missing", got, err)
	}
	if !strings.Contains(out.String(), "connection=vpnkit-native") {
		t.Fatal(out.String())
	}
}

func TestNativeAutostartPreservesDockerUnit(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.ConfigHome, "systemd/user", autostartUnit)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configHome := a.ConfigHome
	a = testAutostart(t)
	a.ConfigHome = configHome
	a.Runtime = "native"
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Runtime: "native"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Docker autostart changed", err)
	}
	if _, err := os.Stat(filepath.Join(a.ConfigHome, "systemd/user", a.unitName())); err != nil {
		t.Fatal(err)
	}
}

func TestNativeConnectGuardPreservesOtherVPN(t *testing.T) {
	n := NetworkManager{Native: true}
	n.command = func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "-t -f NAME,UUID,TYPE,DEVICE connection show --active" {
			t.Fatalf("guard attempted mutation: %v", args)
		}
		return "vpnkit-local:11111111-1111-4111-8111-111111111111:vpn:tun0\n", nil
	}
	if err := n.checkOtherVPN(context.Background(), "22222222-2222-4222-8222-222222222222"); err != ErrOtherVPNActive {
		t.Fatal(err)
	}
	if err := n.checkOtherVPN(context.Background(), "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
}
