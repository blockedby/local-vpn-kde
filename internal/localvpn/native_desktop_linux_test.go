//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeNativeDesktopService struct {
	events          *[]string
	status          GatewayStatus
	statusErr       error
	startErr        error
	stopErr         error
	stopContextErr  error
	stopDeadline    time.Time
	stopHasDeadline bool
}

func (f *fakeNativeDesktopService) Start(context.Context) error {
	*f.events = append(*f.events, "service-start")
	return f.startErr
}

func (f *fakeNativeDesktopService) Stop(ctx context.Context) error {
	*f.events = append(*f.events, "service-stop")
	f.stopContextErr = ctx.Err()
	f.stopDeadline, f.stopHasDeadline = ctx.Deadline()
	return f.stopErr
}

func (f *fakeNativeDesktopService) Status(context.Context) (GatewayStatus, error) {
	*f.events = append(*f.events, "service-status")
	return f.status, f.statusErr
}

type fakeNativeDesktopNM struct {
	events  *[]string
	status  string
	errors  map[string]error
	onRun   func(context.Context, string)
	actions []string
}

func (f *fakeNativeDesktopNM) Run(ctx context.Context, action string, yes bool, output io.Writer) error {
	*f.events = append(*f.events, "nm-"+action)
	f.actions = append(f.actions, action)
	if !yes {
		return errors.New("test received unauthorized mutation")
	}
	if f.onRun != nil {
		f.onRun(ctx, action)
	}
	if err := f.errors[action]; err != nil {
		return err
	}
	if action == "status" {
		_, _ = io.WriteString(output, f.status)
	}
	return nil
}

func installNativeDesktopFakes(t *testing.T, service nativeDesktopService, nm nativeDesktopNetworkManager) {
	t.Helper()
	old := newNativeDesktopDependencies
	newNativeDesktopDependencies = func(string) nativeDesktopDependencies {
		return nativeDesktopDependencies{service: service, nm: nm}
	}
	t.Cleanup(func() { newNativeDesktopDependencies = old })
}

func TestRunNativeDesktopLifecycleOrdering(t *testing.T) {
	tests := []struct {
		action string
		want   []string
	}{
		{"backend-start", []string{"service-start"}},
		{"start", []string{"service-status", "service-start", "nm-connect"}},
		{"disconnect", []string{"nm-disconnect"}},
		{"stop", []string{"nm-disconnect", "service-stop"}},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			events := []string{}
			service := &fakeNativeDesktopService{events: &events}
			nm := &fakeNativeDesktopNM{events: &events, errors: map[string]error{}}
			installNativeDesktopFakes(t, service, nm)
			var output bytes.Buffer
			if err := RunNativeDesktop(context.Background(), t.TempDir(), test.action, true, &output); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, test.want) {
				t.Fatalf("events = %#v, want %#v", events, test.want)
			}
			if strings.Contains(strings.ToLower(output.String()), "docker") {
				t.Fatal("native lifecycle emitted a legacy runtime reference")
			}
		})
	}
}

func TestRunNativeDesktopStartRollbackIgnoresCancellationAndIsBounded(t *testing.T) {
	events := []string{}
	ctx, cancel := context.WithCancel(context.Background())
	service := &fakeNativeDesktopService{events: &events}
	nm := &fakeNativeDesktopNM{
		events: &events,
		errors: map[string]error{"connect": context.Canceled},
		onRun: func(_ context.Context, action string) {
			if action == "connect" {
				cancel()
			}
		},
	}
	installNativeDesktopFakes(t, service, nm)
	var output bytes.Buffer
	err := RunNativeDesktop(ctx, t.TempDir(), "start", true, &output)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	want := []string{"service-status", "service-start", "nm-connect", "service-stop"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if service.stopContextErr != nil {
		t.Fatal("compensation inherited caller cancellation")
	}
	if !service.stopHasDeadline || time.Until(service.stopDeadline) <= 0 || time.Until(service.stopDeadline) > nativeDesktopCompensateTimeout {
		t.Fatal("compensation context was not bounded")
	}
}

func TestRunNativeDesktopStartPreservesPreexistingGateway(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: "active", Network: "active", Ready: true}}
	nmErr := errors.New("owned profile connect failed")
	nm := &fakeNativeDesktopNM{events: &events, errors: map[string]error{"connect": nmErr}}
	installNativeDesktopFakes(t, service, nm)
	err := RunNativeDesktop(context.Background(), t.TempDir(), "start", true, io.Discard)
	if !errors.Is(err, nmErr) {
		t.Fatalf("error = %v", err)
	}
	if slicesContain(events, "service-stop") {
		t.Fatal("preexisting gateway was stopped after connect failure")
	}
}

