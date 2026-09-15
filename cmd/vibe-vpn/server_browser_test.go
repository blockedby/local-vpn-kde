package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/spf13/cobra"
)

func browserFixture(t *testing.T) (string, string, picker.BrowserCatalog) {
	t.Helper()
	dir := t.TempDir()
	cfg := writeTestConfig(t, dir)
	salt := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(dir, browserSaltFile), salt, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := picker.NewBrowserCatalog(4, salt, []picker.NodeResult{
		{Name: "Paris", Host: "private-one.example", Port: 443, Network: "ws", Security: "tls", Link: "vless://credential-one@private-one.example:443?type=ws&security=tls#Paris", Outbound: map[string]any{"secret": "credential-one"}, OK: true, Seconds: .025},
		{Name: "Paris", Host: "private-two.example", Port: 8443, Network: "grpc", Security: "reality", Link: "vless://credential-two@private-two.example:8443?type=grpc&security=reality#Paris", Outbound: map[string]any{"secret": "credential-two"}, OK: true},
	})
	if err := state.SaveJSON(dir, browserCatalogFile, catalog); err != nil {
		t.Fatal(err)
	}
	return dir, cfg, catalog
}

func executeBrowserCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func publishBrowserCatalogForTest(t *testing.T, dir, kind string, mutate func(*picker.BrowserCatalog)) picker.BrowserCatalog {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := recoverBrowserPublicationLocked(dir); err != nil {
		t.Fatal(err)
	}
	baseline, err := captureBrowserPublicationBaselineLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := baseline.catalog
	mutate(&catalog)
	picker.SealBrowserCatalog(baseline.salt, &catalog)
	highWater := baseline.generation.Generation
	if kind == browserPublicationRefresh {
		highWater++
		catalog.Generation = highWater
		picker.SealBrowserCatalog(baseline.salt, &catalog)
	}
	if err := publishBrowserCatalogLocked(context.Background(), dir, baseline, catalog, highWater, kind); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestBrowserJSONInvocationDetectionCoversFixedContract(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--json"}, {"test-all", "--json=true"}, {"ping", "--server-id", "srv_x", "--json"},
		{"select", "--server-id", "srv_x", "--json"}, {"current", "--json"}, {"--config", "/private/path", "list", "--json", "--unknown"},
	} {
		if !isBrowserJSONInvocation(args) {
			t.Fatalf("fixed JSON invocation not detected: %v", args)
		}
	}
	for _, args := range [][]string{
		{"list"}, {"status", "--json"}, {"status", "list", "--json"},
		{"status", "--config", "list", "--json"}, {"test", "--include", "list", "--json"},
		{"--unknown", "list", "--json"}, {"status", "--", "list", "--json"},
	} {
		if isBrowserJSONInvocation(args) {
			t.Fatalf("arbitrary argument token triggered browser JSON routing: %v", args)
		}
	}
}

