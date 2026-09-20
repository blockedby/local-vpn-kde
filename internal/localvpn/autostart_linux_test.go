package localvpn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testAutostart(t *testing.T) Autostart {
	t.Helper()
	root := t.TempDir()
	return Autostart{Repo: root, Base: filepath.Join(root, "secrets/vpnkit-local"), ConfigHome: filepath.Join(root, "config"), command: func(context.Context, string, ...string) ([]byte, error) { return []byte("connected"), nil }, wait: func(context.Context) error { return nil }}
}
func TestAutostartOptInAndNoImmediateStart(t *testing.T) {
	a := testAutostart(t)
	var calls []string
	a.command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: true}); err != nil {
		t.Fatal(err)
	}
	options, err := a.Status(context.Background())
	if err != nil || !options.Connect {
		t.Fatalf("%+v %v", options, err)
	}
	if len(calls) != 2 || strings.Contains(strings.Join(calls, " "), "--now") {
		t.Fatal(calls)
	}
	unit, err := os.ReadFile(filepath.Join(a.ConfigHome, "systemd/user", autostartUnit))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "TimeoutStartSec=6min") || strings.Contains(string(unit), "Restart=") {
		t.Fatal(string(unit))
	}
	if err := a.Configure(context.Background(), AutostartOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(calls, " "), " stop ") {
		t.Fatal("disable stopped VPN", calls)
	}
}
func TestAutostartBoundedReadinessAndLifecycle(t *testing.T) {
	for _, connect := range []bool{false, true} {
		t.Run(map[bool]string{false: "gateway", true: "connect"}[connect], func(t *testing.T) {
			a := testAutostart(t)
			if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: connect}); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			a.command = func(_ context.Context, name string, _ ...string) ([]byte, error) {
				if name == "docker" {
					attempts++
					if attempts < 3 {
						return nil, errors.New("unready")
					}
				}
				return []byte("connected"), nil
			}
			var calls []string
			a.lifecycle = func(_ context.Context, args []string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			if err := a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 || len(calls) != 1+map[bool]int{false: 0, true: 1}[connect] || calls[0] != "backend start" {
				t.Fatal(attempts, calls)
			}
		})
	}
}
func TestAutostartNoUnboundedRetriesOrBlindMutationRetry(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: true}); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	a.command = func(context.Context, string, ...string) ([]byte, error) {
		attempts++
		return nil, errors.New("offline")
	}
	a.lifecycle = func(context.Context, []string) error { t.Fatal("must not mutate offline"); return nil }
	if a.Run(context.Background()) == nil || attempts != 24 {
		t.Fatal(attempts)
	}
	a.command = func(context.Context, string, ...string) ([]byte, error) { return []byte("connected"), nil }
	mutations := 0
	a.lifecycle = func(context.Context, []string) error { mutations++; return errors.New("failed") }
	if a.Run(context.Background()) == nil || mutations != 1 {
		t.Fatal(mutations)
	}
}
func TestAutostartFailureLeavesDisabledIntent(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: true}); err != nil {
		t.Fatal(err)
	}
	a.command = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("systemd unavailable")
	}
	if a.Configure(context.Background(), AutostartOptions{}) == nil {
		t.Fatal("expected failure")
	}
	options, err := a.Status(context.Background())
	if err != nil || options.Gateway || options.Connect {
		t.Fatal(options, err)
	}
}
func TestAutostartRefusesForeignUnitAndInvalidOptions(t *testing.T) {
	a := testAutostart(t)
	if a.Configure(context.Background(), AutostartOptions{Connect: true}) == nil {
		t.Fatal("connect without gateway")
	}
	dir := filepath.Join(a.ConfigHome, "systemd/user")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, autostartUnit)
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if a.Configure(context.Background(), AutostartOptions{Gateway: true}) == nil {
		t.Fatal("overwrote foreign unit")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "foreign" {
		t.Fatal(string(data))
	}
}
func TestAutostartCancellation(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.lifecycle = func(context.Context, []string) error { t.Fatal("must not mutate"); return nil }
	if !errors.Is(a.Run(ctx), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestAutostartEscapesSystemdArguments(t *testing.T) {
	a := testAutostart(t)
	a.Repo = "/tmp/repo space%$\""
	unit, err := a.unit()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "repo space%%$$\\\"") {
		t.Fatal(unit)
	}
	a.Repo = "/tmp/injected\nExecStart=/bin/false"
	if _, err = a.unit(); err == nil {
		t.Fatal("newline accepted")
	}
}

func TestAutostartMissingSettingsDoesNothing(t *testing.T) {
	a := testAutostart(t)
	a.command = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("default must be off")
		return nil, nil
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestAutostartDisableDuringReadinessWins(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: true}); err != nil {
		t.Fatal(err)
	}
	ready := false
	a.command = func(context.Context, string, ...string) ([]byte, error) {
		if !ready {
			return nil, errors.New("wait")
		}
		return []byte("connected"), nil
	}
	a.wait = func(ctx context.Context) error { ready = true; return a.Configure(ctx, AutostartOptions{}) }
	a.lifecycle = func(context.Context, []string) error { t.Fatal("disabled autostart mutated runtime"); return nil }
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAutostartRejectsCustomBase(t *testing.T) {
	a := testAutostart(t)
	a.Base = filepath.Join(a.Repo, "custom-private")
	if a.Configure(context.Background(), AutostartOptions{Gateway: true}) == nil {
		t.Fatal("accepted custom configure base")
	}
	if a.Run(context.Background()) == nil {
		t.Fatal("accepted custom runtime base")
	}
	if _, err := os.Stat(a.Base); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote custom base")
	}
}
func TestAutostartPersistsSafeFailureReasons(t *testing.T) {
	for _, reason := range []string{"docker-unavailable", "network-unavailable", "gateway-failed", "connect-failed", "ok"} {
		t.Run(reason, func(t *testing.T) {
			a := testAutostart(t)
			if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: true, LastResult: "injected"}); err != nil {
				t.Fatal(err)
			}
			a.command = func(_ context.Context, name string, _ ...string) ([]byte, error) {
				if (reason == "docker-unavailable" && name == "docker") || (reason == "network-unavailable" && name == "nmcli") {
					return nil, errors.New("private endpoint failure")
				}
				return []byte("connected"), nil
			}
			a.lifecycle = func(_ context.Context, args []string) error {
				if (reason == "gateway-failed" && len(args) == 2) || (reason == "connect-failed" && len(args) == 1) {
					return errors.New("private endpoint failure")
				}
				return nil
			}
			err := a.Run(context.Background())
			if (err == nil) != (reason == "ok") {
				t.Fatal(err)
			}
			status, err := a.Status(context.Background())
			expected := map[string]string{"docker-unavailable": "prerequisites-unavailable", "network-unavailable": "prerequisites-unavailable", "gateway-failed": "failed", "connect-failed": "failed", "ok": "ready"}[reason]
			if err != nil || status.LastResult != expected {
				t.Fatal(status, err)
			}
			data, err := os.ReadFile(filepath.Join(a.Base, "autostart-result.json"))
			if err != nil || strings.Contains(string(data), "private") {
				t.Fatal(string(data), err)
			}
		})
	}
}
func TestAutostartIgnoresResultInSettings(t *testing.T) {
	a := testAutostart(t)
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Base, "autostart.json"), []byte(`{"gateway":true,"connect":false,"last_result":"injected"}`), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(context.Background())
	if err != nil || status.LastResult != "" {
		t.Fatal(status, err)
	}
	if err := os.WriteFile(filepath.Join(a.Base, "autostart-result.json"), []byte(`{"result":"private arbitrary text"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Status(context.Background()); err == nil {
		t.Fatal("accepted arbitrary result")
	}
}
