package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/state"
)

type commandRunner func(context.Context, string, ...string) error

var runCommand commandRunner = func(ctx context.Context, name string, args ...string) error {
	return runExternal(ctx, name, args...)
}
var lookupIP = net.LookupIP

var restartSequence uint64

const (
	// These bounds keep a misconfigured local supervisor from making a manual
	// apply or rollback wait forever. Normal request-file operations require the
	// token, generation, and health acknowledgement path.
	DefaultAckTimeout = 30 * time.Second
	MaxAckTimeout     = 5 * time.Minute
)

type RestartMode string

const (
	RestartModeRequestFile RestartMode = "request-file"
)

type RestartConfig struct {
	Mode              RestartMode
	Service           string
	RequestFile       string
	AckGenerationFile string
	// AckFile is the supervisor's health acknowledgement file. Its content
	// binds the request token to the exact generation and a healthy predicate.
	AckFile string
	// HealthAckFile is an explicit-name alias retained for callers that prefer
	// to distinguish the health acknowledgement from the generation marker.
	HealthAckFile string
	// ConfigPath, ProbeAddress, and HealthTimeout define the host-systemd
	// verification contract. Container/request-file supervision has its own
	// token/generation/health acknowledgement contract.
	ConfigPath    string
	ProbeAddress  string
	HealthTimeout time.Duration
	AckTimeout    time.Duration
	SingBoxBin    string
}

const (
	DefaultHealthTimeout = 30 * time.Second
	MaxHealthTimeout     = 5 * time.Minute
)

func (r RestartConfig) normalized() RestartConfig {
	if r.Mode == "" {
		r.Mode = RestartModeRequestFile
	}
	r.AckGenerationFile = strings.TrimSpace(r.AckGenerationFile)
	if r.AckFile == "" {
		r.AckFile = r.HealthAckFile
	}
	r.AckFile = strings.TrimSpace(r.AckFile)

	if r.HealthTimeout <= 0 {
		r.HealthTimeout = r.AckTimeout
	}
	if r.HealthTimeout <= 0 {
		r.HealthTimeout = DefaultHealthTimeout
	}
	if r.HealthTimeout > MaxHealthTimeout {
		r.HealthTimeout = MaxHealthTimeout
	}
	if r.AckGenerationFile != "" {
		if r.AckFile == "" {
			r.AckFile = r.AckGenerationFile + ".ack"
		}
		if r.AckTimeout == 0 {
			r.AckTimeout = DefaultAckTimeout
		}
		if r.AckTimeout > MaxAckTimeout {
			r.AckTimeout = MaxAckTimeout
		}
	}
	return r
}

func CheckContext(ctx context.Context, bin, configPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	args, err := singBoxCheckArgs(configPath)
	if err != nil {
		return err
	}
	return runCommand(ctx, bin, args...)
}

func singBoxCheckArgs(configPath string) ([]string, error) {
	if canonical, err := canonicalConfigPath(configPath); err == nil {
		configPath = canonical
	}
	info, err := os.Stat(configPath)
	if err != nil {
		// Preserve the external command's historical error handling for a
		// missing path; the systemd identity gate separately requires an
		// existing canonical path.
		return []string{"check", "-c", configPath}, nil
	}
	if info.IsDir() {
		return []string{"check", "-C", configPath}, nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("sing-box config path is not a regular file or directory")
	}
	return []string{"check", "-c", configPath}, nil
}

// RestartWithAckContext re-runs the supervised restart/health handshake
// without changing the config file. Transaction recovery uses it to ensure a
// child that died after an earlier acknowledgement cannot turn a stale
// runtime file into a newly committed selected state.
func RestartWithAckContext(ctx context.Context, restart RestartConfig) error {
	return restartSingBoxContext(ctx, restart)
}

// SyncFromSourcePreserveSelected refreshes a persisted runtime config from the
// rendered source config while preserving the runtime selected-native-out
// outbound. This pure wrapper is retained for callers that only need the file
// transformation. The CLI uses SyncFromSourcePreserveSelectedWithLock so sync
// participates in the same state-dir transaction lock as apply/rollback/prune.

// SyncFromSourcePreserveSelectedWithLock is the process-safe sync entrypoint.

// SyncFromSourcePreserveSelectedLocked performs the transformation for a
// caller that already owns stateDir's lock.
func SyncFromSourcePreserveSelectedLocked(sourcePath, runtimePath string) error {
	return syncFromSourcePreserveSelected(sourcePath, runtimePath)
}

