//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if action == "status" || action == "connection-state" {
		_, _ = io.WriteString(output, f.status)
	}
	return nil
}

func installNativeDesktopFakes(t *testing.T, service nativeDesktopService, nm nativeDesktopNetworkManager) {
	t.Helper()
	old := newNativeDesktopDependencies
	newNativeDesktopDependencies = func(string) nativeDesktopDependencies {
		return nativeDesktopDependencies{service: service, nm: nm, egress: func(context.Context) (bool, error) { return true, nil }}
	}
	t.Cleanup(func() { newNativeDesktopDependencies = old })
}

func TestRunNativeDesktopLifecycleOrdering(t *testing.T) {
	tests := []struct {
		action string
		want   []string
	}{
		{"backend-start", []string{"service-start"}},
		{"start", []string{"service-status", "nm-check", "service-start", "nm-disconnect", "nm-connect"}},
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

func TestReadyGatewayWithBrokenEgress(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Ready: true, Network: "active"}}
	nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=yes\n"}
	old := newNativeDesktopDependencies
	defer func() { newNativeDesktopDependencies = old }()
	newNativeDesktopDependencies = func(string) nativeDesktopDependencies {
		return nativeDesktopDependencies{service: service, nm: nm, egress: func(context.Context) (bool, error) { return false, nil }, repair: func(context.Context) error { events = append(events, "repair"); return nil }}
	}
	base := t.TempDir()
	var output bytes.Buffer
	if err := RunNativeDesktop(context.Background(), base, "status", false, &output); err != nil || !strings.Contains(output.String(), `"container":"egress-unavailable"`) {
		t.Fatalf("status %s: %v", output.String(), err)
	}
	events = nil
	if err := RunNativeDesktop(context.Background(), base, "start", true, io.Discard); err != nil || !reflect.DeepEqual(events, []string{"service-status", "repair", "service-start", "nm-connect"}) {
		t.Fatalf("%v: %v", events, err)
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
	want := []string{"service-status", "nm-check", "service-start", "nm-disconnect", "nm-connect", "service-stop"}
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

func TestNativePolicyReconnectsOnlyPreviouslyActiveOwnedProfile(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%v/fails=%v", active, fails), func(t *testing.T) {
				var events []string
				state := "no"
				if active {
					state = "yes"
				}
				nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=" + state + "\n"}
				service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Ready: true}}
				failure := errors.New("candidate failed and was restored")
				d := nativeDesktopDependencies{service: service, nm: nm, policy: func(context.Context, io.Writer) error {
					events = append(events, "policy")
					if fails {
						return failure
					}
					return nil
				}}
				err := runNativeDesktopPolicy(context.Background(), io.Discard, d)
				if (err != nil) != fails {
					t.Fatalf("unexpected result %v", err)
				}
				want := []string{"nm-status", "policy"}
				if active {
					want = []string{"nm-status", "nm-disconnect", "policy", "service-status", "nm-connect"}
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events %v want %v", events, want)
				}
			})
		}
	}
}

func TestRunNativeDesktopRecoveryDisconnectsStaleTunnelAfterGatewayReady(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: "active", Ready: false}}
	nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=yes\n"}
	installNativeDesktopFakes(t, service, nm)
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "start", true, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"service-status", "nm-check", "service-start", "nm-disconnect", "nm-connect"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestRunNativeDesktopRecoveryRefusesForeignProfileBeforeGatewayStart(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events}
	refused := errors.New("foreign owned-profile capability")
	nm := &fakeNativeDesktopNM{events: &events, errors: map[string]error{"check": refused}}
	installNativeDesktopFakes(t, service, nm)
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "start", true, io.Discard); !errors.Is(err, refused) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(events, []string{"service-status", "nm-check"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestNativeStartFailureDoesNotReachProfileImportOrActivation(t *testing.T) {
	events := []string{}
	startup := errors.New("gateway startup failed")
	service := &fakeNativeDesktopService{events: &events, startErr: startup}
	nm := &fakeNativeDesktopNM{events: &events, errors: map[string]error{}}
	installNativeDesktopFakes(t, service, nm)
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "start", true, io.Discard); !errors.Is(err, startup) {
		t.Fatalf("error=%v", err)
	}
	want := []string{"service-status", "nm-check", "service-start"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v", events)
	}
}

func TestNativeNetworkReapplyPreservesConnectionState(t *testing.T) {
	for _, connected := range []bool{false, true} {
		for _, failed := range []string{"", "connection-state", "disconnect", "start", "reapply", "connect"} {
			t.Run(fmt.Sprintf("connected=%v/failure=%s", connected, failed), func(t *testing.T) {
				events := []string{}
				service := &fakeNativeDesktopService{events: &events}
				active := "no"
				if connected {
					active = "yes"
				}
				nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=" + active + "\n", errors: map[string]error{}}
				fault := errors.New("injected failure")
				switch failed {
				case "stop":
					service.stopErr = fault
				case "start":
					service.startErr = fault
				case "":
				default:
					nm.errors[failed] = fault
				}
				installNativeDesktopFakes(t, service, nm)
				err := RunNativeDesktop(context.Background(), t.TempDir(), "reapply-network", true, io.Discard)
				want := []string{"nm-connection-state"}
				if connected {
					want = append(want, "nm-disconnect")
				}
				want = append(want, "service-start", "nm-reapply")
				if connected {
					want = append(want, "nm-connect")
				}
				failureReached := false
				for i, event := range want {
					if event == "nm-"+failed || event == "service-"+failed {
						want = want[:i+1]
						failureReached = true
						break
					}
				}
				if (err != nil) != failureReached || !reflect.DeepEqual(events, want) {
					t.Fatalf("err=%v events=%v want=%v", err, events, want)
				}
			})
		}
	}
}

func TestNativeNetworkReapplyRequiresAuthorization(t *testing.T) {
	events := []string{}
	service := &fakeNativeDesktopService{events: &events}
	nm := &fakeNativeDesktopNM{events: &events}
	installNativeDesktopFakes(t, service, nm)
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "reapply-network", false, io.Discard); err == nil || len(events) != 0 {
		t.Fatalf("unauthorized reapply: err=%v events=%v", err, events)
	}
}

func TestRunNativeDesktopRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateway string
		active  string
		want    []string
		phase   string
	}{
		{"stopped gateway is left alone", "inactive", "no", []string{"service-status"}, "phase=gateway-not-running"},
		{"missing gateway is left alone", "absent", "no", []string{"service-status"}, "phase=gateway-not-running"},
		{"connected", "active", "yes", []string{"service-status", "nm-connection-state", "nm-disconnect", "service-stop", "service-start", "nm-connect"}, "phase=networkmanager-connect"},
		{"gateway only", "active", "no", []string{"service-status", "nm-connection-state", "service-stop", "service-start"}, "phase=gateway-start"},
		{"failed gateway", "failed", "no", []string{"service-status", "nm-connection-state", "service-stop", "service-start"}, "phase=gateway-start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: tc.gateway, Network: "active", Ready: tc.gateway == "active"}}
			nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=" + tc.active + "\n", errors: map[string]error{}}
			installNativeDesktopFakes(t, service, nm)
			var output bytes.Buffer
			if err := RunNativeDesktop(context.Background(), t.TempDir(), "restart", true, &output); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, tc.want) || !strings.Contains(output.String(), tc.phase) {
				t.Fatalf("events %v output %q", events, output.String())
			}
		})
	}
	events := []string{}
	installNativeDesktopFakes(t, &fakeNativeDesktopService{events: &events}, &fakeNativeDesktopNM{events: &events})
	if err := RunNativeDesktop(context.Background(), t.TempDir(), "restart", false, io.Discard); err == nil || len(events) != 0 {
		t.Fatalf("unauthorized restart: %v %v", err, events)
	}
}

func TestRunNativeDesktopRestartFailureBoundaries(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		statusErr error
		preflight error
		stopErr   error
		startErr  error
		connect   error
		want      []string
		wantErr   string
	}{
		{name: "status error with partial state mutates nothing", statusErr: boom, want: []string{"service-status"}, wantErr: "native gateway status failed"},
		{name: "preflight refusal mutates nothing", preflight: errNativePackageTransaction, want: []string{"service-status", "preflight"}, wantErr: "package update is in progress"},
		{name: "stop refusal reconnects on the old gateway", stopErr: boom, want: []string{"service-status", "preflight", "nm-connection-state", "nm-disconnect", "service-stop", "nm-connect"}, wantErr: "the old gateway keeps running"},
		{name: "start failure says what is down", startErr: boom, want: []string{"service-status", "preflight", "nm-connection-state", "nm-disconnect", "service-stop", "service-start"}, wantErr: "the gateway is stopped (start it in kvpn); the VPN is disconnected"},
		{name: "reconnect failure says how to recover", connect: boom, want: []string{"service-status", "preflight", "nm-connection-state", "nm-disconnect", "service-stop", "service-start", "nm-connect"}, wantErr: "reconnecting the VPN failed (connect it in kvpn)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			service := &fakeNativeDesktopService{events: &events, status: GatewayStatus{Gateway: "active", Network: "active"}, statusErr: tc.statusErr, stopErr: tc.stopErr, startErr: tc.startErr}
			nm := &fakeNativeDesktopNM{events: &events, status: "configured=yes\nactive=yes\n", errors: map[string]error{"connect": tc.connect}}
			old := newNativeDesktopDependencies
			t.Cleanup(func() { newNativeDesktopDependencies = old })
			newNativeDesktopDependencies = func(string) nativeDesktopDependencies {
				return nativeDesktopDependencies{service: service, nm: nm,
					egress:           func(context.Context) (bool, error) { return true, nil },
					restartPreflight: func(context.Context) error { events = append(events, "preflight"); return tc.preflight }}
			}
			err := RunNativeDesktop(context.Background(), t.TempDir(), "restart", true, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want %q", err, tc.wantErr)
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events %v want %v", events, tc.want)
			}
		})
	}
}
