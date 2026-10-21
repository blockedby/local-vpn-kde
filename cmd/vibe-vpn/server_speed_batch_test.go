package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"github.com/spf13/cobra"
)

func TestSpeedBatchPublishesAllMeasurementEvidence(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	ids := []string{catalog.Servers[0].ID, catalog.Servers[1].ID}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, ids, "https://example.com/", func(_ context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		r.PingMS, r.PingStatus, r.Availability = 25, "ready", "failed"
		r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes = true, "", 80, 3, 30000000
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSafeBrowserJSON(t, out.String())
	var reply picker.BrowserResponse
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || len(reply.Servers) != 2 {
		t.Fatal("missing response", err)
	}
	kept, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range kept.Servers {
		if r.Result.Mbps != 80 || r.Result.PingMS != 25 || r.Result.Availability != "failed" || !r.Result.OK {
			t.Fatal("missing persisted evidence")
		}
	}
}

func TestSpeedBatchCancellationKeepsCompletedRows(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(ctx)
	completed := make(chan struct{})
	// The second worker waits until the first completion has been durably emitted.
	var once sync.Once
	cmd.SetOut(&speedCompletionWriter{Buffer: &out, complete: func() { once.Do(func() { close(completed) }) }})
	err := runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, []string{catalog.Servers[0].ID, catalog.Servers[1].ID}, "https://example.com/", func(ctx context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		if r.ServerID == catalog.Servers[1].ID {
			<-completed
			cancel()
			<-ctx.Done()
			return r
		}
		r.PingMS, r.PingStatus, r.Availability = 25, "ready", "ready"
		r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes = true, "", 80, 3, 30000000
		return r
	}, true)
	if err == nil {
		t.Fatal("cancelled pass succeeded")
	}
	kept, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := kept.Find(catalog.Servers[0].ID)
	second, _ := kept.Find(catalog.Servers[1].ID)
	if first.Result.Mbps != 80 || second.Result.Mbps != catalog.Servers[1].Result.Mbps {
		t.Fatal("completed row lost or incomplete row persisted")
	}
}

type speedCompletionWriter struct {
	*bytes.Buffer
	complete func()
}

func (w *speedCompletionWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte(`"stage":"complete"`)) {
		w.complete()
	}
	return n, err
}

func TestSpeedNodePreparesConcurrentlyButSerializesDownloads(t *testing.T) {
	var proxies, downloads, active, maxActive atomic.Int32
	prepared := make(chan struct{}, 2)
	release := make(chan struct{})
	deps := speedProbeDependencies{
		ping: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
			return nettest.Result{Seconds: .02}, nil
		},
		proxy: func(_ context.Context, _ config.Config, _ vless.Node, check func() error) error {
			proxies.Add(1)
			return check()
		},
		site: func(context.Context, string, string, time.Duration) error {
			prepared <- struct{}{}
			<-release
			return errors.New("site blocked")
		},
		download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
			downloads.Add(1)
			n := active.Add(1)
			for prev := maxActive.Load(); n > prev && !maxActive.CompareAndSwap(prev, n); prev = maxActive.Load() {
			}
			time.Sleep(15 * time.Millisecond)
			active.Add(-1)
			return nettest.Result{Mbps: 80, Bytes: 30000000, Seconds: 3}, nil
		},
	}
	slot := make(chan struct{}, 1)
	var wg sync.WaitGroup
	results := make([]picker.NodeResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{}, "https://example.com", slot, deps)
		}(i)
	}
	for range results {
		select {
		case <-prepared:
		case <-time.After(time.Second):
			t.Fatal("preparation did not overlap")
		}
	}
	close(release)
	wg.Wait()
	if proxies.Load() != 2 || downloads.Load() != 2 || maxActive.Load() != 1 {
		t.Fatal("proxies not reused or downloads overlap")
	}
	for _, r := range results {
		if !r.OK || r.Availability != "failed" || r.PingStatus != "ready" {
			t.Fatal("site failure skipped valid speed result")
		}
	}
}

func TestSpeedNodeSkipsUnreachableWithoutProxy(t *testing.T) {
	r := speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{OK: true, Mbps: 99}, "https://example.com", make(chan struct{}, 1), speedProbeDependencies{ping: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
		return nettest.Result{}, errors.New("unreachable")
	}})
	if r.OK || r.Mbps != 0 || r.PingStatus != "failed" || r.Availability != "untested" {
		t.Fatal("unreachable result retained cached metrics")
	}
}

