package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
)

func TestBenchmarkCommandUsesNonApplyingTestAndBoundedActualFlags(t *testing.T) {
	got := benchmarkCommandArgs("/run/native-vpn-lab/benchmark/config.json", 37)
	want := []string{"test", "--config", "/run/native-vpn-lab/benchmark/config.json", "--max", "37", "--limit-kib", "64", "--duration-sec", "0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("benchmark arguments = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "pick") {
		t.Fatal("benchmark command can apply a winner")
	}
}

func TestRealLabCandidateCapDefaultsTo50AndRejectsOutOfRange(t *testing.T) {
	if got, ok := parseRealLabArgs(nil); got != 50 || !ok {
		t.Fatalf("default cap = %d valid=%t", got, ok)
	}
	if got, ok := parseRealLabArgs([]string{"--max-candidates", "1"}); got != 1 || !ok {
		t.Fatalf("explicit cap = %d valid=%t", got, ok)
	}
	for _, args := range [][]string{{"--max-candidates", "0"}, {"--max-candidates", "51"}, {"--max-candidates", "private"}, {"--other", "1"}} {
		if _, ok := parseRealLabArgs(args); ok {
			t.Fatalf("invalid arguments accepted: %q", args)
		}
	}
}

func TestRunRealBenchmarkLoadsPrivateStateAndChoosesExactFilteredWinner(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	privateLink := "vless://00000000-0000-4000-8000-000000000001@winner.example:443?security=tls#Winner"
	previous := runBenchmarkProcess
	t.Cleanup(func() { runBenchmarkProcess = previous })
	var gotPath string
	var gotArgs []string
	runBenchmarkProcess = func(ctx context.Context, path string, args []string) error {
		gotPath, gotArgs = path, append([]string(nil), args...)
		_, err := state.SaveJSONVersioned(ctx, stateDir, "last-results.json", []picker.NodeResult{
			{Index: 1, Link: "vless://00000000-0000-4000-8000-000000000002@excluded.example:443?security=tls", Mbps: 100, OK: true, Excluded: true},
			{Index: 2, Link: privateLink, Mbps: 20, OK: true},
			{Index: 3, Link: "vless://00000000-0000-4000-8000-000000000003@failed.example:443?security=tls", Error: "probe failed"},
		})
		return err
	}
	winner, tested, successful, err := runRealBenchmark(context.Background(), realBenchmark{configPath: configPath, stateDir: stateDir, max: 3})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != fixtureRoot+"/vibe-vpn" || !reflect.DeepEqual(gotArgs, benchmarkCommandArgs(configPath, 3)) {
		t.Fatalf("unexpected benchmark invocation path=%q args=%q", gotPath, gotArgs)
	}
	if winner.Link != privateLink || winner.Index != 2 || tested != 3 || successful != 1 {
		t.Fatalf("winner/counts = index=%d tested=%d successful=%d", winner.Index, tested, successful)
	}
	candidate, err := candidateFromBenchmarkWinner(winner)
	if err != nil || candidate.link != privateLink || candidate.ordinal != winner.Index {
		t.Fatal("benchmark winner identity changed before candidate preparation")
	}
}

func TestRunRealBenchmarkReturnsGenericFailureWithoutPrivateDiagnostics(t *testing.T) {
	stateDir := t.TempDir()
	privateText := "vless://credential@private-endpoint.example"
	previous := runBenchmarkProcess
	t.Cleanup(func() { runBenchmarkProcess = previous })
	runBenchmarkProcess = func(ctx context.Context, _ string, _ []string) error {
		_, err := state.SaveJSONVersioned(ctx, stateDir, "last-results.json", []picker.NodeResult{{Index: 1, Link: privateText, Error: privateText}})
		if err != nil {
			return err
		}
		return errors.New(privateText)
	}
	_, tested, successful, err := runRealBenchmark(context.Background(), realBenchmark{configPath: "/run/native-vpn-lab/benchmark/config.json", stateDir: stateDir, max: 1})
	if err == nil || tested != 1 || successful != 0 {
		t.Fatalf("failure/counts = err=%v tested=%d successful=%d", err, tested, successful)
	}
	if strings.Contains(err.Error(), privateText) || err.Error() != "missing working baseline" {
		t.Fatalf("private benchmark diagnostic escaped: %q", err)
	}
}

