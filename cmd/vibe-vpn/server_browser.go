package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockedby/local-vpn-kde/internal/config"

	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"github.com/spf13/cobra"
)

const (
	browserCatalogFile            = "server-browser-catalog.json"
	browserGenerationFile         = ".server-browser-generation.json"
	browserCommitFile             = ".server-browser-commit.json"
	browserPublicationJournalFile = ".server-browser-publication.txn.json"
	browserPublicationBackupFile  = ".server-browser-catalog.backup"
	browserSaltFile               = ".server-browser-id-salt"
	browserSaltSize               = 32
	browserMaxServers             = 1000
	browserMaxCatalogBytes        = 64 << 20
	browserMaxGenerationLen       = 4096
	browserMaxCommitLen           = 8192
	browserMaxJournalLen          = 16384
)

const (
	browserCommitSchema             = "vibe-vpn.private-server-commit.v1"
	browserPublicationJournalSchema = "vibe-vpn.private-server-publication-transaction.v1"
	browserPublicationPrepared      = "prepared"
	browserPublicationRollback      = "rollback-requested"
	browserPublicationRefresh       = "refresh"
	browserPublicationProbe         = "probe"
)

type safeJSONExit struct{ status string }

func (e safeJSONExit) Error() string  { return "JSON command did not complete successfully" }
func (e safeJSONExit) SafeJSON() bool { return true }
func isSafeJSONExit(err error) bool {
	var safe interface{ SafeJSON() bool }
	return errors.As(err, &safe) && safe.SafeJSON()
}

func isBrowserJSONInvocation(args []string) bool {
	commandAt := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				return false
			}
			i++
		case strings.HasPrefix(arg, "--config="):
			continue
		case strings.HasPrefix(arg, "-"):
			// Unknown flags before the command are not evidence that a later
			// arbitrary token names the browser contract.
			return false
		default:
			commandAt = i
		}
		if commandAt >= 0 {
			break
		}
	}
	if commandAt < 0 {
		return false
	}
	switch args[commandAt] {
	case "check-batch", "list", "refresh", "test-all", "availability", "speed", "ping", "select", "current":
	default:
		return false
	}
	for _, arg := range args[commandAt+1:] {
		if arg == "--" {
			return false
		}
		if arg == "--json" {
			return true
		}
		if strings.HasPrefix(arg, "--json=") {
			requested, err := strconv.ParseBool(strings.TrimPrefix(arg, "--json="))
			if err == nil && requested {
				return true
			}
		}
	}
	return false
}

func writeBrowserFallbackJSONTo(out io.Writer) {
	_ = json.NewEncoder(out).Encode(picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "unavailable", Generation: 0})
}

func writeBrowserFallbackJSON() { writeBrowserFallbackJSONTo(os.Stdout) }

func routeCommandError(err error, args []string, stdout, stderr io.Writer) {
	if err == nil || isSafeJSONExit(err) {
		return
	}
	if isBrowserJSONInvocation(args) {
		writeBrowserFallbackJSONTo(stdout)
		return
	}
	fmt.Fprintln(stderr, "ERROR:", err)
}

// browserPublicationFailpoint is a test-only seam. Production leaves it nil.
// Labels bracket the durable generation, catalog, and commit publications.
var browserPublicationFailpoint func(string)

type browserDependencies struct {
	targetURL string
	tcpPing   bool
	listOnly  bool
	probe     func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error)
	load      func(context.Context, config.Config) ([]picker.NodeResult, error)
	test      func(context.Context, config.Config, picker.NodeResult) (float64, error)
	apply     func(context.Context, config.Config, picker.NodeResult) error
	recover   func(context.Context, config.Config) error
}

func defaultBrowserDependencies() browserDependencies {
	return browserDependencies{
		load: loadBrowserCandidates,
		probe: func(ctx context.Context, c config.Config, result picker.NodeResult) (nettest.Result, error) {
			node := vless.Node{Link: result.Link, Name: result.Name, Host: result.Host, Port: result.Port, Network: result.Network, Security: result.Security, Outbound: result.Outbound}
			// The browser explicitly measures download throughput, never RTT.
			c.TestDurationSeconds = 3
			return testOneContext(ctx, c, node, false)
		},
		apply: func(ctx context.Context, c config.Config, result picker.NodeResult) error {
			return applyResultLockedWithOptionsOutput(ctx, c, result, false, false)
		},
		recover: recoverTransactionsLocked,
	}
}

func (d browserDependencies) measure(ctx context.Context, c config.Config, r picker.NodeResult) (nettest.Result, error) {
	if d.targetURL != "" {
		c.TestURL, c.TestDurationSeconds, c.TestLimitKiB = d.targetURL, 0, 1
		n := vless.Node{Link: r.Link, Name: r.Name, Host: r.Host, Port: r.Port, Network: r.Network, Security: r.Security, Outbound: r.Outbound}
		return testOneContextUsing(ctx, c, n, false, func() (nettest.Result, error) {
			return nettest.Result{}, nettest.CheckAvailability(ctx, c.TestSocks, d.targetURL, 10*time.Second)
		})
	}
	if d.tcpPing {
		started := time.Now()
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(r.Host, strconv.Itoa(r.Port)))
		if conn != nil {
			_ = conn.Close()
		}
		return nettest.Result{Seconds: time.Since(started).Seconds()}, err
	}
	if d.probe != nil {
		return d.probe(ctx, c, r)
	}
	seconds, err := d.test(ctx, c, r)
	return nettest.Result{Seconds: seconds}, err
}

