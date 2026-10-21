//go:build linux

package nativegateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const priorTestBoot = "00000000-0000-4000-8000-000000000001"
const currentTestBoot = "00000000-0000-4000-8000-000000000002"

func fixedBoot(boot string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return boot, nil }
}

func appliedPriorJournal(t *testing.T, legacy bool) NetworkJournal {
	t.Helper()
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	controller.bootID = fixedBoot(priorTestBoot)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	journal := store.journal
	if legacy {
		journal.Version = legacyNetworkJournalVersion
		journal.BootID = ""
	}
	return journal
}

func TestControllerRecoversAbsentPriorBootAndLegacyJournalWithFreshForwarding(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, forwarding := range []bool{false, true} {
			t.Run(map[bool]string{true: "legacy", false: "prior-boot"}[legacy]+map[bool]string{true: "-forwarding-on", false: "-forwarding-off"}[forwarding], func(t *testing.T) {
				old := appliedPriorJournal(t, legacy)
				network := newFakeNetwork(t, forwarding)
				// The old ledger contains all applied effects, but the new kernel has
				// none. The shared forwarding value is current host state, not ownership.
				store := &fakeJournalStore{exists: true, journal: old}
				runner := &fakeRunner{network: network}
				controller := controllerFor(network, store, runner)
				controller.bootID = fixedBoot(currentTestBoot)
				if err := controller.Start(context.Background(), validUplink()); err != nil {
					t.Fatal(err)
				}
				want, _ := PlanNetwork(validUplink(), network.snapshot)
				if !reflect.DeepEqual(runner.commands, want) {
					t.Fatal("recovery did not execute only the fresh creation plan")
				}
				if store.journal.Version != NetworkJournalVersion || store.journal.BootID != currentTestBoot || store.journal.InitialIPv4Forwarding != forwarding {
					t.Fatalf("wrong fresh epoch/forwarding: %+v", store.journal)
				}
			})
		}
	}
}

func TestControllerRecoveryRefusesRemainingConflictAndUnverifiableEffects(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, scenario := range []string{"namespace-present", "last-effect-present", "conflict", "unverifiable"} {
			t.Run(map[bool]string{true: "legacy", false: "prior-boot"}[legacy]+"-"+scenario, func(t *testing.T) {
				old := appliedPriorJournal(t, legacy)
				network := newFakeNetwork(t, false)
				switch scenario {
				case "namespace-present":
					network.present[0] = true
				case "last-effect-present":
					network.present[len(network.kinds)-1] = true
				case "conflict":
					network.conflict = 0
				}
				store := &fakeJournalStore{exists: true, journal: old}
				runner := &fakeRunner{network: network}
				controller := controllerFor(network, store, runner)
				controller.bootID = fixedBoot(currentTestBoot)
				if scenario == "unverifiable" {
					controller.Inspector = unverifiableRecoveryNetwork{network}
				}
				if err := controller.Start(context.Background(), validUplink()); err == nil {
					t.Fatal("unsafe recovery succeeded")
				}
				if len(runner.commands) != 0 || store.saves != 0 || !reflect.DeepEqual(store.journal, old) {
					t.Fatal("unsafe recovery changed ledger or host")
				}
			})
		}
	}
}

type unverifiableRecoveryNetwork struct{ *fakeNetwork }

func (n unverifiableRecoveryNetwork) Inspect(ctx context.Context, e NetworkExpectation) (NetworkInspection, error) {
	i, err := n.fakeNetwork.Inspect(ctx, e)
	i.Effects[0].State = ObservationUnverifiable
	return i, err
}

func TestControllerSameBootMissingAppliedRemainsFailure(t *testing.T) {
	old := appliedPriorJournal(t, false)
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{exists: true, journal: old}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	controller.bootID = fixedBoot(priorTestBoot)
	err := controller.Start(context.Background(), validUplink())
	if err == nil || !strings.Contains(err.Error(), "applied effect-1 is absent") {
		t.Fatalf("sameboot disappearance error=%v", err)
	}
	if store.saves != 0 || len(runner.commands) != 0 {
		t.Fatal("sameboot missing effect was reset")
	}
}

