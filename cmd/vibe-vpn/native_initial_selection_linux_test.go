package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/singbox"
	"github.com/blockedby/local-vpn-kde/internal/state"
)

const (
	nativeInitialSlowLink = "vless://11111111-1111-1111-1111-111111111111@slow.example:443?encryption=none&security=tls&type=tcp&sni=slow-tls.example#slow"
	nativeInitialFastLink = "vless://22222222-2222-2222-2222-222222222222@fast.example:443?encryption=none&security=reality&type=ws&host=cdn.example&path=%2Fedge&sni=tls.example&fp=chrome&pbk=PUBLICKEY&sid=abcd#fast"
)

type nativeInitialFixture struct {
	stateDir    string
	sourcePath  string
	runtimePath string
	source      []byte
	cfg         config.Config
	o           *cliOptions
}

func newNativeInitialFixture(t *testing.T) nativeInitialFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "protected", "config.json")
	runtimePath := filepath.Join(root, "runtime", "config.json")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0700); err != nil {
		t.Fatal(err)
	}
	source := []byte(`{
  "log": {"level": "warn", "marker": "source-metadata"},
  "inbounds": [{"type": "tun", "tag": "tun-in"}],
  "outbounds": [
    {"type": "direct", "tag": "direct-out"},
    {"type": "vless", "tag": "selected-native-out", "server": "placeholder.invalid", "tls": {"source": true}}
  ],
  "route": {"final": "selected-native-out", "rule_set": ["geoip-ru"]}
}
`)
	if err := os.WriteFile(sourcePath, source, 0400); err != nil {
		t.Fatal(err)
	}
	return nativeInitialFixture{
		stateDir:    stateDir,
		sourcePath:  sourcePath,
		runtimePath: runtimePath,
		source:      source,
		cfg:         config.Config{StateDir: stateDir},
		o:           &cliOptions{},
	}
}

func (f nativeInitialFixture) deps(run func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error, prepare func(context.Context, map[string]any) (map[string]any, error)) nativeInitialSelectionDeps {
	return nativeInitialSelectionDeps{runBenchmark: run, prepareOutbound: prepare, sourcePath: f.sourcePath, runtimePath: f.runtimePath}
}

func saveNativeInitialResults(ctx context.Context, dir string, results []picker.NodeResult) error {
	_, err := state.SaveJSONVersioned(ctx, dir, "last-results.json", results)
	return err
}

func TestNativeInitialSelectionPublishesExactWinnerAndStartupSyncPreservesIt(t *testing.T) {
	f := newNativeInitialFixture(t)
	results := []picker.NodeResult{
		{Index: 1, Name: "slow", Link: nativeInitialSlowLink, Mbps: 5, OK: true},
		{Index: 2, Name: "fast", Link: nativeInitialFastLink, Mbps: 90, OK: true, Outbound: map[string]any{"type": "vless", "server": "persisted-data-must-not-win.invalid"}},
		{Index: 3, Name: "excluded", Link: nativeInitialSlowLink, Mbps: 900, OK: true, Excluded: true},
	}
	benchmark := func(ctx context.Context, gotO *cliOptions, apply bool, max, limit, duration int, verbose, debug bool) error {
		if gotO != f.o || apply || max != 50 || limit != 64 || duration != 0 || verbose || debug {
			t.Fatalf("unexpected benchmark invocation: apply=%v max=%d limit=%d duration=%d verbose=%v debug=%v", apply, max, limit, duration, verbose, debug)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > nativeInitialTimeout+time.Second {
			t.Fatal("benchmark context is not bounded")
		}
		return saveNativeInitialResults(ctx, f.stateDir, results)
	}
	prepare := func(ctx context.Context, outbound map[string]any) (map[string]any, error) {
		if outbound["server"] != "fast.example" || outbound["uuid"] != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("exact winner was not converted: %#v", outbound)
		}
		if _, ok := outbound["tls"].(map[string]any); !ok {
			t.Fatal("winner TLS metadata missing before resolution")
		}
		if _, ok := outbound["transport"].(map[string]any); !ok {
			t.Fatal("winner transport metadata missing before resolution")
		}
		resolved := cloneMap(outbound)
		resolved["server"] = net.IPv4(192, 0, 2, 44).String()
		return resolved, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), nativeInitialTimeout)
	defer cancel()
	if err := runNativeInitialSelection(ctx, f.o, f.cfg, f.deps(benchmark, prepare)); err != nil {
		t.Fatal(err)
	}
	gotSource, err := os.ReadFile(f.sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSource, f.source) {
		t.Fatal("protected source was modified")
	}

	runtime := readNativeInitialJSON(t, f.runtimePath)
	selected := selectedNativeInitialOutbound(t, runtime)
	if selected["server"] != "192.0.2.44" || selected["uuid"] != "22222222-2222-2222-2222-222222222222" || selected["tag"] != "selected-native-out" {
		t.Fatalf("runtime did not contain the exact prepared winner: %#v", selected)
	}
	tls, _ := selected["tls"].(map[string]any)
	reality, _ := tls["reality"].(map[string]any)
	transport, _ := selected["transport"].(map[string]any)
	headers, _ := transport["headers"].(map[string]any)
	if tls["server_name"] != "tls.example" || reality["public_key"] != "PUBLICKEY" || reality["short_id"] != "abcd" || transport["type"] != "ws" || transport["path"] != "/edge" || headers["Host"] != "cdn.example" {
		t.Fatalf("TLS/Reality/transport metadata changed: %#v", selected)
	}
	if !reflect.DeepEqual(runtime["log"], map[string]any{"level": "warn", "marker": "source-metadata"}) {
		t.Fatalf("source metadata was not preserved: %#v", runtime["log"])
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "current-node.json")); !os.IsNotExist(err) {
		t.Fatal("initial selection invented current/last-good metadata")
	}

	updatedSource := strings.Replace(string(f.source), "source-metadata", "next-startup-source", 1)
	if err := os.Chmod(f.sourcePath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.sourcePath, []byte(updatedSource), 0400); err != nil {
		t.Fatal(err)
	}
	lock, err := state.AcquireLock(context.Background(), f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	err = singbox.SyncFromSourcePreserveSelectedLocked(f.sourcePath, f.runtimePath)
	_ = lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	afterSync := readNativeInitialJSON(t, f.runtimePath)
	if afterSync["log"].(map[string]any)["marker"] != "next-startup-source" || !reflect.DeepEqual(selectedNativeInitialOutbound(t, afterSync), selected) {
		t.Fatal("next-startup sync did not refresh source metadata while preserving selection")
	}
}