func addServerBrowserCommands(root *cobra.Command, o *cliOptions) {
	addServerCheckBatchCommand(root, o)
	refresh := &cobra.Command{Use: "refresh", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		if !jsonOut {
			return fmt.Errorf("--json is required")
		}
		deps := defaultBrowserDependencies()
		deps.listOnly = true
		return runBrowserTestAll(cmd, o, deps)
	}}
	refresh.Flags().Bool("json", false, "print the fixed redacted JSON schema")
	root.AddCommand(refresh)

	testAll := &cobra.Command{
		Use:   "test-all",
		Short: "Test the private server catalog and emit only redacted JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			if !jsonOut {
				return fmt.Errorf("--json is required")
			}
			return runBrowserTestAll(cmd, o, defaultBrowserDependencies())
		},
	}
	testAll.Flags().Bool("json", false, "print the fixed redacted JSON schema")
	root.AddCommand(testAll)

	ping := &cobra.Command{
		Use:   "ping",
		Short: "Test one opaque server ID and emit only redacted JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := cmd.Flags().GetString("server-id")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if !jsonOut {
				return fmt.Errorf("--json is required")
			}
			deps := defaultBrowserDependencies()
			deps.tcpPing = true
			return runBrowserPing(cmd, o, id, deps)
		},
	}
	ping.Flags().String("server-id", "", "opaque server ID from list --json")
	ping.Flags().Bool("json", false, "print the fixed redacted JSON schema")
	root.AddCommand(ping)

	speed := &cobra.Command{Use: "speed", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, _ := cmd.Flags().GetString("server-id")
		jsonOut, _ := cmd.Flags().GetBool("json")
		if !jsonOut {
			return fmt.Errorf("--json is required")
		}
		return runBrowserPing(cmd, o, id, defaultBrowserDependencies())
	}}
	speed.Flags().String("server-id", "", "opaque server ID")
	speed.Flags().Bool("json", false, "print redacted JSON")
	root.AddCommand(speed)

	availability := &cobra.Command{Use: "availability", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, _ := cmd.Flags().GetString("server-id")
		target, _ := cmd.Flags().GetString("url")
		jsonOut, _ := cmd.Flags().GetBool("json")
		u, err := url.Parse(target)
		if !jsonOut || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(target) > 2048 {
			return writeBrowserFailure(cmd, "unavailable", 0)
		}
		deps := defaultBrowserDependencies()
		deps.targetURL = target
		return runBrowserPing(cmd, o, id, deps)
	}}
	availability.Flags().String("server-id", "", "opaque server ID")
	availability.Flags().String("url", "", "HTTPS availability target")
	availability.Flags().Bool("json", false, "print redacted JSON")
	root.AddCommand(availability)

	selectServer := &cobra.Command{
		Use:   "select",
		Short: "Select one opaque server ID with runtime acknowledgement",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := cmd.Flags().GetString("server-id")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if !jsonOut {
				return fmt.Errorf("--json is required")
			}
			return runBrowserSelect(cmd, o, id, defaultBrowserDependencies())
		},
	}
	selectServer.Flags().String("server-id", "", "opaque server ID from list --json")
	selectServer.Flags().Bool("json", false, "print the fixed redacted JSON schema")
	root.AddCommand(selectServer)
}

func loadBrowserCandidates(ctx context.Context, c config.Config) ([]picker.NodeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	links, _, fetchErr := loadSubscriptionLinksContext(ctx, c)
	if fetchErr != nil {
		return nil, fetchErr
	}
	if len(links) > browserMaxServers {
		return nil, fmt.Errorf("private catalog exceeds bounded size")
	}
	results := make([]picker.NodeResult, 0, len(links))
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, err := vless.Parse(link)
		if err != nil {
			continue
		}
		results = append(results, browserResult(len(results)+1, node))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("private catalog has no supported servers")
	}
	return results, nil
}

func browserResult(index int, node vless.Node) picker.NodeResult {
	return picker.NodeResult{Index: index, Name: node.Name, Host: node.Host, Port: node.Port, Network: node.Network, Security: node.Security, Link: node.Link, Outbound: node.Outbound}
}

func runBrowserList(cmd *cobra.Command, o *cliOptions) error {
	ctx := browserCommandContext(cmd)
	c, err := loadConfig(o.configPath)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	defer lock.Close()
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	catalog, err := loadBrowserCatalogLocked(c.StateDir)
	if os.IsNotExist(err) {
		// Discovery does not need a working selected upstream or a host VPN.
		// Network fetches run outside the catalog lock, like test-all.
		_ = lock.Close()
		deps := defaultBrowserDependencies()
		deps.listOnly = true
		return runBrowserTestAll(cmd, o, deps)
	}
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	selected := selectedBrowserIDLocked(c.StateDir, catalog)
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: catalog.Generation, Servers: catalog.SafeServers(selected)}, false)
}

func runBrowserCurrent(cmd *cobra.Command, o *cliOptions) error {
	ctx := browserCommandContext(cmd)
	c, err := loadConfig(o.configPath)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	defer lock.Close()
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	catalog, err := loadBrowserCatalogLocked(c.StateDir)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	selected := selectedBrowserIDLocked(c.StateDir, catalog)
	record, ok := catalog.Find(selected)
	if !ok {
		return writeBrowserFailure(cmd, "unavailable", catalog.Generation)
	}
	server := record.Safe(true)
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: catalog.Generation, Server: &server}, false)
}

type browserPublicationCommit struct {
	Schema            string `json:"schema"`
	HighWater         uint64 `json:"high_water"`
	Revision          uint64 `json:"revision"`
	CatalogGeneration uint64 `json:"catalog_generation"`
	CatalogDigest     string `json:"catalog_digest,omitempty"`
	Integrity         string `json:"integrity"`
}

type browserPublicationJournal struct {
	Schema               string `json:"schema"`
	Phase                string `json:"phase"`
	Kind                 string `json:"kind"`
	OldHighWater         uint64 `json:"old_high_water"`
	OldRevision          uint64 `json:"old_revision"`
	NewHighWater         uint64 `json:"new_high_water"`
	NewRevision          uint64 `json:"new_revision"`
	OldCatalogGeneration uint64 `json:"old_catalog_generation"`
	OldCatalogDigest     string `json:"old_catalog_digest,omitempty"`
	NewCatalogGeneration uint64 `json:"new_catalog_generation"`
	NewCatalogDigest     string `json:"new_catalog_digest"`
	Integrity            string `json:"integrity"`
}

type browserPublicationBaseline struct {
	catalog           picker.BrowserCatalog
	catalogVersion    state.FileVersion
	generation        picker.BrowserGenerationState
	generationVersion state.FileVersion
	commit            browserPublicationCommit
	commitVersion     state.FileVersion
	salt              []byte
	saltVersion       state.FileVersion
}

