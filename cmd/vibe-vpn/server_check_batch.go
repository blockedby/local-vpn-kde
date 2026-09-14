package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/spf13/cobra"
)

const serverCheckWorkers = 5

type serverCheckProbe func(context.Context, config.Config, picker.NodeResult, string) picker.NodeResult

func checkNode(ctx context.Context, c config.Config, result picker.NodeResult, target string) picker.NodeResult {
	result.PingMS, result.PingStatus, result.Availability = 0, "failed", "untested"
	ping, err := (browserDependencies{tcpPing: true}).measure(ctx, c, result)
	if err != nil || ctx.Err() != nil {
		return result
	}
	result.PingMS = max(1, int(ping.Seconds*1000+.5))
	result.PingStatus = "ready"
	result.Availability = "failed"
	if _, err := (browserDependencies{targetURL: target}).measure(ctx, c, result); err == nil {
		result.Availability = "ready"
	}
	return result
}

// All sockets are reserved together to guarantee distinct ports within a batch.
// Each probe reuses the existing temporary sing-box lifecycle, never the active
// proxy's port or configuration. The sockets are released just before launch.
func checkNodes(ctx context.Context, c config.Config, records []picker.BrowserRecord, target string, probe serverCheckProbe) ([]picker.NodeResult, error) {
	if len(records) == 0 || len(records) > serverCheckWorkers {
		return nil, fmt.Errorf("invalid batch size")
	}
	listeners := make([]net.Listener, 0, len(records))
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range records {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
	}
	configs := make([]config.Config, len(records))
	for i, l := range listeners {
		configs[i] = c
		configs[i].TestSocks = l.Addr().String()
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	results := make([]picker.NodeResult, len(records))
	var workers sync.WaitGroup
	for i, record := range records {
		workers.Add(1)
		go func(i int, record picker.BrowserRecord) {
			defer workers.Done()
			if ctx.Err() == nil {
				results[i] = probe(ctx, configs[i], record.Result, target)
			}
		}(i, record)
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func addServerCheckBatchCommand(root *cobra.Command, o *cliOptions) {
	cmd := &cobra.Command{Use: "check-batch", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		ids, _ := cmd.Flags().GetString("server-id")
		target, _ := cmd.Flags().GetString("url")
		jsonOut, _ := cmd.Flags().GetBool("json")
		if !jsonOut {
			return fmt.Errorf("--json is required")
		}
		return runBrowserCheckBatch(cmd, o, strings.Split(ids, ","), target, checkNode)
	}}
	cmd.Flags().String("server-id", "", "one to five opaque IDs separated by commas")
	cmd.Flags().String("url", "", "HTTPS availability target after successful TCP ping")
	cmd.Flags().Bool("json", false, "print bounded redacted results")
	root.AddCommand(cmd)
}

func runBrowserCheckBatch(cmd *cobra.Command, o *cliOptions, ids []string, target string, probe serverCheckProbe) error {
	ctx := browserCommandContext(cmd)
	u, err := url.Parse(target)
	if len(ids) == 0 || len(ids) > serverCheckWorkers || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(target) > 2048 {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
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
	_ = lock.Close()
	if err != nil || !baseline.catalogVersion.Exists() {
		return writeBrowserFailure(cmd, "unavailable", 0)
	}
	records := make([]picker.BrowserRecord, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		record, ok := baseline.catalog.Find(id)
		if !ok || seen[id] {
			return writeBrowserFailure(cmd, "stale", baseline.catalog.Generation)
		}
		seen[id] = true
		records = append(records, record)
	}
	results, err := checkNodes(ctx, c, records, target, probe)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, baseline.catalog.Generation)
	}
	lock, err = state.AcquireLock(ctx, c.StateDir)
	if err != nil {
		return writeBrowserContextFailure(cmd, ctx, baseline.catalog.Generation)
	}
	defer lock.Close()
	if browserRecoveryPendingLocked(c.StateDir) {
		return writeBrowserFailure(cmd, "recovery_required", baseline.catalog.Generation)
	}
	if err := recoverBrowserPublicationLocked(c.StateDir); err != nil {
		return writeBrowserFailure(cmd, "unavailable", baseline.catalog.Generation)
	}
	current, err := captureBrowserPublicationBaselineLocked(c.StateDir)
	if err != nil || !current.catalogVersion.Equal(baseline.catalogVersion) || !current.generationVersion.Equal(baseline.generationVersion) || !current.commitVersion.Equal(baseline.commitVersion) || !current.saltVersion.Equal(baseline.saltVersion) {
		return writeBrowserFailure(cmd, "stale", baseline.catalog.Generation)
	}
	latest := current.catalog
	for i, record := range records {
		for j := range latest.Servers {
			if latest.Servers[j].ID == record.ID {
				// Only this pipeline's evidence changes; speed and selection stay intact.
				latest.Servers[j].Result.PingMS = results[i].PingMS
				latest.Servers[j].Result.PingStatus = results[i].PingStatus
				latest.Servers[j].Result.Availability = results[i].Availability
			}
		}
	}
	if current.generation.Revision == math.MaxUint64 {
		return writeBrowserFailure(cmd, "unavailable", latest.Generation)
	}
	picker.SealBrowserCatalog(current.salt, &latest)
	if err := publishBrowserCatalogLocked(ctx, c.StateDir, current, latest, current.generation.Generation, browserPublicationProbe); err != nil {
		return writeBrowserContextFailure(cmd, ctx, latest.Generation)
	}
	selected := selectedBrowserIDLocked(c.StateDir, latest)
	servers := make([]picker.BrowserServer, 0, len(ids))
	for _, id := range ids {
		r, _ := latest.Find(id)
		servers = append(servers, r.Safe(id == selected))
	}
	// Individual probe failures are row results, not a failed batch transport.
	return json.NewEncoder(cmd.OutOrStdout()).Encode(picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: latest.Generation, Servers: servers})
}
