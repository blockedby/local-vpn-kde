package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func bridgeFixture(t *testing.T, status string) *Bridge {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "vibe-vpn"), 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "adapter")
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then\ncat <<'STATUS'\n" + status + "\nSTATUS\nelse\nprintf 'vpnkit_phase=compose-up\\nlocal vpnkit stack failed to start\\nprivate-output-marker\\n'\nexit 1\nfi\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := NewBridge(BridgeOptions{Base: base, Executable: executable, Grace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestBridgeStatusPythonParity(t *testing.T) {
	status := `{"container":"healthy","subscription":"configured","routing_policy":"smart","networkmanager":{"configured":"yes","active":"no"},"secret":"private-output-marker"}`
	b := bridgeFixture(t, status)
	var native bytes.Buffer
	if err := b.Serve(context.Background(), strings.NewReader("{\"action\":\"status\"}\n"), &native); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python migration oracle unavailable")
	}
	cmd := exec.Command(python, "../../scripts/vpnkit/vpnkit_local_kde_tui.py", "--bridge", "--lifecycle-executable", b.options.Executable)
	cmd.Env = append(os.Environ(), "VPNKIT_LOCAL_TEST_FIXTURE=1", "VPNKIT_LOCAL_SECRETS_DIR="+b.options.Base)
	cmd.Stdin = strings.NewReader("{\"action\":\"status\"}\n")
	legacy, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if json.Unmarshal(native.Bytes(), &got) != nil || json.Unmarshal(legacy, &want) != nil {
		t.Fatal("invalid protocol output")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status differs: native=%s legacy=%s", native.Bytes(), legacy)
	}
	if strings.Contains(native.String(), "private-output-marker") {
		t.Fatal("status leaked output")
	}
}
func TestBridgeFailureClassificationAndPrivateLog(t *testing.T) {
	b := bridgeFixture(t, `{"container":"absent","networkmanager":{"configured":"yes","active":"no"}}`)
	var output bytes.Buffer
	if err := b.Serve(context.Background(), strings.NewReader("{\"action\":\"backend/start\",\"progress\":true}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-output-marker") || !strings.Contains(output.String(), "gateway-start-failed") || !strings.Contains(output.String(), `"phase":"compose-up"`) {
		t.Fatalf("unsafe/unclassified reply: %s", output.String())
	}
	files, err := filepath.Glob(filepath.Join(b.options.Base, "diagnostics/*.log"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing private log")
	}
	data, err := os.ReadFile(files[0])
	if err != nil || !strings.Contains(string(data), "private-output-marker") {
		t.Fatal("private evidence lost")
	}
	st, err := os.Stat(files[0])
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("unsafe log mode")
	}
}
func TestBridgeRefusesTruncatedMutation(t *testing.T) {
	b := bridgeFixture(t, `{}`)
	var output bytes.Buffer
	err := b.Serve(context.Background(), strings.NewReader(`{"action":"start"}`), &output)
	if err == nil || output.Len() != 0 {
		t.Fatal("incomplete line was executed")
	}
	logs, _ := filepath.Glob(filepath.Join(b.options.Base, "diagnostics/*"))
	if len(logs) > 0 {
		t.Fatal("mutation started")
	}
}
func TestBridgeDoesNotClaimConnectedWithUnknownNM(t *testing.T) {
	b := bridgeFixture(t, `{"container":"healthy"}`)
	b.refresh(context.Background())
	if b.Status()["vpn_state"] != "unknown" {
		t.Fatal("healthy gateway claimed a host VPN without NetworkManager evidence")
	}
}
func TestCatalogRejectsForeignBatchAndSanitizesUntested(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	other := "srv_" + strings.Repeat("b", 27)
	raw := []byte(`{"schema":"vibe-vpn.server-browser.v2","status":"ok","servers":[{"server_id":"` + id + `","display_name":"node\u001b[31m","secret":"private-output-marker","download_mbps":-1}]}`)
	if _, err := sanitizeCatalog(raw, []string{other}); err == nil {
		t.Fatal("foreign batch accepted")
	}
	got, err := sanitizeCatalog(raw, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(got)
	if strings.Contains(string(serialized), "private-output-marker") || strings.Contains(string(serialized), "download_mbps") || strings.Contains(string(serialized), `\u001b`) {
		t.Fatal("invalid catalog fields leaked")
	}
	row := got["servers"].([]any)[0].(map[string]any)
	if row["status"] != "untested" || row["ping_status"] != "untested" || row["availability"] != "untested" {
		t.Fatal("missing measurement became failed")
	}
}
