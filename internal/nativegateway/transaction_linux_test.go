//go:build linux

package nativegateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeJournalStore struct {
	journal  NetworkJournal
	exists   bool
	saves    int
	failSave int
}

func (s *fakeJournalStore) Load(context.Context) (NetworkJournal, bool, error) {
	return s.journal, s.exists, nil
}
func (s *fakeJournalStore) Save(_ context.Context, journal NetworkJournal) error {
	s.saves++
	if s.failSave == s.saves {
		return errors.New("save failed")
	}
	s.journal, s.exists = journal, true
	return nil
}
func (s *fakeJournalStore) Clear(context.Context) error {
	s.journal, s.exists = NetworkJournal{}, false
	return nil
}

type fakeNetwork struct {
	snapshot Snapshot
	kinds    []EffectKind
	present  [MaxNetworkEffects]bool
	conflict int
	ns       NamespaceIdentity
	veth     VethIdentity
}

func newFakeNetwork(t *testing.T, forwarding bool) *fakeNetwork {
	t.Helper()
	commands, err := PlanNetwork(validUplink(), Snapshot{IPv4Forwarding: forwarding})
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := effectKinds(commands, forwarding)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeNetwork{
		snapshot: Snapshot{IPv4Forwarding: forwarding},
		kinds:    kinds,
		conflict: -1,
		ns:       NamespaceIdentity{Device: 7, Inode: 101},
		veth:     VethIdentity{HostIfIndex: 31, GuestIfIndex: 9},
	}
}

func (n *fakeNetwork) Snapshot(context.Context) (Snapshot, error) { return n.snapshot, nil }
func (n *fakeNetwork) Inspect(_ context.Context, expected NetworkExpectation) (NetworkInspection, error) {
	var result NetworkInspection
	for i := 0; i < int(expected.EffectCount); i++ {
		if i == n.conflict {
			result.Effects[i].State = ObservationConflict
			continue
		}
		if n.present[i] {
			result.Effects[i].State = ObservationExact
			switch expected.Effects[i] {
			case EffectNamespace:
				result.Effects[i].Identity.Namespace = n.ns
			case EffectVeth:
				result.Effects[i].Identity.Veth = n.veth
			case EffectGuestMoved:
				result.Effects[i].Identity = CreatedIdentity{Namespace: n.ns, Veth: n.veth}
			}
		} else {
			result.Effects[i].State = ObservationAbsent
		}
	}
	return result, nil
}

type fakeRunner struct {
	network          *fakeNetwork
	commands         []Command
	failCall         int
	applyBeforeError bool
	cancelCall       int
	cancel           context.CancelFunc
}

func (r *fakeRunner) Run(ctx context.Context, command Command) error {
	r.commands = append(r.commands, cloneCommand(command))
	call := len(r.commands)
	if call == r.cancelCall && r.cancel != nil {
		r.cancel()
	}
	shouldFail := call == r.failCall || call == r.cancelCall
	if !shouldFail || r.applyBeforeError {
		if !r.apply(command) {
			return errors.New("unexpected command")
		}
	}
	if call == r.cancelCall {
		return ctx.Err()
	}
	if call == r.failCall {
		return errors.New("runner failed")
	}
	return nil
}

func (r *fakeRunner) apply(command Command) bool {
	commands, _ := PlanNetwork(validUplink(), r.network.snapshot)
	for i, candidate := range commands {
		if reflect.DeepEqual(command, candidate) {
			r.network.present[i] = true
			return true
		}
	}
	for i, kind := range r.network.kinds {
		reverse, ok := reverseCommand(kind, validUplink())
		if ok && reflect.DeepEqual(command, reverse) {
			r.network.present[i] = false
			if kind == EffectVeth {
				for j, other := range r.network.kinds {
					if other == EffectGuestMoved || other == EffectRouteLocalnet {
						r.network.present[j] = false
					}
				}
			}
			if kind == EffectNamespace {
				for j, other := range r.network.kinds {
					if other == EffectDisableIPv6All || other == EffectDisableIPv6Default {
						r.network.present[j] = false
					}
				}
			}
			return true
		}
	}
	return false
}

