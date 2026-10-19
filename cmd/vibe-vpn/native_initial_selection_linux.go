package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/singbox"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const (
	nativeInitialConfigPath  = "/etc/local-vpn-kde/vibe-vpn/config.yaml"
	nativeInitialSourcePath  = "/etc/local-vpn-kde/sing-box/config.json"
	nativeInitialRuntimePath = "/var/lib/local-vpn-kde/sing-box/config.json"
	nativeInitialStateDir    = "/var/lib/local-vpn-kde/vibe-vpn"
	nativeInitialSingBoxBin  = "/usr/libexec/local-vpn-kde/sing-box"
	nativeInitialServiceUser = "local-vpn-kde"
	nativeInitialTimeout     = 9 * time.Minute
	nativeInitialResultsMax  = 16 << 20
)

var (
	nativeInitialGetUID       = os.Getuid
	nativeInitialGetEUID      = os.Geteuid
	nativeInitialLookupUser   = user.Lookup
	nativeInitialReadProcSelf = func() ([]byte, error) { return os.ReadFile("/proc/self/status") }
	nativeInitialGetPPID      = os.Getppid
	nativeInitialPrctl        = unix.Prctl
)

type nativeInitialSelectionDeps struct {
	runBenchmark    func(context.Context, *cliOptions, bool, int, int, int, bool, bool) error
	prepareOutbound func(context.Context, map[string]any) (map[string]any, error)
	sourcePath      string
	runtimePath     string
	beforePublish   func()
}

func productionNativeInitialSelectionDeps() nativeInitialSelectionDeps {
	return nativeInitialSelectionDeps{
		runBenchmark:    runTestContext,
		prepareOutbound: prepareNativeInitialOutbound,
		sourcePath:      nativeInitialSourcePath,
		runtimePath:     nativeInitialRuntimePath,
	}
}

func prepareNativeInitialOutbound(ctx context.Context, outbound map[string]any) (map[string]any, error) {
	prepared, err := singbox.PrepareOutboundForNamespace(ctx, outbound)
	if err != nil {
		return nil, err
	}
	server, _ := prepared["server"].(string)
	ip := net.ParseIP(server)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, fmt.Errorf("selected endpoint is not supported by the native IPv4 uplink")
	}
	return prepared, nil
}

// newNativeInitialSelectionCommand is intentionally not a general-purpose
// picker. The root setup process must execute it as the dedicated service UID,
// with no capabilities, while the gateway is inactive and the wider setup is
// serialized. The root parent is also responsible for discarding stdout,
// because the existing benchmark prints private node details there.
func newNativeInitialSelectionCommand(o *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:    "native-initialize-selection",
		Short:  "Benchmark and publish the initial native selected outbound",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o == nil || filepath.Clean(o.configPath) != nativeInitialConfigPath || o.configPath != nativeInitialConfigPath {
				return fmt.Errorf("native initial selection requires fixed production configuration")
			}
			if err := requireNativeInitialServiceIdentity(); err != nil {
				return err
			}
			cfg, err := loadConfig(nativeInitialConfigPath)
			if err != nil {
				return err
			}
			if err := validateNativeInitialConfig(cfg); err != nil {
				return err
			}
			signalCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()
			if err := requireNativeInitialParent(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(signalCtx, nativeInitialTimeout)
			defer cancel()
			return runNativeInitialSelection(ctx, o, cfg, productionNativeInitialSelectionDeps())
		},
	}
}

func requireNativeInitialParent() error {
	parent := nativeInitialGetPPID()
	if parent <= 1 {
		return fmt.Errorf("native initial selection parent disappeared")
	}
	if err := nativeInitialPrctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0); err != nil {
		return fmt.Errorf("native initial selection cannot bind to setup parent: %w", err)
	}
	if nativeInitialGetPPID() != parent {
		return fmt.Errorf("native initial selection parent changed")
	}
	return nil
}

func requireNativeInitialServiceIdentity() error {
	uid, euid := nativeInitialGetUID(), nativeInitialGetEUID()
	if uid == 0 || euid == 0 || uid != euid {
		return fmt.Errorf("native initial selection requires an unprivileged service identity")
	}
	account, err := nativeInitialLookupUser(nativeInitialServiceUser)
	if err != nil {
		return fmt.Errorf("native initial selection service identity is unavailable")
	}
	serviceUID, err := strconv.Atoi(account.Uid)
	if err != nil || serviceUID <= 0 || uid != serviceUID {
		return fmt.Errorf("native initial selection requires the dedicated service identity")
	}
	status, err := nativeInitialReadProcSelf()
	if err != nil {
		return fmt.Errorf("native initial selection cannot verify process capabilities")
	}
	required := map[string]bool{"CapInh:": false, "CapPrm:": false, "CapEff:": false, "CapAmb:": false}
	for _, line := range strings.Split(string(status), "\n") {
		for label := range required {
			if !strings.HasPrefix(line, label) {
				continue
			}
			caps, parseErr := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, label)), 16, 64)
			if parseErr != nil || caps != 0 {
				return fmt.Errorf("native initial selection requires no process capabilities")
			}
			required[label] = true
		}
	}
	for _, found := range required {
		if !found {
			return fmt.Errorf("native initial selection cannot verify process capabilities")
		}
	}
	return nil
}

func validateNativeInitialConfig(cfg config.Config) error {
	if normalizedRuntime(cfg) != "singbox" ||
		filepath.Clean(cfg.SingBoxConfig) != nativeInitialRuntimePath || cfg.SingBoxConfig != nativeInitialRuntimePath ||
		filepath.Clean(cfg.StateDir) != nativeInitialStateDir || cfg.StateDir != nativeInitialStateDir ||
		filepath.Clean(cfg.SingBoxBin) != nativeInitialSingBoxBin || cfg.SingBoxBin != nativeInitialSingBoxBin {
		return fmt.Errorf("native initial selection configuration does not match the fixed native layout")
	}
	return nil
}

