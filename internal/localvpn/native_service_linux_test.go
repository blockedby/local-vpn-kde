//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"golang.org/x/sys/unix"
)

type nativeServiceTestCall struct {
	name string
	args []string
}

func newNativeServiceTestFixture(t *testing.T, states map[string][]byte) (*NativeService, *[]nativeServiceTestCall) {
	t.Helper()
	units := nativegateway.UnitFiles()
	calls := []nativeServiceTestCall{}
	service := &NativeService{
		readUnit: func(name string) ([]byte, error) {
			contents, ok := units[name]
			if !ok {
				return nil, errors.New("unexpected unit")
			}
			return []byte(contents), nil
		},
		command: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, nativeServiceTestCall{name: name, args: append([]string(nil), args...)})
			if name != "systemctl" {
				return nil, fmt.Errorf("unexpected command %s", name)
			}
			if len(args) == 0 {
				return nil, errors.New("missing systemctl action")
			}
			switch args[0] {
			case "show":
				if len(args) < 2 {
					return nil, errors.New("missing unit")
				}
				unit := args[len(args)-1]
				data, ok := states[unit]
				if !ok {
					return nil, errors.New("unexpected unit")
				}
				return append([]byte(nil), data...), nil
			case "start", "stop":
				if len(args) != 3 || args[1] != "--no-ask-password" {
					return nil, errors.New("unexpected lifecycle arguments")
				}
				return nil, nil
			default:
				return nil, errors.New("unexpected systemctl action")
			}
		},
	}
	return service, &calls
}

func nativeServiceTestState(load, active, sub, unitType, dropIns string) []byte {
	fragment := ""
	switch unitType {
	case "notify":
		fragment = filepath.Join(nativeServiceUnitDirectory, nativegateway.GatewayUnit)
	case "oneshot":
		fragment = filepath.Join(nativeServiceUnitDirectory, nativegateway.NetworkUnit)
	}
	return nativeServiceTestStateWithProperties(load, active, sub, unitType, dropIns, fragment, "no")
}

func nativeServiceTestStateWithProperties(load, active, sub, unitType, dropIns, fragment, needDaemonReload string) []byte {
	return []byte("LoadState=" + load + "\n" +
		"ActiveState=" + active + "\n" +
		"SubState=" + sub + "\n" +
		"Type=" + unitType + "\n" +
		"DropInPaths=" + dropIns + "\n" +
		"FragmentPath=" + fragment + "\n" +
		"NeedDaemonReload=" + needDaemonReload + "\n")
}

func nativeServiceReadyTestStates() map[string][]byte {
	return map[string][]byte{
		nativegateway.GatewayUnit: nativeServiceTestState("loaded", "active", "running", "notify", ""),
		nativegateway.NetworkUnit: nativeServiceTestState("loaded", "active", "exited", "oneshot", ""),
	}
}

func nativeServiceLifecycleTestCalls(calls []nativeServiceTestCall, action string) []nativeServiceTestCall {
	result := make([]nativeServiceTestCall, 0, len(calls))
	for _, call := range calls {
		if len(call.args) > 0 && call.args[0] == action {
			result = append(result, call)
		}
	}
	return result
}

func TestNativeServiceStartWaitsForNotifyReadiness(t *testing.T) {
	service, calls := newNativeServiceTestFixture(t, nativeServiceReadyTestStates())
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	starts := nativeServiceLifecycleTestCalls(*calls, "start")
	if len(starts) != 1 || len(starts[0].args) != 3 || starts[0].args[2] != nativegateway.GatewayUnit {
		t.Fatalf("start calls = %#v", starts)
	}
	for _, call := range *calls {
		if call.name != "systemctl" {
			t.Fatalf("unexpected command: %#v", call)
		}
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "docker") || strings.Contains(joined, "enable") || strings.Contains(joined, "daemon-reload") {
			t.Fatalf("unexpected lifecycle command: %#v", call)
		}
		if call.args[0] == "show" && !strings.Contains(joined, "--property="+nativeServiceShowProperties) {
			t.Fatalf("show did not request exact properties: %#v", call)
		}
	}
	if got, err := service.Status(context.Background()); err != nil || !got.Ready || got.Gateway != "active" || got.Network != "active" {
		t.Fatalf("status = %#v, err = %v", got, err)
	}
}

