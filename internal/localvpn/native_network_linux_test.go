//go:build linux

package localvpn

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

type fakeNativeNetworkLifecycle struct {
	uplinks           []nativegateway.Uplink
	starts, stops     int
	startErr, stopErr error
}

func (f *fakeNativeNetworkLifecycle) Start(_ context.Context, u nativegateway.Uplink) error {
	f.starts++
	f.uplinks = append(f.uplinks, u)
	return f.startErr
}
func (f *fakeNativeNetworkLifecycle) Stop(context.Context) error { f.stops++; return f.stopErr }

type fakeNamespacePIDRead struct {
	calls int
	pids  string
	err   error
}

func (f *fakeNamespacePIDRead) Run(_ context.Context, program string, args ...string) ([]byte, error) {
	f.calls++
	if program != "ip" || !reflect.DeepEqual(args, []string{"netns", "pids", nativegateway.Namespace}) {
		return nil, errors.New("unexpected command")
	}
	return []byte(f.pids), f.err
}

func TestNativeNetworkStartDiscoversCurrentUplinkWithExistingJournal(t *testing.T) {
	old := nativegateway.Uplink{Interface: "eth0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}
	current := nativegateway.Uplink{Interface: "eth1", AddressCIDR: "198.51.100.20/24", Gateway: "198.51.100.1"}
	controller := &fakeNativeNetworkLifecycle{}
	probes, initializations := 0, 0
	err := startNativeNetwork(context.Background(), controller, true, func(context.Context) (nativegateway.Uplink, error) { probes++; return current, nil }, func(context.Context) error { initializations++; return nil }, controller.Stop)
	if err != nil {
		t.Fatal(err)
	}
	if probes != 1 || initializations != 1 || len(controller.uplinks) != 1 || controller.uplinks[0] != current || controller.uplinks[0] == old {
		t.Fatal("existing ledger bypassed current underlay discovery")
	}
}

func TestNativeNetworkStartDiscoveryOrProofFailureCannotInitializeOrCleanup(t *testing.T) {
	for _, scenario := range []string{"probe-failure", "controller-refusal"} {
		t.Run(scenario, func(t *testing.T) {
			expected := errors.New(scenario)
			controller := &fakeNativeNetworkLifecycle{}
			if scenario == "controller-refusal" {
				controller.startErr = expected
			}
			initializations := 0
			err := startNativeNetwork(context.Background(), controller, true, func(context.Context) (nativegateway.Uplink, error) {
				if scenario == "probe-failure" {
					return nativegateway.Uplink{}, expected
				}
				return nativegateway.Uplink{}, nil
			}, func(context.Context) error { initializations++; return nil }, controller.Stop)
			if !errors.Is(err, expected) || initializations != 0 || controller.stops != 0 {
				t.Fatalf("error=%v initialize=%d cleanup=%d", err, initializations, controller.stops)
			}
			if scenario == "probe-failure" && controller.starts != 0 {
				t.Fatal("probe failure reached controller")
			}
		})
	}
}

func TestNativeNetworkStopRequiresPIDProofOnlyForPresentNamespace(t *testing.T) {
	for _, scenario := range []string{"absent", "empty", "holder", "pids-error", "stat-error", "controller-refusal"} {
		t.Run(scenario, func(t *testing.T) {
			controller := &fakeNativeNetworkLifecycle{}
			reads := &fakeNamespacePIDRead{}
			expected := errors.New(scenario)
			switch scenario {
			case "holder":
				reads.pids = "31415\n"
			case "pids-error":
				reads.err = expected
			case "controller-refusal":
				controller.stopErr = expected
			}
			err := stopNativeNetwork(context.Background(), controller, reads, func() (bool, error) {
				if scenario == "stat-error" {
					return false, expected
				}
				return scenario != "absent", nil
			})
			success := scenario == "absent" || scenario == "empty"
			if (err == nil) != success {
				t.Fatalf("error=%v", err)
			}
			wantStops := 0
			if success || scenario == "controller-refusal" {
				wantStops = 1
			}
			if controller.stops != wantStops {
				t.Fatalf("controller calls=%d want=%d", controller.stops, wantStops)
			}
			if (scenario == "absent" || scenario == "stat-error") && reads.calls != 0 {
				t.Fatal("absent/unreadable marker still ran netns pids")
			}
		})
	}
}

func TestNativeNetworkStartRecoversMissingOwnedState(t *testing.T) {
	for _, scenario := range []string{"recover", "holder", "conflict", "retry-fails", "rediscovery-fails", "init-fails", "fresh-failure"} {
		t.Run(scenario, func(t *testing.T) {
			controller := &fakeNativeNetworkLifecycle{startErr: nativegateway.ErrNetworkNeedsCleanup}
			reads := &fakeNamespacePIDRead{}
			if scenario == "holder" {
				reads.pids = "31415\n"
			}
			if scenario == "conflict" {
				controller.stopErr = errors.New("ownership conflict")
			}
			probes, initializations, cleanups := 0, 0, 0
			err := startNativeNetwork(context.Background(), controller, scenario != "fresh-failure", func(context.Context) (nativegateway.Uplink, error) {
				probes++
				if scenario == "rediscovery-fails" && probes == 2 {
					return nativegateway.Uplink{}, errors.New("uplink unavailable")
				}
				return nativegateway.Uplink{Interface: "eth0"}, nil
			}, func(context.Context) error {
				initializations++
				if scenario == "init-fails" {
					return errors.New("initialization failed")
				}
				return nil
			}, func(ctx context.Context) error {
				cleanups++
				if err := stopNativeNetwork(ctx, controller, reads, func() (bool, error) { return true, nil }); err != nil {
					return err
				}
				if scenario != "retry-fails" {
					controller.startErr = nil
				}
				return nil
			})
			if (scenario == "holder" || scenario == "conflict") && errors.Is(err, nativegateway.ErrNetworkNeedsCleanup) {
				t.Fatal("cleanup failure was masked as retryable network drift")
			}
			if (err == nil) != (scenario == "recover") {
				t.Fatalf("unexpected result: %v", err)
			}
			wantStarts, wantStops, wantProbes, wantInit, wantCleanup := 2, 1, 2, 0, 1
			switch scenario {
			case "recover":
				wantInit = 1
			case "holder":
				wantStarts, wantStops, wantProbes = 1, 0, 1
			case "conflict":
				wantStarts, wantProbes = 1, 1
			case "rediscovery-fails":
				wantStarts = 1
			case "init-fails":
				wantStops, wantInit = 2, 1
			case "fresh-failure":
				wantStarts, wantStops, wantProbes, wantCleanup = 1, 0, 1, 0
			}
			if controller.starts != wantStarts || controller.stops != wantStops || probes != wantProbes || initializations != wantInit || cleanups != wantCleanup {
				t.Fatalf("starts=%d stops=%d probes=%d initializations=%d cleanups=%d", controller.starts, controller.stops, probes, initializations, cleanups)
			}
		})
	}
}
