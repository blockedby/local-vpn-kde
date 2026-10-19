// Package profiles stores private source profiles independently of VPN activation.
package profiles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/blockedby/local-vpn-kde/internal/sharelink"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/subscription"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const File = "source-profiles.json"

var IDPattern = regexp.MustCompile(`^profile_[a-zA-Z0-9_-]{1,40}$`)

type Request struct {
	Action string `json:"action"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}
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

func SourceKind(s string) (string, error) {
	if len(s) == 0 || len(s) > 16384 || strings.ContainsAny(s, "\r\n\x00") {
		return "", errors.New("invalid profile source")
	}
	if sharelink.Supported(s) {
		if _, e := sharelink.Parse(s); e != nil {
			return "", e
		}
		return "server", nil
	}
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("expected HTTPS subscription or supported server link")
	}
	return "subscription", nil
}
func (r Request) Validate() error {
	switch r.Action {
	case "list":
		if r.ID == "" && r.Name == "" && r.Source == "" {
			return nil
		}
	case "open", "remove":
		if IDPattern.MatchString(r.ID) && r.Name == "" && r.Source == "" {
			return nil
		}
	case "add":
		if r.ID != "" || len([]rune(r.Name)) < 1 || len([]rune(r.Name)) > 60 || strings.Contains(r.Name, "://") {
			break
		}
		for _, c := range r.Name {
			if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
				return errors.New("invalid profile name")
			}
		}
		_, e := SourceKind(r.Source)
		return e
	}
	return errors.New("invalid profile request")
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
			if i >= 64 {
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
	if json.Unmarshal(b, &d) != nil || d.Version != 1 || len(d.Items) > 64 {
		return d, errors.New("invalid profile store")
	}
	seen := map[string]bool{}
	for _, p := range d.Items {
		if !IDPattern.MatchString(p.ID) || seen[p.ID] || len(p.Cache) > 1000 {
			return d, errors.New("invalid profile store")
		}
		seen[p.ID] = true
		kind, err := SourceKind(p.Source)
		if err != nil || kind != p.Kind {
			return d, errors.New("invalid profile kind")
		}
		if e := (Request{Action: "add", Name: p.Name, Source: p.Source}).Validate(); e != nil {
			return d, e
		}
	}
	if d.Active != "" && !seen[d.Active] {
		return d, errors.New("invalid active profile")
	}
	return d, nil
}
func SaveLocked(ctx context.Context, dir string, d Document) error {
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
		if len(d.Items) >= 64 {
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