func TestRunNativeDesktopStopDoesNotStopServiceAfterForeignNMError(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events}
	foreign := errors.New("refusing unowned or changed NetworkManager profile")
	nm := &fakeNativeDesktopNM{events: &events, errors: map[string]error{"disconnect": foreign}}
	installNativeDesktopFakes(t, service, nm)
	err := RunNativeDesktop(context.Background(), t.TempDir(), "stop", true, io.Discard)
	if !errors.Is(err, foreign) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(events, []string{"nm-disconnect"}) {
		t.Fatalf("service stop followed failed ownership check: %#v", events)
	}
}

func TestRunNativeDesktopMalformedNMStatusDoesNotInventHealthy(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: "active", Network: "active", Ready: true}}
	nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=maybe\n", errors: map[string]error{}}
	installNativeDesktopFakes(t, service, nm)
	var output bytes.Buffer
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "status", false, &output); err == nil {
		t.Fatal("malformed NetworkManager status reported success")
	}
	if strings.Contains(output.String(), "healthy") {
		t.Fatal("malformed combined status emitted a healthy result")
	}
}

func TestRunNativeDesktopStatusJSONAndMissingBasePolicy(t *testing.T) {
	for _, test := range []struct {
		name       string
		missing    bool
		nmStatus   string
		configured string
		active     string
		nmCalls    int
	}{
		{name: "existing", nmStatus: "command=status\nmutation=none\nconnection=vpnkit-local\nconfigured=yes\nactive=no\nownership=owned\ndevice=none\nprofile=ready\nprivate_values=not_printed\n", configured: "yes", active: "no", nmCalls: 1},
		{name: "missing", missing: true, configured: "no", active: "no", nmCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: "inactive"}}
			nm := &fakeNativeDesktopNM{events: &events, status: test.nmStatus, errors: map[string]error{}}
			installNativeDesktopFakes(t, service, nm)
			base := t.TempDir()
			if test.missing {
				base = filepath.Join(base, "not-created")
			}
			var output bytes.Buffer
			if err := RunNativeDesktop(context.Background(), base, "status", false, &output); err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["container"] != "inactive" || got["runtime"] != "native" || got["networkmanager_configured"] != test.configured || got["networkmanager_active"] != test.active {
				t.Fatalf("unexpected status: %#v", got)
			}
			if len(nm.actions) != test.nmCalls {
				t.Fatalf("NetworkManager calls = %d", len(nm.actions))
			}
			if test.missing {
				if _, err := os.Stat(base); !os.IsNotExist(err) {
					t.Fatal("status created a missing base")
				}
			}
		})
	}
}

func TestRunNativeDesktopRejectsUnknownAndUnauthorizedActions(t *testing.T) {
	for _, test := range []struct {
		action string
		yes    bool
	}{
		{action: "restart", yes: true},
		{action: "start", yes: false},
	} {
		if err := RunNativeDesktop(context.Background(), t.TempDir(), test.action, test.yes, io.Discard); err == nil {
			t.Fatalf("action %q was accepted", test.action)
		}
	}
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
