package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/nettest"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"github.com/spf13/cobra"
)

func TestBenchmarkCountryCollector(t *testing.T) {
	for _, tc := range []struct{ name, lines, want string }{
		{"one", "DEBUG[0001] [12 0ms] dns: lookup succeed for host.example: 1.1.1.1\n", "AU"},
		{"same country", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1 1.0.0.1\n", "AU"},
		{"mixed countries", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1 2001:200::1\n", ""},
		{"private address", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1 127.0.0.1\n", ""},
		{"other host", "DEBUG[0001] dns: lookup succeed for other.example: 1.1.1.1\n", ""},
		{"unrelated log", "DEBUG[0001] outbound: lookup succeed for host.example: 1.1.1.1\n", ""},
		{"different lookups", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1\nDEBUG[0001] dns: lookup succeed for host.example: 2001:200::1\n", ""},
		{"oversized", strings.Repeat("x", 17000) + " dns: lookup succeed for host.example: 1.1.1.1\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBenchmarkCountryCollector("host.example")
			for _, b := range []byte(tc.lines) {
				_, _ = c.Write([]byte{b})
			}
			if got := c.country(); got != tc.want {
				t.Fatalf("country = %q, want %q", got, tc.want)
			}
		})
	}
	if newBenchmarkCountryCollector("1.1.1.1") != nil || newBenchmarkCountryCollector("bad\nname") != nil {
		t.Fatal("invalid host accepted")
	}
}

func TestMeasureProxyCountryPreservesMeasurement(t *testing.T) {
	expected := nettest.Result{Bytes: 123, Mbps: 4, Seconds: 5}
	for _, tc := range []struct {
		name, log, country string
		err                error
	}{
		{"lookup", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1\n", "AU", nil},
		{"no lookup", "", "", nil},
		{"failed measurement", "DEBUG[0001] dns: lookup succeed for host.example: 1.1.1.1\n", "", errors.New("measurement failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := measureProxyCountry(context.Background(), "host.example", func(ctx context.Context) (nettest.Result, error) {
				collector := ctx.Value(benchmarkCountryKey{}).(*benchmarkCountryCollector)
				_, _ = collector.Write([]byte(tc.log))
				return expected, tc.err
			})
			want := expected
			want.Country = tc.country
			want.CountryObserved = tc.log != ""
			if tc.err != nil {
				want.Country = "AU"
			}
			if got != want || err != tc.err {
				t.Fatalf("result = %+v, err = %v; want %+v, %v", got, err, want, tc.err)
			}
		})
	}
}

func TestProxySpeedWithoutLookupPreservesResult(t *testing.T) {
	deps := speedProbeDependencies{
		proxy: func(_ context.Context, _ config.Config, _ vless.Node, check func() error) error { return check() },
		site:  func(context.Context, string, string, time.Duration) error { return nil },
		download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
			return nettest.Result{Bytes: 123, Seconds: 1, Mbps: 4}, nil
		},
	}
	r := speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{Host: "host.example", Network: "quic"}, "https://example.com", make(chan struct{}, 1), deps)
	if r.Country != "" || !r.OK || r.PingStatus != "ready" || r.Availability != "ready" || r.Bytes != 123 || r.Mbps != 4 || r.Seconds != 1 {
		t.Fatalf("unexpected measurement result: %+v", r)
	}
}

func TestBenchmarkCountryLogConfig(t *testing.T) {
	n := vless.Node{Outbound: map[string]any{"type": "socks", "server": "host.example", "server_port": 443}}
	for _, enabled := range []bool{false, true} {
		b, err := singBoxTempConfig(n, "127.0.0.1:1080", enabled)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Log struct {
				Level string `json:"level"`
			} `json:"log"`
		}
		if err := json.Unmarshal(b, &config); err != nil {
			t.Fatal(err)
		}
		want := "warn"
		if enabled {
			want = "debug"
		}
		if config.Log.Level != want {
			t.Fatalf("country log enabled=%v: level %q", enabled, config.Log.Level)
		}
	}
}

