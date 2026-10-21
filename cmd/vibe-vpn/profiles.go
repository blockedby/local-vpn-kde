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
	"os"
	"time"
)

func addProfileCommands(root *cobra.Command, o *cliOptions) {
	root.AddCommand(&cobra.Command{Use: "profiles", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		r, err := profiles.DecodeRequest(cmd.InOrStdin())
		if err != nil {
			return err
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
	active, ok := d.ActiveProfile()
	if !ok {
		lock.Close()
		return nil, errors.New("profile has no cached servers")
	}
	lock.Close()
	var fetched []string
	var fetchErr error
	if active.Kind == "server" {
		fetched = []string{active.Source}
	} else {
		fetched, fetchErr = fetchSubscriptionContext(ctx, active.Source, time.Duration(c.TimeoutSeconds)*time.Second)
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
	if e = latest.RefreshCache(active, fetched, fetchErr); e != nil {
		return nil, e
	}
	if e = profiles.SaveLocked(ctx, c.StateDir, latest); e != nil {
		return nil, e
	}
	results, err := profileCandidates(latest)
	if err == nil && len(results) == 0 {
		err = errors.New("profile has no cached servers")
	}
	return results, err
}

// Both refresh and profile mutations must assign the same profile boundaries.
func profileCandidates(d profiles.Document) ([]picker.NodeResult, error) {
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
	results, e := profileCandidates(d)
	if e != nil {
		return e
	}
	if len(results) == 0 {
		return nil
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