func TestControllerLegacyLiveJournalMigratesOnlyAfterIdentityProof(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	controller.bootID = fixedBoot(currentTestBoot)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	store.journal.Version = legacyNetworkJournalVersion
	store.journal.BootID = ""
	calls := len(runner.commands)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != calls || store.journal.Version != NetworkJournalVersion || store.journal.BootID != currentTestBoot {
		t.Fatal("live legacy migration restarted or omitted epoch")
	}
}

func TestControllerRecoveryAtomicReplacementFailureAndCrashIntent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{true: "legacy", false: "prior-boot"}[legacy], func(t *testing.T) {
			old := appliedPriorJournal(t, legacy)
			network := newFakeNetwork(t, false)
			store := &fakeJournalStore{exists: true, journal: old, failSave: 1}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			controller.bootID = fixedBoot(currentTestBoot)
			if err := controller.Start(context.Background(), validUplink()); err == nil {
				t.Fatal("replacement failure succeeded")
			}
			if !reflect.DeepEqual(store.journal, old) || len(runner.commands) != 0 {
				t.Fatal("failed intent replacement lost old proof or mutated host")
			}
			// A crash after atomic intent publication but before the first command:
			// the new epoch is durable, and ordinary current-boot intent reconciliation
			// resumes safely rather than attempting obsolete cleanup.
			store.journal = NetworkJournal{Version: NetworkJournalVersion, BootID: currentTestBoot, Uplink: validUplink(), EffectCount: 1}
			store.journal.Effects[0] = JournalEffect{Kind: EffectNamespace, Phase: EffectIntent}
			store.failSave = 0
			if err := controller.Start(context.Background(), validUplink()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControllerRecoveryInspectsBeyondRecordedPrefixAndValidatesFreshSnapshot(t *testing.T) {
	for _, scenario := range []string{"trailing-conflict", "fresh-snapshot-conflict", "snapshot-error"} {
		t.Run(scenario, func(t *testing.T) {
			old := appliedPriorJournal(t, false)
			old.EffectCount = 1
			for i := 1; i < MaxNetworkEffects; i++ {
				old.Effects[i] = JournalEffect{}
			}
			network := newFakeNetwork(t, false)
			if scenario == "trailing-conflict" {
				network.conflict = len(network.kinds) - 1
			}
			if scenario == "fresh-snapshot-conflict" {
				network.snapshot.Namespaces = []string{Namespace}
			}
			store := &fakeJournalStore{exists: true, journal: old}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			controller.bootID = fixedBoot(currentTestBoot)
			if scenario == "snapshot-error" {
				controller.Inspector = failedRecoverySnapshot{network}
			}
			if err := controller.Start(context.Background(), validUplink()); err == nil {
				t.Fatal("unsafe fresh plan accepted")
			}
			if store.saves != 0 || len(runner.commands) != 0 || !reflect.DeepEqual(store.journal, old) {
				t.Fatal("proof failure changed journal or network")
			}
		})
	}
}

type failedRecoverySnapshot struct{ *fakeNetwork }

func (failedRecoverySnapshot) Snapshot(context.Context) (Snapshot, error) {
	return Snapshot{}, errors.New("snapshot unavailable")
}

func TestControllerPriorBootStatusAndStopDoNotTrustReusedIdentities(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{true: "reused", false: "absent"}[present], func(t *testing.T) {
			old := appliedPriorJournal(t, false)
			network := newFakeNetwork(t, false)
			if present {
				for i := range network.kinds {
					network.present[i] = true
				}
			}
			store := &fakeJournalStore{exists: true, journal: old}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			controller.bootID = fixedBoot(currentTestBoot)
			ready, err := controller.Status(context.Background())
			if err != nil || ready {
				t.Fatalf("old boot ready=%v error=%v", ready, err)
			}
			err = controller.Stop(context.Background())
			if (err == nil) == present {
				t.Fatalf("priorboot stop present=%v error=%v", present, err)
			}
			if len(runner.commands) != 0 {
				t.Fatal("old boot stop issued network mutation")
			}
			if present && !reflect.DeepEqual(store.journal, old) {
				t.Fatal("refused stop lost old ledger")
			}
		})
	}
}

