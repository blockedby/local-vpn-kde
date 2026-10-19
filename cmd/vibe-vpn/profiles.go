package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/profiles"
	"github.com/blockedby/local-vpn-kde/internal/sharelink"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/spf13/cobra"
	"io"
	"os"
	"time"
)

func addProfileCommands(root *cobra.Command, o *cliOptions) {
	root.AddCommand(&cobra.Command{Use: "profiles", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var r profiles.Request
		dec := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 32<<10))
		dec.DisallowUnknownFields()
		if dec.Decode(&r) != nil || r.Validate() != nil {
			return errors.New("invalid profile request")
		}
		c, e := loadConfig(o.configPath)
		if e != nil {
			return e
		}
		ctx := cmd.Context()
		lock, e := state.AcquireLock(ctx, c.StateDir)
		if e != nil {
			return e
		}
		defer lock.Close()
		d, e := loadProfilesWithLegacyCache(c)
		if e != nil {
			return e
		}
		if e = d.Apply(r); e != nil {
			return e
		}
		if e = profiles.SaveLocked(ctx, c.StateDir, d); e != nil {
			return e
		}
		if e = syncProfileCatalogLocked(ctx, c, d); e != nil {
			return e
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(d.Safe())
	}})
}
func loadProfilesWithLegacyCache(c config.Config) (profiles.Document, error) {
	if e := recoverBrowserPublicationLocked(c.StateDir); e != nil {
		return profiles.Document{}, e
	}
	d, e := profiles.LoadLocked(c.StateDir, c.SubscriptionFile)
	if e != nil {
		return d, e
	}
	if _, e := os.Stat(c.StateDir + "/" + profiles.File); os.IsNotExist(e) && len(d.Items) > 0 {
		if catalog, e := loadBrowserCatalogLocked(c.StateDir); e == nil {
			for _, r := range catalog.Servers {
				if r.Result.Link != "" {
					d.Items[0].Cache = append(d.Items[0].Cache, r.Result.Link)
				}
			}
		} else if !os.IsNotExist(e) {
			return d, e
		}
	}
	return d, nil
}

// Only the browsed profile is refreshed. An unavailable source retains its
// cached nodes; other profiles never depend on that source being reachable.
func loadProfileCandidates(ctx context.Context, c config.Config) ([]picker.NodeResult, error) {
	lock, e := state.AcquireLock(ctx, c.StateDir)
	if e != nil {
		return nil, e
	}
	d, e := loadProfilesWithLegacyCache(c)
	if e != nil {
		lock.Close()
		return nil, e
	}
	if e = profiles.SaveLocked(ctx, c.StateDir, d); e != nil {
		lock.Close()
		return nil, e
	}
	active := d.Active
	var source string
	var kind string
	for _, p := range d.Items {
		if p.ID == active {
			source, kind = p.Source, p.Kind
		}
	}
	lock.Close()
	var fetched []string
	var fetchErr error
	if kind == "server" {
		fetched = []string{source}
	} else if source != "" {
		fetched, fetchErr = fetchSubscriptionContext(ctx, source, time.Duration(c.TimeoutSeconds)*time.Second)
	}
	valid := []string{}
	for _, s := range fetched {
		if _, e := sharelink.Parse(s); e == nil {
			valid = append(valid, s)
		}
	}
	lock, e = state.AcquireLock(ctx, c.StateDir)
	if e != nil {
		return nil, e
	}
	defer lock.Close()
	latest, e := profiles.LoadLocked(c.StateDir, c.SubscriptionFile)
	if e != nil {
		return nil, e
	}
	if latest.Active != active {
		return nil, errors.New("profile changed during refresh")
	}
	for i, p := range latest.Items {
		if p.ID == active && p.Source == source {
			if fetchErr == nil && len(valid) > 0 {
				latest.Items[i].Cache = valid
				latest.Items[i].Stale = false
			} else {
				latest.Items[i].Stale = true
			}
		}
	}
	if e = profiles.SaveLocked(ctx, c.StateDir, latest); e != nil {
		return nil, e
	}
	results := []picker.NodeResult{}
	for _, p := range latest.Items {
		for _, s := range p.Cache {
			n, e := sharelink.Parse(s)
			if e != nil {
				continue
			}
			r := browserResult(len(results)+1, n)
			r.ProfileID = p.ID
			results = append(results, r)
		}
	}
	if len(results) == 0 {
		return nil, errors.New("profile has no cached servers")
	}
	if len(results) > browserMaxServers {
		return nil, errors.New("profile catalog too large")
	}
	return results, nil
}

func syncProfileCatalogLocked(ctx context.Context, c config.Config, d profiles.Document) error {
	if browserRecoveryPendingLocked(c.StateDir) {
		return errors.New("server selection recovery required")
	}
	if e := recoverBrowserPublicationLocked(c.StateDir); e != nil {
		return e
	}
	baseline, e := captureBrowserPublicationBaselineLocked(c.StateDir)
	if e != nil {
		return e
	}
	results := []picker.NodeResult{}
	for _, p := range d.Items {
		for _, s := range p.Cache {
			n, e := sharelink.Parse(s)
			if e != nil {
				continue
			}
			r := browserResult(len(results)+1, n)
			r.ProfileID = p.ID
			results = append(results, r)
		}
	}
	if len(results) == 0 {
		return nil
	}
	if len(results) > browserMaxServers {
		return errors.New("profile catalog too large")
	}
	salt := baseline.salt
	if len(salt) == 0 {
		salt, e = state.LoadOrCreateSecretLocked(c.StateDir, browserSaltFile, browserSaltSize)
		if e != nil {
			return e
		}
		baseline.salt = salt
	}
	same := len(results) == len(baseline.catalog.Servers)
	for i, r := range results {
		id := picker.BrowserServerID(salt, r)
		if old, ok := baseline.catalog.Find(id); ok {
			results[i] = old.Result
		} else {
			same = false
		}
	}
	if same {
		return nil
	}
	generation := baseline.generation.Generation + 1
	if generation == 0 {
		return errors.New("catalog generation exhausted")
	}
	catalog := picker.NewBrowserCatalog(generation, salt, results)
	return publishBrowserCatalogLocked(ctx, c.StateDir, baseline, catalog, generation, browserPublicationRefresh)
}