func TestBrowserErrorRoutingDoesNotSuppressOrDuplicateUnrelatedErrors(t *testing.T) {
	for _, args := range [][]string{
		{"status", "list", "--json"}, {"test", "--include", "list", "--json"}, {"--unknown", "current", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		routeCommandError(errors.New("unrelated sentinel"), args, &stdout, &stderr)
		if stdout.Len() != 0 || strings.Count(stderr.String(), "unrelated sentinel") != 1 {
			t.Fatalf("unrelated error misrouted args=%v stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	routeCommandError(errors.New("private parser detail"), []string{"list", "--json", "--unknown"}, &stdout, &stderr)
	assertSafeBrowserJSON(t, stdout.String())
	if stderr.Len() != 0 || strings.Contains(stdout.String(), "private parser detail") {
		t.Fatalf("browser fallback leaked or duplicated error stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestBrowserSIGTERMUsesContextAndEmitsOneSafeCancellation(t *testing.T) {
	if os.Getenv("VIBE_VPN_BROWSER_SIGTERM_HELPER") == "1" {
		marker := os.Getenv("VIBE_VPN_BROWSER_SIGTERM_MARKER")
		browserSignalContextReadyHook = func() {
			if err := os.WriteFile(marker, []byte("ready\n"), 0600); err != nil {
				os.Exit(2)
			}
		}
		args := []string{"--config", os.Getenv("VIBE_VPN_BROWSER_SIGTERM_CONFIG"), "list", "--json"}
		err := executeRootCommand(newRootCommand(), args)
		routeCommandError(err, args, os.Stdout, os.Stderr)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	dir, cfg, _ := browserFixture(t)
	before, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	marker := filepath.Join(t.TempDir(), "signal-ready")
	child := exec.Command(os.Args[0], "-test.run=TestBrowserSIGTERMUsesContextAndEmitsOneSafeCancellation$")
	child.Env = append(os.Environ(),
		"VIBE_VPN_BROWSER_SIGTERM_HELPER=1",
		"VIBE_VPN_BROWSER_SIGTERM_MARKER="+marker,
		"VIBE_VPN_BROWSER_SIGTERM_CONFIG="+cfg,
	)
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatal("browser helper did not install its signal context")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("SIGTERM-canceled browser command unexpectedly succeeded")
	}
	assertSafeBrowserJSON(t, stdout.String())
	if !strings.Contains(stdout.String(), `"status":"canceled"`) || stderr.Len() != 0 {
		t.Fatalf("SIGTERM output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("SIGTERM changed catalog err=%v", err)
	}
}

func TestBrowserListAndCurrentJSONAreStrictlyRedacted(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	if err := state.SaveCurrent(dir, state.Current{
		Name: "Paris", Host: "private-one.example", Port: 443, Network: "ws", Security: "tls",
		Link:     "vless://credential-one@private-one.example:443?type=ws&security=tls#Paris",
		ServerID: catalog.Servers[0].ID, Generation: catalog.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config", cfg, "list", "--json"}, {"--config", cfg, "current", "--json"}} {
		body, err := executeBrowserCommand(t, args...)
		if err != nil {
			t.Fatalf("%v: %v output=%s", args, err, body)
		}
		assertSafeBrowserJSON(t, body)
		for _, forbidden := range []string{"vless://", "credential-one", "credential-two", "private-one.example", "private-two.example", dir, cfg, `"host"`, `"link"`, `"error"`, `"outbound"`, `"port"`} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%v leaked %q: %s", args, forbidden, body)
			}
		}
	}
}

func TestBrowserTestAllUsesStableIDsAndAdvancesGeneration(t *testing.T) {
	_, cfg, _ := browserFixture(t)
	results := []picker.NodeResult{
		{Name: "Same", Host: "alpha.private", Port: 443, Network: "tcp", Security: "tls", Link: "vless://first-secret@alpha.private:443?security=tls#Same"},
		{Name: "Same", Host: "beta.private", Port: 443, Network: "ws", Security: "reality", Link: "vless://second-secret@beta.private:443?type=ws&security=reality#Same"},
	}
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return append([]picker.NodeResult(nil), results...), nil
		},
		test: func(_ context.Context, _ config.Config, r picker.NodeResult) (float64, error) {
			if strings.Contains(r.Link, "second-secret") {
				return 75, errors.New("backend exposed beta.private and second-secret")
			}
			return .011, nil
		},
	}
	run := func() picker.BrowserResponse {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps); err != nil {
			t.Fatal(err)
		}
		assertSafeBrowserJSON(t, out.String())
		if strings.Contains(out.String(), "private") || strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "backend") {
			t.Fatalf("test-all leaked private data: %s", out.String())
		}
		var response picker.BrowserResponse
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := run()
	second := run()
	if second.Generation != first.Generation+1 || len(first.Servers) != 2 || len(second.Servers) != 2 {
		t.Fatalf("generation/server counts first=%+v second=%+v", first, second)
	}
	for i := range first.Servers {
		if first.Servers[i].ServerID != second.Servers[i].ServerID {
			t.Fatalf("stable ID changed: %+v %+v", first.Servers, second.Servers)
		}
	}
}

func TestBrowserCatalogReadinessTamperingFailsClosed(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	catalog.Servers[0].Result.OK = false
	// Deliberately do not reseal after mutation.
	if err := state.SaveJSON(dir, browserCatalogFile, catalog); err != nil {
		t.Fatal(err)
	}
	body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("tampered readiness accepted err=%v output=%s", err, body)
	}
	assertSafeBrowserJSON(t, body)
}

func TestBrowserCatalogLoadRejectsSymlinkHardlinkAndPermissiveMode(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "symlink", mutate: func(t *testing.T, dir string) {
			path := filepath.Join(dir, browserCatalogFile)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "catalog-target")
			if err := os.WriteFile(target, body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", mutate: func(t *testing.T, dir string) {
			if err := os.Link(filepath.Join(dir, browserCatalogFile), filepath.Join(dir, "catalog-second-link")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mode", mutate: func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, browserCatalogFile), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, cfg, _ := browserFixture(t)
			test.mutate(t, dir)
			body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
			if err == nil || !isSafeJSONExit(err) || !strings.Contains(body, `"status":"unavailable"`) {
				t.Fatalf("unsafe catalog accepted err=%v output=%s", err, body)
			}
			assertSafeBrowserJSON(t, body)
			if test.name == "mode" {
				info, statErr := os.Stat(filepath.Join(dir, browserCatalogFile))
				if statErr != nil {
					t.Fatal(statErr)
				}
				if info.Mode().Perm() != 0644 {
					t.Fatalf("catalog mode was repaired: mode=%v", info.Mode().Perm())
				}
			}
		})
	}
}

func TestBrowserCatalogStrictJSONAndDuplicateIdentityCorruptionFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, picker.BrowserCatalog)
	}{
		{name: "unknown-field", mutate: func(t *testing.T, path string, _ picker.BrowserCatalog) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			at := bytes.LastIndex(body, []byte("\n}"))
			if at < 0 {
				t.Fatal("catalog closing delimiter not found")
			}
			body = append(append(append([]byte(nil), body[:at]...), []byte(",\n  \"unknown\": true")...), body[at:]...)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "duplicate-key", mutate: func(t *testing.T, path string, _ picker.BrowserCatalog) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			needle := []byte(`"schema": "` + picker.BrowserCatalogSchema + `",`)
			body = bytes.Replace(body, needle, append(append([]byte(nil), needle...), append([]byte("\n  "), needle...)...), 1)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "trailing-value", mutate: func(t *testing.T, path string, _ picker.BrowserCatalog) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("{}\n"); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}},
		{name: "duplicate-id", mutate: func(t *testing.T, path string, catalog picker.BrowserCatalog) {
			catalog.Servers = append(catalog.Servers, catalog.Servers[0])
			salt := []byte("0123456789abcdef0123456789abcdef")
			picker.SealBrowserCatalog(salt, &catalog)
			body, err := json.MarshalIndent(catalog, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, cfg, catalog := browserFixture(t)
			test.mutate(t, filepath.Join(dir, browserCatalogFile), catalog)
			body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
			if err == nil || !isSafeJSONExit(err) || !strings.Contains(body, `"status":"unavailable"`) {
				t.Fatalf("catalog corruption accepted err=%v output=%s", err, body)
			}
			assertSafeBrowserJSON(t, body)
		})
	}
}