func TestJournalBootIdentityValidation(t *testing.T) {
	for _, scenario := range []string{"missing", "malformed", "legacy-with-boot", "unsupported", "unavailable-current"} {
		t.Run(scenario, func(t *testing.T) {
			old := appliedPriorJournal(t, false)
			switch scenario {
			case "missing":
				old.BootID = ""
			case "malformed":
				old.BootID = "not-a-kernel-epoch"
			case "legacy-with-boot":
				old.Version = legacyNetworkJournalVersion
			case "unsupported":
				old.Version = 3
			}
			network := newFakeNetwork(t, false)
			store := &fakeJournalStore{exists: true, journal: old}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			controller.bootID = fixedBoot(currentTestBoot)
			if scenario == "unavailable-current" {
				controller.bootID = func(context.Context) (string, error) { return "", errors.New("boot unavailable") }
			}
			if err := controller.Start(context.Background(), validUplink()); err == nil {
				t.Fatal("invalid epoch accepted")
			}
			if store.saves != 0 || len(runner.commands) != 0 {
				t.Fatal("invalid epoch changed ledger or network")
			}
		})
	}
}

func TestControllerObsoleteJournalRecoveryAcrossDurableCrashPhases(t *testing.T) {
	for _, phase := range []EffectPhase{EffectIntent, EffectApplied, EffectRemoving, EffectRemoved, EffectRelinquished} {
		t.Run(map[EffectPhase]string{EffectIntent: "intent", EffectApplied: "applied", EffectRemoving: "removing", EffectRemoved: "removed", EffectRelinquished: "relinquished"}[phase], func(t *testing.T) {
			old := appliedPriorJournal(t, false)
			if phase == EffectIntent { // A crash immediately before the first apply.
				old.EffectCount = 1
				old.Effects = [MaxNetworkEffects]JournalEffect{{Kind: EffectNamespace, Phase: EffectIntent}}
			} else if phase == EffectRemoving || phase == EffectRemoved {
				// Reverse cleanup records a suffix; its old epoch still needs retirement.
				old.Effects[int(old.EffectCount)-1].Phase = phase
			} else if phase == EffectRelinquished {
				for i := 0; i < int(old.EffectCount); i++ {
					if old.Effects[i].Kind == EffectIPv4Forwarding {
						old.Effects[i].Phase = EffectRelinquished
					}
					if i > 0 && old.Effects[i-1].Phase >= EffectRemoving {
						old.Effects[i].Phase = EffectRemoved
					}
				}
			}
			network := newFakeNetwork(t, false)
			store := &fakeJournalStore{exists: true, journal: old}
			runner := &fakeRunner{network: network}
			controller := controllerFor(network, store, runner)
			controller.bootID = fixedBoot(currentTestBoot)
			if err := controller.Start(context.Background(), validUplink()); err != nil {
				t.Fatal(err)
			}
			if store.journal.BootID != currentTestBoot {
				t.Fatal("old crash epoch retained")
			}
		})
	}
}

func TestControllerLegacyChangedIdentityCannotBeStampedCurrent(t *testing.T) {
	network := newFakeNetwork(t, false)
	store := &fakeJournalStore{}
	runner := &fakeRunner{network: network}
	controller := controllerFor(network, store, runner)
	controller.bootID = fixedBoot(currentTestBoot)
	if err := controller.Start(context.Background(), validUplink()); err != nil {
		t.Fatal(err)
	}
	store.journal.Version = legacyNetworkJournalVersion
	store.journal.BootID = ""
	old := store.journal
	saves := store.saves
	calls := len(runner.commands)
	network.ns.Inode++
	if err := controller.Start(context.Background(), validUplink()); err == nil {
		t.Fatal("replaced legacy namespace was adopted")
	}
	if store.saves != saves || len(runner.commands) != calls || !reflect.DeepEqual(store.journal, old) {
		t.Fatal("identity failure changed legacy ledger or network")
	}
}
