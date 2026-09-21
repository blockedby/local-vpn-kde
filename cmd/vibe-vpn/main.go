package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"

	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"

	"github.com/blockedby/local-vpn-kde/internal/singbox"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/subscription"
	"github.com/blockedby/local-vpn-kde/internal/vless"

	"github.com/spf13/cobra"
)

var browserSignalContextReadyHook func()

func executeRootCommand(root *cobra.Command, args []string) error {
	root.SetArgs(args)
	if !isBrowserJSONInvocation(args) {
		return root.Execute()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if browserSignalContextReadyHook != nil {
		browserSignalContextReadyHook()
	}
	return root.ExecuteContext(ctx)
}

func main() {
	err := executeRootCommand(newRootCommand(), os.Args[1:])
	if err != nil {
		routeCommandError(err, os.Args[1:], os.Stdout, os.Stderr)
		os.Exit(1)
	}
}

const defaultConfigPath = "/etc/vibe-vpn/config.json"

type cliOptions struct {
	configPath   string
	restartAsync bool
	filter       picker.FilterOptions
}

func newRootCommand() *cobra.Command {
	o := &cliOptions{}
	root := &cobra.Command{Use: "vibe-vpn", Short: "Safely test and select VLESS subscription nodes", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&o.configPath, "config", defaultConfigPath, "config path")

	addFilters := func(cmd *cobra.Command) {
		cmd.Flags().StringArrayVar(&o.filter.Include, "include", nil, "include only nodes whose name/host contains text (repeatable)")
		cmd.Flags().StringArrayVar(&o.filter.Exclude, "exclude", nil, "exclude nodes whose name/host contains text (repeatable)")
		cmd.Flags().StringArrayVar(&o.filter.Transport, "transport", nil, "include only transport/network values such as tcp, ws, grpc (repeatable)")
		cmd.Flags().StringArrayVar(&o.filter.Security, "security", nil, "include only security values such as tls or reality (repeatable)")
		cmd.Flags().Float64Var(&o.filter.MinMbps, "min-mbps", 0, "exclude successful nodes slower than this Mbps")
		cmd.Flags().BoolVar(&o.filter.DefaultExclude, "default-exclude", true, "exclude subscription metadata/traffic nodes")
		cmd.Flags().BoolVar(&o.filter.DefaultExclude, "no-default-exclude", true, "disable default subscription metadata exclusions")
		cmd.Flags().Lookup("no-default-exclude").NoOptDefVal = "false"
	}

	pick := &cobra.Command{Use: "pick", Short: "Benchmark in isolation, then apply the best non-excluded working node", RunE: func(cmd *cobra.Command, args []string) error {
		max, _ := cmd.Flags().GetInt("max")
		lim, _ := cmd.Flags().GetInt("limit-kib")
		verbose, _ := cmd.Flags().GetBool("verbose")
		debug, _ := cmd.Flags().GetBool("debug")
		dur, _ := cmd.Flags().GetInt("duration-sec")
		return runTest(o, true, max, lim, dur, verbose, debug)
	}}
	pick.Flags().Int("max", 0, "max nodes")
	pick.Flags().Int("limit-kib", 0, "test KiB")
	pick.Flags().Int("duration-sec", -1, "download duration per node in seconds; 0 disables duration mode")
	pick.Flags().Bool("verbose", false, "print every node while testing")
	pick.Flags().Bool("debug", false, "show temporary benchmark backend logs")
	pick.Flags().BoolVar(&o.restartAsync, "restart-async", false, "deprecated bootstrap compatibility flag; supervised request acknowledgement remains required")
	addFilters(pick)
	root.AddCommand(pick)

	for name, handler := range map[string]func(*cobra.Command, *cliOptions) error{"list": runBrowserList, "current": runBrowserCurrent} {
		handler := handler
		cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			if !jsonOut {
				return fmt.Errorf("--json is required")
			}
			return handler(cmd, o)
		}}
		cmd.Flags().Bool("json", false, "print redacted results")
		root.AddCommand(cmd)
	}
	addServerBrowserCommands(root, o)
	syncSingBox := &cobra.Command{Use: "sync-sing-box-config", Short: "Refresh runtime sing-box config from source while preserving selected outbound", Hidden: true, RunE: func(cmd *cobra.Command, args []string) error {
		source, _ := cmd.Flags().GetString("source")
		runtime, _ := cmd.Flags().GetString("runtime")
		c, err := loadConfig(o.configPath)
		if err != nil {
			return err
		}
		lock, err := state.AcquireLock(context.Background(), c.StateDir)
		if err != nil {
			return err
		}
		defer lock.Close()
		if os.Getenv("VIBE_VPN_DEFER_TRANSACTION_RECOVERY") != "1" {
			if err := recoverTransactionsLocked(context.Background(), c); err != nil {
				return fmt.Errorf("recover pending transaction: %w", err)
			}
		} else if pending, pendingErr := state.PendingTransactions(c.StateDir); pendingErr != nil {
			return pendingErr
		} else if len(pending) != 0 {
			// Entrypoint startup runs this sync before the request supervisor. Do
			// not overwrite a crash candidate; the post-health recovery command
			// below resolves it while the supervisor can emit acknowledgements.
			return nil
		}
		return singbox.SyncFromSourcePreserveSelectedLocked(source, runtime)
	}}
	syncSingBox.Flags().String("source", "/etc/sing-box/config.json", "rendered source sing-box config")
	syncSingBox.Flags().String("runtime", "/var/lib/vpnkit/sing-box/config.json", "persisted runtime sing-box config")
	root.AddCommand(syncSingBox)
	root.AddCommand(&cobra.Command{Use: "recover-transactions", Short: "Recover pending runtime/state transactions", Hidden: true, RunE: func(cmd *cobra.Command, args []string) error { return cmdRecoverTransactions(o) }})
	return root
}