func TestBrowserTestAllFailsClosedOnCorruptCatalogGeneration(t *testing.T) {
	dir, cfg, _ := browserFixture(t)
	corrupt := []byte(`{"schema":"broken","generation":99}`)
	if err := os.WriteFile(filepath.Join(dir, browserCatalogFile), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "Safe", Host: "hidden.invalid", Port: 443, Network: "tcp", Security: "tls", Link: "vless://secret@hidden.invalid:443", OK: true}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) { return .01, nil },
	}
	err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"unavailable"`) {
		t.Fatalf("corrupt generation accepted err=%v output=%s", err, out.String())
	}
	got, readErr := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if readErr != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("corrupt catalog was replaced/reset: %q err=%v", got, readErr)
	}
}

func TestBrowserLegacyHighWaterMismatchRemainsUnavailable(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	salt := []byte("0123456789abcdef0123456789abcdef")
	legacy := picker.BrowserGenerationState{Schema: "vibe-vpn.private-server-generation.v1", Generation: catalog.Generation + 1}
	legacy.Integrity = browserAuthentication(salt, legacy)
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveBrowserPrivateJSONLocked(context.Background(), dir, browserGenerationFile, legacy); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if err == nil || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("legacy marker/catalog mismatch was made readable err=%v output=%s", err, body)
	}
	if _, statErr := os.Stat(filepath.Join(dir, browserCommitFile)); !os.IsNotExist(statErr) {
		t.Fatalf("ambiguous legacy publication was rebound to a commit: %v", statErr)
	}
}

func TestBrowserTestAllDoesNotSupersedeConcurrentRemovedSelection(t *testing.T) {
	dir, cfg, oldCatalog := browserFixture(t)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "Replacement", Host: "replacement.invalid", Port: 443, Network: "tcp", Security: "tls", Link: "vless://replacement-secret@replacement.invalid:443", OK: true}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) {
			selected := oldCatalog.Servers[0]
			if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
				t.Fatal(err)
			}
			return .01, nil
		},
	}
	err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"stale"`) {
		t.Fatalf("concurrent selection superseded err=%v output=%s", err, out.String())
	}
	kept, loadErr := loadBrowserCatalogLocked(dir)
	if loadErr != nil || kept.Generation != oldCatalog.Generation {
		t.Fatalf("old catalog not preserved: generation=%d err=%v", kept.Generation, loadErr)
	}
}

func TestBrowserCurrentReportsPendingTransactionRecovery(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	selected := catalog.Servers[0]
	if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: catalog.Generation, Link: selected.Result.Link}); err != nil {
		t.Fatal(err)
	}
	oldState, err := state.Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.BeginTransaction(dir, "browser-pending", []byte("old-runtime"), nil, oldState, oldState); err != nil {
		t.Fatal(err)
	}
	body, err := executeBrowserCommand(t, "--config", cfg, "current", "--json")
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(body, `"status":"recovery_required"`) {
		t.Fatalf("pending transaction hidden err=%v output=%s", err, body)
	}
	assertSafeBrowserJSON(t, body)
}

func TestBrowserPendingRecoveryTakesPriorityOverCatalogCorruption(t *testing.T) {
	dir, cfg, _ := browserFixture(t)
	snapshot, err := state.Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.BeginTransaction(dir, "browser-pending-corrupt", []byte("old"), []byte("candidate"), snapshot, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, browserCatalogFile), []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"list", "current"} {
		body, err := executeBrowserCommand(t, "--config", cfg, command, "--json")
		if err == nil || !isSafeJSONExit(err) || !strings.Contains(body, `"status":"recovery_required"`) {
			t.Fatalf("%s hid pending recovery err=%v output=%s", command, err, body)
		}
		assertSafeBrowserJSON(t, body)
	}
}

func TestBrowserTestAllCancellationPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfig(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "A", Link: "vless://secret@hidden.invalid:443"}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) {
			t.Fatal("test ran after cancellation")
			return 0, nil
		},
	}
	err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"canceled"`) {
		t.Fatalf("cancel result err=%v output=%s", err, out.String())
	}
	if _, statErr := os.Stat(filepath.Join(dir, browserCatalogFile)); !os.IsNotExist(statErr) {
		t.Fatalf("canceled test published catalog: %v", statErr)
	}
}

func TestBrowserPublicationCancellationAtEveryDurableBoundaryKeepsOldCatalog(t *testing.T) {
	for _, failpoint := range browserPublicationFailpoints() {
		t.Run(failpoint, func(t *testing.T) {
			dir, cfg, oldCatalog := browserFixture(t)
			oldBody, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
			if err != nil {
				t.Fatal(err)
			}
			selected := oldCatalog.Servers[0]
			if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			var out bytes.Buffer
			cmd.SetOut(&out)
			deps := browserPublicationTestDependencies()
			previousHook := browserPublicationFailpoint
			browserPublicationFailpoint = func(label string) {
				if label == failpoint {
					cancel()
				}
			}
			t.Cleanup(func() { browserPublicationFailpoint = previousHook })

			err = runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps)
			if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"canceled"`) {
				t.Fatalf("publication cancellation err=%v output=%s", err, out.String())
			}
			browserPublicationFailpoint = nil
			body, readErr := os.ReadFile(filepath.Join(dir, browserCatalogFile))
			if readErr != nil || !bytes.Equal(body, oldBody) {
				t.Fatalf("canceled publication changed catalog err=%v", readErr)
			}
			listBody, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
			if listErr != nil || !strings.Contains(listBody, `"generation":4`) || !strings.Contains(listBody, selected.ID) {
				t.Fatalf("old catalog unavailable after cancellation err=%v output=%s", listErr, listBody)
			}
			current, currentErr := state.LoadCurrent(dir)
			if currentErr != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
				t.Fatalf("canceled publication changed selection current=%+v err=%v", current, currentErr)
			}
			assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation)
		})
	}
}

func TestBrowserPreparedJournalPostRenameSyncFailureRetainsRecoverableBackup(t *testing.T) {
	dir, cfg, oldCatalog := normalizedBrowserFixture(t)
	before := captureBrowserBaselineForTest(t, dir)
	oldBody, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	selected := oldCatalog.Servers[0]
	if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
		t.Fatal(err)
	}

	injected := 0
	restore := state.SetPrivateFileDirectorySyncHookForTesting(func(operation, name string) error {
		if operation == "write" && name == browserPublicationJournalFile && injected == 0 {
			injected++
			return errors.New("injected prepared journal directory fsync failure")
		}
		return nil
	})
	t.Cleanup(restore)
	runFailedBrowserPublication(t, cfg)
	if injected != 1 {
		t.Fatalf("prepared journal fault injections=%d", injected)
	}
	assertPrivateBrowserTransactionFiles(t, dir)
	assertBrowserJournalPhase(t, dir, browserPublicationPrepared)
	backup, err := loadBrowserPrivateBytesForTest(t, dir, browserPublicationBackupFile, browserMaxCatalogBytes)
	if err != nil || !bytes.Equal(backup, oldBody) {
		t.Fatalf("old catalog backup was not retained err=%v", err)
	}

	body, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr != nil || !strings.Contains(body, `"generation":4`) || !strings.Contains(body, selected.ID) {
		t.Fatalf("journal/backup did not recover old catalog err=%v output=%s", listErr, body)
	}
	current, currentErr := state.LoadCurrent(dir)
	if currentErr != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
		t.Fatalf("journal sync fault changed selection current=%+v err=%v", current, currentErr)
	}
	currentBody, currentCommandErr := executeBrowserCommand(t, "--config", cfg, "current", "--json")
	if currentCommandErr != nil || !strings.Contains(currentBody, `"generation":4`) || !strings.Contains(currentBody, selected.ID) {
		t.Fatalf("current did not remain usable err=%v output=%s", currentCommandErr, currentBody)
	}
	recovered := assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation)
	if recovered.generation.Revision != before.generation.Revision+1 || recovered.commit.Revision != recovered.generation.Revision {
		t.Fatalf("publication revision did not advance monotonically before=%+v after=%+v", before.generation, recovered.generation)
	}
}

