//go:build linux

package nativegateway

import (
	"context"
	"testing"
)

func TestControllerStatusIsReadOnlyAndIdentityBound(t *testing.T) {
	network, store, runner, controller := startedController(t)
	commands, saves := len(runner.commands), store.saves
	ready, err := controller.Status(context.Background())
	if err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if len(runner.commands) != commands || store.saves != saves {
		t.Fatal("status mutated state")
	}
	network.ns.Inode++
	if ready, err := controller.Status(context.Background()); err == nil || ready {
		t.Fatal("replacement namespace reported ready")
	}
	if len(runner.commands) != commands || store.saves != saves {
		t.Fatal("failed status mutated state")
	}
}

func TestControllerStatusIncompleteIsNotReady(t *testing.T) {
	_, store, _, controller := startedController(t)
	store.journal.Effects[store.journal.EffectCount-1].Phase = EffectIntent
	if ready, err := controller.Status(context.Background()); err != nil || ready {
		t.Fatalf("incomplete ready=%v err=%v", ready, err)
	}
}
