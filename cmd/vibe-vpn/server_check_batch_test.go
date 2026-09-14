package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/spf13/cobra"
)

func TestCheckNodesRunsFiveIsolatedWorkersAndJoinsCancellation(t *testing.T) {
	records := make([]picker.BrowserRecord, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 5)
	finished := make(chan error, 1)
	var exited atomic.Int32
	go func() {
		_, err := checkNodes(ctx, config.Config{}, records, "https://example.com/", func(ctx context.Context, c config.Config, r picker.NodeResult, _ string) picker.NodeResult {
			started <- c.TestSocks
			<-ctx.Done()
			exited.Add(1)
			return r
		})
		finished <- err
	}()
	ports := map[string]bool{}
	for range records {
		select {
		case port := <-started:
			if ports[port] {
				t.Fatal("shared proxy port")
			}
			ports[port] = true
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not start concurrently")
		}
	}
	cancel()
	if err := <-finished; err == nil || exited.Load() != 5 {
		t.Fatal("cancellation returned before all probes stopped")
	}
	if _, err := checkNodes(context.Background(), config.Config{}, make([]picker.BrowserRecord, 6), "", nil); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}

func TestCheckNodeSkipsSiteAfterFailedPing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	result := checkNode(context.Background(), config.Config{}, picker.NodeResult{Host: "127.0.0.1", Port: port, Availability: "ready"}, "https://example.invalid/")
	if result.PingStatus != "failed" || result.Availability != "untested" {
		t.Fatal("site not skipped or stale result retained")
	}
}

func TestCheckBatchPublishesOnlyPingAndSiteEvidence(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	ids := []string{catalog.Servers[0].ID, catalog.Servers[1].ID}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrowserCheckBatch(cmd, &cliOptions{configPath: cfg}, ids, "https://example.com/", func(_ context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		r.PingMS = 25
		r.PingStatus = "ready"
		r.Availability = "ready"
		r.Mbps = 9999
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSafeBrowserJSON(t, out.String())
	var reply picker.BrowserResponse
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || len(reply.Servers) != 2 {
		t.Fatal("missing batch result")
	}
	kept, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range kept.Servers {
		if r.Result.PingMS != 25 || r.Result.Availability != "ready" || r.Result.Mbps != catalog.Servers[i].Result.Mbps {
			t.Fatal("batch overwrote unrelated fields")
		}
	}
}

func TestCheckBatchRejectsConcurrentCatalogChange(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	var once sync.Once
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrowserCheckBatch(cmd, &cliOptions{configPath: cfg}, []string{catalog.Servers[0].ID, catalog.Servers[1].ID}, "https://example.com/", func(_ context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
		once.Do(func() {
			publishBrowserCatalogForTest(t, dir, browserPublicationRefresh, func(*picker.BrowserCatalog) {})
		})
		r.PingMS = 25
		r.PingStatus = "ready"
		return r
	})
	if err == nil || !bytes.Contains(out.Bytes(), []byte(`"status":"stale"`)) {
		t.Fatal("stale publication accepted")
	}
}
