package picker

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestBrowserIDsAreStableSaltedAndOpaque(t *testing.T) {
	result := NodeResult{
		Name: "Paris", Host: "private.example", Port: 443, Network: "ws", Security: "tls",
		Link:     "vless://credential@private.example:443?type=ws&security=tls#Paris",
		Outbound: map[string]any{"server": "private.example", "uuid": "credential"},
	}
	a := BrowserServerID([]byte("0123456789abcdef0123456789abcdef"), result)
	b := BrowserServerID([]byte("0123456789abcdef0123456789abcdef"), result)
	otherSalt := BrowserServerID([]byte("abcdef0123456789abcdef0123456789"), result)
	renamed := result
	renamed.Name = "Renamed"
	renamed.Link = strings.Replace(renamed.Link, "#Paris", "#Renamed", 1)
	if a == "" || a != b || a != BrowserServerID([]byte("0123456789abcdef0123456789abcdef"), renamed) {
		t.Fatalf("ID is not stable across identical identity/name change: %q %q", a, b)
	}
	if a == otherSalt {
		t.Fatal("different local salts produced the same ID")
	}
	if !regexp.MustCompile(`^srv_[A-Za-z0-9_-]{27}$`).MatchString(a) {
		t.Fatalf("ID is not opaque fixed form: %q", a)
	}
	for _, forbidden := range []string{"private", "example", "credential", "443"} {
		if strings.Contains(strings.ToLower(a), forbidden) {
			t.Fatalf("ID leaked %q: %q", forbidden, a)
		}
	}
}

func TestBrowserCatalogDisambiguatesNamesAndRedactsResponse(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")
	catalog := NewBrowserCatalog(7, salt, []NodeResult{
		{Name: "Paris France", Host: "one.private.example", Port: 443, Network: "ws", Security: "tls", Link: "vless://secret-one@one.private.example:443?type=ws#Paris-France", OK: true, Seconds: .012, PingMS: 12, PingStatus: "ready"},
		{Name: "Paris France", Host: "two.private.example", Port: 443, Network: "grpc", Security: "reality", Link: "vless://secret-two@two.private.example:443?type=grpc#Paris-France", Error: "dial two.private.example failed", Seconds: 99, PingMS: 60000, PingStatus: "ready"},
		{Name: "two.private.example:443", Host: "two.private.example", Port: 443, Network: "grpc", Security: "reality", Link: "vless://secret-two@two.private.example:443?type=grpc"}, // exact duplicate
	})
	if len(catalog.Servers) != 2 {
		t.Fatalf("catalog has %d servers, want exact duplicate collapsed", len(catalog.Servers))
	}
	safe := catalog.SafeServers(catalog.Servers[0].ID)
	if safe[0].DisplayName != "Paris France (1)" || safe[1].DisplayName != "Paris France (2)" {
		t.Fatalf("duplicate names not disambiguated: %+v", safe)
	}
	latencies := map[string]int{}
	for _, server := range safe {
		if server.LatencyMS == nil {
			t.Fatalf("latency missing: %+v", safe)
		}
		latencies[server.Status] = *server.LatencyMS
	}
	if latencies["ready"] != 12 || latencies["failed"] != 60000 {
		t.Fatalf("latency not bounded: %+v", safe)
	}
	body, err := json.Marshal(BrowserResponse{Schema: BrowserSchema, Status: "ok", Generation: catalog.Generation, Servers: safe})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"vless://", "secret-one", "secret-two", "private.example", "dial ", `"host"`, `"link"`, `"error"`, `"outbound"`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("safe response leaked %q: %s", forbidden, body)
		}
	}
}

func TestBrowserCatalogIntegrityCoversReadinessAndDisplayMetadata(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")
	catalog := NewBrowserCatalog(3, salt, []NodeResult{{Name: "Paris", Host: "hidden.invalid", Port: 443, Network: "tcp", Security: "tls", Link: "vless://secret@hidden.invalid:443"}})
	if !VerifyBrowserCatalog(salt, catalog) {
		t.Fatal("new catalog integrity did not verify")
	}
	catalog.Servers[0].Result.OK = true
	if VerifyBrowserCatalog(salt, catalog) {
		t.Fatal("readiness tampering was accepted")
	}
	SealBrowserCatalog(salt, &catalog)
	catalog.Servers[0].DisplayName = "credential-sentinel"
	if VerifyBrowserCatalog(salt, catalog) {
		t.Fatal("display-name tampering was accepted")
	}
}

func TestBrowserGenerationStateIntegrityRejectsRollbackAndTampering(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")
	state := NewBrowserGenerationState(9, salt)
	if !VerifyBrowserGenerationState(salt, state) {
		t.Fatal("new generation state did not verify")
	}
	state.Generation--
	if VerifyBrowserGenerationState(salt, state) {
		t.Fatal("generation rollback retained valid integrity")
	}
	state = NewBrowserGenerationStateWithRevision(9, 12, salt)
	state.Revision--
	if VerifyBrowserGenerationState(salt, state) {
		t.Fatal("publication revision rollback retained valid integrity")
	}
	if VerifyBrowserGenerationState([]byte("abcdef0123456789abcdef0123456789"), NewBrowserGenerationState(9, salt)) {
		t.Fatal("generation state verified with another local salt")
	}
}

func TestSafeDisplayNameRejectsEndpointCredentialPathAndSingleTokenShapes(t *testing.T) {
	unsafe := []string{
		"one.example.test", "192.0.2.10", "host:443", "user@host",
		"vless://secret", "/private/path", "credential-sentinel",
		"subscription-token", "550e8400-e29b-41d4-a716-446655440000",
		"localhost", "vpn", "proxy01", "private-endpoint", "intranet", "東京",
	}
	for _, raw := range unsafe {
		if got := SafeDisplayName(raw); got != "Server" {
			t.Fatalf("SafeDisplayName(%q)=%q, want exact generic label", raw, got)
		}
	}
	for _, safe := range []string{"Server", "Paris France", "New York", "São Paulo Brazil", "🇫🇷 Paris Premium"} {
		if got := SafeDisplayName(safe); got != safe {
			t.Fatalf("SafeDisplayName(%q)=%q, want unchanged", safe, got)
		}
	}
}

func TestBrowserCatalogKeepsGenericDuplicatesUsableByOpaqueID(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")
	catalog := NewBrowserCatalog(8, salt, []NodeResult{
		{Name: "private-endpoint", Host: "one.private.example", Port: 443, Network: "ws", Security: "tls", Link: "vless://first@one.private.example:443"},
		{Name: "intranet", Host: "two.private.example", Port: 443, Network: "grpc", Security: "reality", Link: "vless://second@two.private.example:443"},
	})
	if len(catalog.Servers) != 2 {
		t.Fatalf("catalog has %d generic rows, want 2", len(catalog.Servers))
	}
	for _, record := range catalog.Servers {
		if record.DisplayName != "Server" {
			t.Fatalf("generic label carried a private fragment: %q", record.DisplayName)
		}
		if found, ok := catalog.Find(record.ID); !ok || found.ID != record.ID {
			t.Fatalf("generic row is not addressable by opaque ID %q", record.ID)
		}
	}
	if catalog.Servers[0].ID == catalog.Servers[1].ID {
		t.Fatal("distinct generic rows received the same opaque ID")
	}
	for _, server := range catalog.SafeServers("") {
		if server.DisplayName != "Server" {
			t.Fatalf("safe response changed closed generic label: %q", server.DisplayName)
		}
	}
}