func runBrowserTestAll(cmd *cobra.Command, o *cliOptions, deps browserDependencies) error {
	ctx := browserCommandContext(cmd)
	c, err := loadConfig(o.configPath)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	preflightLock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	if browserRecoveryPendingLocked(c.StateDir) {
		_ = preflightLock.Close()
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		_ = preflightLock.Close()
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	baseline, err := captureBrowserPublicationBaselineLocked(c.StateDir)
	if err != nil {
		_ = preflightLock.Close()
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	// Capture selection while the same preflight lock is held. A selection in
	// the former unlock-to-capture gap could otherwise be silently invalidated.
	selectionBaseline, err := state.CaptureFileVersion(filepath.Join(c.StateDir, "current-node.json"))
	_ = preflightLock.Close()
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}

	results, err := deps.load(ctx, c)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	for i := range results {
		if deps.listOnly {
			continue
		}
		if err := ctx.Err(); err != nil {
			return writeBrowserContextFailure(cmd, ctx, 0)
		}
		probe, testErr := deps.measure(ctx, c, results[i])
		if err := ctx.Err(); err != nil {
			return writeBrowserContextFailure(cmd, ctx, 0)
		}
		results[i].Seconds = boundedSeconds(probe.Seconds)
		results[i].Mbps = probe.Mbps
		results[i].Bytes = probe.Bytes
		results[i].OK = testErr == nil
		if testErr != nil {
			// A private marker distinguishes tested failure from untested state;
			// the backend error itself is deliberately discarded.
			results[i].Error = "probe failed"
		}
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	defer lock.Close()
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	current, err := captureBrowserPublicationBaselineLocked(c.StateDir)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	if !current.catalogVersion.Equal(baseline.catalogVersion) ||
		!current.generationVersion.Equal(baseline.generationVersion) ||
		!current.commitVersion.Equal(baseline.commitVersion) ||
		!current.saltVersion.Equal(baseline.saltVersion) {
		return writeBrowserFailure(cmd, "stale", baseline.catalog.Generation)
	}
	if err := ctx.Err(); err != nil {
		return writeBrowserContextFailure(cmd, ctx, baseline.catalog.Generation)
	}

	salt := current.salt
	if len(salt) == 0 {
		salt, err = state.LoadOrCreateSecretLocked(c.StateDir, browserSaltFile, browserSaltSize)
		if err != nil {
			return writeBrowserFailure(cmd, "unavailable", 0)
		}
		current.salt = salt
	}
	highWater := current.generation.Generation
	if current.catalog.Generation > highWater {
		highWater = current.catalog.Generation
	}
	if highWater == math.MaxUint64 || current.generation.Revision == math.MaxUint64 {
		return writeBrowserFailure(cmd, "unavailable", highWater)
	}
	generation := highWater + 1
	catalog := picker.NewBrowserCatalog(generation, salt, results)
	if len(catalog.Servers) == 0 {
		return writeBrowserFailure(cmd, "unavailable", generation)
	}
	selectionCurrent, err := state.CaptureFileVersion(filepath.Join(c.StateDir, "current-node.json"))
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", generation)
	}
	if !selectionCurrent.Equal(selectionBaseline) && selectedBrowserIDLocked(c.StateDir, catalog) == "" {
		// A selection that completed while testing wins. Do not publish a catalog
		// that immediately makes its freshly acknowledged ID stale.
		return writeBrowserFailure(cmd, "stale", highWater)
	}
	if err := publishBrowserCatalogLocked(ctx, c.StateDir, current, catalog, generation, browserPublicationRefresh); err != nil {
		return writeBrowserContextFailure(cmd, ctx, highWater)
	}
	selected := selectedBrowserIDLocked(c.StateDir, catalog)
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: generation, Servers: catalog.SafeServers(selected)}, false)
}

func runBrowserPing(cmd *cobra.Command, o *cliOptions, id string, deps browserDependencies) error {
	ctx := browserCommandContext(cmd)
	c, err := loadConfig(o.configPath)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	if browserRecoveryPendingLocked(c.StateDir) {
		_ = lock.Close()
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		_ = lock.Close()
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	baseline, err := captureBrowserPublicationBaselineLocked(c.StateDir)
	if err != nil || !baseline.catalogVersion.Exists() {
		_ = lock.Close()
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	catalog := baseline.catalog
	record, ok := catalog.Find(strings.TrimSpace(id))
	generation := catalog.Generation
	_ = lock.Close()
	if !ok {
		return writeBrowserFailure(cmd, "stale", generation)
	}
	probe, probeErr := deps.measure(ctx, c, record.Result)
	if err := ctx.Err(); err != nil {
		return writeBrowserContextFailure(cmd, ctx, generation)
	}

	lock, err = state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, generation)
	}
	defer lock.Close()
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", generation)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", generation)
	}
	current, err := captureBrowserPublicationBaselineLocked(c.StateDir)
	if err != nil || current.catalog.Generation != generation ||
		!current.catalogVersion.Equal(baseline.catalogVersion) ||
		!current.generationVersion.Equal(baseline.generationVersion) ||
		!current.commitVersion.Equal(baseline.commitVersion) ||
		!current.saltVersion.Equal(baseline.saltVersion) {
		return writeBrowserFailure(cmd, "stale", generation)
	}
	latest := current.catalog
	updated, ok := latest.Find(record.ID)
	if !ok {
		return writeBrowserFailure(cmd, "stale", generation)
	}
	for i := range latest.Servers {
		if latest.Servers[i].ID == updated.ID {
			if deps.targetURL != "" {
				latest.Servers[i].Result.Availability = "failed"
				if probeErr == nil {
					latest.Servers[i].Result.Availability = "ready"
				}
			} else if deps.tcpPing {
				latest.Servers[i].Result.PingMS = 0
				latest.Servers[i].Result.PingStatus = "failed"
				if probeErr == nil {
					latest.Servers[i].Result.PingMS = int(probe.Seconds*1000 + .5)
					if latest.Servers[i].Result.PingMS < 1 {
						latest.Servers[i].Result.PingMS = 1
					}
					latest.Servers[i].Result.PingStatus = "ready"
				}
			} else {
				latest.Servers[i].Result.Seconds = boundedSeconds(probe.Seconds)
				latest.Servers[i].Result.Mbps = probe.Mbps
				latest.Servers[i].Result.Bytes = probe.Bytes
				latest.Servers[i].Result.OK = probeErr == nil
				if probeErr != nil {
					latest.Servers[i].Result.Error = "probe failed"
				} else {
					latest.Servers[i].Result.Error = ""
				}
			}
			updated = latest.Servers[i]
			break
		}
	}
	picker.SealBrowserCatalog(current.salt, &latest)
	if current.generation.Revision == math.MaxUint64 {
		return writeBrowserFailure(cmd, "unavailable", generation)
	}
	if err := publishBrowserCatalogLocked(ctx, c.StateDir, current, latest, current.generation.Generation, browserPublicationProbe); err != nil {
		return writeBrowserContextFailure(cmd, ctx, generation)
	}
	selected := selectedBrowserIDLocked(c.StateDir, latest) == updated.ID
	server := updated.Safe(selected)
	status := "ok"
	failed := false
	if probeErr != nil {
		status, failed = "failed", true
	}
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: status, Generation: generation, Server: &server}, failed)
}

