// Package profiles stores private source profiles independently of VPN activation.
package profiles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/subscription"
	"os"
	"regexp"
	"strconv"
)

const (
	File             = "source-profiles.json"
	MaxProfiles      = 64
	MaxCachedServers = 1000
	MaxRequestBytes  = 32 << 10
)

var IDPattern = regexp.MustCompile(`^profile_[a-zA-Z0-9_-]{1,40}$`)

type Profile struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Source string   `json:"source"`
	Cache  []string `json:"cache,omitempty"`
	Stale  bool     `json:"stale,omitempty"`
}
type Document struct {
	Version int       `json:"version"`
	Active  string    `json:"active"`
	Items   []Profile `json:"items"`
}
type View struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Stale bool   `json:"stale"`
}
type Response struct {
	Active string `json:"active"`
	Items  []View `json:"items"`
}

func LoadLocked(dir, legacy string) (Document, error) {
	b, _, e := state.LoadPrivateFileLocked(dir, File, 16<<20)
	if os.IsNotExist(e) {
		d := Document{Version: 1, Items: []Profile{}}
		data, e := os.ReadFile(legacy)
		if e != nil && !os.IsNotExist(e) {
			return d, e
		}
		for i, s := range subscription.URLList(string(data)) {
			if i >= MaxProfiles {
				return d, errors.New("too many legacy sources")
			}
			kind, e := SourceKind(s)
			if e != nil {
				continue
			}
			id := "profile_legacy" + strconv.Itoa(i+1)
			d.Items = append(d.Items, Profile{ID: id, Name: "Подписка " + strconv.Itoa(i+1), Kind: kind, Source: s})
			if d.Active == "" {
				d.Active = id
			}
		}
		return d, nil
	}
	if e != nil {
		return Document{}, e
	}
	var d Document
	if json.Unmarshal(b, &d) != nil {
		return d, errors.New("invalid profile store")
	}
	if err := d.Validate(); err != nil {
		return d, err
	}
	return d, nil
}

// Validate keeps persisted documents readable by the next operation as well.
func (d Document) Validate() error {
	if d.Version != 1 || len(d.Items) > MaxProfiles {
		return errors.New("invalid profile store")
	}
	seen := map[string]bool{}
	total := 0
	for _, p := range d.Items {
		if !IDPattern.MatchString(p.ID) || seen[p.ID] || len(p.Cache) > MaxCachedServers {
			return errors.New("invalid profile store")
		}
		total += len(p.Cache)
		if total > MaxCachedServers {
			return errors.New("profile catalog too large")
		}
		seen[p.ID] = true
		kind, err := SourceKind(p.Source)
		if err != nil || kind != p.Kind {
			return errors.New("invalid profile kind")
		}
		if e := (Request{Action: "add", Name: p.Name, Source: p.Source}).Validate(); e != nil {
			return e
		}
	}
	if d.Active != "" && !seen[d.Active] {
		return errors.New("invalid active profile")
	}
	return nil
}

func SaveLocked(ctx context.Context, dir string, d Document) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, e := json.Marshal(d)
	if e != nil {
		return e
	}
	return state.WritePrivateFileAtomicLocked(ctx, dir, File, b)
}
func (d *Document) Apply(r Request) error {
	if e := r.Validate(); e != nil {
		return e
	}
	if r.Action == "list" {
		return nil
	}
	if r.Action == "add" {
		if len(d.Items) >= MaxProfiles {
			return errors.New("profile limit reached")
		}
		for _, p := range d.Items {
			if p.Source == r.Source {
				return errors.New("profile source already exists")
			}
		}
		var id [12]byte
		if _, e := rand.Read(id[:]); e != nil {
			return e
		}
		kind, _ := SourceKind(r.Source)
		p := Profile{ID: "profile_" + hex.EncodeToString(id[:]), Name: r.Name, Kind: kind, Source: r.Source}
		if kind == "server" {
			p.Cache = []string{r.Source}
		}
		d.Items = append(d.Items, p)
		d.Active = p.ID
		return nil
	}
	for i, p := range d.Items {
		if p.ID != r.ID {
			continue
		}
		if r.Action == "open" {
			d.Active = p.ID
			return nil
		}
		d.Items = append(d.Items[:i], d.Items[i+1:]...)
		if d.Active == p.ID {
			d.Active = ""
			if len(d.Items) > 0 {
				d.Active = d.Items[0].ID
			}
		}
		return nil
	}
	return errors.New("profile not found")
}
func (d Document) Safe() Response {
	r := Response{Active: d.Active, Items: []View{}}
	for _, p := range d.Items {
		r.Items = append(r.Items, View{p.ID, p.Name, p.Kind, len(p.Cache), p.Stale})
	}
	return r
}