func TestNativeInitialSelectionDoesNotWriteRuntimeOnNoneFailureOrCancellation(t *testing.T) {
	tests := []struct {
		name string
		run  func(nativeInitialFixture) (func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error, func(context.Context, map[string]any) (map[string]any, error))
	}{
		{
			name: "no winner",
			run: func(f nativeInitialFixture) (func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error, func(context.Context, map[string]any) (map[string]any, error)) {
				return func(ctx context.Context, _ *cliOptions, _ bool, _, _, _ int, _, _ bool) error {
					return saveNativeInitialResults(ctx, f.stateDir, []picker.NodeResult{{Link: nativeInitialSlowLink, Mbps: 100, OK: false}})
				}, func(_ context.Context, out map[string]any) (map[string]any, error) { return out, nil }
			},
		},
		{
			name: "benchmark failure",
			run: func(f nativeInitialFixture) (func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error, func(context.Context, map[string]any) (map[string]any, error)) {
				return func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error {
					return errors.New("injected benchmark failure")
				}, func(_ context.Context, out map[string]any) (map[string]any, error) { return out, nil }
			},
		},
		{
			name: "resolver cancellation",
			run: func(f nativeInitialFixture) (func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error, func(context.Context, map[string]any) (map[string]any, error)) {
				return func(ctx context.Context, _ *cliOptions, _ bool, _, _, _ int, _, _ bool) error {
						return saveNativeInitialResults(ctx, f.stateDir, []picker.NodeResult{{Link: nativeInitialFastLink, Mbps: 100, OK: true}})
					}, func(ctx context.Context, _ map[string]any) (map[string]any, error) {
						if cancel, ok := ctx.Value(nativeInitialCancelKey{}).(context.CancelFunc); ok {
							cancel()
						}
						return nil, context.Canceled
					}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeInitialFixture(t)
			oldRuntime := []byte(`{"sentinel":"unchanged"}`)
			if err := os.MkdirAll(filepath.Dir(f.runtimePath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.runtimePath, oldRuntime, 0600); err != nil {
				t.Fatal(err)
			}
			benchmark, prepare := test.run(f)
			ctx := context.Background()
			if test.name == "resolver cancellation" {
				cancelCtx, cancel := context.WithCancel(ctx)
				ctx = context.WithValue(cancelCtx, nativeInitialCancelKey{}, context.CancelFunc(cancel))
			}
			if err := runNativeInitialSelection(ctx, f.o, f.cfg, f.deps(benchmark, prepare)); err == nil {
				t.Fatal("failure path unexpectedly succeeded")
			}
			got, err := os.ReadFile(f.runtimePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, oldRuntime) {
				t.Fatal("runtime changed on failure")
			}
		})
	}
}

type nativeInitialCancelKey struct{}

func TestNativeInitialSelectionRejectsConcurrentRuntimeReplacement(t *testing.T) {
	f := newNativeInitialFixture(t)
	oldRuntime := []byte(`{"generation":"old"}`)
	if err := os.MkdirAll(filepath.Dir(f.runtimePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.runtimePath, oldRuntime, 0600); err != nil {
		t.Fatal(err)
	}
	benchmark := func(ctx context.Context, _ *cliOptions, _ bool, _, _, _ int, _, _ bool) error {
		return saveNativeInitialResults(ctx, f.stateDir, []picker.NodeResult{{Link: nativeInitialFastLink, Mbps: 100, OK: true}})
	}
	deps := f.deps(benchmark, func(_ context.Context, out map[string]any) (map[string]any, error) {
		out["server"] = "192.0.2.55"
		return out, nil
	})
	foreign := []byte(`{"generation":"foreign"}`)
	deps.beforePublish = func() {
		if err := os.WriteFile(f.runtimePath, foreign, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runNativeInitialSelection(context.Background(), f.o, f.cfg, deps); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("concurrent replacement was not rejected: %v", err)
	}
	got, err := os.ReadFile(f.runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatal("concurrent runtime replacement was overwritten")
	}
}

func TestNativeInitialSelectionCommandAndProductionGuards(t *testing.T) {
	o := &cliOptions{configPath: nativeInitialConfigPath}
	cmd := newNativeInitialSelectionCommand(o)
	if cmd.Name() != "native-initialize-selection" || !cmd.Hidden {
		t.Fatal("native initial selection command contract changed")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Fatal("extra arguments accepted")
	}
	o.configPath = filepath.Clean(nativeInitialConfigPath) + ".other"
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("non-production config path accepted")
	}

	valid := config.Config{Runtime: "singbox", SingBoxConfig: nativeInitialRuntimePath, StateDir: nativeInitialStateDir, SingBoxBin: nativeInitialSingBoxBin}
	if err := validateNativeInitialConfig(valid); err != nil {
		t.Fatal(err)
	}
	valid.SingBoxConfig = "/tmp/config.json"
	if err := validateNativeInitialConfig(valid); err == nil {
		t.Fatal("non-native runtime path accepted")
	}
}

func TestRequireNativeInitialServiceIdentityIsFullyInjected(t *testing.T) {
	oldUID, oldEUID, oldLookup, oldStatus := nativeInitialGetUID, nativeInitialGetEUID, nativeInitialLookupUser, nativeInitialReadProcSelf
	t.Cleanup(func() {
		nativeInitialGetUID, nativeInitialGetEUID, nativeInitialLookupUser, nativeInitialReadProcSelf = oldUID, oldEUID, oldLookup, oldStatus
	})
	nativeInitialGetUID = func() int { return 12345 }
	nativeInitialGetEUID = func() int { return 12345 }
	nativeInitialLookupUser = func(name string) (*user.User, error) {
		if name != nativeInitialServiceUser {
			t.Fatalf("unexpected account lookup: %s", name)
		}
		return &user.User{Uid: "12345", Username: name}, nil
	}
	nativeInitialReadProcSelf = func() ([]byte, error) {
		return []byte("Name:\ttest\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\n"), nil
	}
	if err := requireNativeInitialServiceIdentity(); err != nil {
		t.Fatal(err)
	}
	nativeInitialReadProcSelf = func() ([]byte, error) {
		return []byte("CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000001\nCapAmb:\t0000000000000000\n"), nil
	}
	if err := requireNativeInitialServiceIdentity(); err == nil {
		t.Fatal("effective capabilities accepted")
	}
	nativeInitialGetEUID = func() int { return 0 }
	if err := requireNativeInitialServiceIdentity(); err == nil {
		t.Fatal("root accepted")
	}
}

func readNativeInitialJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func selectedNativeInitialOutbound(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	values, ok := cfg["outbounds"].([]any)
	if !ok {
		t.Fatal("runtime has no outbounds")
	}
	for _, value := range values {
		outbound, ok := value.(map[string]any)
		if ok && outbound["tag"] == "selected-native-out" {
			return outbound
		}
	}
	t.Fatal("runtime has no selected outbound")
	return nil
}