func runBrowserSelect(cmd *cobra.Command, o *cliOptions, id string, deps browserDependencies) error {
	ctx := browserCommandContext(cmd)
	c, err := loadConfig(o.configPath)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	lock, err := state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	defer lock.Close()
	if err := deps.recover(ctx, c); err != nil {
		if browserRecoveryPendingLocked(c.StateDir) {
			return writeBrowserFailure(cmd, "recovery_required", 0)
		}
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", 0)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	if err := ctx.Err(); err != nil {
		return writeBrowserContextFailure(cmd, ctx, 0)
	}
	catalog, err := loadBrowserCatalogLocked(c.StateDir)
	if err != nil {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	record, ok := catalog.Find(strings.TrimSpace(id))
	if !ok {
		return writeBrowserFailure(cmd, "stale", catalog.Generation)
	}
	if err := ctx.Err(); err != nil {
		return writeBrowserContextFailure(cmd, ctx, catalog.Generation)
	}
	record.Result.ServerID = record.ID
	record.Result.Generation = catalog.Generation
	if err := deps.apply(ctx, c, record.Result); err != nil {
		if browserRecoveryPendingLocked(c.StateDir) {
			return writeBrowserFailure(cmd, "recovery_required", catalog.Generation)
		}
		if ctx.Err() != nil {
			return writeBrowserContextFailure(cmd, ctx, catalog.Generation)
		}
		return writeBrowserFailure(cmd, "failed", catalog.Generation)
	}
	current, err := state.LoadCurrent(c.StateDir)
	if err != nil || current.ServerID != record.ID || current.Generation != catalog.Generation {
		return writeBrowserFailure(cmd, "recovery_required", catalog.Generation)
	}
	server := record.Safe(true)
	server.Status = "selected"
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: catalog.Generation, Server: &server}, false)
}

func loadBrowserCatalogLocked(stateDir string) (picker.BrowserCatalog, error) {
	catalog, _, err := loadBrowserCatalogVersionLocked(stateDir)
	return catalog, err
}

func loadBrowserCatalogVersionLocked(stateDir string) (picker.BrowserCatalog, state.FileVersion, error) {
	baseline, err := captureBrowserPublicationBaselineLocked(stateDir)
	if err != nil || !baseline.catalogVersion.Exists() {
		if err == nil {
			err = os.ErrNotExist
		}
		return picker.BrowserCatalog{}, state.FileVersion{}, err
	}
	return baseline.catalog, baseline.catalogVersion, nil
}

func loadBrowserCatalogDataLocked(stateDir string) (picker.BrowserCatalog, state.FileVersion, []byte, state.FileVersion, error) {
	body, version, err := state.LoadPrivateFileLocked(stateDir, browserCatalogFile, browserMaxCatalogBytes)
	if err != nil {
		return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, err
	}
	var catalog picker.BrowserCatalog
	if err := decodeStrictPrivateJSON(body, &catalog); err != nil || !catalog.Valid() || len(catalog.Servers) == 0 || len(catalog.Servers) > browserMaxServers {
		return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, fmt.Errorf("invalid private browser catalog")
	}
	salt, saltVersion, err := loadBrowserSaltLocked(stateDir)
	if err != nil {
		return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, err
	}
	if !picker.VerifyBrowserCatalog(salt, catalog) {
		return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, fmt.Errorf("invalid private browser catalog integrity")
	}
	seen := make(map[string]struct{}, len(catalog.Servers))
	for _, record := range catalog.Servers {
		if record.ID == "" || record.ID != picker.BrowserServerID(salt, record.Result) {
			return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, fmt.Errorf("invalid private browser catalog identity")
		}
		if _, duplicate := seen[record.ID]; duplicate {
			return picker.BrowserCatalog{}, state.FileVersion{}, nil, state.FileVersion{}, fmt.Errorf("duplicate private browser catalog identity")
		}
		seen[record.ID] = struct{}{}
	}
	return catalog, version, salt, saltVersion, nil
}

func loadBrowserSaltLocked(stateDir string) ([]byte, state.FileVersion, error) {
	salt, version, err := state.LoadPrivateFileLocked(stateDir, browserSaltFile, browserSaltSize)
	if err != nil {
		return nil, state.FileVersion{}, err
	}
	if len(salt) != browserSaltSize {
		return nil, state.FileVersion{}, fmt.Errorf("private browser salt has invalid size")
	}
	return salt, version, nil
}

func loadBrowserGenerationLocked(stateDir string, salt []byte) (picker.BrowserGenerationState, state.FileVersion, error) {
	body, version, err := state.LoadPrivateFileLocked(stateDir, browserGenerationFile, browserMaxGenerationLen)
	if err != nil {
		return picker.BrowserGenerationState{}, state.FileVersion{}, err
	}
	var generation picker.BrowserGenerationState
	if err := decodeStrictPrivateJSON(body, &generation); err != nil || !picker.VerifyBrowserGenerationState(salt, generation) {
		return picker.BrowserGenerationState{}, state.FileVersion{}, fmt.Errorf("invalid private browser generation")
	}
	return generation, version, nil
}

func loadBrowserCommitLocked(stateDir string, salt []byte) (browserPublicationCommit, state.FileVersion, error) {
	body, version, err := state.LoadPrivateFileLocked(stateDir, browserCommitFile, browserMaxCommitLen)
	if err != nil {
		return browserPublicationCommit{}, state.FileVersion{}, err
	}
	var commit browserPublicationCommit
	if err := decodeStrictPrivateJSON(body, &commit); err != nil || !verifyBrowserPublicationCommit(salt, commit) {
		return browserPublicationCommit{}, state.FileVersion{}, fmt.Errorf("invalid private browser commit")
	}
	return commit, version, nil
}

