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
	for _, action := range []string{"subscription", "subscription/read", "toggle-mode", "servers/list", "servers/select", "retest/select", "diagnostics", "update/check", "update/install"} {
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

func TestInstalledBridgeUpdateActionsNeedRealNativeRuntime(t *testing.T) {
	b, err := NewBridge(BridgeOptions{Runtime: "native", Repo: InstalledAssetRoot, Mock: true, Base: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`{"action":"update/check"}`, `{"action":"update/install","value":"0.4.4"}`} {
		reply, err := b.request(context.Background(), []byte(line), nil, nil)
		if err != nil || reply["ok"] != false || reply["reason"] != "invalid-request" {
			t.Fatalf("%s in mock mode: %v %v", line, reply, err)
		}
	}
}
