package profiles

import (
	"errors"

	"github.com/blockedby/local-vpn-kde/internal/sharelink"
)

// ActiveProfile returns a snapshot for an unlocked source fetch.
func (d Document) ActiveProfile() (Profile, bool) {
	for _, p := range d.Items {
		if p.ID == d.Active {
			return p, true
		}
	}
	return Profile{}, false
}

// RefreshCache applies only to the source that was fetched. Failed, empty, or
// oversized responses retain the last usable cache and mark that source stale.
func (d *Document) RefreshCache(expected Profile, links []string, fetchErr error) error {
	active, ok := d.ActiveProfile()
	if !ok || active.ID != expected.ID || active.Source != expected.Source || active.Kind != expected.Kind {
		return errors.New("profile changed during refresh")
	}
	valid := []string{}
	if fetchErr == nil {
		for _, link := range links {
			if _, err := sharelink.Parse(link); err == nil {
				valid = append(valid, link)
			}
			if len(valid) > MaxCachedServers {
				break
			}
		}
	}
	total := len(valid)
	for _, p := range d.Items {
		if p.ID != active.ID {
			total += len(p.Cache)
		}
	}
	for i := range d.Items {
		if d.Items[i].ID != active.ID {
			continue
		}
		if len(valid) == 0 || total > MaxCachedServers {
			d.Items[i].Stale = true
		} else {
			d.Items[i].Cache = valid
			d.Items[i].Stale = false
		}
		return nil
	}
	return errors.New("profile not found")
}