func TestNativeServiceStartFailureIsGenericAndDoesNotPrompt(t *testing.T) {
	states := nativeServiceReadyTestStates()
	service, calls := newNativeServiceTestFixture(t, states)
	secret := "subscription-secret-marker"
	service.command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, nativeServiceTestCall{name: name, args: append([]string(nil), args...)})
		if name != "systemctl" {
			return nil, errors.New("unexpected command")
		}
		switch args[0] {
		case "show":
			return states[args[len(args)-1]], nil
		case "start":
			return []byte(secret), errors.New("authorization denied: " + secret)
		default:
			return nil, errors.New("unexpected command")
		}
	}

	err := service.Start(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(strings.ToLower(err.Error()), "sudo") {
		t.Fatalf("start error = %v", err)
	}
	if len(nativeServiceLifecycleTestCalls(*calls, "stop")) != 0 {
		t.Fatal("start failure unexpectedly stopped a unit")
	}
}

func TestNativeServiceRefusesForeignUnitsAndDropInsBeforeMutation(t *testing.T) {
	t.Run("foreign unit", func(t *testing.T) {
		service, calls := newNativeServiceTestFixture(t, nativeServiceReadyTestStates())
		service.readUnit = func(name string) ([]byte, error) {
			if name == nativegateway.GatewayUnit {
				return []byte("[Service]\nExecStart=/foreign\n"), nil
			}
			return []byte(nativegateway.UnitFiles()[name]), nil
		}
		if err := service.Start(context.Background()); err == nil {
			t.Fatal("foreign unit accepted")
		}
		if len(*calls) != 0 {
			t.Fatalf("systemctl called before foreign unit refusal: %#v", *calls)
		}
	})

	t.Run("drop-in", func(t *testing.T) {
		states := nativeServiceReadyTestStates()
		states[nativegateway.GatewayUnit] = nativeServiceTestState("loaded", "active", "running", "notify", "/etc/systemd/system/local-vpn-kde-gateway.service.d/override.conf")
		service, calls := newNativeServiceTestFixture(t, states)
		if err := service.Stop(context.Background()); err == nil {
			t.Fatal("drop-in accepted")
		}
		if stops := nativeServiceLifecycleTestCalls(*calls, "stop"); len(stops) != 0 {
			t.Fatalf("stop called before drop-in refusal: %#v", stops)
		}
	})
}

func TestNativeServiceRefusesStaleOrForeignFragmentsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name     string
		fragment string
		reload   string
	}{
		{name: "foreign fragment", fragment: "/run/systemd/generator/local-vpn-kde-gateway.service", reload: "no"},
		{name: "stale daemon", fragment: filepath.Join(nativeServiceUnitDirectory, nativegateway.GatewayUnit), reload: "yes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			states := nativeServiceReadyTestStates()
			states[nativegateway.GatewayUnit] = nativeServiceTestStateWithProperties("loaded", "active", "running", "notify", "", test.fragment, test.reload)
			service, calls := newNativeServiceTestFixture(t, states)
			if err := service.Start(context.Background()); err == nil {
				t.Fatal("unsafe systemd state accepted")
			}
			if starts := nativeServiceLifecycleTestCalls(*calls, "start"); len(starts) != 0 {
				t.Fatalf("start called before unsafe state refusal: %#v", starts)
			}
		})
	}
}

func TestNativeServiceStatusNotReadyForStaleOrForeignFragments(t *testing.T) {
	for _, test := range []struct {
		name     string
		fragment string
		reload   string
	}{
		{name: "foreign fragment", fragment: "/run/systemd/generator/local-vpn-kde-gateway.service", reload: "no"},
		{name: "stale daemon", fragment: filepath.Join(nativeServiceUnitDirectory, nativegateway.GatewayUnit), reload: "yes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			states := nativeServiceReadyTestStates()
			states[nativegateway.GatewayUnit] = nativeServiceTestStateWithProperties("loaded", "active", "running", "notify", "", test.fragment, test.reload)
			service, _ := newNativeServiceTestFixture(t, states)
			status, err := service.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.Gateway != "active" || status.Network != "active" || status.Ready {
				t.Fatalf("status = %#v, want active but not ready", status)
			}
		})
	}
}

func TestNativeServiceStatusReportsMissingUnitsAsAbsent(t *testing.T) {
	states := map[string][]byte{
		nativegateway.GatewayUnit: nativeServiceTestState("not-found", "inactive", "dead", "", ""),
		nativegateway.NetworkUnit: nativeServiceTestState("not-found", "inactive", "dead", "", ""),
	}
	service, calls := newNativeServiceTestFixture(t, states)
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Gateway != "absent" || status.Network != "absent" || status.Ready {
		t.Fatalf("status = %#v", status)
	}
	if len(nativeServiceLifecycleTestCalls(*calls, "start")) != 0 || len(nativeServiceLifecycleTestCalls(*calls, "stop")) != 0 {
		t.Fatal("status mutated systemd")
	}
}

