package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	cmd := exec.Command(python, "../../test/fixtures/legacy-tui.py", "--bridge", "--lifecycle-executable", b.options.Executable)
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

func TestServerCheckQueueRequestBounds(t *testing.T) {
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv_%027d", i)
	}
	for _, n := range []int{1, 6, 1000, 1001} {
		raw, _ := json.Marshal(map[string]any{"ids": ids[:n], "url": "https://example.com"})
		_, accepted, err := serverArguments("check-batch", string(raw))
		if n <= 1000 && (err != nil || len(accepted) != n) {
			t.Fatalf("queue of %d rejected: %v", n, err)
		}
		if n > 1000 && err == nil {
			t.Fatal("oversized queue accepted")
		}
	}
}

func TestBridgeForwardsCheckProgressBeforeFinalAndDrainsCancellation(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			b := bridgeFixture(t, `{"container":"healthy"}`)
			release := filepath.Join(t.TempDir(), "release")
			t.Setenv("VPNKIT_CHECK_RELEASE", release)
			id := "srv_" + strings.Repeat("a", 27)
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"event":"server-check","server_id":"%s","stage":"ping","ping_status":"ready","latency_ms":23,"availability":"untested","secret":"private-marker"}'
while [ ! -e "$VPNKIT_CHECK_RELEASE" ]; do sleep 0.01; done
printf '%%s\n' '{"schema":"vibe-vpn.server-browser.v2","status":"ok","servers":[{"server_id":"%s","display_name":"Fixture","ping_status":"ready","latency_ms":23,"availability":"ready"}]}'
`, id, id)
			if err := os.WriteFile(b.options.Executable, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			value, _ := json.Marshal(map[string]any{"ids": []string{id}, "url": "https://example.com"})
			request, _ := json.Marshal(map[string]any{"action": "servers/check-batch", "value": string(value), "progress": true})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			done := make(chan error, 1)
			go func() { defer writer.Close(); done <- b.Serve(ctx, bytes.NewReader(append(request, '\n')), writer) }()
			decoder := json.NewDecoder(reader)
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event["event"] != "server-check" || event["latency_ms"] != float64(23) || event["secret"] != nil {
				t.Fatal("bad progress", event)
			}
			select {
			case <-done:
				t.Fatal("progress buffered until exit")
			default:
			}
			if stop {
				b.Cancel()
			} else if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			var final map[string]any
			if err := decoder.Decode(&final); err != nil {
				t.Fatal(err)
			}
			if stop && final["reason"] != "cancelled" {
				t.Fatal("lost cancellation", final)
			}
			if !stop && final["ok"] != true {
				t.Fatal("lost final result", final)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBridgeKeepsFailedMeasurementFromNonzeroExit(t *testing.T) {
	b := bridgeFixture(t, `{}`)
	id := "srv_" + strings.Repeat("a", 27)
	for _, status := range []string{"failed", "ok"} {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\n' '%s'\nexit 1\n", `{"schema":"vibe-vpn.server-browser.v2","status":"`+status+`","server":{"server_id":"`+id+`","status":"failed"}}`)
		if err := os.WriteFile(b.options.Executable, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := b.Serve(context.Background(), strings.NewReader(`{"action":"servers/speed","value":"`+id+`"}`+"\n"), &out); err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		json.Unmarshal(out.Bytes(), &reply)
		want := "failed"
		if status == "ok" {
			want = "unavailable"
		}
		if reply["reason"] != want {
			t.Fatalf("reason=%v want=%s", reply["reason"], want)
		}
	}
}
