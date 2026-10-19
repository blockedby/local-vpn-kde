package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"github.com/spf13/cobra"
)

type speedStageKey struct{}

func speedBatchProgress(id, stage string, r picker.NodeResult) picker.CheckProgress {
	safe := (picker.BrowserRecord{ID: id, Result: r}).Safe(false)
	p := picker.CheckProgress{Event: "server-check", ServerID: id, Stage: stage, PingStatus: r.PingStatus, LatencyMS: r.PingMS, Availability: r.Availability}
	if stage == "complete" {
		p.Status = safe.Status
		if safe.DownloadMbps != nil {
			p.DownloadMbps = *safe.DownloadMbps
		}
		if safe.DownloadSeconds != nil {
			p.DownloadSeconds = *safe.DownloadSeconds
		}
		if safe.DownloadedBytes != nil {
			p.DownloadedBytes = *safe.DownloadedBytes
		}
	}
	return p
}

func addServerSpeedBatchCommand(root *cobra.Command, o *cliOptions) {
	cmd := &cobra.Command{Use: "speed-batch", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		ids, _ := cmd.Flags().GetString("server-id")
		target, _ := cmd.Flags().GetString("url")
		jsonOut, _ := cmd.Flags().GetBool("json")
		if !jsonOut {
			return fmt.Errorf("--json is required")
		}
		stream, _ := cmd.Flags().GetBool("progress")
		// Preparation is bounded and concurrent; throughput tests never overlap.
		downloadSlot := make(chan struct{}, 1)
		probe := func(ctx context.Context, c config.Config, r picker.NodeResult, target string) picker.NodeResult {
			return speedNode(ctx, c, r, target, downloadSlot)
		}
		return runBrowserSpeedBatch(cmd, o, strings.Split(ids, ","), target, probe, stream)
	}}
	cmd.Flags().String("server-id", "", "one to 1000 opaque IDs separated by commas")
	cmd.Flags().String("url", "", "HTTPS availability target")
	cmd.Flags().Bool("progress", false, "stream persisted measurement evidence")
	cmd.Flags().Bool("json", false, "print bounded redacted results")
	root.AddCommand(cmd)
}

// Reuse one isolated proxy for website availability and download. Failed website
// access is independent of throughput: only failed reachability skips the test.
type speedProbeDependencies struct {
	ping     func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error)
	proxy    func(context.Context, config.Config, vless.Node, func() error) error
	site     func(context.Context, string, string, time.Duration) error
	download func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error)
}

func speedNode(ctx context.Context, c config.Config, r picker.NodeResult, target string, slot chan struct{}) picker.NodeResult {
	return speedNodeUsing(ctx, c, r, target, slot, speedProbeDependencies{
		ping: func(ctx context.Context, c config.Config, r picker.NodeResult) (nettest.Result, error) {
			return (browserDependencies{tcpPing: true}).measure(ctx, c, r)
		},
		proxy: withSpeedProxy, site: nettest.CheckAvailability, download: nettest.DownloadForContext,
	})
}

func speedNodeUsing(ctx context.Context, c config.Config, r picker.NodeResult, target string, slot chan struct{}, deps speedProbeDependencies) picker.NodeResult {
	r.PingMS, r.PingStatus, r.Availability = 0, "failed", "untested"
	r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes = false, "probe failed", 0, 0, 0
	emitPing := func() {
		if emit, ok := ctx.Value(checkProgressKey{}).(func(picker.NodeResult)); ok {
			emit(r)
		}
	}
	if r.Network != "quic" {
		ping, err := deps.ping(ctx, c, r)
		if err != nil || ctx.Err() != nil {
			emitPing()
			return r
		}
		r.PingMS, r.PingStatus = max(1, int(ping.Seconds*1000+.5)), "ready"
		emitPing()
	}
	n := vless.Node{Link: r.Link, Name: r.Name, Host: r.Host, Port: r.Port, Network: r.Network, Security: r.Security, Outbound: r.Outbound}
	err := deps.proxy(ctx, c, n, func() error {
		if r.Network == "quic" {
			started := time.Now()
			pingErr := deps.site(ctx, c.TestSocks, c.TestURL, 3*time.Second)
			if pingErr != nil {
				emitPing()
				return pingErr
			}
			r.PingMS, r.PingStatus = max(1, int(time.Since(started).Milliseconds())), "ready"
			emitPing()
		}
		r.Availability = "failed"
		if siteErr := deps.site(ctx, c.TestSocks, target, 3*time.Second); siteErr == nil {
			r.Availability = "ready"
		}
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-slot }()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if emit, ok := ctx.Value(speedStageKey{}).(func(string, picker.NodeResult)); ok {
			emit(r.ServerID, r)
		}
		measured, downloadErr := deps.download(ctx, c.TestSocks, c.TestURL, 3*time.Second, time.Duration(c.TimeoutSeconds)*time.Second)
		if downloadErr == nil {
			r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes = true, "", measured.Mbps, boundedSeconds(measured.Seconds), measured.Bytes
		}
		return downloadErr
	})
	if err != nil {
		r.OK = false
	}
	return r
}