func captureBrowserPublicationBaselineLocked(stateDir string) (browserPublicationBaseline, error) {
	var baseline browserPublicationBaseline
	salt, saltVersion, err := loadBrowserSaltLocked(stateDir)
	if os.IsNotExist(err) {
		for _, file := range []struct {
			name string
			max  int64
		}{{browserCatalogFile, browserMaxCatalogBytes}, {browserGenerationFile, browserMaxGenerationLen}, {browserCommitFile, browserMaxCommitLen}} {
			if _, _, loadErr := state.LoadPrivateFileLocked(stateDir, file.name, file.max); loadErr == nil || !os.IsNotExist(loadErr) {
				return browserPublicationBaseline{}, fmt.Errorf("private browser publication exists without its salt")
			}
		}
		return baseline, nil
	}
	if err != nil {
		return browserPublicationBaseline{}, err
	}
	baseline.salt, baseline.saltVersion = salt, saltVersion

	generation, generationVersion, err := loadBrowserGenerationLocked(stateDir, salt)
	if os.IsNotExist(err) {
		if _, _, commitErr := state.LoadPrivateFileLocked(stateDir, browserCommitFile, browserMaxCommitLen); commitErr == nil || !os.IsNotExist(commitErr) {
			return browserPublicationBaseline{}, fmt.Errorf("private browser commit exists without generation state")
		}
		if _, _, catalogErr := state.LoadPrivateFileLocked(stateDir, browserCatalogFile, browserMaxCatalogBytes); catalogErr == nil || !os.IsNotExist(catalogErr) {
			return browserPublicationBaseline{}, fmt.Errorf("private browser catalog exists without generation state")
		}
		return baseline, nil
	}
	if err != nil || !picker.IsCurrentBrowserGenerationState(generation) {
		return browserPublicationBaseline{}, fmt.Errorf("private browser generation is not current")
	}
	baseline.generation, baseline.generationVersion = generation, generationVersion
	commit, commitVersion, err := loadBrowserCommitLocked(stateDir, salt)
	if err != nil {
		return browserPublicationBaseline{}, err
	}
	if commit.HighWater != generation.Generation || commit.Revision != generation.Revision {
		return browserPublicationBaseline{}, fmt.Errorf("private browser commit is not synchronized")
	}
	baseline.commit, baseline.commitVersion = commit, commitVersion
	if commit.CatalogGeneration == 0 {
		if _, _, catalogErr := state.LoadPrivateFileLocked(stateDir, browserCatalogFile, browserMaxCatalogBytes); catalogErr == nil || !os.IsNotExist(catalogErr) {
			return browserPublicationBaseline{}, fmt.Errorf("uncommitted private browser catalog exists")
		}
		return baseline, nil
	}
	catalog, catalogVersion, _, _, err := loadBrowserCatalogDataLocked(stateDir)
	if err != nil {
		return browserPublicationBaseline{}, err
	}
	if catalog.Generation != commit.CatalogGeneration || catalog.Generation > generation.Generation || browserDigest(catalogVersion.Data()) != commit.CatalogDigest {
		return browserPublicationBaseline{}, fmt.Errorf("private browser catalog does not match its commit")
	}
	baseline.catalog, baseline.catalogVersion = catalog, catalogVersion
	return baseline, nil
}

func recoverBrowserPublicationLocked(stateDir string) error {
	journalBody, _, err := state.LoadPrivateFileLocked(stateDir, browserPublicationJournalFile, browserMaxJournalLen)
	if os.IsNotExist(err) {
		if err := state.RemovePrivateFileLocked(stateDir, browserPublicationBackupFile, browserMaxCatalogBytes); err != nil {
			return err
		}
		return normalizeBrowserPublicationLocked(stateDir)
	}
	if err != nil {
		return err
	}
	salt, _, err := loadBrowserSaltLocked(stateDir)
	if err != nil {
		return err
	}
	var journal browserPublicationJournal
	if err := decodeStrictPrivateJSON(journalBody, &journal); err != nil || !verifyBrowserPublicationJournal(salt, journal) {
		return fmt.Errorf("invalid private browser publication transaction")
	}
	return recoverBrowserPublicationTransactionLocked(stateDir, salt, journal)
}

func normalizeBrowserPublicationLocked(stateDir string) error {
	salt, _, err := loadBrowserSaltLocked(stateDir)
	if os.IsNotExist(err) {
		for _, file := range []struct {
			name string
			max  int64
		}{{browserCatalogFile, browserMaxCatalogBytes}, {browserGenerationFile, browserMaxGenerationLen}, {browserCommitFile, browserMaxCommitLen}} {
			if _, _, loadErr := state.LoadPrivateFileLocked(stateDir, file.name, file.max); loadErr == nil || !os.IsNotExist(loadErr) {
				return fmt.Errorf("private browser publication exists without its salt")
			}
		}
		return nil
	}
	if err != nil {
		return err
	}

	catalog, catalogVersion, _, _, catalogErr := loadBrowserCatalogDataLocked(stateDir)
	generation, _, generationErr := loadBrowserGenerationLocked(stateDir, salt)
	commit, _, commitErr := loadBrowserCommitLocked(stateDir, salt)
	catalogExists := catalogErr == nil
	generationExists := generationErr == nil
	commitExists := commitErr == nil
	if catalogErr != nil && !os.IsNotExist(catalogErr) {
		return catalogErr
	}
	if generationErr != nil && !os.IsNotExist(generationErr) {
		return generationErr
	}
	if commitErr != nil && !os.IsNotExist(commitErr) {
		return commitErr
	}

	if generationExists && picker.IsCurrentBrowserGenerationState(generation) {
		if !commitExists {
			return fmt.Errorf("private browser commit is missing")
		}
		return validateBrowserPublicationPair(generation, commit, catalog, catalogVersion, catalogExists)
	}
	if !generationExists && !catalogExists {
		if commitExists {
			return fmt.Errorf("private browser commit exists without publication state")
		}
		// A salt can be durably created before the first publication journal.
		return nil
	}

	highWater := uint64(0)
	if generationExists {
		highWater = generation.Generation
	}
	if catalogExists {
		if highWater == 0 {
			highWater = catalog.Generation
		} else if highWater != catalog.Generation {
			// A legacy high-water/catalog mismatch is exactly the ambiguous state
			// that REV-001 must not make readable by accepting generation < marker.
			return fmt.Errorf("legacy private browser publication is ambiguous")
		}
	}
	if highWater == 0 {
		return fmt.Errorf("private browser publication has no high-water mark")
	}
	revision := uint64(1)
	targetCommit := newBrowserPublicationCommit(highWater, revision, catalog.Generation, catalogVersion.Data(), catalogExists, salt)
	if commitExists {
		if !sameBrowserPublicationCommit(commit, targetCommit) {
			return fmt.Errorf("private browser migration commit is inconsistent")
		}
	} else if err := saveBrowserPrivateJSONLocked(context.Background(), stateDir, browserCommitFile, targetCommit); err != nil {
		return err
	}
	targetGeneration := picker.NewBrowserGenerationStateWithRevision(highWater, revision, salt)
	if err := saveBrowserPrivateJSONLocked(context.Background(), stateDir, browserGenerationFile, targetGeneration); err != nil {
		return err
	}
	_, err = captureBrowserPublicationBaselineLocked(stateDir)
	return err
}