func syncFromSourcePreserveSelected(sourcePath, runtimePath string) error {
	b, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	var source map[string]any
	if err := json.Unmarshal(b, &source); err != nil {
		return fmt.Errorf("read source sing-box config: %w", err)
	}

	if rb, err := os.ReadFile(runtimePath); err == nil {
		var runtime map[string]any
		if err := json.Unmarshal(rb, &runtime); err != nil {
			return fmt.Errorf("read runtime sing-box config: %w", err)
		}
		if selected, ok := findOutbound(runtime, "selected-native-out"); ok {
			if err := replaceOutbound(source, "selected-native-out", selected); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	nb, err := json.MarshalIndent(source, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0755); err != nil {
		return err
	}
	return writeFileAtomic(runtimePath, append(nb, '\n'), 0644)
}

func findOutbound(cfg map[string]any, tag string) (map[string]any, bool) {
	arr, ok := cfg["outbounds"].([]any)
	if !ok {
		return nil, false
	}
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := m["tag"].(string); got == tag {
			return m, true
		}
	}
	return nil, false
}

func replaceOutbound(cfg map[string]any, tag string, outbound map[string]any) error {
	arr, ok := cfg["outbounds"].([]any)
	if !ok {
		return fmt.Errorf("source sing-box config has no outbounds")
	}
	for i, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := m["tag"].(string); got == tag {
			arr[i] = outbound
			cfg["outbounds"] = arr
			return nil
		}
	}
	return fmt.Errorf("source sing-box config missing outbound tag %q", tag)
}

// ApplyWithRestart takes the state-dir process lock for callers that do not
// already own the transaction boundary.

// ApplyWithRestartLocked performs the runtime mutation for a caller that
// already owns state-dir's lock (local transaction paths use this form).

func ApplyWithRestartLockedContext(ctx context.Context, configPath, stateDir string, out map[string]any, restart RestartConfig) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(restart.ConfigPath) == "" {
		restart.ConfigPath = configPath
	}
	restart = restart.normalized()
	if err := state.MigrateLegacySnapshots(stateDir); err != nil {
		return "", err
	}
	backupDir := filepath.Join(stateDir, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}
	snapshot, err := state.Capture(stateDir)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", err
	}
	arr, ok := cfg["outbounds"].([]any)
	if !ok || len(arr) == 0 {
		return "", fmt.Errorf("no outbounds")
	}
	idx := firstProxyOutbound(arr)
	nextOut, err := outboundForApply(out, arr[idx], restart)
	if err != nil {
		return "", err
	}
	arr[idx] = nextOut
	cfg["outbounds"] = arr
	nb, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := validateCandidateContext(ctx, configPath, stateDir, nb, restart); err != nil {
		return "", err
	}
	backup := filepath.Join(backupDir, "sing-box-"+time.Now().Format("20060102-150405.000000000")+".json")
	if err := os.WriteFile(backup, b, 0600); err != nil {
		return "", err
	}
	cleanupUncommitted := func() {
		_ = os.Remove(backup)
		_ = state.RemoveSnapshotForBackup(stateDir, backup)
	}
	// The sidecar is written before the runtime file is changed, so a failure
	// here cannot leave a new runtime active without its exact prior state.
	if err := state.SaveSnapshotForBackup(stateDir, backup, snapshot); err != nil {
		cleanupUncommitted()
		return "", err
	}
	if err := ctx.Err(); err != nil {
		cleanupUncommitted()
		return "", err
	}
	if err := writeFileAtomic(configPath, append(nb, '\n'), 0644); err != nil {
		cleanupUncommitted()
		return "", err
	}
	if err := restartSingBoxContext(ctx, restart); err != nil {
		// A failed acknowledgement must not leave the candidate selected.
		// Compensation uses the same supervised protocol; a bare request is
		// not proof that the old runtime is serving.
		restoreErr := writeFileAtomic(configPath, b, 0644)
		restartOldErr := error(nil)
		if restoreErr == nil {
			restartOldErr = restartSingBoxContext(context.Background(), restart)
		}
		if restoreErr != nil || restartOldErr != nil {
			return backup, fmt.Errorf("restart after apply failed: %w; restore failed: %v; restart restored config: %v", err, restoreErr, restartOldErr)
		}
		return backup, fmt.Errorf("restart after apply failed: %w; restored backup", err)
	}
	return backup, nil
}

// Parse the exact sidecar before changing the runtime. A corrupt state
// sidecar must fail closed rather than leaving runtime and state mismatched.

