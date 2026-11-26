package localvpn

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeSettingsPersistence(t *testing.T) {
	parent, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", parent)
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "state")
	t.Setenv("XDG_STATE_HOME", state)
	ctx := context.Background()
	calls := []string{}
	a := Autostart{Repo: InstalledAssetRoot, Base: filepath.Join(state, "local-vpn-kde"), Runtime: "native", Installed: true, ConfigHome: filepath.Join(home, "config"), command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	if options, err := a.Status(ctx); err != nil || options.Resume {
		t.Fatalf("default: %+v %v", options, err)
	}
	for _, enabled := range []bool{true, false} {
		if err := a.Configure(ctx, AutostartOptions{Runtime: "native", Resume: enabled}); err != nil {
			t.Fatal(err)
		}
		options, err := a.Status(ctx)
		if err != nil || options.Resume != enabled || options.Connect || options.Gateway {
			t.Fatalf("settings %+v: %v", options, err)
		}
		verb := "enable"
		if !enabled {
			verb = "disable"
		}
		if got := calls[len(calls)-1]; got != "systemctl --user "+verb+" --now "+resumePathUnit {
			t.Fatal(got)
		}
	}
	for name, field := range map[string]string{resumePathUnit: "PathChanged=/run/local-vpn-kde-resumed", "local-vpn-kde-resume.service": "--action resume"} {
		data, err := os.ReadFile(filepath.Join(a.ConfigHome, "systemd/user", name))
		if err != nil || !strings.Contains(string(data), field) || !strings.Contains(string(data), "PartOf=graphical-session.target") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestResumeAvoidsCompetingTunnels(t *testing.T) {
	const own = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		kind, uuid string
		skip       bool
	}{{"ethernet", other, false}, {"vpn", own, false}, {"vpn", other, true}, {"wireguard", other, true}, {"tun", other, true}} {
		nm := NetworkManager{command: func(context.Context, ...string) (string, error) {
			return "profile:" + tc.uuid + ":" + tc.kind + ":fixture0", nil
		}}
		err := nm.checkOtherVPN(context.Background(), own)
		if errors.Is(err, ErrOtherVPNActive) != tc.skip {
			t.Fatalf("%s: %v", tc.kind, err)
		}
	}
}

func TestResumeRechecksIntentBeforeActivation(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Network: "active", Ready: true}}
	nm := &fakeNativeDesktopNM{events: &events}
	installNativeDesktopFakes(t, service, nm)
	count := 0
	guard := func(context.Context) error {
		count++
		if count == 1 {
			return nil
		}
		return ErrOtherVPNActive
	}
	err := runNativeDesktop(context.Background(), t.TempDir(), "start", true, io.Discard, guard)
	result := resumeResult(err)
	if result.Reason != "other-vpn-active" || *result.Code != 0 || len(nm.actions) != 0 || count != 2 {
		t.Fatalf("%+v: %v", result, events)
	}
}
