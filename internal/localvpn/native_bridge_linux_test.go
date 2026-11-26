//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/nativeupdate"
)

func TestNativeBridgeRejectsUnsupportedActionsBeforeAdapter(t *testing.T) {
	b, err := NewBridge(BridgeOptions{Runtime: "native", Mock: true, Base: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status()["runtime"] != "native" {
		t.Fatal("missing native runtime label")
	}
	for _, action := range []string{"subscription", "subscription/read", "toggle-mode", "servers/list", "servers/select", "retest/select", "diagnostics", "update/check", "update/install", "gateway/restart"} {
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
	for _, line := range []string{`{"action":"update/check"}`, `{"action":"update/install","value":"0.4.4"}`, `{"action":"gateway/restart"}`} {
		reply, err := b.request(context.Background(), []byte(line), nil, nil)
		if err != nil || reply["ok"] != false || reply["reason"] != "invalid-request" {
			t.Fatalf("%s in mock mode: %v %v", line, reply, err)
		}
	}
}

func installedUpdateBridge(t *testing.T) *Bridge {
	t.Helper()
	b, err := NewBridge(BridgeOptions{Runtime: "native", Repo: InstalledAssetRoot, Base: filepath.Join(t.TempDir(), "missing"), Executable: filepath.Join(InstalledAssetRoot, "scripts/vpnkit/vpnkit-native.sh")})
	if err != nil {
		t.Fatal(err)
	}
	b.options.Executable = "/bin/false"
	b.updateHooks = nativeupdate.Hooks{}
	return b
}

func TestBridgeUpdateInstallReportsRunningGateway(t *testing.T) {
	b := installedUpdateBridge(t)
	for _, running := range []bool{true, false} {
		installed := false
		b.installUpdate = func(_ context.Context, version string, _ nativeupdate.InstallOptions, _ func(string)) (string, error) {
			installed = true
			return version, nil
		}
		b.updateHooks.Running = func(context.Context) bool {
			if !installed {
				t.Fatal("checked the gateway before installation")
			}
			return running
		}
		reply, err := b.request(context.Background(), []byte(`{"action":"update/install","value":"0.4.4"}`), nil, nil)
		update, ok := reply["update"].(map[string]any)
		if err != nil || reply["ok"] != true || !ok || update["installed"] != "0.4.4" || update["gateway_running"] != running {
			t.Fatalf("running=%v: reply=%v err=%v", running, reply, err)
		}
	}
}

func TestBridgeGatewayRestartUsesExclusiveInstalledHook(t *testing.T) {
	b := installedUpdateBridge(t)
	for _, restartErr := range []error{nil, errors.New("service failed")} {
		calls := 0
		b.updateHooks.Restart = func(context.Context) error {
			calls++
			if !b.mutationMu.writer || !b.operationMu.writer {
				t.Fatal("restart must hold both exclusive lifecycle gates")
			}
			return restartErr
		}
		var output bytes.Buffer
		if err := b.Serve(context.Background(), strings.NewReader(`{"action":"gateway/restart","progress":true}`+"\n"), &output); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(&output)
		var event, reply map[string]any
		if decoder.Decode(&event) != nil || event["phase"] != "gateway-restart" || decoder.Decode(&reply) != nil || calls != 1 || reply["ok"] != (restartErr == nil) {
			t.Fatalf("restart: calls=%d event=%v reply=%v", calls, event, reply)
		}
		if restartErr != nil && (reply["reason"] != "gateway-restart-failed" || reply["error"] != restartErr.Error()) {
			t.Fatalf("missing restart error: %v", reply)
		}
	}
}