func TestBrowserRollbackJournalPostRenameSyncFailureRetainsRecoverableBackup(t *testing.T) {
	dir, cfg, oldCatalog := normalizedBrowserFixture(t)
	selected := oldCatalog.Servers[0]
	if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
		t.Fatal(err)
	}

	generationFailed, rollbackJournalFailed, journalWrites := false, false, 0
	restore := state.SetPrivateFileDirectorySyncHookForTesting(func(operation, name string) error {
		if operation != "write" {
			return nil
		}
		switch name {
		case browserPublicationJournalFile:
			journalWrites++
			if journalWrites == 2 {
				rollbackJournalFailed = true
				return errors.New("injected rollback journal directory fsync failure")
			}
		case browserGenerationFile:
			if !generationFailed {
				generationFailed = true
				return errors.New("injected generation directory fsync failure")
			}
		}
		return nil
	})
	t.Cleanup(restore)
	runFailedBrowserPublication(t, cfg)
	if !generationFailed || !rollbackJournalFailed || journalWrites != 2 {
		t.Fatalf("fault path generation_failed=%t rollback_failed=%t journal_writes=%d", generationFailed, rollbackJournalFailed, journalWrites)
	}
	assertPrivateBrowserTransactionFiles(t, dir)
	assertBrowserJournalPhase(t, dir, browserPublicationRollback)

	body, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr != nil || !strings.Contains(body, `"generation":4`) || !strings.Contains(body, selected.ID) {
		t.Fatalf("rollback journal/backup did not recover old catalog err=%v output=%s", listErr, body)
	}
	current, currentErr := state.LoadCurrent(dir)
	if currentErr != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
		t.Fatalf("rollback journal sync fault changed selection current=%+v err=%v", current, currentErr)
	}
	assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation)
}

func TestBrowserRollbackCleanupSyncFailureLeavesExactRollbackRecoverable(t *testing.T) {
	dir, cfg, oldCatalog := normalizedBrowserFixture(t)
	selected := oldCatalog.Servers[0]
	if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
		t.Fatal(err)
	}

	generationFailed, cleanupFailed := false, false
	restore := state.SetPrivateFileDirectorySyncHookForTesting(func(operation, name string) error {
		if operation == "write" && name == browserGenerationFile && !generationFailed {
			generationFailed = true
			return errors.New("injected generation directory fsync failure")
		}
		if operation == "remove" && name == browserPublicationBackupFile && !cleanupFailed {
			cleanupFailed = true
			return errors.New("injected backup removal directory fsync failure")
		}
		return nil
	})
	t.Cleanup(restore)
	runFailedBrowserPublication(t, cfg)
	if !generationFailed || !cleanupFailed {
		t.Fatalf("fault path generation_failed=%t cleanup_failed=%t", generationFailed, cleanupFailed)
	}
	if _, err := os.Lstat(filepath.Join(dir, browserPublicationBackupFile)); !os.IsNotExist(err) {
		t.Fatalf("post-unlink backup state is unexpected: %v", err)
	}
	assertBrowserJournalPhase(t, dir, browserPublicationRollback)
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, baselineErr := captureBrowserPublicationBaselineLocked(dir)
	if closeErr := lock.Close(); baselineErr == nil && closeErr != nil {
		baselineErr = closeErr
	}
	if baselineErr != nil || baseline.generation.Generation != oldCatalog.Generation+1 || baseline.catalog.Generation != oldCatalog.Generation || baseline.commit.CatalogGeneration != oldCatalog.Generation || baseline.generation.Revision != baseline.commit.Revision {
		t.Fatalf("cleanup began before exact rollback baseline=%+v err=%v", baseline, baselineErr)
	}

	body, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr != nil || !strings.Contains(body, `"generation":4`) || !strings.Contains(body, selected.ID) {
		t.Fatalf("exact rollback cleanup did not recover err=%v output=%s", listErr, body)
	}
	current, currentErr := state.LoadCurrent(dir)
	if currentErr != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
		t.Fatalf("cleanup sync fault changed selection current=%+v err=%v", current, currentErr)
	}
	assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation)
}

func TestBrowserCatalogLossFailsClosedWithoutResettingHighWater(t *testing.T) {
	dir, cfg, oldCatalog := browserFixture(t)
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "A", Host: "hidden.invalid", Port: 443, Link: "vless://secret@hidden.invalid:443"}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) { return .01, nil },
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps); err != nil {
		t.Fatalf("test-all failed: %v output=%s", err, out.String())
	}
	var first picker.BrowserResponse
	if err := json.Unmarshal(out.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Generation != oldCatalog.Generation+1 {
		t.Fatalf("first generation=%d", first.Generation)
	}
	if err := os.Remove(filepath.Join(dir, browserCatalogFile)); err != nil {
		t.Fatal(err)
	}
	cmd = &cobra.Command{}
	out.Reset()
	cmd.SetOut(&out)
	if err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps); err == nil || !strings.Contains(out.String(), `"status":"unavailable"`) {
		t.Fatalf("missing committed catalog was reset err=%v output=%s", err, out.String())
	}
	salt, _, err := loadBrowserSaltLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	generation, _, err := loadBrowserGenerationLocked(dir, salt)
	if err != nil || generation.Generation != first.Generation {
		t.Fatalf("high-water changed after catalog loss generation=%+v err=%v", generation, err)
	}
}