func validateBrowserPublicationPair(generation picker.BrowserGenerationState, commit browserPublicationCommit, catalog picker.BrowserCatalog, catalogVersion state.FileVersion, catalogExists bool) error {
	if commit.HighWater != generation.Generation || commit.Revision != generation.Revision {
		return fmt.Errorf("private browser commit is not synchronized")
	}
	if commit.CatalogGeneration == 0 {
		if catalogExists || commit.CatalogDigest != "" {
			return fmt.Errorf("private browser empty commit has a catalog")
		}
		return nil
	}
	if !catalogExists || catalog.Generation != commit.CatalogGeneration || catalog.Generation > generation.Generation || browserDigest(catalogVersion.Data()) != commit.CatalogDigest {
		return fmt.Errorf("private browser catalog does not match its commit")
	}
	return nil
}

func publishBrowserCatalogLocked(ctx context.Context, stateDir string, baseline browserPublicationBaseline, catalog picker.BrowserCatalog, newHighWater uint64, kind string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(baseline.salt) != browserSaltSize || catalog.Generation == 0 || catalog.Generation > newHighWater || !picker.VerifyBrowserCatalog(baseline.salt, catalog) {
		return fmt.Errorf("invalid private browser publication candidate")
	}
	oldHighWater, oldRevision := baseline.generation.Generation, baseline.generation.Revision
	if oldRevision == math.MaxUint64 || newHighWater < oldHighWater {
		return fmt.Errorf("private browser publication high-water overflow")
	}
	switch kind {
	case browserPublicationRefresh:
		if oldHighWater == math.MaxUint64 || newHighWater != oldHighWater+1 || catalog.Generation != newHighWater {
			return fmt.Errorf("invalid private browser refresh transition")
		}
	case browserPublicationProbe:
		if newHighWater != oldHighWater || !baseline.catalogVersion.Exists() || catalog.Generation != baseline.catalog.Generation {
			return fmt.Errorf("invalid private browser probe transition")
		}
	default:
		return fmt.Errorf("invalid private browser publication kind")
	}
	catalogBody, err := marshalBrowserPrivateJSON(catalog)
	if err != nil {
		return err
	}
	journal := browserPublicationJournal{
		Schema: browserPublicationJournalSchema, Phase: browserPublicationPrepared, Kind: kind,
		OldHighWater: oldHighWater, OldRevision: oldRevision, NewHighWater: newHighWater, NewRevision: oldRevision + 1,
		OldCatalogGeneration: baseline.catalog.Generation, NewCatalogGeneration: catalog.Generation,
		NewCatalogDigest: browserDigest(catalogBody),
	}
	if baseline.catalogVersion.Exists() {
		journal.OldCatalogDigest = browserDigest(baseline.catalogVersion.Data())
	}
	sealBrowserPublicationJournal(baseline.salt, &journal)

	if baseline.catalogVersion.Exists() {
		if err := state.WritePrivateFileAtomicLocked(ctx, stateDir, browserPublicationBackupFile, baseline.catalogVersion.Data()); err != nil {
			return err
		}
	} else if err := state.RemovePrivateFileLocked(stateDir, browserPublicationBackupFile, browserMaxCatalogBytes); err != nil {
		return err
	}
	if err := saveBrowserPrivateJSONLocked(ctx, stateDir, browserPublicationJournalFile, journal); err != nil {
		// Atomic publication errors after rename are uncertain: the prepared
		// journal may already be visible even though its directory fsync failed.
		// Retain the only old-catalog backup; no-journal recovery removes it as
		// stale, while visible-journal recovery requires it for rollback.
		return err
	}

	generation := picker.NewBrowserGenerationStateWithRevision(journal.NewHighWater, journal.NewRevision, baseline.salt)
	browserPublicationPoint("before-generation-fsync")
	if err := saveBrowserPrivateJSONLocked(ctx, stateDir, browserGenerationFile, generation); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}
	browserPublicationPoint("after-generation-fsync")
	if err := ctx.Err(); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}

	browserPublicationPoint("before-catalog-fsync")
	if err := state.WritePrivateFileAtomicLocked(ctx, stateDir, browserCatalogFile, catalogBody); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}
	browserPublicationPoint("after-catalog-fsync")
	if err := ctx.Err(); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}

	commit := newBrowserPublicationCommit(journal.NewHighWater, journal.NewRevision, catalog.Generation, catalogBody, true, baseline.salt)
	browserPublicationPoint("before-commit-fsync")
	if err := saveBrowserPrivateJSONLocked(ctx, stateDir, browserCommitFile, commit); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}
	browserPublicationPoint("after-commit-fsync")
	if err := ctx.Err(); err != nil {
		return abortBrowserPublicationLocked(stateDir, baseline.salt, journal, err)
	}
	if err := cleanupBrowserPublicationTransactionLocked(stateDir); err != nil {
		return err
	}
	return nil
}