func controllerFor(network *fakeNetwork, store *fakeJournalStore, runner *fakeRunner) Controller {
	return Controller{Runner: runner, Journal: store, Inspector: network}
}

func TestControllerStartAndStopExactOwnedNetwork(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	creationCalls := len(runner.commands)
	if creationCalls != len(network.kinds) {
		t.Fatalf("ran %d creation commands, want %d", creationCalls, len(network.kinds))
	}
	if !store.exists || store.journal.Effects[0].Identity.Namespace != network.ns || store.journal.Effects[1].Identity.Veth != network.veth {
		t.Fatal("durable journal did not capture immutable created identities")
	}
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatalf("exact idempotent start: %v", err)
	}
	if len(runner.commands) != creationCalls {
		t.Fatal("idempotent start executed a mutation")
	}
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.exists {
		t.Fatal("journal was not cleared")
	}
	if !network.present[indexKind(network.kinds, EffectIPv4Forwarding)] {
		t.Fatal("global forwarding was restored blindly")
	}
	for _, kind := range []EffectKind{EffectGuestUp, EffectHostUp, EffectEgressRule, EffectUnreachableRule, EffectVeth, EffectNamespace} {
		if network.present[indexKind(network.kinds, kind)] {
			t.Fatalf("owned %v remains", kind)
		}
	}
}

func TestControllerFailureAtEveryCreationStageCleansProvableEffects(t *testing.T) {
	probe := newFakeNetwork(t, false)
	for stage := range probe.kinds {
		t.Run(effectName(probe.kinds[stage]), func(t *testing.T) {
			for _, appliedThenErrored := range []bool{false, true} {
				network := newFakeNetwork(t, false)
				store := &fakeJournalStore{}
				runner := &fakeRunner{network: network, failCall: stage + 1, applyBeforeError: appliedThenErrored}
				err := controllerFor(network, store, runner).Start(context.Background(), validUplink())
				if err == nil || !strings.Contains(err.Error(), "runner failed") {
					t.Fatalf("applied=%v: got %v", appliedThenErrored, err)
				}
				if store.exists {
					t.Fatalf("applied=%v: cleanup retained journal", appliedThenErrored)
				}
				for _, kind := range []EffectKind{EffectGuestUp, EffectHostUp, EffectEgressRule, EffectUnreachableRule, EffectVeth, EffectNamespace} {
					if network.present[indexKind(network.kinds, kind)] {
						t.Fatalf("applied=%v: effect %v remains", appliedThenErrored, kind)
					}
				}
			}
		})
	}
}

func TestControllerSavesIntentBeforeMutation(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{failSave: 1}
	runner := &fakeRunner{network: network}
	err := controllerFor(network, store, runner).Start(context.Background(), validUplink())
	if err == nil || len(runner.commands) != 0 {
		t.Fatalf("Start error=%v commands=%d; mutation ran without durable intent", err, len(runner.commands))
	}
}

func TestControllerRecoversAppliedCommandAfterJournalSaveFailure(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{failSave: 2}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	if err := controller.Start(context.Background(), validUplink()); err == nil {
		t.Fatal("wanted applied-record save failure")
	}
	if !store.exists || store.journal.Effects[0].Phase != EffectIntent || !network.present[0] {
		t.Fatal("test did not leave durable intent with an ambiguously applied mutation")
	}
	store.failSave = 0
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.exists || network.present[0] {
		t.Fatal("recovered immutable identity was not safely cleaned")
	}
}

func TestControllerCancellationReconcilesAndCleans(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{network: network, cancelCall: 7, cancel: cancel, applyBeforeError: true}
	err := controllerFor(network, store, runner).Start(ctx, validUplink())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	if store.exists || network.present[indexKind(network.kinds, EffectNamespace)] {
		t.Fatal("cancellation did not reconcile and clean the owned transaction")
	}
}