func TestBrowserTestAllCatalogCASRejectsSameGenerationReplacement(t *testing.T) {
	dir, cfg, _ := browserFixture(t)
	deps := browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "A", Host: "hidden.invalid", Port: 443, Link: "vless://secret@hidden.invalid:443"}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) {
			publishBrowserCatalogForTest(t, dir, browserPublicationProbe, func(latest *picker.BrowserCatalog) {
				latest.Servers[0].Result.Seconds = .777
			})
			return .01, nil
		},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"stale"`) {
		t.Fatalf("same-generation replacement bypassed CAS err=%v output=%s", err, out.String())
	}
	kept, loadErr := loadBrowserCatalogLocked(dir)
	if loadErr != nil || kept.Servers[0].Result.Seconds != .777 {
		t.Fatalf("concurrent catalog update was overwritten seconds=%v err=%v", kept.Servers[0].Result.Seconds, loadErr)
	}
}

func TestBrowserSelectCancellationBeforeApplyPublishesNoSelection(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	applied := false
	deps := browserDependencies{
		recover: func(context.Context, config.Config) error {
			cancel()
			return nil
		},
		apply: func(context.Context, config.Config, picker.NodeResult) error {
			applied = true
			return nil
		},
	}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"canceled"`) || applied {
		t.Fatalf("canceled selection err=%v applied=%t output=%s", err, applied, out.String())
	}
	if _, err := state.LoadCurrent(dir); !os.IsNotExist(err) {
		t.Fatalf("canceled selection published current state: %v", err)
	}
}

func TestBrowserSelectCancellationDoesNotHidePendingRecovery(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps := browserDependencies{
		recover: func(context.Context, config.Config) error { return nil },
		apply: func(context.Context, config.Config, picker.NodeResult) error {
			snapshot, err := state.Capture(dir)
			if err != nil {
				return err
			}
			if err := state.BeginTransaction(dir, "browser-canceled-pending", []byte("old"), []byte("candidate"), snapshot, snapshot); err != nil {
				return err
			}
			cancel()
			return context.Canceled
		},
	}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"recovery_required"`) {
		t.Fatalf("pending recovery was hidden by cancellation err=%v output=%s", err, out.String())
	}
	pending, pendingErr := state.PendingTransactions(dir)
	if pendingErr != nil || len(pending) != 1 {
		t.Fatalf("pending transaction was not retained pending=%v err=%v", pending, pendingErr)
	}
}

func TestBrowserPingRejectsStaleGenerationAfterConcurrentRefresh(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	deps := browserDependencies{test: func(_ context.Context, _ config.Config, _ picker.NodeResult) (float64, error) {
		publishBrowserCatalogForTest(t, dir, browserPublicationRefresh, func(*picker.BrowserCatalog) {})
		return .01, nil
	}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"stale"`) {
		t.Fatalf("stale ping err=%v output=%s", err, out.String())
	}
	assertSafeBrowserJSON(t, out.String())
}

func TestBrowserPingCASRejectsSameGenerationCatalogUpdate(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	deps := browserDependencies{test: func(_ context.Context, _ config.Config, _ picker.NodeResult) (float64, error) {
		publishBrowserCatalogForTest(t, dir, browserPublicationProbe, func(latest *picker.BrowserCatalog) {
			latest.Servers[1].Result.Seconds = .555
		})
		return .01, nil
	}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"stale"`) {
		t.Fatalf("same-generation ping update bypassed CAS err=%v output=%s", err, out.String())
	}
	kept, loadErr := loadBrowserCatalogLocked(dir)
	if loadErr != nil || kept.Servers[1].Result.Seconds != .555 {
		t.Fatalf("concurrent ping update was overwritten seconds=%v err=%v", kept.Servers[1].Result.Seconds, loadErr)
	}
}

func TestBrowserPingPublishesSameGenerationWithMonotonicRevision(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	if body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json"); err != nil {
		t.Fatalf("fixture normalization failed: %v output=%s", err, body)
	}
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := captureBrowserPublicationBaselineLocked(dir)
	if closeErr := lock.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	deps := browserDependencies{test: func(context.Context, config.Config, picker.NodeResult) (float64, error) { return .123, nil }}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps); err != nil {
		t.Fatalf("ping publication failed: %v output=%s", err, out.String())
	}
	lock, err = state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := captureBrowserPublicationBaselineLocked(dir)
	if closeErr := lock.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if after.catalog.Generation != before.catalog.Generation || after.generation.Generation != before.generation.Generation || after.generation.Revision != before.generation.Revision+1 || after.commit.Revision != after.generation.Revision {
		t.Fatalf("same-generation ping revision before=%+v after=%+v", before.generation, after.generation)
	}
	assertBrowserPublicationRecovered(t, dir, catalog.Generation, catalog.Generation)
}

func TestBrowserSelectSerializesAndRequiresSelectionAcknowledgement(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	var mu sync.Mutex
	applies := 0
	deps := browserDependencies{
		recover: func(context.Context, config.Config) error { return nil },
		apply: func(_ context.Context, c config.Config, result picker.NodeResult) error {
			mu.Lock()
			applies++
			mu.Unlock()
			return state.SaveCurrent(c.StateDir, state.Current{
				Name: result.Name, Host: result.Host, Port: result.Port, Network: result.Network,
				Security: result.Security, Link: result.Link, ServerID: result.ServerID, Generation: result.Generation,
			})
		},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps); err != nil {
		t.Fatalf("select failed: %v output=%s", err, out.String())
	}
	assertSafeBrowserJSON(t, out.String())
	if !strings.Contains(out.String(), `"status":"selected"`) || strings.Contains(out.String(), "credential") || strings.Contains(out.String(), "private") {
		t.Fatalf("selection output is unsafe or unacknowledged: %s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if applies != 1 {
		t.Fatalf("apply calls=%d", applies)
	}

	// A backend that returns without committing the selected ID/generation is
	// not acknowledged, even if it claims success.
	bad := deps
	bad.apply = func(context.Context, config.Config, picker.NodeResult) error { return nil }
	if err := state.SaveCurrent(dir, state.Current{Name: "old"}); err != nil {
		t.Fatal(err)
	}
	cmd = &cobra.Command{}
	out.Reset()
	cmd.SetOut(&out)
	err := runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[1].ID, bad)
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"recovery_required"`) {
		t.Fatalf("missing acknowledgement accepted err=%v output=%s", err, out.String())
	}
}

