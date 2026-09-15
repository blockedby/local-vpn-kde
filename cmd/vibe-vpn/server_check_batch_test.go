package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	if _, err := checkNodes(context.Background(), config.Config{}, make([]picker.BrowserRecord, serverCheckLimit+1), "", nil); err == nil {
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

func TestCheckNodesRefillsWorkersWithoutWaitingForSlowPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	records := make([]picker.BrowserRecord, 12)
	for i := range records {
		records[i].Result.Host = fmt.Sprint(i)
	}
	firstFive := make(chan struct{})
	next := make(chan struct{})
	releaseSlow := make(chan struct{})
	finished := make(chan error, 1)
	var mu sync.Mutex
	seen := map[string]int{}
	ports := map[string]bool{}
	count, peak := 0, 0
	go func() {
		_, err := checkNodes(ctx, config.Config{}, records, "https://example.com", func(ctx context.Context, c config.Config, r picker.NodeResult, _ string) picker.NodeResult {
			mu.Lock()
			seen[r.Host]++
			if ports[c.TestSocks] {
				t.Error("concurrent probes share a port")
			}
			ports[c.TestSocks] = true
			count++
			peak = max(peak, count)
			if len(seen) == 5 {
				close(firstFive)
			}
			if r.Host == "5" {
				close(next)
			}
			mu.Unlock()
			select {
			case <-firstFive:
			case <-ctx.Done():
			}
			if r.Host == "0" {
				select {
				case <-releaseSlow:
				case <-ctx.Done():
				}
			}
			mu.Lock()
			count--
			delete(ports, c.TestSocks)
			mu.Unlock()
			return r
		})
		finished <- err
	}()
	select {
	case <-next:
	case <-ctx.Done():
		t.Fatal("sixth node waited for the slow first node")
	}
	close(releaseSlow)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if peak != 5 || len(seen) != len(records) {
		t.Fatalf("peak=%d checked=%d", peak, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("node %s checked %d times", id, n)
		}
	}
}

func TestCheckBatchStreamsPingBeforeSiteAndBeforeSlowPeer(t *testing.T) {
	_, cfg, catalog := browserFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	release := make(chan struct{})
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(writer)
	done := make(chan error, 1)
	ids := []string{catalog.Servers[0].ID, catalog.Servers[1].ID}
	go func() {
		defer writer.Close()
		done <- runBrowserCheckBatch(cmd, &cliOptions{configPath: cfg}, ids, "https://example.com", func(ctx context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
			if r.Link == catalog.Servers[0].Result.Link {
				select {
				case <-release:
				case <-ctx.Done():
				}
				r.PingStatus = "failed"
				r.Availability = "untested"
				r.PingMS = 0
				return r
			}
			r.PingStatus = "ready"
			r.PingMS = 31
			r.Availability = "untested"
			ctx.Value(checkProgressKey{}).(func(picker.NodeResult))(r)
			r.Availability = "ready"
			return r
		}, true)
	}()
	dec := json.NewDecoder(reader)
	for _, stage := range []string{"ping", "complete"} {
		var p picker.CheckProgress
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.ServerID != ids[1] || p.Stage != stage || p.LatencyMS != 31 {
			t.Fatalf("wrong event: %+v", p)
		}
		if stage == "ping" && p.Availability != "untested" {
			t.Fatal("ping waited for site")
		}
	}
	close(release)
	var complete picker.CheckProgress
	if err := dec.Decode(&complete); err != nil || complete.ServerID != ids[0] {
		t.Fatal("slow peer result lost", err)
	}
	var final picker.BrowserResponse
	if err := dec.Decode(&final); err != nil || len(final.Servers) != 2 {
		t.Fatal("final catalog lost", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