func abortBrowserPublicationLocked(stateDir string, salt []byte, journal browserPublicationJournal, cause error) error {
	journal.Phase = browserPublicationRollback
	sealBrowserPublicationJournal(salt, &journal)
	if err := saveBrowserPrivateJSONLocked(context.Background(), stateDir, browserPublicationJournalFile, journal); err != nil {
		return cause
	}
	if err := recoverBrowserPublicationTransactionLocked(stateDir, salt, journal); err != nil {
		return cause
	}
	return cause
}

func recoverBrowserPublicationTransactionLocked(stateDir string, salt []byte, journal browserPublicationJournal) error {
	generation, _, generationErr := loadBrowserGenerationLocked(stateDir, salt)
	commit, _, commitErr := loadBrowserCommitLocked(stateDir, salt)
	catalog, catalogVersion, _, _, catalogErr := loadBrowserCatalogDataLocked(stateDir)
	generationExists, commitExists, catalogExists := generationErr == nil, commitErr == nil, catalogErr == nil
	if generationErr != nil && !os.IsNotExist(generationErr) {
		return generationErr
	}
	if commitErr != nil && !os.IsNotExist(commitErr) {
		return commitErr
	}
	if catalogErr != nil && !os.IsNotExist(catalogErr) {
		return catalogErr
	}
	oldCommit := newBrowserPublicationCommit(journal.OldHighWater, journal.OldRevision, journal.OldCatalogGeneration, nil, false, salt)
	if journal.OldCatalogGeneration > 0 {
		oldCommit = newBrowserPublicationCommitFromDigest(journal.OldHighWater, journal.OldRevision, journal.OldCatalogGeneration, journal.OldCatalogDigest, salt)
	}
	rollbackCommit := newBrowserPublicationCommitFromDigest(journal.NewHighWater, journal.NewRevision, journal.OldCatalogGeneration, journal.OldCatalogDigest, salt)
	finalCommit := newBrowserPublicationCommitFromDigest(journal.NewHighWater, journal.NewRevision, journal.NewCatalogGeneration, journal.NewCatalogDigest, salt)
	oldGeneration := picker.NewBrowserGenerationStateWithRevision(journal.OldHighWater, journal.OldRevision, salt)
	newGeneration := picker.NewBrowserGenerationStateWithRevision(journal.NewHighWater, journal.NewRevision, salt)

	finalState := generationExists && sameBrowserGeneration(generation, newGeneration) && commitExists && sameBrowserPublicationCommit(commit, finalCommit) && catalogExists && catalog.Generation == journal.NewCatalogGeneration && browserDigest(catalogVersion.Data()) == journal.NewCatalogDigest
	if journal.Phase == browserPublicationPrepared && finalState {
		return cleanupBrowserPublicationTransactionLocked(stateDir)
	}
	rollbackState := generationExists && sameBrowserGeneration(generation, newGeneration) && commitExists && sameBrowserPublicationCommit(commit, rollbackCommit) && browserCatalogMatchesJournal(catalog, catalogVersion, catalogExists, journal.OldCatalogGeneration, journal.OldCatalogDigest)
	if rollbackState {
		return cleanupBrowserPublicationTransactionLocked(stateDir)
	}

	if generationExists && !sameBrowserGeneration(generation, oldGeneration) && !sameBrowserGeneration(generation, newGeneration) {
		return fmt.Errorf("private browser transaction generation is unexpected")
	}
	if commitExists && !sameBrowserPublicationCommit(commit, oldCommit) && !sameBrowserPublicationCommit(commit, rollbackCommit) && !sameBrowserPublicationCommit(commit, finalCommit) {
		return fmt.Errorf("private browser transaction commit is unexpected")
	}
	if !browserCatalogMatchesJournal(catalog, catalogVersion, catalogExists, journal.OldCatalogGeneration, journal.OldCatalogDigest) && !browserCatalogMatchesJournal(catalog, catalogVersion, catalogExists, journal.NewCatalogGeneration, journal.NewCatalogDigest) {
		return fmt.Errorf("private browser transaction catalog is unexpected")
	}

	if journal.OldCatalogGeneration > 0 {
		backup, _, err := state.LoadPrivateFileLocked(stateDir, browserPublicationBackupFile, browserMaxCatalogBytes)
		if err != nil {
			return err
		}
		var backupCatalog picker.BrowserCatalog
		if err := decodeStrictPrivateJSON(backup, &backupCatalog); err != nil || !picker.VerifyBrowserCatalog(salt, backupCatalog) || backupCatalog.Generation != journal.OldCatalogGeneration || browserDigest(backup) != journal.OldCatalogDigest {
			return fmt.Errorf("invalid private browser publication backup")
		}
		if err := state.WritePrivateFileAtomicLocked(context.Background(), stateDir, browserCatalogFile, backup); err != nil {
			return err
		}
	} else if err := state.RemovePrivateFileLocked(stateDir, browserCatalogFile, browserMaxCatalogBytes); err != nil {
		return err
	}
	if err := saveBrowserPrivateJSONLocked(context.Background(), stateDir, browserGenerationFile, newGeneration); err != nil {
		return err
	}
	if err := saveBrowserPrivateJSONLocked(context.Background(), stateDir, browserCommitFile, rollbackCommit); err != nil {
		return err
	}
	return cleanupBrowserPublicationTransactionLocked(stateDir)
}

func cleanupBrowserPublicationTransactionLocked(stateDir string) error {
	if err := state.RemovePrivateFileLocked(stateDir, browserPublicationBackupFile, browserMaxCatalogBytes); err != nil {
		return err
	}
	return state.RemovePrivateFileLocked(stateDir, browserPublicationJournalFile, browserMaxJournalLen)
}

func browserCatalogMatchesJournal(catalog picker.BrowserCatalog, version state.FileVersion, exists bool, generation uint64, digest string) bool {
	if generation == 0 {
		return !exists && digest == ""
	}
	return exists && catalog.Generation == generation && browserDigest(version.Data()) == digest
}

func newBrowserPublicationCommit(highWater, revision, catalogGeneration uint64, catalogBody []byte, catalogExists bool, salt []byte) browserPublicationCommit {
	digest := ""
	if catalogExists {
		digest = browserDigest(catalogBody)
	}
	return newBrowserPublicationCommitFromDigest(highWater, revision, catalogGeneration, digest, salt)
}