func TestBrowserSelectWaitsForConcurrentCatalogPublication(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	deps := browserDependencies{
		recover: func(context.Context, config.Config) error { return nil },
		apply: func(_ context.Context, c config.Config, result picker.NodeResult) error {
			return state.SaveCurrent(c.StateDir, state.Current{ServerID: result.ServerID, Generation: result.Generation, Link: result.Link})
		},
	}
	go func() {
		close(started)
		done <- runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("select bypassed shared lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("select did not continue after lock release")
	}
}

func TestBrowserPublicationCrashRecoveryAtEveryDurableBoundary(t *testing.T) {
	if os.Getenv("VIBE_VPN_BROWSER_PUBLICATION_CRASH_HELPER") == "1" {
		phase := os.Getenv("VIBE_VPN_BROWSER_PUBLICATION_CRASH_PHASE")
		browserPublicationFailpoint = func(label string) {
			if label == phase {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		if err := runBrowserTestAll(cmd, &cliOptions{configPath: os.Getenv("VIBE_VPN_BROWSER_PUBLICATION_CRASH_CONFIG")}, browserPublicationTestDependencies()); err != nil {
			os.Exit(3)
		}
		os.Exit(4)
	}

	for _, failpoint := range browserPublicationFailpoints() {
		t.Run(failpoint, func(t *testing.T) {
			dir, cfg, oldCatalog := browserFixture(t)
			selected := oldCatalog.Servers[0]
			if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestBrowserPublicationCrashRecoveryAtEveryDurableBoundary$")
			child.Env = append(os.Environ(),
				"VIBE_VPN_BROWSER_PUBLICATION_CRASH_HELPER=1",
				"VIBE_VPN_BROWSER_PUBLICATION_CRASH_PHASE="+failpoint,
				"VIBE_VPN_BROWSER_PUBLICATION_CRASH_CONFIG="+cfg,
			)
			var stdout, stderr bytes.Buffer
			child.Stdout, child.Stderr = &stdout, &stderr
			if err := child.Run(); err == nil {
				t.Fatal("publication crash helper unexpectedly completed")
			}
			if stdout.Len() != 0 || strings.Contains(stderr.String(), "vless://") || strings.Contains(stderr.String(), "credential") {
				t.Fatalf("crash helper leaked private output stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			assertPrivateBrowserTransactionFiles(t, dir)

			body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
			if err != nil {
				t.Fatalf("list did not recover crash: %v output=%s", err, body)
			}
			expectedCatalogGeneration := oldCatalog.Generation
			if failpoint == "after-commit-fsync" {
				expectedCatalogGeneration++
			}
			var response picker.BrowserResponse
			if err := json.Unmarshal([]byte(body), &response); err != nil || response.Generation != expectedCatalogGeneration {
				t.Fatalf("recovered catalog generation=%d want=%d err=%v output=%s", response.Generation, expectedCatalogGeneration, err, body)
			}
			current, err := state.LoadCurrent(dir)
			if err != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
				t.Fatalf("publication crash changed selection current=%+v err=%v", current, err)
			}
			assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, expectedCatalogGeneration)
		})
	}
}

func TestBrowserPublicationRejectsReplayedOlderCatalogAndCommit(t *testing.T) {
	dir, cfg, oldCatalog := browserFixture(t)
	if body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json"); err != nil {
		t.Fatalf("fixture normalization failed: %v output=%s", err, body)
	}
	oldCatalogBody, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	oldCommitBody, err := os.ReadFile(filepath.Join(dir, browserCommitFile))
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, browserPublicationTestDependencies()); err != nil {
		t.Fatalf("new publication failed: %v output=%s", err, out.String())
	}
	newCatalogBody, err := os.ReadFile(filepath.Join(dir, browserCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	newCommitBody, err := os.ReadFile(filepath.Join(dir, browserCommitFile))
	if err != nil {
		t.Fatal(err)
	}

	writeBrowserPrivateBytesForTest(t, dir, browserCatalogFile, oldCatalogBody)
	body, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr == nil || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("older authenticated catalog replay accepted err=%v output=%s", listErr, body)
	}
	writeBrowserPrivateBytesForTest(t, dir, browserCommitFile, oldCommitBody)
	body, listErr = executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr == nil || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("older catalog/commit replay accepted err=%v output=%s", listErr, body)
	}
	salt, _, err := loadBrowserSaltLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	generation, _, err := loadBrowserGenerationLocked(dir, salt)
	if err != nil || generation.Generation != oldCatalog.Generation+1 {
		t.Fatalf("replay changed high-water generation=%+v err=%v", generation, err)
	}

	writeBrowserPrivateBytesForTest(t, dir, browserCatalogFile, newCatalogBody)
	writeBrowserPrivateBytesForTest(t, dir, browserCommitFile, newCommitBody)
	body, listErr = executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr != nil || !strings.Contains(body, `"generation":5`) {
		t.Fatalf("newer completed catalog did not recover err=%v output=%s", listErr, body)
	}
	assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation+1)
}

