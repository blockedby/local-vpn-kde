package localvpn

import (
	"context"
	"strings"
	"testing"
)

func TestNativeOperationArguments(t *testing.T) {
	id := "srv_" + strings.Repeat("A", 27)
	for _, args := range [][]string{{"servers", "list"}, {"servers", "select", id}, {"servers", "availability", id, "https://example.com/"}, {"servers", "check-batch", id, "https://example.com/"}, {"toggle-mode"}, {"diagnostics"}, {"policy-status"}} {
		if err := validateNativeOperation(args); err != nil {
			t.Errorf("valid %v: %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"/bin/sh"}, {"servers", "list", "--config", "/tmp/x"}, {"servers", "select", "bad"}, {"servers", "availability", id, "file:///etc/shadow"}, {"toggle-mode", "--path", "/etc/shadow"}, {"servers", "check-batch", id + "," + id, "https://example.com/"}} {
		if validateNativeOperation(args) == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
func TestInstalledNativeBridgeUsesExistingServerActions(t *testing.T) {
	b, err := NewBridge(BridgeOptions{Runtime: "native", Repo: InstalledAssetRoot, Mock: true, Base: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"servers/list", "servers/refresh", "servers/current", "diagnostics", "toggle-mode"} {
		r, err := b.request(context.Background(), []byte(`{"action":"`+action+`"}`), nil, nil)
		if err != nil || r["reason"] == "unsupported-native-action" {
			t.Fatalf("%s: %v %v", action, r, err)
		}
	}
}
func TestNativePolicyDetection(t *testing.T) {
	for _, tc := range []struct{ data, policy string }{{`{"route":{"rules":[{"action":"sniff"}]}}`, "strict"}, {`{"route":{"rules":[{"outbound":"direct-out","ip_is_private":true}]}}`, "smart"}} {
		got, err := policyFromNativeConfig([]byte(tc.data))
		if err != nil || got != tc.policy {
			t.Fatalf("%s %v", got, err)
		}
	}
}