func newBrowserPublicationCommitFromDigest(highWater, revision, catalogGeneration uint64, digest string, salt []byte) browserPublicationCommit {
	commit := browserPublicationCommit{Schema: browserCommitSchema, HighWater: highWater, Revision: revision, CatalogGeneration: catalogGeneration, CatalogDigest: digest}
	sealBrowserPublicationCommit(salt, &commit)
	return commit
}

func sealBrowserPublicationCommit(salt []byte, commit *browserPublicationCommit) {
	if commit == nil {
		return
	}
	commit.Integrity = ""
	commit.Integrity = browserAuthentication(salt, *commit)
}

func verifyBrowserPublicationCommit(salt []byte, commit browserPublicationCommit) bool {
	if commit.Schema != browserCommitSchema || commit.HighWater == 0 || commit.Revision == 0 || commit.CatalogGeneration > commit.HighWater || (commit.CatalogGeneration == 0) != (commit.CatalogDigest == "") {
		return false
	}
	actual := commit.Integrity
	commit.Integrity = ""
	return verifyBrowserAuthentication(salt, commit, actual)
}

func sameBrowserPublicationCommit(left, right browserPublicationCommit) bool {
	return left.Schema == right.Schema && left.HighWater == right.HighWater && left.Revision == right.Revision && left.CatalogGeneration == right.CatalogGeneration && hmac.Equal([]byte(left.CatalogDigest), []byte(right.CatalogDigest)) && hmac.Equal([]byte(left.Integrity), []byte(right.Integrity))
}

func sameBrowserGeneration(left, right picker.BrowserGenerationState) bool {
	return left.Schema == right.Schema && left.Generation == right.Generation && left.Revision == right.Revision && hmac.Equal([]byte(left.Integrity), []byte(right.Integrity))
}

func sealBrowserPublicationJournal(salt []byte, journal *browserPublicationJournal) {
	if journal == nil {
		return
	}
	journal.Integrity = ""
	journal.Integrity = browserAuthentication(salt, *journal)
}

func verifyBrowserPublicationJournal(salt []byte, journal browserPublicationJournal) bool {
	if journal.Schema != browserPublicationJournalSchema || (journal.Phase != browserPublicationPrepared && journal.Phase != browserPublicationRollback) || journal.NewHighWater == 0 || journal.NewRevision == 0 || journal.NewRevision != journal.OldRevision+1 || journal.NewCatalogGeneration == 0 || journal.NewCatalogGeneration > journal.NewHighWater || journal.NewCatalogDigest == "" || (journal.OldCatalogGeneration == 0) != (journal.OldCatalogDigest == "") || journal.OldCatalogGeneration > journal.OldHighWater {
		return false
	}
	switch journal.Kind {
	case browserPublicationRefresh:
		if journal.OldHighWater == math.MaxUint64 || journal.NewHighWater != journal.OldHighWater+1 || journal.NewCatalogGeneration != journal.NewHighWater {
			return false
		}
	case browserPublicationProbe:
		if journal.NewHighWater != journal.OldHighWater || journal.OldCatalogGeneration == 0 || journal.NewCatalogGeneration != journal.OldCatalogGeneration {
			return false
		}
	default:
		return false
	}
	actual := journal.Integrity
	journal.Integrity = ""
	return verifyBrowserAuthentication(salt, journal, actual)
}

func browserAuthentication(salt []byte, value any) string {
	body, err := json.Marshal(value)
	if err != nil || len(salt) < 16 {
		return ""
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifyBrowserAuthentication(salt []byte, value any, actual string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(actual)
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(browserAuthentication(salt, value))
	return err == nil && hmac.Equal(decoded, expected)
}

func browserDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func browserPublicationPoint(label string) {
	if browserPublicationFailpoint != nil {
		browserPublicationFailpoint(label)
	}
}

func marshalBrowserPrivateJSON(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func saveBrowserPrivateJSONLocked(ctx context.Context, stateDir, name string, value any) error {
	body, err := marshalBrowserPrivateJSON(value)
	if err != nil {
		return err
	}
	return state.WritePrivateFileAtomicLocked(ctx, stateDir, name, body)
}

func decodeStrictPrivateJSON(body []byte, target any) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("private JSON is not UTF-8")
	}
	duplicates := json.NewDecoder(bytes.NewReader(body))
	if err := consumeUniqueJSONValue(duplicates); err != nil {
		return err
	}
	if _, err := duplicates.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("private JSON has trailing data")
		}
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("private JSON has trailing value")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("private JSON object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("private JSON contains duplicate key")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("private JSON object is incomplete")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("private JSON array is incomplete")
		}
	default:
		return fmt.Errorf("private JSON delimiter is invalid")
	}
	return nil
}

func selectedBrowserIDLocked(stateDir string, catalog picker.BrowserCatalog) string {
	current, err := state.LoadCurrent(stateDir)
	if err != nil {
		return ""
	}
	if _, ok := catalog.Find(current.ServerID); ok {
		return current.ServerID
	}
	// Older automatic selections predate explicit browser IDs. Match only
	// against the private catalog; no endpoint or link enters the response.
	for _, record := range catalog.Servers {
		if record.Result.Link == current.Link && current.Link != "" {
			return record.ID
		}
	}
	return ""
}

func browserRecoveryPendingLocked(stateDir string) bool {
	pending, err := state.PendingTransactions(stateDir)
	return err != nil || len(pending) > 0
}

func browserCommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func boundedSeconds(seconds float64) float64 {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	if seconds > 60 {
		return 60
	}
	return seconds
}

func writeBrowserContextFailure(cmd *cobra.Command, ctx context.Context, generation uint64) error {
	if ctx != nil && ctx.Err() != nil {
		return writeBrowserFailure(cmd, "canceled", generation)
	}
	return writeBrowserFailure(cmd, "unavailable", generation)
}

func writeBrowserFailure(cmd *cobra.Command, status string, generation uint64) error {
	return writeBrowserResponse(cmd, picker.BrowserResponse{Schema: picker.BrowserSchema, Status: status, Generation: generation}, true)
}

func writeBrowserResponse(cmd *cobra.Command, response picker.BrowserResponse, failed bool) error {
	if response.Schema == "" {
		response.Schema = picker.BrowserSchema
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(response); err != nil {
		return safeJSONExit{status: "unavailable"}
	}
	if failed {
		return safeJSONExit{status: response.Status}
	}
	return nil
}