func TestCandidateFromBenchmarkWinnerFailsClosed(t *testing.T) {
	for _, winner := range []picker.NodeResult{
		{Index: 1, OK: false, Link: "vless://00000000-0000-4000-8000-000000000001@example.com:443"},
		{Index: 1, OK: true, Excluded: true, Link: "vless://00000000-0000-4000-8000-000000000001@example.com:443"},
		{Index: 1, OK: true, Link: "private material"},
	} {
		if _, err := candidateFromBenchmarkWinner(winner); err == nil || strings.Contains(err.Error(), winner.Link) {
			t.Fatal("invalid winner was accepted or leaked")
		}
	}
}

func TestResetRealCandidateRuntimeRemovesPreservedSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"outbounds":[{"tag":"selected-native-out","server":"stale.invalid"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := resetRealCandidateRuntime(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale runtime candidate config was retained")
	}
	if err := resetRealCandidateRuntime(path); err != nil {
		t.Fatal("missing runtime candidate config was not idempotent")
	}
}

func TestClassifyHTTPSFailureIsFixedAndPrivate(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{x509.UnknownAuthorityError{}, "tls-validation"},
		{fmt.Errorf("private endpoint: %w", syscall.ECONNREFUSED), "dial-refused"},
		{fmt.Errorf("private endpoint: %w", syscall.ECONNRESET), "connection-reset"},
		{context.DeadlineExceeded, "dial-timeout"},
		{&net.DNSError{IsTimeout: true}, "dial-timeout"},
		{errors.New("private endpoint and address"), "transport"},
	}
	for _, test := range tests {
		if got := classifyHTTPSFailure(test.err); got != test.want {
			t.Fatalf("classification = %q, want %q", got, test.want)
		}
	}
}

func TestChildStopAllowsGracefulCleanupBeforeContextKill(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stopped")
	child, err := startChild(context.Background(), "graceful", "sh", "-c",
		`trap 'printf stopped > "$1"; exit 0' TERM; while :; do sleep 1; done`, "sh", marker)
	if err != nil {
		t.Fatal(err)
	}
	// Give the shell time to install its handler before requesting shutdown.
	time.Sleep(100 * time.Millisecond)
	child.stopWithGrace(2 * time.Second)
	if data, err := os.ReadFile(marker); err != nil || string(data) != "stopped" {
		t.Fatalf("graceful cleanup was bypassed: data=%q err=%v", data, err)
	}
}

func TestPhaseWriterBuffersFragmentsAndEmitsOnlySafeClasses(t *testing.T) {
	var output bytes.Buffer
	writer := newPhaseWriter(&output)
	for _, fragment := range []string{
		`private fixture-key vpnkit_phase=not-at-start`, "\n",
		`vpnkit_phase=runtime-`, "ready\n",
		`private fixture-key: failed: Device or resource busy`, "\n",
	} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := output.String(), "vpnkit_phase=runtime-ready\nvpnkit_reason=runtime-resource-busy\n"; got != want {
		t.Fatalf("safe phase output = %q, want %q", got, want)
	}
}

func TestReadinessFailureClassificationPreservesSyntheticReason(t *testing.T) {
	child := &childProcess{
		cmd:        &exec.Cmd{},
		diagnostic: "native_gateway_vpn_lab_error=runtime: owned native gateway child is not running",
		reason:     "runtime-resource-busy",
	}
	if got := child.readinessFailureClassification(); got != "runtime-resource-busy" {
		t.Fatalf("classification = %q", got)
	}
	child.reason = ""
	if got := child.readinessFailureClassification(); got != "owned-child" {
		t.Fatalf("fallback classification = %q", got)
	}
}

func TestParseEth0DefaultRequiresUnfilteredIdentity(t *testing.T) {
	gateway, err := parseEth0Default([]byte(`[{"dst":"default","gateway":"172.17.0.1","dev":"eth0","flags":[]}]`))
	if err != nil || gateway != "172.17.0.1" {
		t.Fatal("unfiltered Docker default rejected")
	}
	for _, data := range []string{
		`[{"dst":"default","gateway":"172.17.0.1","flags":[]}]`,
		`[{"dst":"default","gateway":"172.17.0.1","dev":"tun0"}]`,
		`[{"dst":"default","gateway":"invalid","dev":"eth0"}]`,
		`[]`,
		`[{"dst":"default","gateway":"172.17.0.1","dev":"eth0"},{"dst":"default","gateway":"172.17.0.2","dev":"eth0"}]`,
	} {
		if _, err := parseEth0Default([]byte(data)); err == nil {
			t.Fatal("ambiguous default accepted")
		}
	}
}