// RestoreBackupWithRestart restores one exact runtime backup and uses the same
// restart/request-file supervision path as a normal apply. It is intentionally
// separate from selecting the latest backup so a transaction can restore the
// backup it just created without exposing node material in logs.

func RestoreBackupWithRestartLockedContext(ctx context.Context, configPath, stateDir, backupPath string, restart RestartConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(restart.ConfigPath) == "" {
		restart.ConfigPath = configPath
	}
	restart = restart.normalized()
	current, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		current = nil
	}
	b, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	if err := validateCandidateContext(ctx, configPath, stateDir, b, restart); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeFileAtomic(configPath, b, 0644); err != nil {
		return err
	}
	if err := restartSingBoxContext(ctx, restart); err != nil {
		restoreErr := restoreRuntimeFile(configPath, current)
		restartOldErr := error(nil)
		if restoreErr == nil {
			// A compensation restart is supervised by the same token,
			// generation, and health-ack protocol. An unacknowledged request
			// cannot be used to close a transaction journal.
			restartOldErr = restartSingBoxContext(context.Background(), restart)
		}
		if restoreErr != nil || restartOldErr != nil {
			return fmt.Errorf("restart after rollback failed: %w; restore failed: %v; restart restored config: %v", err, restoreErr, restartOldErr)
		}
		return fmt.Errorf("restart after rollback failed: %w; restored prior config", err)
	}
	return nil
}

func restoreRuntimeFile(path string, b []byte) error {
	if b == nil {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeFileAtomic(path, b, 0644)
}

func validateCandidateContext(ctx context.Context, configPath, stateDir string, b []byte, restart RestartConfig) error {
	if restart.Mode != RestartModeRequestFile || restart.SingBoxBin == "" {
		return nil
	}
	if _, err := exec.LookPath(restart.SingBoxBin); err != nil {
		return nil
	}
	dir := filepath.Dir(configPath)
	if dir == "" || dir == "." {
		dir = stateDir
	}
	f, err := os.CreateTemp(dir, ".vibe-vpn-check-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(bytesTrimFinalNewline(b), '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := CheckContext(ctx, restart.SingBoxBin, tmp); err != nil {
		return fmt.Errorf("sing-box check candidate: %w", err)
	}
	return nil
}

// restartSingBox preserves the original synchronous helper for package-local
// callers; runtime paths that propagate cancellation use the context variant.

func restartSingBoxContext(ctx context.Context, restart RestartConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	restart = restart.normalized()
	switch restart.Mode {
	case RestartModeRequestFile:
		if restart.RequestFile == "" {
			return fmt.Errorf("restart request file is empty")
		}
		// Request-file mode is always supervised. A generation bump without a
		// matching token-bound healthy acknowledgement is not proof of a live
		// runtime and can never close a transaction.
		if restart.AckGenerationFile == "" {
			return fmt.Errorf("restart generation acknowledgement is required")
		}
		if restart.AckTimeout < 0 {
			return fmt.Errorf("restart acknowledgement timeout is negative")
		}
		before, err := readGeneration(restart.AckGenerationFile)
		if err != nil {
			return err
		}
		if restart.AckFile == "" {
			return fmt.Errorf("restart health acknowledgement file is empty")
		}
		if err := os.MkdirAll(filepath.Dir(restart.RequestFile), 0755); err != nil {
			return err
		}
		// The request body is deliberately nonempty and unique so two quick
		// applies cannot collapse into one supervisor observation.
		token := fmt.Sprintf("restart-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), atomic.AddUint64(&restartSequence, 1))
		if err := writeFileAtomic(restart.RequestFile, []byte(token+"\n"), 0600); err != nil {
			return err
		}
		return waitForRestartAck(ctx, restart.RequestFile, restart.AckFile, restart.AckGenerationFile, token, before, restart.AckTimeout)
	default:
		return fmt.Errorf("unsupported restart mode %q", restart.Mode)
	}
}

// runExternal runs one command in its own process group. A context timeout
// kills the group, not just the sing-box parent, so a blocking fake
// or wrapper cannot perform a late unit mutation after the caller has failed.
func runExternal(ctx context.Context, name string, args ...string) error {
	_, err := runExternalOutput(ctx, name, args...)
	return err
}

func runExternalOutput(ctx context.Context, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-ctx.Done():
		terminateExternalProcessGroup(cmd, done)
		return out.String(), ctx.Err()
	}
}

func terminateExternalProcessGroup(cmd *exec.Cmd, done <-chan error) {
	if cmd.Process != nil {
		pid := cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
		}
		// The parent may have exited on SIGTERM while a descendant ignored it;
		// always issue the group SIGKILL before declaring cancellation complete.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func canonicalConfigPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path is not absolute")
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// EvalSymlinks deliberately returns the no-symlink identity used for both
	// file and directory invocations. A lexical path and a symlink alias must
	// not be treated as two different runtime configurations.
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", fmt.Errorf("path is not a regular file or directory")
	}
	return filepath.Clean(p), nil
}

