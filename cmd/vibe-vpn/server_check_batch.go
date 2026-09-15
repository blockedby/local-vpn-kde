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
const serverCheckLimit = 1000

type checkProgressKey struct{}

type serverCheckProbe func(context.Context, config.Config, picker.NodeResult, string) picker.NodeResult

func checkNode(ctx context.Context, c config.Config, result picker.NodeResult, target string) picker.NodeResult {
	result.PingMS, result.PingStatus, result.Availability = 0, "failed", "untested"
	ping, err := (browserDependencies{tcpPing: true}).measure(ctx, c, result)
	if err != nil || ctx.Err() != nil {
		if emit, ok := ctx.Value(checkProgressKey{}).(func(picker.NodeResult)); ok {
			emit(result)
		}
		return result
	}
	result.PingMS = max(1, int(ping.Seconds*1000+.5))
	result.PingStatus = "ready"
	if emit, ok := ctx.Value(checkProgressKey{}).(func(picker.NodeResult)); ok {
		emit(result)
	}
	result.Availability = "failed"
	if _, err := (browserDependencies{targetURL: target}).measure(ctx, c, result); err == nil {
		result.Availability = "ready"
	}
	return result
}

// All sockets are reserved together to guarantee distinct ports within a batch.
// Each probe reuses the existing temporary sing-box lifecycle, never the active
// proxy's port or configuration. The sockets are released just before launch.
func checkNodes(ctx context.Context, c config.Config, records []picker.BrowserRecord, target string, probe serverCheckProbe, completed ...func(int, picker.NodeResult)) ([]picker.NodeResult, error) {
	if len(records) == 0 || len(records) > serverCheckLimit {
		return nil, fmt.Errorf("invalid batch size")
	}
	listeners := make([]net.Listener, 0, min(len(records), serverCheckWorkers))
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range min(len(records), serverCheckWorkers) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
	}
	configs := make([]config.Config, len(listeners))
	for i, l := range listeners {
		configs[i] = c
		configs[i].TestSocks = l.Addr().String()
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	results := make([]picker.NodeResult, len(records))
	// One snapshot, one pass. A worker owns its proxy port until it exits and
	// takes the next job immediately, independent of the other workers.
	jobs := make(chan int)
	var workers sync.WaitGroup
	for _, workerConfig := range configs {
		workers.Add(1)
		go func(c config.Config) {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				probeCtx := ctx
				if emit, ok := ctx.Value(checkProgressKey{}).(func(int, picker.NodeResult)); ok {
					probeCtx = context.WithValue(ctx, checkProgressKey{}, func(r picker.NodeResult) { emit(i, r) })
				}
				results[i] = probe(probeCtx, c, records[i].Result, target)
				if ctx.Err() == nil && len(completed) > 0 {
					completed[0](i, results[i])
				}
			}
		}(workerConfig)
	}
feed:
	for i := range records {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- i:
		}
	}
	close(jobs)
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
		stream, _ := cmd.Flags().GetBool("progress")
		return runBrowserCheckBatch(cmd, o, strings.Split(ids, ","), target, checkNode, stream)
	}}
	cmd.Flags().String("server-id", "", "one to 1000 opaque IDs separated by commas")
	cmd.Flags().String("url", "", "HTTPS availability target after successful TCP ping")
	cmd.Flags().Bool("progress", false, "stream provisional measurement evidence")
	cmd.Flags().Bool("json", false, "print bounded redacted results")
	root.AddCommand(cmd)
}

func runBrowserCheckBatch(cmd *cobra.Command, o *cliOptions, ids []string, target string, probe serverCheckProbe, stream ...bool) error {
	ctx, cancel := context.WithCancel(browserCommandContext(cmd))
	defer cancel()
	u, err := url.Parse(target)
	if len(ids) == 0 || len(ids) > serverCheckLimit || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(target) > 2048 {
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
	var outputMu sync.Mutex
	encoder := json.NewEncoder(cmd.OutOrStdout())
	emit := func(id, stage string, r picker.NodeResult) {
		if len(stream) == 0 || !stream[0] || ctx.Err() != nil {
			return
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		if err := encoder.Encode(picker.CheckProgress{Event: "server-check", ServerID: id, Stage: stage, PingStatus: r.PingStatus, LatencyMS: r.PingMS, Availability: r.Availability}); err != nil {
			cancel()
		}
	}
	// The worker's private record is never serialized; only its opaque ID and
	// finite measurement fields can cross the live progress channel.
	ctx = context.WithValue(ctx, checkProgressKey{}, func(i int, r picker.NodeResult) { emit(records[i].ID, "ping", r) })
	results, err := checkNodes(ctx, c, records, target, probe, func(i int, r picker.NodeResult) { emit(records[i].ID, "complete", r) })
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
