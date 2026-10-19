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

func TestNativeProfileOperationValidatesWholeRequest(t *testing.T) {
	for _, request := range []string{
		`{"action":"list"}`,
		`{"action":"open","id":"profile_one"}`,
		`{"action":"remove","id":"profile_one"}`,
		`{"action":"add","name":"VPS","source":"hy2://fixture@example.invalid"}`,
	} {
		if err := validateNativeOperation([]string{"profiles", request}); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range []string{
		`{"action":"list"}{"action":"remove","id":"profile_one"}`,
		`{"action":"list","config":"/tmp/private-marker"}`,
		`{"action":"add","name":"VPS","source":"file:///private-marker"}`,
		`{"action":"list"}` + strings.Repeat(" ", 32<<10),
	} {
		if err := validateNativeOperation([]string{"profiles", request}); err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("unsafe profile operation accepted or exposed")
		}
	}
}

func TestNativeSpeedBatchFixedContract(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	if err := validateNativeOperation([]string{"servers", "speed-batch", id, "https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"servers", "speed-batch", id + "," + id, "https://example.com/"},
		{"servers", "speed-batch", id, "file:///etc/passwd"},
		{"servers", "speed-batch", id, "https://example.com/", "--config", "/tmp/x"},
	} {
		if validateNativeOperation(args) == nil {
			t.Fatal("unsafe speed batch accepted")
		}
	}
}