func TestNativeServiceStopOrder(t *testing.T) {
	service, calls := newNativeServiceTestFixture(t, nativeServiceReadyTestStates())
	if err := service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stops := nativeServiceLifecycleTestCalls(*calls, "stop")
	if len(stops) != 2 {
		t.Fatalf("stop calls = %#v", stops)
	}
	if stops[0].args[2] != nativegateway.GatewayUnit || stops[1].args[2] != nativegateway.NetworkUnit {
		t.Fatalf("stop order = %#v", stops)
	}
	for _, call := range *calls {
		if strings.Contains(strings.Join(call.args, " "), "docker") {
			t.Fatal("native lifecycle invoked Docker")
		}
	}
}

func nativeServiceTestUnitLocation(t *testing.T) nativeServiceUnitLocation {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return nativeServiceUnitLocation{
		rootPath:     root,
		relativePath: filepath.Join("systemd", nativegateway.GatewayUnit),
		ownerUID:     uint32(os.Geteuid()),
	}
}

func nativeServiceTestUnitPath(location nativeServiceUnitLocation) string {
	return filepath.Join(location.rootPath, filepath.FromSlash(location.relativePath))
}

func writeNativeServiceTestUnit(t *testing.T, location nativeServiceUnitLocation, mode os.FileMode) {
	t.Helper()
	path := nativeServiceTestUnitPath(location)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(nativegateway.UnitFiles()[nativegateway.GatewayUnit]), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadInstalledNativeUnitUsesPinnedSafeDescriptors(t *testing.T) {
	location := nativeServiceTestUnitLocation(t)
	writeNativeServiceTestUnit(t, location, 0600)
	got, err := readInstalledNativeUnitAt(location)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != nativegateway.UnitFiles()[nativegateway.GatewayUnit] {
		t.Fatal("safe unit reader returned unexpected contents")
	}
}

func TestReadInstalledNativeUnitRejectsUnsafeMetadata(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		writeNativeServiceTestUnit(t, location, 0660)
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted group-writable unit")
		}
	})

	t.Run("ownership", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		writeNativeServiceTestUnit(t, location, 0600)
		location.ownerUID++
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted unit with an unexpected owner")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		path := nativeServiceTestUnitPath(location)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(location.rootPath, "target.service")
		if err := os.WriteFile(target, []byte(nativegateway.UnitFiles()[nativegateway.GatewayUnit]), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted symlink unit")
		}
	})

	t.Run("ancestor symlink", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		outside := t.TempDir()
		outsidePath := filepath.Join(outside, "systemd", nativegateway.GatewayUnit)
		if err := os.MkdirAll(filepath.Dir(outsidePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outsidePath, []byte(nativegateway.UnitFiles()[nativegateway.GatewayUnit]), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(outsidePath), filepath.Join(location.rootPath, "systemd")); err != nil {
			t.Fatal(err)
		}
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted symlink ancestor")
		}
	})

	t.Run("fifo", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		path := nativeServiceTestUnitPath(location)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := readInstalledNativeUnitAt(location)
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("accepted FIFO unit")
			}
		case <-time.After(time.Second):
			t.Fatal("FIFO unit reader blocked")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		writeNativeServiceTestUnit(t, location, 0600)
		if err := os.Link(nativeServiceTestUnitPath(location), filepath.Join(location.rootPath, "second-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted multiply-linked unit")
		}
	})

	t.Run("unsafe parent", func(t *testing.T) {
		location := nativeServiceTestUnitLocation(t)
		writeNativeServiceTestUnit(t, location, 0600)
		if err := os.Chmod(filepath.Dir(nativeServiceTestUnitPath(location)), 0770); err != nil {
			t.Fatal(err)
		}
		if _, err := readInstalledNativeUnitAt(location); err == nil {
			t.Fatal("accepted unit from a group-writable parent")
		}
	})
}

func TestNativeServiceBoundsTimeoutAndDoesNotExposeCommandOutput(t *testing.T) {
	service, _ := newNativeServiceTestFixture(t, nativeServiceReadyTestStates())
	secret := "private-command-output"
	service.command = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return []byte(secret), ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := service.Status(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("status was not bounded: %s", elapsed)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("status error exposed command output: %v", err)
	}

	service.command = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return bytes.Repeat([]byte(secret), nativeServiceOutputLimit), nil
	}
	if _, err := service.Status(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("oversized status error = %v", err)
	}
}