func TestBrowserSelectionRecoveryRetainsPriorityOverPublicationRecovery(t *testing.T) {
	dir, cfg, oldCatalog := browserFixture(t)
	if body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json"); err != nil {
		t.Fatalf("fixture normalization failed: %v output=%s", err, body)
	}
	selected := oldCatalog.Servers[0]
	if err := state.SaveCurrent(dir, state.Current{ServerID: selected.ID, Generation: oldCatalog.Generation, Link: selected.Result.Link}); err != nil {
		t.Fatal(err)
	}
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := captureBrowserPublicationBaselineLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	candidate := baseline.catalog
	candidate.Generation = baseline.generation.Generation + 1
	picker.SealBrowserCatalog(baseline.salt, &candidate)
	previousHook := browserPublicationFailpoint
	browserPublicationFailpoint = func(label string) {
		if label == "after-generation-fsync" {
			panic("publication crash sentinel")
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("publication failpoint did not panic")
			}
		}()
		_ = publishBrowserCatalogLocked(context.Background(), dir, baseline, candidate, candidate.Generation, browserPublicationRefresh)
	}()
	browserPublicationFailpoint = previousHook
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.BeginTransaction(dir, "browser-selection-priority", []byte("old-runtime"), nil, snapshot, snapshot); err != nil {
		t.Fatal(err)
	}

	body, listErr := executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr == nil || !strings.Contains(body, `"status":"recovery_required"`) {
		t.Fatalf("publication recovery bypassed selection priority err=%v output=%s", listErr, body)
	}
	if _, err := os.Stat(filepath.Join(dir, browserPublicationJournalFile)); err != nil {
		t.Fatalf("publication journal changed while selection was pending: %v", err)
	}
	commit, _, err := loadBrowserCommitLocked(dir, baseline.salt)
	if err != nil || commit.HighWater != oldCatalog.Generation {
		t.Fatalf("publication recovery ran before selection recovery commit=%+v err=%v", commit, err)
	}
	current, err := state.LoadCurrent(dir)
	if err != nil || current.ServerID != selected.ID || current.Generation != oldCatalog.Generation {
		t.Fatalf("partial selection/publication current=%+v err=%v", current, err)
	}

	if err := os.RemoveAll(state.TransactionsDir(dir)); err != nil {
		t.Fatal(err)
	}
	body, listErr = executeBrowserCommand(t, "--config", cfg, "list", "--json")
	if listErr != nil || !strings.Contains(body, `"generation":4`) {
		t.Fatalf("publication did not recover after selection journal removal err=%v output=%s", listErr, body)
	}
	assertBrowserPublicationRecovered(t, dir, oldCatalog.Generation+1, oldCatalog.Generation)
}

func normalizedBrowserFixture(t *testing.T) (string, string, picker.BrowserCatalog) {
	t.Helper()
	dir, cfg, catalog := browserFixture(t)
	if body, err := executeBrowserCommand(t, "--config", cfg, "list", "--json"); err != nil {
		t.Fatalf("fixture normalization failed: %v output=%s", err, body)
	}
	return dir, cfg, catalog
}

func captureBrowserBaselineForTest(t *testing.T, dir string) browserPublicationBaseline {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	baseline, err := captureBrowserPublicationBaselineLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	return baseline
}

func runFailedBrowserPublication(t *testing.T, cfg string) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, browserPublicationTestDependencies())
	if err == nil || !isSafeJSONExit(err) || !strings.Contains(out.String(), `"status":"unavailable"`) {
		t.Fatalf("faulted publication did not fail safely err=%v output=%s", err, out.String())
	}
	assertSafeBrowserJSON(t, out.String())
}

func assertBrowserJournalPhase(t *testing.T, dir, phase string) {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	body, _, err := state.LoadPrivateFileLocked(dir, browserPublicationJournalFile, browserMaxJournalLen)
	if err != nil {
		t.Fatal(err)
	}
	var journal browserPublicationJournal
	if err := decodeStrictPrivateJSON(body, &journal); err != nil {
		t.Fatal(err)
	}
	salt, _, err := loadBrowserSaltLocked(dir)
	if err != nil || !verifyBrowserPublicationJournal(salt, journal) || journal.Phase != phase {
		t.Fatalf("journal phase=%q want=%q valid=%t err=%v", journal.Phase, phase, verifyBrowserPublicationJournal(salt, journal), err)
	}
}

func loadBrowserPrivateBytesForTest(t *testing.T, dir, name string, maxSize int64) ([]byte, error) {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	body, _, err := state.LoadPrivateFileLocked(dir, name, maxSize)
	return body, err
}

func browserPublicationFailpoints() []string {
	return []string{
		"before-generation-fsync", "after-generation-fsync",
		"before-catalog-fsync", "after-catalog-fsync",
		"before-commit-fsync", "after-commit-fsync",
	}
}

func browserPublicationTestDependencies() browserDependencies {
	return browserDependencies{
		load: func(context.Context, config.Config) ([]picker.NodeResult, error) {
			return []picker.NodeResult{{Name: "Replacement", Host: "replacement.invalid", Port: 443, Network: "tcp", Security: "tls", Link: "vless://replacement-secret@replacement.invalid:443"}}, nil
		},
		test: func(context.Context, config.Config, picker.NodeResult) (float64, error) { return .01, nil },
	}
}

func assertPrivateBrowserTransactionFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{browserPublicationJournalFile, browserPublicationBackupFile} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing private transaction file %s: %v", name, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() {
			t.Fatalf("unsafe private transaction file %s mode=%v", name, info.Mode())
		}
	}
}

func assertBrowserPublicationRecovered(t *testing.T, dir string, highWater, catalogGeneration uint64) browserPublicationBaseline {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for i := 0; i < 2; i++ {
		if err := recoverBrowserPublicationLocked(dir); err != nil {
			t.Fatalf("idempotent publication recovery %d failed: %v", i, err)
		}
	}
	baseline, err := captureBrowserPublicationBaselineLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.generation.Generation != highWater || baseline.commit.HighWater != highWater || baseline.catalog.Generation != catalogGeneration || baseline.commit.CatalogGeneration != catalogGeneration || baseline.generation.Revision != baseline.commit.Revision {
		t.Fatalf("publication pair mismatch generation=%+v commit=%+v catalog_generation=%d", baseline.generation, baseline.commit, baseline.catalog.Generation)
	}
	for _, name := range []string{browserPublicationJournalFile, browserPublicationBackupFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("publication transaction residue %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "server-browser") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("publication temporary residue: %s", entry.Name())
		}
	}
	return baseline
}

func writeBrowserPrivateBytesForTest(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	lock, err := state.AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := state.WritePrivateFileAtomicLocked(context.Background(), dir, name, body); err != nil {
		t.Fatal(err)
	}
}