func cmdRecoverTransactions(o *cliOptions) error {
	c, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	lock, err := state.AcquireLock(context.Background(), c.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	return recoverTransactionsLocked(context.Background(), c)
}

func loadConfig(path string) (config.Config, error) {
	if path != defaultConfigPath {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return config.Config{}, fmt.Errorf("config %s does not exist", path)
			}
			return config.Config{}, err
		}
	}
	return config.Load(path)
}

func runTest(o *cliOptions, apply bool, max, lim, dur int, verbose, debug bool) error {
	return runTestContext(context.Background(), o, apply, max, lim, dur, verbose, debug)
}

func runTestContext(ctx context.Context, o *cliOptions, apply bool, max, lim, dur int, verbose, debug bool) error {
	return runTestContextVersioned(ctx, o, apply, max, lim, dur, verbose, debug)
}

// Publish results under the shared state lock.
func runTestContextVersioned(ctx context.Context, o *cliOptions, apply bool, max, lim, dur int, verbose, debug bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	if lim > 0 {
		c.TestLimitKiB = lim
	}
	if dur >= 0 {
		c.TestDurationSeconds = dur
	}
	links, warnings, err := loadSubscriptionLinksContext(ctx, c)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "WARN %v\n", w)
	}
	if err != nil {
		return err
	}
	type candidate struct {
		idx  int
		link string
		node vless.Node
	}
	candidates := []candidate{}
	parseFailures := []picker.NodeResult{}
	for i, l := range links {
		n, err := vless.Parse(l)
		if err != nil {
			parseFailures = append(parseFailures, picker.NodeResult{Index: i + 1, OK: false, Error: err.Error(), Link: l})
			continue
		}
		probe := picker.NodeResult{Index: i + 1, Name: n.Name, Host: n.Host, Port: n.Port, Network: n.Network, Security: n.Security, Link: l}
		if ok, _ := o.filter.MatchResult(probe); ok {
			candidates = append(candidates, candidate{i + 1, l, n})
		}
	}
	beforeMax := len(candidates)
	if max > 0 && max < len(candidates) {
		candidates = candidates[:max]
	}
	if len(candidates) == 0 {
		return fmt.Errorf("no nodes match filters")
	}
	if tcpOpen(c.TestSocks, 200*time.Millisecond) {
		if n := cleanupStaleTestBackends(); n > 0 {
			fmt.Printf("Test SOCKS address %s is busy; cleaned up %d stale temporary benchmark backend process(es).\n", c.TestSocks, n)
			if err := waitContext(ctx, 300*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if tcpOpen(c.TestSocks, 200*time.Millisecond) {
		alt, err := freeLocalSocksAddr()
		if err != nil {
			return fmt.Errorf("test SOCKS address %s is already in use and no free fallback port found: %w", c.TestSocks, err)
		}
		fmt.Printf("Test SOCKS address %s is still busy; using fallback %s for this run.\n", c.TestSocks, alt)
		c.TestSocks = alt
	}
	fmt.Printf("Fetched %d subscription nodes, %d after filters", len(links), beforeMax)
	if max > 0 && max < beforeMax {
		fmt.Printf(", testing first %d", len(candidates))
	}
	fmt.Printf(".\n")
	fmt.Printf("Testing isolated on %s; production stays untouched.\n", c.TestSocks)
	if c.TestDurationSeconds > 0 {
		fmt.Printf("Benchmark mode: download for %ds per node.\n", c.TestDurationSeconds)
	} else {
		fmt.Printf("Benchmark mode: download up to %d KiB per node.\n", c.TestLimitKiB)
	}
	if !verbose {
		fmt.Println("Progress is quiet by default; use --verbose to print every node.")
	}
	results := append([]picker.NodeResult{}, parseFailures...)
	for j, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := cand.node
		r, err := testOneContext(ctx, c, n, debug)
		if err != nil {
			if verbose {
				fmt.Printf("[%03d/%03d] FAIL %v\n", j+1, len(candidates), err)
			}
			results = append(results, picker.NodeResult{Index: cand.idx, OK: false, Error: err.Error(), Link: cand.link, Name: n.Name, Host: n.Host, Port: n.Port, Network: n.Network, Security: n.Security})
			continue
		}
		threshold := successThreshold(int64(c.TestLimitKiB) * 1024)
		if c.TestDurationSeconds > 0 {
			threshold = 64 * 1024
		}
		ok := r.Bytes >= threshold
		nr := picker.FromNode(cand.idx, n, r.Mbps, r.Bytes, r.Seconds)
		nr.OK = ok
		results = append(results, nr)
		if verbose {
			fmt.Printf("[%03d/%03d] %7.2f Mbps %5.2fs #%03d %s\n", j+1, len(candidates), r.Mbps, r.Seconds, cand.idx, n.Name)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	results = o.filter.Apply(results)
	if _, err := state.SaveJSONVersioned(ctx, c.StateDir, "last-results.json", results); err != nil {
		return err
	}
	printSummary(results, 20)
	b := picker.BestFiltered(results)
	if b == nil {
		return fmt.Errorf("no working non-excluded node")
	}
	fmt.Printf("\nBEST:\n  #%03d %s\n  %.2f Mbps\n", b.Index, b.Name, b.Mbps)
	if apply {
		return applyResultWithOptions(ctx, c, *b, o.restartAsync)
	}
	fmt.Println("Dry run only. Use 'vibe-vpn pick' to apply winner.")
	return nil
}

type downloadProgressKey struct{}

func testOneContext(ctx context.Context, c config.Config, n vless.Node, debug bool) (nettest.Result, error) {
	return testOneContextUsing(ctx, c, n, debug, nil)
}

// Share the isolated proxy lifecycle with availability probes; only the request differs.
func testOneContextUsing(ctx context.Context, c config.Config, n vless.Node, debug bool, check func() (nettest.Result, error)) (nettest.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nettest.Result{}, err
	}
	if tcpOpen(c.TestSocks, 200*time.Millisecond) {
		return nettest.Result{}, fmt.Errorf("test SOCKS address %s became busy during run", c.TestSocks)
	}
	backend, err := tempBenchmarkBackend(c, n)
	if err != nil {
		return nettest.Result{}, err
	}
	defer os.Remove(backend.configPath)
	backendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(backendCtx, backend.bin, backend.args...)
	if debug {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nettest.Result{}, err
	}
	defer func() { cancel(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := waitTCPContext(backendCtx, c.TestSocks, 3*time.Second); err != nil {
		return nettest.Result{}, err
	}
	resultCh := make(chan struct {
		result nettest.Result
		err    error
	}, 1)
	go func() {
		var result nettest.Result
		var err error
		if check != nil {
			result, err = check()
		} else if c.TestDurationSeconds > 0 {
			progress, _ := ctx.Value(downloadProgressKey{}).(func(nettest.Result) error)
			result, err = nettest.DownloadForContext(ctx, c.TestSocks, c.TestURL, time.Duration(c.TestDurationSeconds)*time.Second, time.Duration(c.TimeoutSeconds)*time.Second, progress)
		} else {
			result, err = nettest.Download(c.TestSocks, c.TestURL, int64(c.TestLimitKiB)*1024, time.Duration(c.TimeoutSeconds)*time.Second)
		}
		resultCh <- struct {
			result nettest.Result
			err    error
		}{result: result, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.result, result.err
	case <-ctx.Done():
		// Timed downloads close their connection on cancellation. Drain that
		// worker before a final response so no progress can follow completion.
		if check == nil && c.TestDurationSeconds > 0 {
			<-resultCh
		}
		return nettest.Result{}, ctx.Err()
	}
}

type benchmarkBackend struct {
	bin        string
	args       []string
	configPath string
}

func tempBenchmarkBackend(c config.Config, n vless.Node) (benchmarkBackend, error) {

	b, err := singBoxTempConfig(n, c.TestSocks)
	if err != nil {
		return benchmarkBackend{}, err
	}
	path, err := writeTempBenchmarkConfig("vibe-vpn-singbox-*.json", b)
	if err != nil {
		return benchmarkBackend{}, err
	}
	return benchmarkBackend{bin: c.SingBoxBin, args: []string{"run", "-c", path}, configPath: path}, nil
}

func writeTempBenchmarkConfig(pattern string, b []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func singBoxTempConfig(n vless.Node, socksAddr string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(socksAddr)
	if err != nil {
		return nil, err
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		return nil, fmt.Errorf("invalid test_socks port %q: %w", portText, err)
	}
	out, err := singBoxOutboundForNode(n)
	if err != nil {
		return nil, err
	}
	out["tag"] = "benchmark-out"
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn"},
		"inbounds":  []any{map[string]any{"type": "socks", "tag": "test-socks", "listen": host, "listen_port": port}},
		"outbounds": []any{out},
		"route":     map[string]any{"final": "benchmark-out"},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func singBoxOutboundForNode(n vless.Node) (map[string]any, error) {
	if t, _ := n.Outbound["type"].(string); t != "" {
		return cloneMap(n.Outbound), nil
	}
	return vless.SingBoxOutbound(n.Link)
}

func singBoxOutboundForResult(r picker.NodeResult) (map[string]any, error) {
	if t, _ := r.Outbound["type"].(string); t != "" {
		return cloneMap(r.Outbound), nil
	}
	return vless.SingBoxOutbound(r.Link)
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func successThreshold(limitBytes int64) int64 {
	if limitBytes <= 0 {
		return 1
	}
	const maxThreshold = 64 * 1024
	if limitBytes < maxThreshold {
		return limitBytes
	}
	return maxThreshold
}

func cleanupStaleTestSingBox() int {
	return cleanupStaleProcesses("sing-box run -c /tmp/vibe-vpn-singbox-")
}

func cleanupStaleTestBackends() int { return cleanupStaleTestSingBox() }

func cleanupStaleProcesses(pattern string) int {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_ = exec.Command("kill", line).Run()
		count++
	}
	return count
}

func freeLocalSocksAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func tcpOpen(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitContext(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func waitTCPContext(ctx context.Context, addr string, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if tcpOpen(addr, 200*time.Millisecond) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("temp benchmark backend did not open %s", addr)
		case <-tick.C:
		}
	}
}

func loadSubscriptionLinksContext(ctx context.Context, c config.Config) ([]string, []error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b, err := os.ReadFile(c.SubscriptionFile)
	if err != nil {
		return nil, nil, err
	}
	urls := subscription.URLList(string(b))
	if len(urls) == 0 {
		return nil, nil, fmt.Errorf("%s contains no subscription URLs", c.SubscriptionFile)
	}
	links := []string{}
	warnings := []error{}
	for i, rawURL := range urls {
		var fetched []string
		var fetchErr error
		for attempt := 0; attempt < 3; attempt++ {
			fetched, fetchErr = fetchSubscriptionContext(ctx, rawURL, time.Duration(c.TimeoutSeconds)*time.Second)
			if fetchErr == nil {
				break
			}
			if ctx.Err() != nil {
				return nil, warnings, ctx.Err()
			}
			if attempt < 2 {
				t := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
				select {
				case <-ctx.Done():
					t.Stop()
					return nil, warnings, ctx.Err()
				case <-t.C:
				}
			}
		}
		if fetchErr != nil {
			// Do not include the subscription URL or response body in warnings.
			warnings = append(warnings, fmt.Errorf("subscription %d fetch failed", i+1))
			continue
		}
		links = append(links, fetched...)
	}
	if len(links) == 0 && len(warnings) > 0 {
		return nil, warnings, fmt.Errorf("all %d subscription URLs failed", len(urls))
	}
	return links, warnings, nil
}

func fetchSubscriptionContext(ctx context.Context, rawURL string, timeout time.Duration) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid subscription request")
	}
	req.Header.Set("User-Agent", "vibe-vpn/1")
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subscription request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("subscription response status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("subscription response read failed")
	}
	return subscription.Parse(string(body))
}

func sortedOK(results []picker.NodeResult) []picker.NodeResult {
	ok := make([]picker.NodeResult, 0, len(results))
	for _, r := range results {
		if r.OK && !r.Excluded {
			ok = append(ok, r)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].Mbps > ok[j].Mbps })
	return ok
}

func printSummary(results []picker.NodeResult, top int) {
	ok := sortedOK(results)
	failed := len(results) - len(ok)
	fmt.Printf("\nDone: %d ok, %d failed. Results saved to last-results.json.\n", len(ok), failed)
	if len(ok) == 0 {
		return
	}
	if top <= 0 || top > len(ok) {
		top = len(ok)
	}
	fmt.Printf("\nTop %d by speed:\n", top)
	for _, r := range ok[:top] {
		fmt.Printf("  #%03d  %7.2f Mbps  %-42s  %s:%d %s/%s\n", r.Index, r.Mbps, truncate(r.Name, 42), r.Host, r.Port, r.Network, r.Security)
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func applyResultWithOptions(ctx context.Context, c config.Config, b picker.NodeResult, asyncRestart bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return fmt.Errorf("apply state lock: %w", err)
	}
	defer lock.Close()
	return applyResultLockedWithOptions(ctx, c, b, asyncRestart)
}

// applyResultContext is the manual/CLI transaction boundary. Daemon callers
// already hold the state-dir lock in the failover/rotation manager and use
// applyResultLocked directly to avoid nested file locks.

var transactionSequence uint64

func transactionID() string {
	return fmt.Sprintf("txn-%d-%d", time.Now().UTC().UnixNano(), atomic.AddUint64(&transactionSequence, 1))
}

func transactionRuntimePath(c config.Config) string {

	return c.SingBoxConfig
}

func transactionRuntimeName(c config.Config) string {

	return "singbox"
}

func beginRuntimeTransactionLocked(c config.Config, operation state.TransactionOperation, candidate state.Snapshot) (string, error) {
	runtimePath := transactionRuntimePath(c)
	oldRuntime, err := os.ReadFile(runtimePath)
	if err != nil {
		return "", err
	}
	oldState, err := state.Capture(c.StateDir)
	if err != nil {
		return "", err
	}
	id := transactionID()
	if err := state.BeginTransactionWithMetadata(c.StateDir, id, operation, transactionRuntimeName(c), runtimePath, oldRuntime, nil, oldState, candidate); err != nil {
		return "", err
	}
	return id, nil
}

func recoverTransactionsLocked(ctx context.Context, c config.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := state.MigrateLegacyTransactionArtifacts(c.StateDir); err != nil {
		return err
	}
	transactions, err := state.PendingTransactions(c.StateDir)
	if err != nil {
		return err
	}
	for _, tx := range transactions {
		if err := recoverTransactionLocked(ctx, c, tx); err != nil {
			return err
		}
	}
	return state.PruneOrphanTransactionArtifacts(c.StateDir)
}

func recoverTransactionLocked(ctx context.Context, c config.Config, tx state.Transaction) error {
	configPath := transactionRuntimePath(c)
	if tx.Runtime != "" && tx.Runtime != transactionRuntimeName(c) {
		return fmt.Errorf("pending transaction targets a different runtime")
	}
	if tx.ConfigPath != "" && filepath.Clean(tx.ConfigPath) != filepath.Clean(configPath) {
		return fmt.Errorf("pending transaction targets a different runtime config")
	}
	current, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		current = nil
	}

	switch tx.Phase {
	case state.PhasePrepared:
		// Equality with OldRuntime only proves the file bytes. It says nothing
		// about a service that died after compensation wrote those bytes. Apply
		// the old runtime again and require the backend health contract before
		// publishing the old state or deleting the journal.
		if err := restoreTransactionRuntimeLocked(ctx, c, tx, true); err != nil {
			return fmt.Errorf("prepared transaction old runtime is not healthy: %w", err)
		}
		if err := tx.OldState.Restore(c.StateDir); err != nil {
			return err
		}
		if err := state.AcknowledgeTransaction(c.StateDir, tx.ID, tx.OldRuntime); err != nil {
			return err
		}
		if err := state.UpdateTransactionPhase(c.StateDir, tx.ID, state.PhaseStateCommitted); err != nil {
			return err
		}
		return state.CompleteTransaction(c.StateDir, tx.ID)
	case state.PhaseRuntimeAcknowledged:
		if tx.CandidateRuntimeReady && bytes.Equal(current, tx.CandidateRuntime) {
			// A previous acknowledgement is not durable proof that the child is
			// still serving. Re-run the backend-specific restart/health contract
			// before restoring candidate state and advancing the journal.
			if err := restartCurrentRuntimeLocked(ctx, c); err != nil {
				return recoverTransactionToOldPair(ctx, c, tx, fmt.Errorf("candidate runtime health recheck failed: %w", err))
			}
			if err := verifyRuntimeBytes(configPath, tx.CandidateRuntime); err != nil {
				return recoverTransactionToOldPair(ctx, c, tx, fmt.Errorf("candidate runtime changed during recovery: %w", err))
			}
			if err := tx.CandidateState.Restore(c.StateDir); err != nil {
				return err
			}
			if err := state.UpdateTransactionPhase(c.StateDir, tx.ID, state.PhaseStateCommitted); err != nil {
				return err
			}
			return state.CompleteTransaction(c.StateDir, tx.ID)
		}
		// A torn or externally replaced candidate is resolved to the exact old
		// pair, but only after the old runtime has also passed health checks.
		if err := restoreTransactionRuntimeLocked(ctx, c, tx, true); err != nil {
			return fmt.Errorf("runtime-acknowledged transaction old runtime is not healthy: %w", err)
		}
		if err := verifyRuntimeBytes(configPath, tx.OldRuntime); err != nil {
			return fmt.Errorf("runtime-acknowledged transaction old runtime changed during recovery: %w", err)
		}
		if err := tx.OldState.Restore(c.StateDir); err != nil {
			return err
		}
		if err := state.UpdateTransactionPhase(c.StateDir, tx.ID, state.PhaseStateCommitted); err != nil {
			return err
		}
		return state.CompleteTransaction(c.StateDir, tx.ID)
	case state.PhaseStateCommitted:
		if tx.CandidateRuntimeReady && bytes.Equal(current, tx.CandidateRuntime) {
			// Journal cleanup is still gated by a fresh health proof. If the
			// candidate is unhealthy, restore the old pair for safety but retain
			// the journal so a later invocation must re-prove it before cleanup.
			if err := restartCurrentRuntimeLocked(ctx, c); err != nil {
				return recoverTransactionToOldPair(ctx, c, tx, fmt.Errorf("state-committed candidate runtime is not healthy: %w", err))
			}
			if err := verifyRuntimeBytes(configPath, tx.CandidateRuntime); err != nil {
				return recoverTransactionToOldPair(ctx, c, tx, fmt.Errorf("state-committed candidate runtime changed during recovery: %w", err))
			}
			if err := tx.CandidateState.Restore(c.StateDir); err != nil {
				return err
			}
			return state.CompleteTransaction(c.StateDir, tx.ID)
		}
		// For old, missing, or externally replaced bytes, fail closed to old.
		// The old service must be restarted and verified even when bytes already
		// match, and the old state is restored before journal removal.
		if err := restoreTransactionRuntimeLocked(ctx, c, tx, true); err != nil {
			return fmt.Errorf("state-committed transaction old runtime is not healthy: %w", err)
		}
		if err := verifyRuntimeBytes(configPath, tx.OldRuntime); err != nil {
			return fmt.Errorf("state-committed transaction old runtime changed during recovery: %w", err)
		}
		if err := tx.OldState.Restore(c.StateDir); err != nil {
			return err
		}
		return state.CompleteTransaction(c.StateDir, tx.ID)
	default:
		return fmt.Errorf("invalid pending transaction phase")
	}
}

func restartCurrentRuntimeLocked(ctx context.Context, c config.Config) error {

	return singbox.RestartWithAckContext(ctx, singboxRestartConfig(c))
}

// recoverTransactionToOldPair is deliberately not a journal-closing abort.
// When candidate health fails, the old pair is restored only as a fail-closed
// safety action; the journal remains until a later recovery proves the old
// runtime and completes the durable cleanup.
func recoverTransactionToOldPair(ctx context.Context, c config.Config, tx state.Transaction, cause error) error {
	restoreErr := restoreTransactionRuntimeLocked(ctx, c, tx, true)
	if restoreErr != nil {
		if stateErr := restoreOldStateIfRuntimeMatches(c, tx); stateErr != nil {
			return fmt.Errorf("%w; old runtime recovery failed: %v; old state recovery failed: %v; journal retained", cause, restoreErr, stateErr)
		}
		return fmt.Errorf("%w; old runtime recovery failed: %v; old state restored; journal retained", cause, restoreErr)
	}
	if err := verifyRuntimeBytes(transactionRuntimePath(c), tx.OldRuntime); err != nil {
		return fmt.Errorf("%w; old runtime changed during recovery: %v; journal retained", cause, err)
	}
	if err := tx.OldState.Restore(c.StateDir); err != nil {
		return fmt.Errorf("%w; old state recovery failed: %v; journal retained", cause, err)
	}
	return fmt.Errorf("%w; old pair restored; journal retained for retry", cause)
}

// abortTransactionLocked restores the journal's old runtime and state. It is
// used for bounded cancellation/error paths; the journal is retained if any
// step fails so the next locked invocation can retry recovery.
func abortTransactionLocked(ctx context.Context, c config.Config, id string, forceRestart bool) error {
	tx, err := state.LoadTransaction(c.StateDir, id)
	if err != nil {
		return err
	}
	if err := restoreTransactionRuntimeLocked(ctx, c, tx, forceRestart); err != nil {
		if stateErr := restoreOldStateIfRuntimeMatches(c, tx); stateErr != nil {
			return fmt.Errorf("%w; old state recovery failed: %v", err, stateErr)
		}
		return err
	}
	if err := verifyRuntimeBytes(transactionRuntimePath(c), tx.OldRuntime); err != nil {
		return err
	}
	if err := tx.OldState.Restore(c.StateDir); err != nil {
		return err
	}
	if tx.Phase == state.PhasePrepared {
		if err := state.AcknowledgeTransaction(c.StateDir, id, tx.OldRuntime); err != nil {
			return err
		}
	}
	if err := state.UpdateTransactionPhase(c.StateDir, id, state.PhaseStateCommitted); err != nil {
		return err
	}
	return state.CompleteTransaction(c.StateDir, id)
}

func restoreTransactionRuntimeLocked(ctx context.Context, c config.Config, tx state.Transaction, forceRestart bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// forceRestart is retained for the bounded compensation call sites, but
	// byte equality is never a sufficient reason to skip the runtime health
	// proof. A crash can leave identical bytes with a dead service.
	_ = forceRestart
	oldPath, err := state.TransactionOldRuntimePath(c.StateDir, tx.ID)
	if err != nil {
		return err
	}
	// Compensating restart deliberately uses a non-cancelable context. A
	// canceled caller must not leave the runtime half-restored while the
	// journal is being closed.
	ctx = context.Background()
	var restoreErr error

	restoreErr = singbox.RestoreBackupWithRestartLockedContext(ctx, c.SingBoxConfig, c.StateDir, oldPath, singboxRestartConfig(c))

	if restoreErr != nil {
		return restoreErr
	}
	return verifyRuntimeBytes(transactionRuntimePath(c), tx.OldRuntime)
}

func verifyRuntimeBytes(path string, expected []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, expected) {
		return fmt.Errorf("runtime bytes changed during transaction")
	}
	return nil
}

func restoreOldStateIfRuntimeMatches(c config.Config, tx state.Transaction) error {
	if err := verifyRuntimeBytes(transactionRuntimePath(c), tx.OldRuntime); err != nil {
		return err
	}
	return tx.OldState.Restore(c.StateDir)
}

func transactionFailpoint(name string) {
	point := strings.TrimSpace(os.Getenv("VIBE_VPN_TX_FAILPOINT"))
	if point == "" {
		point = strings.TrimSpace(os.Getenv("VIBE_VPN_TRANSACTION_FAILPOINT"))
	}
	if point == "" {
		point = strings.TrimSpace(os.Getenv("VIBE_VPN_FAILPOINT"))
	}
	point = strings.ReplaceAll(point, "_", "-")
	if point != name && !(name == "after-runtime-ack" && (point == "after-runtime-ack-before-save-current" || point == "after-runtime-ack-before-state-commit")) && !(name == "after-rollback-runtime-ack" && (point == "after-rollback-runtime-ack-before-state-restore" || point == "after-rollback-runtime-ack-before-state-commit")) {
		return
	}
	// Test-only crash injection. SIGKILL is intentional: no deferred cleanup
	// may erase the journal before the recovery subprocess gets to observe it.
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	os.Exit(137)
}

func applyResultLockedWithOptions(ctx context.Context, c config.Config, b picker.NodeResult, asyncRestart bool) error {
	return applyResultLockedWithOptionsOutput(ctx, c, b, asyncRestart, true)
}

func applyResultLockedWithOptionsOutput(ctx context.Context, c config.Config, b picker.NodeResult, asyncRestart, announce bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recoverTransactionsLocked(ctx, c); err != nil {
		return fmt.Errorf("recover pending transaction: %w", err)
	}
	cur := state.Current{Name: b.Name, Host: b.Host, Port: b.Port, Network: b.Network, Security: b.Security, Link: b.Link, Mbps: b.Mbps, TestedAt: time.Now().UTC().Format(time.RFC3339), ServerID: b.ServerID, Generation: b.Generation}
	candidateState, err := state.SnapshotForCurrent(cur)
	if err != nil {
		return fmt.Errorf("prepare selected state: %w", err)
	}
	txID, err := beginRuntimeTransactionLocked(c, state.TransactionApply, candidateState)
	if err != nil {
		return fmt.Errorf("begin runtime transaction: %w", err)
	}

	var backup string

	out, convErr := singBoxOutboundForResult(b)
	if convErr != nil {
		_ = abortTransactionLocked(context.Background(), c, txID, false)
		return fmt.Errorf("build sing-box outbound from selected result: %w", convErr)
	}
	restart := singboxRestartConfig(c)
	// The flag remains accepted for old bootstrap command lines, but it no
	// longer disables the request/generation/health acknowledgement. The
	// entrypoint starts its request supervisor before invoking bootstrap.
	_ = asyncRestart
	backup, err = singbox.ApplyWithRestartLockedContext(ctx, c.SingBoxConfig, c.StateDir, out, restart)

	if err != nil {
		// The runtime package may already have compensated its file, but the
		// transaction boundary still re-applies and verifies the exact old
		// runtime before it can close the prepared journal.
		abortErr := abortTransactionLocked(context.Background(), c, txID, false)
		if abortErr != nil {
			return fmt.Errorf("runtime apply failed: %w; transaction restore failed: %v", err, abortErr)
		}
		return err
	}
	candidateRuntime, err := os.ReadFile(transactionRuntimePath(c))
	if err != nil {
		abortErr := abortTransactionLocked(context.Background(), c, txID, true)
		if abortErr != nil {
			return fmt.Errorf("read acknowledged runtime failed: %w; transaction restore failed: %v", err, abortErr)
		}
		return err
	}
	if err := state.AcknowledgeTransaction(c.StateDir, txID, candidateRuntime); err != nil {
		abortErr := abortTransactionLocked(context.Background(), c, txID, true)
		if abortErr != nil {
			return fmt.Errorf("journal runtime acknowledgement failed: %w; transaction restore failed: %v", err, abortErr)
		}
		return err
	}
	transactionFailpoint("after-runtime-ack")
	if err := ctx.Err(); err != nil {
		if abortErr := abortTransactionLocked(context.Background(), c, txID, true); abortErr != nil {
			return fmt.Errorf("apply canceled; transaction restore failed: %w", abortErr)
		}
		return err
	}
	if err := verifyRuntimeBytes(transactionRuntimePath(c), candidateRuntime); err != nil {
		if abortErr := abortTransactionLocked(context.Background(), c, txID, true); abortErr != nil {
			return fmt.Errorf("candidate runtime changed before state commit: %w; transaction restore failed: %v", err, abortErr)
		}
		return err
	}
	if err := state.SaveCurrent(c.StateDir, cur); err != nil {
		abortErr := abortTransactionLocked(context.Background(), c, txID, true)
		if abortErr != nil {
			return fmt.Errorf("production applied but state update failed: %w; transaction restore failed: %v", err, abortErr)
		}
		return fmt.Errorf("production applied but state update failed: %w", err)
	}
	if err := state.UpdateTransactionPhase(c.StateDir, txID, state.PhaseStateCommitted); err != nil {
		return fmt.Errorf("selected state committed but transaction journal update failed: %w", err)
	}
	if err := state.CompleteTransaction(c.StateDir, txID); err != nil {
		return fmt.Errorf("selected state committed but transaction cleanup failed: %w", err)
	}
	if announce {
		fmt.Printf("Applied to production %s. Backup: %s\n", normalizedRuntime(c), backup)
	}
	return nil
}

func runtimeHealthTimeout(c config.Config) time.Duration {
	if d := c.SingBoxRestartAckTimeout.Duration; d > 0 {
		return d
	}
	return 30 * time.Second
}

func singboxRestartConfig(c config.Config) singbox.RestartConfig {
	generationFile := strings.TrimSpace(c.SingBoxRestartAckGenerationFile)
	// Existing local request-file configs predate the explicit generation and
	// health-ack keys. Infer only the known vpnkit namespace; arbitrary request
	// files must explicitly configure the full protocol.
	if generationFile == "" && strings.EqualFold(strings.TrimSpace(c.SingBoxRestartMode), string(singbox.RestartModeRequestFile)) {
		generationFile = strings.TrimSpace(os.Getenv("SINGBOX_GENERATION_FILE"))
		if generationFile == "" {
			generationFile = strings.TrimSpace(os.Getenv("VPNKIT_SINGBOX_GENERATION_FILE"))
		}
		if generationFile == "" && filepath.Clean(c.SingBoxRestartFile) == "/run/vpnkit/restart-sing-box" {
			generationFile = "/run/vpnkit/sing-box-generation"
		}
	}
	healthAckFile := strings.TrimSpace(c.SingBoxRestartAckFile)
	if healthAckFile == "" && generationFile != "" {
		healthAckFile = generationFile + ".ack"
	}
	return singbox.RestartConfig{
		Mode:              singbox.RestartMode(c.SingBoxRestartMode),
		Service:           c.SingBoxService,
		RequestFile:       c.SingBoxRestartFile,
		AckGenerationFile: generationFile,
		AckFile:           healthAckFile,
		ConfigPath:        c.SingBoxConfig,
		ProbeAddress:      c.ProductionSocks,
		HealthTimeout:     runtimeHealthTimeout(c),
		AckTimeout:        c.SingBoxRestartAckTimeout.Duration,
		SingBoxBin:        c.SingBoxBin,
	}
}

func normalizedRuntime(c config.Config) string {
	r := strings.ToLower(strings.TrimSpace(c.Runtime))
	if r == "" || r == "sing-box" {
		return "singbox"
	}
	return r
}