func TestProxyCountrySpeedBatchPersistsAndRemainsRedacted(t *testing.T) {
	dir, cfg, catalog := browserFixture(t)
	id := catalog.Servers[0].ID
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	probe := func(ctx context.Context, c config.Config, r picker.NodeResult, target string) picker.NodeResult {
		r.Network = "quic"
		deps := speedProbeDependencies{
			proxy: func(ctx context.Context, _ config.Config, _ vless.Node, check func() error) error {
				collector := ctx.Value(benchmarkCountryKey{}).(*benchmarkCountryCollector)
				_, _ = collector.Write([]byte("DEBUG[0001] dns: lookup succeed for " + r.Host + ": 1.1.1.1\n"))
				return check()
			},
			site: func(context.Context, string, string, time.Duration) error { return nil },
			download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
				return nettest.Result{Bytes: 123, Seconds: 1, Mbps: 4}, nil
			},
		}
		return speedNodeUsing(ctx, c, r, target, make(chan struct{}, 1), deps)
	}
	if err := runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, []string{id}, "https://example.com/", probe); err != nil {
		t.Fatal(err)
	}
	assertSafeBrowserJSON(t, out.String())
	for _, secret := range []string{"1.1.1.1", catalog.Servers[0].Result.Host} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("CLI output exposed %q", secret)
		}
	}
	var response picker.BrowserResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || len(response.Servers) != 1 || response.Servers[0].Country != "AU" {
		t.Fatalf("missing country: %s (%v)", out.String(), err)
	}
	stored, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := stored.Find(id)
	if row.Result.Country != "AU" || !row.Result.OK || row.Result.Bytes != 123 {
		t.Fatalf("lost evidence: %+v", row.Result)
	}
	results := make([]picker.NodeResult, 0, len(stored.Servers))
	for _, row := range stored.Servers {
		fresh := row.Result
		fresh.Country = ""
		results = append(results, fresh)
	}
	out.Reset()
	refresh := browserDependencies{listOnly: true, load: func(context.Context, config.Config) ([]picker.NodeResult, error) { return results, nil }}
	if err := runBrowserTestAll(cmd, &cliOptions{configPath: cfg}, refresh); err != nil {
		t.Fatal(err)
	}
	assertSafeBrowserJSON(t, out.String())
	if strings.Contains(out.String(), "1.1.1.1") || strings.Contains(out.String(), catalog.Servers[0].Result.Host) {
		t.Fatal("refresh output exposed address or endpoint")
	}
	stored, err = loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = stored.Find(id)
	if row.Result.Country != "AU" {
		t.Fatalf("country after refresh = %q", row.Result.Country)
	}
}

func TestAmbiguousCountryClearsPreviousMeasurement(t *testing.T) {
	dir, cfg, cat := browserFixture(t)
	id := cat.Servers[0].ID
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	known := browserDependencies{probe: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
		return nettest.Result{Country: "JP"}, nil
	}}
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, id, known); err != nil {
		t.Fatal(err)
	}
	noObservation := browserDependencies{probe: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
		return nettest.Result{}, nil
	}}
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, id, noObservation); err != nil {
		t.Fatal(err)
	}
	previous, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := previous.Find(id)
	if before.Result.Country != "JP" {
		t.Fatalf("no observation erased %q", before.Result.Country)
	}
	ambiguous := browserDependencies{probe: func(ctx context.Context, _ config.Config, _ picker.NodeResult) (nettest.Result, error) {
		return measureProxyCountry(ctx, "host.example", func(ctx context.Context) (nettest.Result, error) {
			c := ctx.Value(benchmarkCountryKey{}).(*benchmarkCountryCollector)
			_, _ = c.Write([]byte("DEBUG dns: lookup succeed for host.example: 1.1.1.1 2001:200::1\n"))
			return nettest.Result{}, nil
		})
	}}
	if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, id, ambiguous); err != nil {
		t.Fatal(err)
	}
	stored, err := loadBrowserCatalogLocked(dir)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := stored.Find(id)
	if row.Result.Country != "" {
		t.Fatalf("ambiguous country retained %q", row.Result.Country)
	}
}

func TestSpeedAmbiguousCountryClearsPrevious(t *testing.T) {
	deps := speedProbeDependencies{
		proxy: func(ctx context.Context, _ config.Config, _ vless.Node, check func() error) error {
			c := ctx.Value(benchmarkCountryKey{}).(*benchmarkCountryCollector)
			_, _ = c.Write([]byte("DEBUG dns: lookup succeed for host.example: 1.1.1.1 2001:200::1\n"))
			return check()
		},
		site: func(context.Context, string, string, time.Duration) error { return nil },
		download: func(context.Context, string, string, time.Duration, time.Duration) (nettest.Result, error) {
			return nettest.Result{}, nil
		},
	}
	r := speedNodeUsing(context.Background(), config.Config{}, picker.NodeResult{Host: "host.example", Network: "quic", Country: "JP"}, "https://example.com", make(chan struct{}, 1), deps)
	if r.Country != "" {
		t.Fatalf("ambiguous speed country retained %q", r.Country)
	}
}

func TestBatchPublishesClearedCountry(t *testing.T) {
	for _, speed := range []bool{false, true} {
		t.Run(fmt.Sprint(speed), func(t *testing.T) {
			dir, cfg, cat := browserFixture(t)
			id := cat.Servers[0].ID
			cmd := &cobra.Command{}
			cmd.SetOut(&bytes.Buffer{})
			if err := runBrowserPing(cmd, &cliOptions{configPath: cfg}, id, browserDependencies{probe: func(context.Context, config.Config, picker.NodeResult) (nettest.Result, error) {
				return nettest.Result{Country: "JP"}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			probe := func(_ context.Context, _ config.Config, r picker.NodeResult, _ string) picker.NodeResult {
				r.Country = ""
				return r
			}
			var err error
			if speed {
				err = runBrowserSpeedBatch(cmd, &cliOptions{configPath: cfg}, []string{id}, "https://example.com", probe)
			} else {
				err = runBrowserCheckBatch(cmd, &cliOptions{configPath: cfg}, []string{id}, "https://example.com", probe)
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, err := loadBrowserCatalogLocked(dir)
			if err != nil {
				t.Fatal(err)
			}
			row, _ := stored.Find(id)
			if row.Result.Country != "" {
				t.Fatalf("batch retained %q", row.Result.Country)
			}
		})
	}
}
