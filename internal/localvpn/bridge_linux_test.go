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
			request, _ := json.Marshal(map[string]any{"id": "batch-1", "action": "servers/check-batch", "value": string(value), "progress": true})
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
			if event["id"] != "batch-1" || event["event"] != "server-check" || event["latency_ms"] != float64(23) || event["secret"] != nil {
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
			if final["id"] != "batch-1" {
				t.Fatal("lost request ID", final)
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

// The real subprocess adapter is held behind files so these checks exercise the
// JSON transport, process cancellation and scheduling without touching a VPN.
func TestBridgeMultiplexesSelectionAndCancelsOnlyProbe(t *testing.T) {
	b := bridgeFixture(t, `{}`)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	id := "srv_" + strings.Repeat("a", 27)
	script := fmt.Sprintf(`#!/bin/sh
if [ "$2" = speed ]; then
 touch '%s'
 while [ ! -e '%s' ]; do sleep 0.01; done
fi
printf '%%s\n' '{"schema":"vibe-vpn.server-browser.v2","status":"ok","server":{"server_id":"%s","display_name":"Fixture","selected":true}}'
`, started, release, id)
	if err := os.WriteFile(b.options.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	input, send := io.Pipe()
	output, write := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer input.Close()
	defer send.Close()
	defer output.Close()
	defer write.Close()
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx, input, write); write.Close() }()
	events := make(chan map[string]any, 10)
	go func() {
		d := json.NewDecoder(output)
		for {
			var event map[string]any
			if d.Decode(&event) != nil {
				return
			}
			events <- event
		}
	}()
	request := func(value any) {
		t.Helper()
		if err := json.NewEncoder(send).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	request(map[string]any{"id": "speed-1", "action": "servers/speed", "value": id})
	waitBridgeFile(t, started)
	request(map[string]any{"id": "select-1", "action": "servers/select", "value": id})
	select {
	case event := <-events:
		if event["id"] != "select-1" || event["ok"] != true {
			t.Fatal(event)
		}
	case <-ctx.Done():
		t.Fatal("selection waited for speed probe")
	}
	request(map[string]any{"id": "speed-1", "action": "cancel"})
	select {
	case event := <-events:
		if event["id"] != "speed-1" || event["reason"] != "cancelled" {
			t.Fatal(event)
		}
	case <-ctx.Done():
		t.Fatal("target cancellation did not stop probe")
	}
	request(map[string]any{"id": "select-2", "action": "servers/select", "value": id})
	send.Close()
	select {
	case event := <-events:
		if event["id"] != "select-2" || event["ok"] != true {
			t.Fatal("cancel damaged next request", event)
		}
	case <-ctx.Done():
		t.Fatal("no final reply")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitBridgeFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("adapter did not reach barrier", path)
}

func TestBridgeSerializesMutationsAndCorrelatesProgress(t *testing.T) {
	b := bridgeFixture(t, `{}`)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	second := filepath.Join(dir, "second")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
start)
 touch '%s'
 printf 'vpnkit_phase=compose-up\n'
 while :; do sleep 0.01; done;;
disconnect) touch '%s';;
status) printf '{}\n';;
esac
`, started, second)
	if err := os.WriteFile(b.options.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	input, send := io.Pipe()
	output, write := io.Pipe()
	defer input.Close()
	defer send.Close()
	defer output.Close()
	defer write.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx, input, write); write.Close() }()
	events := make(chan map[string]any, 10)
	go func() {
		d := json.NewDecoder(output)
		for {
			var event map[string]any
			if d.Decode(&event) != nil {
				return
			}
			events <- event
		}
	}()
	encoder := json.NewEncoder(send)
	if err := encoder.Encode(map[string]any{"id": "start", "action": "start", "progress": true}); err != nil {
		t.Fatal(err)
	}
	waitBridgeFile(t, started)
	select {
	case event := <-events:
		if event["id"] != "start" || event["event"] != "progress" {
			t.Fatal(event)
		}
	case <-ctx.Done():
		t.Fatal("missing correlated progress")
	}
	if err := encoder.Encode(map[string]any{"id": "stop", "action": "disconnect"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("mutations overlapped")
	}
	// The second mutation is queued, but cancellation must not wait for start.
	if err := encoder.Encode(map[string]any{"id": "stop", "action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event["id"] != "stop" || event["reason"] != "cancelled" {
			t.Fatal("queued cancellation was not scoped", event)
		}
	case <-ctx.Done():
		t.Fatal("queued cancellation waited for active mutation")
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("cancelled queued mutation executed")
	}
	if err := encoder.Encode(map[string]any{"id": "stop-2", "action": "disconnect"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(map[string]any{"id": "start", "action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	send.Close()
	replies := map[string]map[string]any{}
	for len(replies) < 2 {
		select {
		case event := <-events:
			if event["event"] == nil {
				replies[event["id"].(string)] = event
			}
		case <-ctx.Done():
			t.Fatal("requests not drained")
		}
	}
	if replies["start"]["reason"] != "cancelled" || replies["stop-2"]["ok"] != true {
		t.Fatal(replies)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitBridgeFile(t, second)
}

func TestBridgeConcurrentStatusRepliesRemainWholeAndCorrelated(t *testing.T) {
	b, err := NewBridge(BridgeOptions{Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	var input, output bytes.Buffer
	for i := 0; i < 16; i++ {
		if err := json.NewEncoder(&input).Encode(map[string]any{"id": fmt.Sprint(i), "action": "status"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Serve(context.Background(), &input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		var reply map[string]any
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal("interleaved JSON output", err)
		}
		id, ok := reply["id"].(string)
		if !ok || seen[id] || reply["ok"] != true {
			t.Fatal(reply)
		}
		seen[id] = true
	}
	if output.Len() != 0 {
		t.Fatal("extra replies")
	}
}