func TestControllerRejectsTamperedAndForeignStateBeforeCleanup(t *testing.T) {
	t.Run("tampered journal", func(t *testing.T) {
		network := newFakeNetwork(t, false)
		store := &fakeJournalStore{exists: true, journal: NetworkJournal{Version: NetworkJournalVersion, Uplink: validUplink(), EffectCount: 1}}
		runner := &fakeRunner{network: network}
		if err := controllerFor(network, store, runner).Stop(context.Background()); err == nil || len(runner.commands) != 0 {
			t.Fatalf("error=%v commands=%d", err, len(runner.commands))
		}
	})

	t.Run("conflicting exact name", func(t *testing.T) {
		network, store, runner, controller := startedController(t)
		network.conflict = indexKind(network.kinds, EffectNFTTable)
		before := len(runner.commands)
		if err := controller.Stop(context.Background()); err == nil || len(runner.commands) != before {
			t.Fatalf("error=%v cleanup commands=%d", err, len(runner.commands)-before)
		}
		if !store.exists {
			t.Fatal("foreign conflict cleared ownership evidence")
		}
	})

	t.Run("replaced immutable identity", func(t *testing.T) {
		network, store, runner, controller := startedController(t)
		network.veth.HostIfIndex++
		before := len(runner.commands)
		if err := controller.Stop(context.Background()); err == nil || len(runner.commands) != before {
			t.Fatalf("error=%v cleanup commands=%d", err, len(runner.commands)-before)
		}
		if !store.exists {
			t.Fatal("identity mismatch cleared journal")
		}
	})
}

func TestControllerKeepsUnreachableRuleUntilGuestPathsAreDisabled(t *testing.T) {
	_, _, runner, controller := startedController(t)
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	barrierDelete := commandPosition(runner.commands, mustReverse(t, EffectUnreachableRule))
	for _, kind := range []EffectKind{EffectGuestUp, EffectHostUp, EffectGuestDefaultRoute, EffectEgressRule} {
		position := commandPosition(runner.commands, mustReverse(t, kind))
		if position < 0 || barrierDelete < position {
			t.Fatalf("barrier delete=%d occurred before %v disable=%d", barrierDelete, kind, position)
		}
	}
}

func startedController(t *testing.T) (*fakeNetwork, *fakeJournalStore, *fakeRunner, Controller) {
	t.Helper()
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	return network, store, runner, controller
}

func indexKind(kinds []EffectKind, wanted EffectKind) int {
	for i, kind := range kinds {
		if kind == wanted {
			return i
		}
	}
	return -1
}

func mustReverse(t *testing.T, kind EffectKind) Command {
	t.Helper()
	command, ok := reverseCommand(kind, validUplink())
	if !ok {
		t.Fatalf("no reverse command for %v", kind)
	}
	return command
}

func commandPosition(commands []Command, wanted Command) int {
	for i, command := range commands {
		if reflect.DeepEqual(command, wanted) {
			return i
		}
	}
	return -1
}

func TestControllerMissingRouteRequiresVerifiedCleanup(t *testing.T) {
	for _, scenario := range []string{"missing-route", "conflicting-route", "replaced-namespace"} {
		t.Run(scenario, func(t *testing.T) {
			network := newFakeNetwork(t, false)
			store := &fakeJournalStore{}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			ctx := context.Background()
			if err := controller.Start(ctx, validUplink()); err != nil {
				t.Fatal(err)
			}
			for i, kind := range network.kinds {
				if kind == EffectConnectedRoute {
					network.present[i] = false
					if scenario == "conflicting-route" {
						network.conflict = i
					}
				}
			}
			if scenario == "replaced-namespace" {
				network.ns.Inode++
			}
			before := len(runner.commands)
			err := controller.Start(ctx, validUplink())
			if err == nil || errors.Is(err, ErrNetworkNeedsCleanup) != (scenario == "missing-route") {
				t.Fatalf("incorrect recovery classification: %v", err)
			}
			if len(runner.commands) != before {
				t.Fatal("failed start mutated network")
			}
			if scenario != "missing-route" {
				if err := controller.Stop(ctx); err == nil || len(runner.commands) != before {
					t.Fatal("unsafe cleanup accepted")
				}
				return
			}
			if err := controller.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			if err := controller.Start(ctx, validUplink()); err != nil {
				t.Fatal(err)
			}
			if ready, err := controller.Status(ctx); err != nil || !ready {
				t.Fatalf("recovered ready=%v error=%v", ready, err)
			}
		})
	}
}
