package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const cachedLink = "vless://fixture@example.invalid:443?security=tls#Old%20Server"
const freshLink = "hy2://fixture@fresh.invalid#New%20Server"

func cacheDocument() Document {
	return Document{Version: 1, Active: "profile_one", Items: []Profile{
		{ID: "profile_one", Name: "One", Kind: "subscription", Source: "https://example.invalid/private-token", Cache: []string{cachedLink}, Stale: true},
		{ID: "profile_two", Name: "Two", Kind: "server", Source: freshLink, Cache: []string{freshLink}},
	}}
}
func TestRefreshCacheRetainsOtherProfilesAndRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links []string
		err   error
		stale bool
	}{
		{"success", []string{"unsupported", freshLink}, nil, false},
		{"network error", []string{freshLink}, errors.New("unavailable"), true},
		{"empty", nil, nil, true},
		{"invalid links", []string{"hy2://missing-password", "vless://invalid"}, nil, true},
		{"profile limit", makeLinks(MaxCachedServers + 1), nil, true},
		{"catalog limit", makeLinks(MaxCachedServers), nil, true},
		{"catalog boundary", makeLinks(MaxCachedServers - 1), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := cacheDocument()
			before := d.Items[1]
			active, _ := d.ActiveProfile()
			if err := d.RefreshCache(active, tc.links, tc.err); err != nil {
				t.Fatal(err)
			}
			if d.Items[0].Stale != tc.stale {
				t.Fatal("incorrect stale state")
			}
			if tc.stale && !reflect.DeepEqual(d.Items[0].Cache, []string{cachedLink}) {
				t.Fatal("last cache lost")
			}
			if !tc.stale && len(d.Items[0].Cache) == 0 {
				t.Fatal("refresh lost valid servers")
			}
			if !reflect.DeepEqual(before, d.Items[1]) {
				t.Fatal("other profile changed")
			}
			dir := t.TempDir()
			if err := SaveLocked(context.Background(), dir, d); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadLocked(dir, ""); err != nil {
				t.Fatal("refresh persisted unreadable cache", err)
			}
		})
	}
}
func makeLinks(n int) []string {
	links := make([]string, n)
	for i := range links {
		links[i] = fmt.Sprintf("hy2://fixture@server-%d.invalid", i)
	}
	return links
}
func TestRefreshCacheRejectsProfileChangesWithoutMutation(t *testing.T) {
	for _, change := range []string{"open", "remove", "source", "kind"} {
		t.Run(change, func(t *testing.T) {
			d := cacheDocument()
			active, _ := d.ActiveProfile()
			switch change {
			case "open":
				d.Active = "profile_two"
			case "remove":
				d.Items = d.Items[1:]
				d.Active = "profile_two"
			case "source":
				d.Items[0].Source = "https://other.invalid/source"
			case "kind":
				d.Items[0].Kind = "server"
			}
			before, _ := json.Marshal(d)
			if err := d.RefreshCache(active, []string{freshLink}, nil); err == nil {
				t.Fatal("stale fetch accepted")
			}
			after, _ := json.Marshal(d)
			if string(after) != string(before) {
				t.Fatal("rejected fetch mutated document")
			}
		})
	}
}
func TestSaveRejectsInvalidDocumentAndPreservesPreviousStore(t *testing.T) {
	for _, change := range []string{"version", "duplicate id", "active", "kind", "name", "cache limit", "catalog limit"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			d := cacheDocument()
			if err := SaveLocked(context.Background(), dir, d); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, File))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "version":
				d.Version++
			case "duplicate id":
				d.Items[1].ID = d.Items[0].ID
			case "active":
				d.Active = "profile_missing"
			case "kind":
				d.Items[0].Kind = "server"
			case "name":
				d.Items[0].Name = "private\nmarker"
			case "cache limit":
				d.Items[0].Cache = makeLinks(MaxCachedServers + 1)
			case "catalog limit":
				d.Items[0].Cache = makeLinks(MaxCachedServers)
			}
			if err := SaveLocked(context.Background(), dir, d); err == nil {
				t.Fatal("invalid document persisted")
			}
			after, err := os.ReadFile(filepath.Join(dir, File))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("store changed after rejected save")
			}
		})
	}
}

func TestApplyLimitsAndRemovalKeepProfileBoundaries(t *testing.T) {
	d := cacheDocument()
	for _, r := range []Request{
		{Action: "add", Name: "Duplicate", Source: d.Items[0].Source},
		{Action: "open", ID: "profile_missing"},
		{Action: "remove", ID: "profile_missing"},
	} {
		before, _ := json.Marshal(d)
		if err := d.Apply(r); err == nil {
			t.Fatal("invalid operation accepted")
		}
		after, _ := json.Marshal(d)
		if string(before) != string(after) {
			t.Fatal("failed operation mutated profiles")
		}
	}
	if err := d.Apply(Request{Action: "remove", ID: "profile_two"}); err != nil {
		t.Fatal(err)
	}
	if d.Active != "profile_one" {
		t.Fatal("removing inactive profile changed active source")
	}
	if err := d.Apply(Request{Action: "remove", ID: "profile_one"}); err != nil {
		t.Fatal(err)
	}
	if d.Active != "" || len(d.Items) != 0 {
		t.Fatal("last profile removal left an active source")
	}
	for i := 0; i < MaxProfiles; i++ {
		if err := d.Apply(Request{Action: "add", Name: "Subscription", Source: fmt.Sprintf("https://example.invalid/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	active := d.Active
	if err := d.Apply(Request{Action: "add", Name: "Overflow", Source: "https://other.invalid/overflow"}); err == nil {
		t.Fatal("profile limit ignored")
	}
	if len(d.Items) != MaxProfiles || d.Active != active {
		t.Fatal("overflow changed profile state")
	}
}

func TestLegacyMigrationIsOneTimeAndSkipsUnsupportedSources(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy")
	if err := os.WriteFile(legacy, []byte("# comment\nfile:///unsupported\nhttps://example.invalid/private-token\nhy2://fixture@example.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := LoadLocked(dir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Items) != 2 || d.Active != d.Items[0].ID || d.Items[0].Kind != "subscription" || d.Items[1].Kind != "server" {
		t.Fatal("migration lost source boundaries")
	}
	if err := SaveLocked(context.Background(), dir, d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("https://changed.invalid/sub"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := LoadLocked(dir, legacy)
	if err != nil || !reflect.DeepEqual(d, after) {
		t.Fatal("persisted profiles reimported legacy input", err)
	}
}