func assertSafeBrowserJSON(t *testing.T, body string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("invalid JSON %q: %v", body, err)
	}
	allowedTop := map[string]bool{"schema": true, "status": true, "generation": true, "servers": true, "server": true}
	for key := range top {
		if !allowedTop[key] {
			t.Fatalf("unexpected top-level field %q in %s", key, body)
		}
	}
	if string(top["schema"]) != `"`+picker.BrowserSchema+`"` {
		t.Fatalf("unexpected schema: %s", body)
	}
	var status string
	var generation uint64
	if err := json.Unmarshal(top["status"], &status); err != nil || status == "" {
		t.Fatalf("invalid status in %s", body)
	}
	if err := json.Unmarshal(top["generation"], &generation); err != nil {
		t.Fatalf("invalid generation in %s", body)
	}
	var servers []map[string]json.RawMessage
	if raw, ok := top["servers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			t.Fatal(err)
		}
	}
	if raw, ok := top["server"]; ok {
		var server map[string]json.RawMessage
		if err := json.Unmarshal(raw, &server); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, server)
	}
	allowedServer := map[string]bool{"availability": true, "ping_status": true, "download_mbps": true, "download_seconds": true, "downloaded_bytes": true, "server_id": true, "display_name": true, "transport": true, "security": true, "status": true, "latency_ms": true, "selected": true}
	mandatoryServer := []string{"server_id", "display_name", "transport", "security", "status", "selected"}
	for _, server := range servers {
		for key := range server {
			if !allowedServer[key] {
				t.Fatalf("unexpected server field %q in %s", key, body)
			}
		}
		for _, key := range mandatoryServer {
			if _, ok := server[key]; !ok {
				t.Fatalf("missing server field %q in %s", key, body)
			}
		}
		var safe picker.BrowserServer
		encoded, err := json.Marshal(server)
		if err != nil || json.Unmarshal(encoded, &safe) != nil || safe.ServerID == "" || safe.DisplayName == "" || safe.Status == "" {
			t.Fatalf("invalid server schema in %s", body)
		}
	}
}

func TestBrowserListDiscoversWithoutStartingProbeOrGateway(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfig(t, dir)
	requests := 0
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("vless://00000000-0000-0000-0000-000000000001@private-node.example:443?security=tls&type=tcp#Test"))
	}))
	defer subscription.Close()
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte(subscription.URL), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		output, err := executeBrowserCommand(t, "--config", cfg, "list", "--json")
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		var response picker.BrowserResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Servers) != 1 || response.Servers[0].Status != "untested" || response.Servers[0].Selected {
			t.Fatalf("unexpected discovery response: %s", output)
		}
		if strings.Contains(output, "private-node") || strings.Contains(output, "00000000-") {
			t.Fatal("private data leaked")
		}
	}
	if requests != 1 {
		t.Fatalf("cached list refetched subscription %d times", requests)
	}
	if _, err := os.Stat(filepath.Join(dir, "current-node.json")); !os.IsNotExist(err) {
		t.Fatal("discovery changed selected node")
	}
}

func TestBrowserTCPPingDoesNotOverwriteSpeedReadiness(t *testing.T) {
	dir, cfg, _ := browserFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	updated := publishBrowserCatalogForTest(t, dir, browserPublicationProbe, func(c *picker.BrowserCatalog) {
		c.Servers[0].Result.Host = addr.IP.String()
		c.Servers[0].Result.Port = addr.Port
		c.Servers[0].Result.OK = false
		c.Servers[0].Result.Error = ""
		c.Servers[0].Result.Seconds = 0
		c.Servers[0].ID = picker.BrowserServerID([]byte("0123456789abcdef0123456789abcdef"), c.Servers[0].Result)
	})
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, updated.Servers[0].ID, browserDependencies{tcpPing: true}); err != nil {
		t.Fatal(err)
	}
	kept, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := kept.Find(updated.Servers[0].ID)
	if r.Result.PingStatus != "ready" || r.Result.PingMS < 1 || r.Result.OK || r.Result.Mbps != 0 {
		t.Fatalf("ping changed download readiness: %+v", r.Safe(false))
	}
	assertSafeBrowserJSON(t, out.String())
}

func TestBrowserSpeedPublishesExistingBenchmarkMetrics(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	deps := browserDependencies{probe: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
		return nettest.Result{Seconds: 3, Mbps: 8, Bytes: 3000000}, nil
	}}
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps); err != nil {
		t.Fatal(err)
	}
	kept, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := kept.Find(catalog.Servers[0].ID)
	safe := r.Safe(false)
	if safe.DownloadMbps == nil || *safe.DownloadMbps != 8 || safe.DownloadSeconds == nil || *safe.DownloadSeconds != 3 || safe.LatencyMS != nil {
		t.Fatalf("benchmark mislabeled: %+v", safe)
	}
	assertSafeBrowserJSON(t, out.String())
}

func writeTestConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := filepath.Join(dir, "config.json")
	body := `{"subscription_file":"` + filepath.Join(dir, "sub") + `","runtime":"singbox","state_dir":"` + dir + `","production_socks":"127.0.0.1:1","test_socks":"127.0.0.1:2","test_url":"http://example.invalid","test_limit_kib":1,"timeout_seconds":1}`
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte("http://example.invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBrowserSelectAcceptsUntestedAndFailedNodes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			dir, cfg, catalog := browserFixture(t)
			catalog = publishBrowserCatalogForTest(t, dir, browserPublicationProbe, func(c *picker.BrowserCatalog) {
				c.Servers[0].Result.OK = false
				if failed {
					c.Servers[0].Result.Availability = "failed"
					c.Servers[0].Result.PingStatus = "failed"
				} else {
					c.Servers[0].Result.Availability = "untested"
					c.Servers[0].Result.PingStatus = "untested"
				}
			})
			applied := false
			deps := browserDependencies{recover: func(context.Context, config.Config) error { return nil }, apply: func(_ context.Context, c config.Config, r picker.NodeResult) error {
				applied = true
				return state.SaveCurrent(c.StateDir, state.Current{ServerID: r.ServerID, Generation: r.Generation})
			}}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := runBrowserSelect(cmd, &cliOptions{configPath: cfg}, catalog.Servers[0].ID, deps); err != nil {
				t.Fatalf("select: %v %s", err, out.String())
			}
			if !applied {
				t.Fatal("selection blocked by measurement results")
			}
		})
	}
}