func TestSpeedBatchRejectsConcurrentCatalogChange(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	var once sync.Once
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, []string{catalog.Servers[0].ID, catalog.Servers[1].ID}, "https://example.com/", func(_ context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		once.Do(func() {
			publishBrowserCatalogForTest(t, dir, browserPublicationRefresh, func(*picker.BrowserCatalog) {})
		})
		r.OK, r.PingStatus, r.Availability, r.Mbps = true, "ready", "ready", 99
		return r
	})
	if err == nil || !bytes.Contains(out.Bytes(), []byte(`"status":"stale"`)) {
		t.Fatal("concurrent catalog publication overwritten")
	}
}

func TestSpeedNodeCancellationWhileWaitingForDownload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	siteDone := make(chan struct{})
	var downloads atomic.Int32
	deps := speedProbeDependencies{
		ping: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
			return nettest.Result{Seconds: .02}, nil
		},
		proxy: func(_ context.Context, _ config.Config, _ vless.Node, check func() error) error { return check() },
		site:  func(context.Context, string, string, time.Duration) error { close(siteDone); return nil },
		download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
			downloads.Add(1)
			return nettest.Result{}, nil
		},
	}
	finished := make(chan picker.NodeResult, 1)
	go func() {
		finished <- speedNodeUsing(ctx, config.Config{}, picker.NodeResult{}, "https://example.com", slot, deps)
	}()
	<-siteDone
	cancel()
	select {
	case r := <-finished:
		if r.OK || downloads.Load() != 0 {
			t.Fatal("cancelled waiter downloaded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not exit")
	}
}

func TestSpeedBatchSyntheticTiming(t *testing.T) {
	// Artificial local work only; this measures scheduling, not network throughput.
	const nodes = 4
	var active, maxActive atomic.Int32
	deps := speedProbeDependencies{
		ping: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
			return nettest.Result{Seconds: .02}, nil
		},
		proxy: func(_ context.Context, _ config.Config, _ vless.Node, check func() error) error { return check() },
		site: func(context.Context, string, string, time.Duration) error {
			time.Sleep(80 * time.Millisecond)
			return nil
		},
		download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
			n := active.Add(1)
			for prev := maxActive.Load(); n > prev && !maxActive.CompareAndSwap(prev, n); prev = maxActive.Load() {
			}
			time.Sleep(30 * time.Millisecond)
			active.Add(-1)
			return nettest.Result{Mbps: 80, Bytes: 30000000, Seconds: 3}, nil
		},
	}
	slot := make(chan struct{}, 1)
	started := time.Now()
	for range nodes {
		speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{}, "https://example.com", slot, deps)
	}
	serial := time.Since(started)
	started = time.Now()
	var wg sync.WaitGroup
	for range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{}, "https://example.com", slot, deps)
		}()
	}
	wg.Wait()
	pipeline := time.Since(started)
	if maxActive.Load() != 1 {
		t.Fatal("throughput downloads overlapped")
	}
	t.Logf("synthetic %d nodes, preparation80ms + download30ms: serial=%s pipelined=%s ratio=%.2f max_active_downloads=%d", nodes, serial.Round(time.Millisecond), pipeline.Round(time.Millisecond), float64(pipeline)/float64(serial), maxActive.Load())
}

func TestSpeedBatchProgressPassesSharedStreamValidator(t *testing.T) {
	_, cfg, catalog := browserFixture(t)
	ids := []string{catalog.Servers[0].ID, catalog.Servers[1].ID}
	var final bytes.Buffer
	stages := map[string][]string{}
	stream := picker.NewCheckStream(ids, &final, func(p picker.CheckProgress) error {
		stages[p.ServerID] = append(stages[p.ServerID], p.Stage)
		if p.Stage == "complete" && (p.DownloadMbps != 80 || p.Status != "ready") {
			t.Error("missing completion metrics")
		}
		return nil
	})
	cmd := &cobra.Command{}
	cmd.SetOut(stream)
	err := runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, ids, "https://example.com/", func(ctx context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		r.PingMS, r.PingStatus, r.Availability = 25, "ready", "untested"
		ctx.Value(checkProgressKey{}).(func(picker.NodeResult))(r)
		r.Availability = "ready"
		ctx.Value(speedStageKey{}).(func(string, picker.NodeResult))(r.ServerID, r)
		r.OK, r.Error, r.Mbps, r.Seconds, r.Bytes = true, "", 80, 3, 30000000
		return r
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Finish(); err != nil {
		t.Fatal("invalid progress stream", err)
	}
	for _, id := range ids {
		if got := stages[id]; len(got) != 4 || got[0] != "start" || got[1] != "ping" || got[2] != "speed" || got[3] != "complete" {
			t.Fatal("missing ordered stages", got)
		}
	}
	assertSafeBrowserJSON(t, final.String())
}
