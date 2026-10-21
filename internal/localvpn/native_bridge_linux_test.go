//go:build linux

package localvpn

import (
	"context"
	"testing"
)

func TestNativeBridgeRejectsUnsupportedActionsBeforeAdapter(t *testing.T) {
	b, err := NewBridge(BridgeOptions{Runtime: "native", Mock: true, Base: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status()["runtime"] != "native" {
		t.Fatal("missing native runtime label")
	}
	for _, action := range []string{"subscription", "subscription/read", "toggle-mode", "servers/list", "servers/select", "retest/select", "diagnostics"} {
		reply, err := b.request(context.Background(), []byte(`{"action":"`+action+`"}`), nil, nil)
		if err != nil || reply["ok"] != false || reply["reason"] != "unsupported-native-action" {
			t.Fatalf("action %s did not fail closed: %v %v", action, reply, err)
		}
	}
}

func TestNativeBridgeRejectsForeignAdapterAndRuntime(t *testing.T) {
	for _, options := range []BridgeOptions{
		{Runtime: "native", Repo: "/fixed/repo", Executable: "/fixed/repo/scripts/vpnkit/vpnkit-local.sh"},
		{Runtime: "other", Mock: true},
	} {
		if _, err := NewBridge(options); err == nil {
			t.Fatal("unsafe runtime configuration accepted")
		}
	}
}