// Resolve a known bare name through PATH so an unrelated executable with
// the same basename cannot satisfy the proof. If the binary is not
// installed in a unit-test/supervisor environment, retain basename
// compatibility for the existing fake-runtime contract.

// parseProcStatStartTime parses Linux /proc/<pid>/stat without assuming that
// comm contains neither whitespace nor parentheses. The field after comm is
// state (field 3), and starttime is field 22, hence offset 19 in the fields
// following the closing comm parenthesis.

// These small predicates retain package-local testability while using the
// strict parser above for the actual health gate.

func readGeneration(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func requestConsumed(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

type restartHealthAck struct {
	Token      string `json:"token"`
	Generation string `json:"generation"`
	Health     string `json:"health"`
}

func readRestartHealthAck(path string) (restartHealthAck, error) {
	var ack restartHealthAck
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ack, nil
		}
		return ack, err
	}
	if err := json.Unmarshal(b, &ack); err == nil {
		return ack, nil
	}
	// The minimal container supervisor uses key=value lines so it does not
	// need jq or Python. Accept only the three expected fields.
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "token":
			ack.Token = value
		case "generation":
			ack.Generation = value
		case "health":
			ack.Health = value
		}
	}
	return ack, nil
}

func waitForRestartAck(ctx context.Context, requestFile, ackFile, generationFile, token, before string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultAckTimeout
	}
	if timeout > MaxAckTimeout {
		timeout = MaxAckTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		consumed, err := requestConsumed(requestFile)
		if err != nil {
			return err
		}
		if !consumed {
			request, readErr := os.ReadFile(requestFile)
			if readErr != nil && !os.IsNotExist(readErr) {
				return readErr
			}
			if readErr == nil && strings.TrimSpace(string(request)) != token {
				return fmt.Errorf("restart request token changed")
			}
		}
		current, err := readGeneration(generationFile)
		if err != nil {
			return err
		}
		ack, err := readRestartHealthAck(ackFile)
		if err != nil {
			return err
		}
		// All four observations are required. In particular, a stale ack file,
		// a generation bump without a health predicate, or a status-only legacy
		// marker cannot satisfy a new request.
		if consumed && current != "" && current != before && ack.Token == token && ack.Generation == current && ack.Health == "healthy" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("restart acknowledgement timed out")
		case <-tick.C:
		}
	}
}

func bytesTrimFinalNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func firstProxyOutbound(arr []any) int {
	for i, v := range arr {
		if m, ok := v.(map[string]any); ok {
			tag, _ := m["tag"].(string)
			if tag == "selected-native-out" || tag == "proxy" || tag == "xray-socks-out" {
				return i
			}
		}
	}
	return 0
}

func outboundForApply(out map[string]any, old any, restart RestartConfig) (map[string]any, error) {
	next := outboundWithPreservedTag(out, old)
	if restart.Mode != RestartModeRequestFile {
		return next, nil
	}
	return preResolveOutboundServer(next)
}

func outboundWithPreservedTag(out map[string]any, old any) map[string]any {
	next := make(map[string]any, len(out)+1)
	for k, v := range out {
		next[k] = v
	}
	if _, ok := next["tag"]; ok {
		return next
	}
	if m, ok := old.(map[string]any); ok {
		if tag, ok := m["tag"].(string); ok && tag != "" {
			next["tag"] = tag
		}
	}
	return next
}

func preResolveOutboundServer(out map[string]any) (map[string]any, error) {
	server, _ := out["server"].(string)
	if server == "" {
		return out, nil
	}
	if net.ParseIP(server) != nil {
		return out, nil
	}
	ips, err := lookupIP(server)
	if err != nil {
		return nil, fmt.Errorf("resolve selected outbound server for container apply: %w", err)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out["server"] = v4.String()
			return out, nil
		}
	}
	if len(ips) > 0 {
		out["server"] = ips[0].String()
		return out, nil
	}
	return nil, fmt.Errorf("resolve selected outbound server for container apply: no addresses")
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