func runNativeInitialSelection(ctx context.Context, o *cliOptions, cfg config.Config, deps nativeInitialSelectionDeps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, nativeInitialTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if deps.runBenchmark == nil || deps.prepareOutbound == nil || deps.sourcePath == "" || deps.runtimePath == "" {
		return fmt.Errorf("native initial selection dependencies are incomplete")
	}

	beforeResults, err := captureNativeInitialResultsVersion(ctx, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("capture benchmark results version: %w", err)
	}
	if err := deps.runBenchmark(ctx, o, false, 50, 64, 0, false, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lock, err := state.AcquireLock(ctx, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("lock native initial selection: %w", err)
	}
	defer lock.Close()

	resultsData, freshResults, err := state.LoadPrivateFileLocked(cfg.StateDir, "last-results.json", nativeInitialResultsMax)
	if err != nil {
		return fmt.Errorf("load benchmark results: %w", err)
	}
	if freshResults.Equal(beforeResults) {
		return fmt.Errorf("benchmark did not publish fresh results")
	}
	var results []picker.NodeResult
	if err := json.Unmarshal(resultsData, &results); err != nil {
		return fmt.Errorf("decode fresh benchmark results: %w", err)
	}
	winner := picker.BestFiltered(results)
	if winner == nil {
		return fmt.Errorf("no working non-excluded node")
	}
	// Parse the exact persisted winner link rather than trusting denormalized
	// result fields or an outbound map supplied by persisted benchmark data.
	node, err := vless.Parse(winner.Link)
	if err != nil {
		return fmt.Errorf("parse selected benchmark winner: %w", err)
	}
	outbound, err := singBoxOutboundForNode(node)
	if err != nil {
		return fmt.Errorf("generate selected sing-box outbound: %w", err)
	}
	outbound, err = deps.prepareOutbound(ctx, outbound)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	outbound["tag"] = "selected-native-out"

	sourceBytes, err := os.ReadFile(deps.sourcePath)
	if err != nil {
		return fmt.Errorf("read protected native sing-box source: %w", err)
	}
	candidateSource, err := nativeInitialCandidateSource(sourceBytes, outbound)
	if err != nil {
		return err
	}
	runtimeVersion, err := state.CaptureFileVersion(deps.runtimePath)
	if err != nil {
		return fmt.Errorf("capture native runtime version: %w", err)
	}
	if err := publishNativeInitialRuntimeLocked(ctx, deps, candidateSource, runtimeVersion); err != nil {
		return err
	}
	return nil
}

func captureNativeInitialResultsVersion(ctx context.Context, stateDir string) (state.FileVersion, error) {
	lock, err := state.AcquireLock(ctx, stateDir)
	if err != nil {
		return state.FileVersion{}, err
	}
	defer lock.Close()
	_, version, err := state.LoadPrivateFileLocked(stateDir, "last-results.json", nativeInitialResultsMax)
	if os.IsNotExist(err) {
		return state.FileVersion{}, nil
	}
	return version, err
}

func nativeInitialCandidateSource(source []byte, outbound map[string]any) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(source, &cfg); err != nil {
		return nil, fmt.Errorf("decode protected native sing-box source: %w", err)
	}
	values, ok := cfg["outbounds"].([]any)
	if !ok {
		return nil, fmt.Errorf("protected native sing-box source has no outbounds")
	}
	replaced := 0
	for i, value := range values {
		entry, ok := value.(map[string]any)
		if !ok || entry["tag"] != "selected-native-out" {
			continue
		}
		values[i] = outbound
		replaced++
	}
	if replaced != 1 {
		return nil, fmt.Errorf("protected native sing-box source must contain exactly one selected outbound")
	}
	cfg["outbounds"] = values
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func publishNativeInitialRuntimeLocked(ctx context.Context, deps nativeInitialSelectionDeps, candidateSource []byte, expected state.FileVersion) error {
	dir := filepath.Dir(deps.runtimePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	sourceFile, err := os.CreateTemp(dir, ".native-initial-source-*.json")
	if err != nil {
		return err
	}
	sourceTemp := sourceFile.Name()
	defer os.Remove(sourceTemp)
	if _, err := sourceFile.Write(candidateSource); err != nil {
		_ = sourceFile.Close()
		return err
	}
	if err := sourceFile.Chmod(0600); err != nil {
		_ = sourceFile.Close()
		return err
	}
	if err := sourceFile.Sync(); err != nil {
		_ = sourceFile.Close()
		return err
	}
	if err := sourceFile.Close(); err != nil {
		return err
	}

	stageFile, err := os.CreateTemp(dir, ".native-initial-runtime-*.json")
	if err != nil {
		return err
	}
	stage := stageFile.Name()
	if err := stageFile.Close(); err != nil {
		return err
	}
	if err := os.Remove(stage); err != nil {
		return err
	}
	defer os.Remove(stage)
	if err := singbox.SyncFromSourcePreserveSelectedLocked(sourceTemp, stage); err != nil {
		return fmt.Errorf("generate native runtime from protected source: %w", err)
	}
	if deps.beforePublish != nil {
		deps.beforePublish()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := state.CaptureFileVersion(deps.runtimePath)
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return fmt.Errorf("native runtime changed during initial selection")
	}
	if err := os.Rename(stage, deps.runtimePath); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
