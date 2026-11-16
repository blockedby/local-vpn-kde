package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"unicode"

	"github.com/blockedby/local-vpn-kde/internal/picker"
	"golang.org/x/sys/unix"
)

const (
	serverNamesFile     = "server-names.json"
	serverNamesLock     = "server-names.lock"
	maxServerNames      = 500
	maxServerNamesBytes = 128 * 1024
)

type serverNamesData struct {
	Schema int               `json:"schema"`
	Names  map[string]string `json:"names"`
}

// The picker returns its generic fallback for unsafe or unusable input. Do not
// persist that fallback as though it were the user's chosen name.
func safeOverride(name string) (string, bool) {
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", false
	}
	safe := picker.SafeDisplayName(name)
	if safe == "Server" && strings.TrimSpace(name) != "Server" {
		return "", false
	}
	return safe, true
}

func (b *Bridge) loadServerNames() {
	b.serverNames = b.readServerNames()
}

func (b *Bridge) readServerNames() map[string]string {
	names := make(map[string]string)
	fd, err := directory(b.options.Base, false, true)
	if errors.Is(err, unix.ENOENT) {
		return names
	}
	if err == nil {
		defer unix.Close(fd)
		var data []byte
		data, err = readAt(fd, serverNamesFile)
		if errors.Is(err, unix.ENOENT) {
			return names
		}
		if err == nil {
			var stored serverNamesData
			if len(data) > maxServerNamesBytes || json.Unmarshal(data, &stored) != nil || stored.Schema != 1 || stored.Names == nil || len(stored.Names) > maxServerNames {
				err = errors.New("invalid names store")
			} else {
				for id, name := range stored.Names {
					safe, valid := safeOverride(name)
					if !serverID.MatchString(id) || !valid || name != safe {
						err = errors.New("invalid names store")
						break
					}
				}
				if err == nil {
					names = stored.Names
				}
			}
		}
	}
	if err != nil {
		// This diagnostic deliberately contains no file contents or user names.
		log.Print("localvpn: server names store unavailable or invalid; using empty preferences")
	}
	return names
}

// Caller holds namesMu exclusively through the subsequent list rewrite.
func (b *Bridge) renameServer(id, name string) string {
	fd, err := directory(b.options.Base, true, true)
	if err != nil {
		return "server-names-unavailable"
	}
	defer unix.Close(fd)
	lock, err := lockFile(context.Background(), fd, serverNamesLock)
	if err != nil {
		return "server-names-unavailable"
	}
	defer unix.Close(lock)
	b.loadServerNames()
	if name != "" {
		var valid bool
		name, valid = safeOverride(name)
		if !valid {
			return "invalid-name"
		}
		if _, exists := b.serverNames[id]; !exists && len(b.serverNames) >= maxServerNames {
			return "too-many-names"
		}
	}
	names := make(map[string]string, len(b.serverNames)+1)
	for key, value := range b.serverNames {
		names[key] = value
	}
	if name == "" {
		delete(names, id)
	} else {
		names[id] = name
	}
	data, err := json.Marshal(serverNamesData{Schema: 1, Names: names})
	if err != nil {
		return "server-names-unavailable"
	}
	if err = atomicWrite(fd, serverNamesFile, append(data, '\n')); err != nil {
		return "server-names-unavailable"
	}
	b.serverNames = names
	return "ok"
}

// Caller holds namesMu for reading or writing. All rows have already passed
// sanitizeCatalog; do not expose an upstream original_name without an override.
func (b *Bridge) applyServerNames(catalog map[string]any) {
	names := b.readServerNames()
	apply := func(row map[string]any) {
		delete(row, "original_name")
		if name, ok := names[row["server_id"].(string)]; ok {
			row["original_name"] = row["display_name"]
			row["display_name"] = name
		}
	}
	if rows, ok := catalog["servers"].([]any); ok {
		for _, row := range rows {
			apply(row.(map[string]any))
		}
	}
	if row, ok := catalog["server"].(map[string]any); ok {
		apply(row)
	}
}
