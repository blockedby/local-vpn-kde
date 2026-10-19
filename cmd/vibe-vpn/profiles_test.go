package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/profiles"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"os"
	"path/filepath"
	"testing"
)

func profileCommand(t *testing.T, cfg string, r profiles.Request) profiles.Response {
	t.Helper()
	cmd := newRootCommand()
	b, _ := json.Marshal(r)
	cmd.SetIn(bytes.NewReader(b))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--config", cfg, "profiles"})
	if e := cmd.Execute(); e != nil {
		t.Fatal(e)
	}
	var v profiles.Response
	if e := json.Unmarshal(out.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestProfilesMigrateAndOpenWithoutChangingSelectedRuntime(t *testing.T) {
	dir, cfg, original := browserFixture(t)
	results := []picker.NodeResult{}
	for _, row := range original.Servers {
		r := row.Result
		if r.Security == "reality" {
			r.Link = "vless://credential-two@private-two.example:8443?type=grpc&security=tls#Paris"
		}
		results = append(results, r)
	}
	if e := state.SaveJSON(dir, browserCatalogFile, picker.NewBrowserCatalog(4, []byte("0123456789abcdef0123456789abcdef"), results)); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "sub"), []byte("https://example.invalid/unavailable"), 0600)
	selected := []byte(`{"name":"still active"}`)
	os.WriteFile(filepath.Join(dir, "current-node.json"), selected, 0600)
	first := profileCommand(t, cfg, profiles.Request{Action: "list"})
	if len(first.Items) != 1 || first.Items[0].Count != 2 {
		t.Fatal(first)
	}
	added := profileCommand(t, cfg, profiles.Request{Action: "add", Name: "VPS", Source: "hysteria2://fixture@192.0.2.1#VPS"})
	if len(added.Items) != 2 || added.Items[1].Count != 1 {
		t.Fatal(added)
	}
	profileCommand(t, cfg, profiles.Request{Action: "open", ID: first.Active})
	after, _ := os.ReadFile(filepath.Join(dir, "current-node.json"))
	if !bytes.Equal(selected, after) {
		t.Fatal("browsing modified runtime selection")
	}
	profileCommand(t, cfg, profiles.Request{Action: "open", ID: added.Active})
	c, e := loadConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	// The active direct profile must not fetch the unavailable subscription.
	results, e = loadProfileCandidates(context.Background(), c)
	if e != nil || len(results) != 3 {
		t.Fatal(e, len(results))
	}
	lock, e := state.AcquireLock(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	catalog, e := loadBrowserCatalogLocked(dir)
	if e != nil || len(catalog.Servers) != 3 {
		t.Fatal(e)
	}
	for _, row := range catalog.Servers {
		if row.Result.ProfileID == "" {
			t.Fatal("missing profile boundary")
		}
	}
}

func TestFailedProfileRefreshKeepsCachedServers(t *testing.T) {
	dir := t.TempDir()
	cfg := writeTestConfig(t, dir)
	c, e := loadConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	d := profiles.Document{Version: 1, Active: "profile_bad", Items: []profiles.Profile{{ID: "profile_bad", Name: "Offline", Kind: "subscription", Source: "https://127.0.0.1:1/unavailable", Cache: []string{"vless://fixture@example.invalid:443?security=tls"}}, {ID: "profile_vps", Name: "VPS", Kind: "server", Source: "hy2://fixture@192.0.2.1", Cache: []string{"hy2://fixture@192.0.2.1"}}}}
	lock, e := state.AcquireLock(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = profiles.SaveLocked(context.Background(), dir, d); e != nil {
		t.Fatal(e)
	}
	lock.Close()
	results, e := loadProfileCandidates(context.Background(), c)
	if e != nil || len(results) != 2 {
		t.Fatal(e, len(results))
	}
	lock, e = state.AcquireLock(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	after, e := profiles.LoadLocked(dir, c.SubscriptionFile)
	if e != nil {
		t.Fatal(e)
	}
	if !after.Items[0].Stale || after.Items[1].Stale || len(after.Items[0].Cache) != 1 {
		t.Fatal("cache was lost or other profile affected")
	}
}
