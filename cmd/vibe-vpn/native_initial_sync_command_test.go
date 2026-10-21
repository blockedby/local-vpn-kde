package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestNativeInitialSyncCommandPreservesPreparedSelection(t *testing.T) {
	root := t.TempDir()
	etcRoot := filepath.Join(root, "etc", "local-vpn-kde")
	varRoot := filepath.Join(root, "var", "lib", "local-vpn-kde")
	runRoot := filepath.Join(root, "run", "local-vpn-kde")
	binRoot := filepath.Join(root, "usr", "libexec", "local-vpn-kde")

	configDir := filepath.Join(etcRoot, "vibe-vpn")
	sourceDir := filepath.Join(etcRoot, "sing-box")
	runtimeDir := filepath.Join(varRoot, "sing-box")
	stateDir := filepath.Join(varRoot, "vibe-vpn")
	for _, dir := range []string{configDir, sourceDir, runtimeDir, stateDir, runRoot, binRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	configPath := filepath.Join(configDir, "config.yaml")
	sourcePath := filepath.Join(sourceDir, "config.json")
	runtimePath := filepath.Join(runtimeDir, "config.json")
	singBoxBin := filepath.Join(binRoot, "sing-box")
	if err := os.WriteFile(singBoxBin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}

	configYAML := fmt.Sprintf(`subscription_file: %s
runtime: singbox
sing_box_bin: %s
sing_box_config: %s
sing_box_service: vpnkit-supervised-sing-box
sing_box_restart_mode: request-file
sing_box_restart_file: %s
sing_box_restart_ack_generation_file: %s
sing_box_restart_ack_file: %s
sing_box_restart_ack_timeout: 60s
state_dir: %s
production_socks: 127.0.0.1:2080
test_socks: 127.0.0.1:18080
test_url: https://example.invalid/fixture
test_limit_kib: 64
test_duration_seconds: 0
timeout_seconds: 1
`,
		strconv.Quote(filepath.Join(configDir, "sub_url")),
		strconv.Quote(singBoxBin),
		strconv.Quote(runtimePath),
		strconv.Quote(filepath.Join(runRoot, "restart-sing-box")),
		strconv.Quote(filepath.Join(runRoot, "sing-box-generation")),
		strconv.Quote(filepath.Join(runRoot, "sing-box-generation.ack")),
		strconv.Quote(stateDir),
	)
	if err := os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}

	source := []byte(`{
  "log": {"level": "info", "source_marker": "refreshed-source"},
  "inbounds": [{"type": "tun", "tag": "tun-in"}],
  "outbounds": [
    {"type": "direct", "tag": "direct-out"},
    {"type": "vless", "tag": "selected-native-out", "server": "placeholder.invalid", "server_port": 443, "tls": {"enabled": true, "server_name": "placeholder.invalid", "source_marker": true}, "transport": {"type": "tcp", "path": "/source-placeholder", "headers": {"Host": "placeholder.invalid"}}}
  ],
  "route": {"final": "selected-native-out", "rule_set": ["source-rule"]}
}
`)
	runtime := []byte(`{
  "log": {"level": "error", "source_marker": "stale-runtime"},
  "inbounds": [{"type": "tun", "tag": "stale-in"}],
  "outbounds": [
    {"type": "direct", "tag": "direct-out"},
    {"type": "vless", "tag": "selected-native-out", "server": "192.0.2.44", "server_port": 443, "uuid": "22222222-2222-2222-2222-222222222222", "tls": {"enabled": true, "server_name": "tls.example", "reality": {"enabled": true, "public_key": "PUBLICKEY", "short_id": "abcd"}}, "transport": {"type": "ws", "path": "/edge", "headers": {"Host": "cdn.example"}}, "packet_encoding": "xudp"}
  ],
  "route": {"final": "selected-native-out", "rule_set": ["stale-rule"]}
}
`)
	if err := os.WriteFile(sourcePath, source, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath, runtime, 0600); err != nil {
		t.Fatal(err)
	}

	beforeRuntime := nativeInitialSyncJSON(t, runtimePath)
	prepared := nativeInitialSyncSelectedOutbound(t, beforeRuntime)
	wantRuntime := nativeInitialSyncJSONBytes(t, source)
	nativeInitialSyncReplaceSelected(t, wantRuntime, prepared)

	// ServeNativeGateway defers transaction recovery during this pre-supervisor
	// sync. Keeping the same bounded mode also proves startup sync does not
	// create an empty transaction namespace as a side effect.
	t.Setenv("VIBE_VPN_DEFER_TRANSACTION_RECOVERY", "1")
	args := []string{"--config", configPath, "sync-sing-box-config", "--source", sourcePath, "--runtime", runtimePath}
	if err := executeRootCommand(newRootCommand(), args); err != nil {
		t.Fatalf("sync-sing-box-config failed: %v", err)
	}

	gotRuntime := nativeInitialSyncJSON(t, runtimePath)
	gotSelected := nativeInitialSyncSelectedOutbound(t, gotRuntime)
	if !reflect.DeepEqual(gotSelected, prepared) {
		t.Fatalf("prepared selected outbound changed: got %#v, want %#v", gotSelected, prepared)
	}
	if !reflect.DeepEqual(gotRuntime, wantRuntime) {
		t.Fatalf("runtime did not apply refreshed source settings: got %#v, want %#v", gotRuntime, wantRuntime)
	}

	gotSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSource, source) {
		t.Fatal("protected source was modified")
	}

	for _, name := range []string{"current-node.json", "current-link.txt"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("startup sync invented %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "transactions")); !os.IsNotExist(err) {
		t.Fatalf("startup sync invented transaction journal namespace: %v", err)
	}
}

func nativeInitialSyncJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return nativeInitialSyncJSONBytes(t, data)
}

func nativeInitialSyncJSONBytes(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func nativeInitialSyncSelectedOutbound(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	outbounds, ok := config["outbounds"].([]any)
	if !ok {
		t.Fatal("config has no outbounds")
	}
	for _, value := range outbounds {
		outbound, ok := value.(map[string]any)
		if ok && outbound["tag"] == "selected-native-out" {
			return outbound
		}
	}
	t.Fatal("config has no selected-native-out outbound")
	return nil
}

func nativeInitialSyncReplaceSelected(t *testing.T, config map[string]any, selected map[string]any) {
	t.Helper()
	outbounds, ok := config["outbounds"].([]any)
	if !ok {
		t.Fatal("config has no outbounds")
	}
	for i, value := range outbounds {
		outbound, ok := value.(map[string]any)
		if ok && outbound["tag"] == "selected-native-out" {
			outbounds[i] = selected
			return
		}
	}
	t.Fatal("config has no selected-native-out outbound")
}
