//go:build linux

package localvpn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configuredNativeAutostart(t *testing.T, connect bool) Autostart {
	t.Helper()
	a := testAutostart(t)
	a.Runtime = "native"
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Connect: connect, Runtime: "native"}); err != nil {
		t.Fatal(err)
	}
	return a
}

func availableNativeStatus() GatewayStatus {
	return GatewayStatus{Gateway: "inactive", Network: "inactive"}
}

func TestNativeAutostartUnitAndRuntimeIntent(t *testing.T) {
	a := configuredNativeAutostart(t, false)
	unit, err := os.ReadFile(filepath.Join(a.ConfigHome, "systemd/user", a.unitName()))
	if err != nil {
		t.Fatal(err)
	}
	want := "ExecStart=" + systemdArg(filepath.Join(a.Repo, ".build/local-vpn-kde.bin")) + " autostart --repo " + systemdArg(a.Repo) + " --runtime " + systemdArg("native") + " --action run"
	if !strings.Contains(string(unit), want) || !strings.HasPrefix(string(unit), autostartMarker) {
		t.Fatal(string(unit))
	}
	status, err := a.Status(context.Background())
	if err != nil || status.Runtime != "native" {
		t.Fatal(status, err)
	}
}

func TestNativeAutostartNeverUsesDockerOrLegacyLifecycle(t *testing.T) {
	a := configuredNativeAutostart(t, true)
	a.command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "nmcli" {
			t.Fatalf("native path invoked %s %v", name, args)
		}
		return []byte("connected"), nil
	}
	a.lifecycle = func(context.Context, []string) error {
		t.Fatal("native path invoked legacy lifecycle")
		return nil
	}
	a.nativeStatus = func(context.Context) (GatewayStatus, error) { return availableNativeStatus(), nil }
	started, connected := 0, 0
	a.nativeStart = func(context.Context) error { started++; return nil }
	a.nativeConnect = func(context.Context) error { connected++; return nil }
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if started != 1 || connected != 1 {
		t.Fatal(started, connected)
	}
}

func TestNativeAutostartEnabledRuntimeMismatchFailsClosed(t *testing.T) {
	a := testAutostart(t)
	if err := os.MkdirAll(a.Base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Base, "autostart.json"), []byte(`{"gateway":true,"runtime":"native"}`), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	a.command = func(context.Context, string, ...string) ([]byte, error) { mutations++; return nil, nil }
	a.lifecycle = func(context.Context, []string) error { mutations++; return nil }
	if _, err := a.Status(context.Background()); err == nil {
		t.Fatal("enabled mismatch accepted by status")
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("enabled mismatch accepted by run")
	}
	if mutations != 0 {
		t.Fatal("runtime mismatch mutated host", mutations)
	}

	a.Runtime = "native"
	if err := os.WriteFile(filepath.Join(a.Base, "autostart.json"), []byte(`{"gateway":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(context.Background())
	if err != nil || status.Runtime != "native" {
		t.Fatal(status, err)
	}
}

func TestNativeAutostartRejectsUnknownRuntimeBeforeMutation(t *testing.T) {
	a := testAutostart(t)
	a.Runtime = "other"
	called := false
	a.command = func(context.Context, string, ...string) ([]byte, error) { called = true; return nil, nil }
	if err := a.Configure(context.Background(), AutostartOptions{Gateway: true, Runtime: "other"}); err == nil {
		t.Fatal("unknown runtime accepted")
	}
	if called {
		t.Fatal("unknown runtime invoked command")
	}
	if _, err := os.Stat(a.Base); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown runtime wrote private state", err)
	}
}

func TestNativeAutostartPrerequisitesAreBoundedAndCancellable(t *testing.T) {
	t.Run("missing units", func(t *testing.T) {
		a := configuredNativeAutostart(t, false)
		attempts := 0
		a.nativeStatus = func(context.Context) (GatewayStatus, error) {
			attempts++
			return GatewayStatus{Gateway: "absent", Network: "absent"}, nil
		}
		a.nativeStart = func(context.Context) error { t.Fatal("started missing native units"); return nil }
		a.command = func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("called external prerequisite")
			return nil, nil
		}
		if err := a.Run(context.Background()); err == nil || attempts != 12 {
			t.Fatal(err, attempts)
		}
	})

	t.Run("network disconnected", func(t *testing.T) {
		a := configuredNativeAutostart(t, true)
		a.nativeStatus = func(context.Context) (GatewayStatus, error) { return availableNativeStatus(), nil }
		networkChecks := 0
		a.command = func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "docker" {
				t.Fatal("native prerequisite invoked Docker")
			}
			networkChecks++
			return []byte("disconnected"), nil
		}
		a.nativeStart = func(context.Context) error { t.Fatal("started without connected NetworkManager"); return nil }
		if err := a.Run(context.Background()); err == nil || networkChecks != 12 {
			t.Fatal(err, networkChecks)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		a := configuredNativeAutostart(t, false)
		a.nativeStatus = func(context.Context) (GatewayStatus, error) { return GatewayStatus{}, errors.New("wait") }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestNativeAutostartDisableWinsBeforeStartAndConnect(t *testing.T) {
	t.Run("during prerequisites", func(t *testing.T) {
		a := configuredNativeAutostart(t, false)
		ready := false
		a.nativeStatus = func(context.Context) (GatewayStatus, error) {
			if !ready {
				return GatewayStatus{}, errors.New("wait")
			}
			return availableNativeStatus(), nil
		}
		a.wait = func(ctx context.Context) error {
			ready = true
			return a.Configure(ctx, AutostartOptions{Runtime: "native"})
		}
		a.nativeStart = func(context.Context) error { t.Fatal("started after disable"); return nil }
		if err := a.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("after gateway start", func(t *testing.T) {
		a := configuredNativeAutostart(t, true)
		a.nativeStatus = func(context.Context) (GatewayStatus, error) { return availableNativeStatus(), nil }
		a.command = func(context.Context, string, ...string) ([]byte, error) { return []byte("connected"), nil }
		a.nativeStart = func(ctx context.Context) error {
			return a.Configure(ctx, AutostartOptions{Runtime: "native"})
		}
		a.nativeConnect = func(context.Context) error { t.Fatal("connected after disable"); return nil }
		if err := a.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNativeAutostartErrorsAreCoarse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect bool
	}{
		{name: "start"},
		{name: "connect", connect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := configuredNativeAutostart(t, tc.connect)
			a.nativeStatus = func(context.Context) (GatewayStatus, error) { return availableNativeStatus(), nil }
			a.command = func(context.Context, string, ...string) ([]byte, error) { return []byte("connected"), nil }
			secret := "private-subscription-output"
			a.nativeStart = func(context.Context) error {
				if tc.name == "start" {
					return errors.New(secret)
				}
				return nil
			}
			a.nativeConnect = func(context.Context) error { return errors.New(secret) }
			err := a.Run(context.Background())
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(filepath.Join(a.Base, "autostart-result.json"))
			if readErr != nil || strings.Contains(string(data), secret) || !strings.Contains(string(data), `"failed"`) {
				t.Fatal(string(data), readErr)
			}
		})
	}
}