func runBrowserSpeedBatch(cmd *cobra.Command, o *cliOptions, ids []string, target string, probe serverCheckProbe, stream ...bool) error {
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
		record.Result.ServerID = record.ID
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
		if err := encoder.Encode(speedBatchProgress(id, stage, r)); err != nil {
			cancel()
		}
	}
	// The worker's private record is never serialized; only its opaque ID and
	// finite measurement fields can cross the live progress channel.
	ctx = context.WithValue(ctx, checkStartedKey{}, func(i int) {
		emit(records[i].ID, "start", picker.NodeResult{PingStatus: "untested", Availability: "untested"})
	})
	ctx = context.WithValue(ctx, speedStageKey{}, func(id string, r picker.NodeResult) { emit(id, "speed", r) })
	ctx = context.WithValue(ctx, checkProgressKey{}, func(i int, r picker.NodeResult) { emit(records[i].ID, "ping", r) })
	// Each completion is durably published before its progress event. Cancellation
	// retains completed rows, while a concurrent catalog refresh stops this pass.
	var publicationMu sync.Mutex
	var publicationErr error
	publish := func(i int, r picker.NodeResult) {
		publicationMu.Lock()
		defer publicationMu.Unlock()
		if publicationErr != nil || ctx.Err() != nil {
			return
		}
		lock, lockErr := state.AcquireLock(ctx, c.StateDir)
		if lockErr != nil {
			publicationErr = lockErr
			cancel()
			return
		}
		defer lock.Close()
		if browserRecoveryPendingLocked(c.StateDir) {
			publicationErr = fmt.Errorf("recovery_required")
			cancel()
			return
		}
		if recoverErr := recoverBrowserPublicationLocked(c.StateDir); recoverErr != nil {
			publicationErr = recoverErr
			cancel()
			return
		}
		current, captureErr := captureBrowserPublicationBaselineLocked(c.StateDir)
		if captureErr != nil {
			publicationErr = captureErr
			cancel()
			return
		}
		if !current.catalogVersion.Equal(baseline.catalogVersion) || !current.generationVersion.Equal(baseline.generationVersion) || !current.commitVersion.Equal(baseline.commitVersion) || !current.saltVersion.Equal(baseline.saltVersion) {
			publicationErr = fmt.Errorf("stale")
			cancel()
			return
		}
		latest := current.catalog
		for j := range latest.Servers {
			if latest.Servers[j].ID == records[i].ID {
				row := &latest.Servers[j].Result
				row.PingMS, row.PingStatus, row.Availability = r.PingMS, r.PingStatus, r.Availability
				row.OK, row.Error, row.Mbps, row.Seconds, row.Bytes = r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes
			}
		}
		if current.generation.Revision == math.MaxUint64 {
			publicationErr = fmt.Errorf("unavailable")
			cancel()
			return
		}
		picker.SealBrowserCatalog(current.salt, &latest)
		if publishErr := publishBrowserCatalogLocked(ctx, c.StateDir, current, latest, current.generation.Generation, browserPublicationProbe); publishErr != nil {
			publicationErr = publishErr
			cancel()
			return
		}
		baseline, publicationErr = captureBrowserPublicationBaselineLocked(c.StateDir)
		if publicationErr != nil {
			cancel()
			return
		}
		emit(records[i].ID, "complete", r)
	}
	_, err = checkNodes(ctx, c, records, target, probe, publish)
	if publicationErr != nil {
		reason := publicationErr.Error()
		if reason != "stale" && reason != "recovery_required" {
			reason = "unavailable"
		}
		return writeBrowserFailure(cmd, reason, baseline.catalog.Generation)
	}
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
	selected := selectedBrowserIDLocked(c.StateDir, latest)
	servers := make([]picker.BrowserServer, 0, len(ids))
	for _, id := range ids {
		r, _ := latest.Find(id)
		servers = append(servers, r.Safe(id == selected))
	}
	// Individual probe failures are row results, not a failed batch transport.
	return json.NewEncoder(cmd.OutOrStdout()).Encode(picker.BrowserResponse{Schema: picker.BrowserSchema, Status: "ok", Generation: latest.Generation, Servers: servers})
}

// A synchronous context-aware callback keeps all per-node state owned by its
// worker. The proxy is joined before completion can be published or a port reused.
func withSpeedProxy(ctx context.Context, c config.Config, n vless.Node, check func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tcpOpen(c.TestSocks, 200*time.Millisecond) {
		return fmt.Errorf("test SOCKS address became busy")
	}
	backend, err := tempBenchmarkBackend(c, n)
	if err != nil {
		return err
	}
	defer os.Remove(backend.configPath)
	backendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(backendCtx, backend.bin, backend.args...)
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { cancel(); _ = command.Process.Kill(); _ = command.Wait() }()
	if err := waitTCPContext(backendCtx, c.TestSocks, 3*time.Second); err != nil {
		return err
	}
	return check()
}
