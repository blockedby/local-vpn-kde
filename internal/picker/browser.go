package picker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	BrowserSchema                 = "vibe-vpn.server-browser.v2"
	BrowserCatalogSchema          = "vibe-vpn.private-server-catalog.v1"
	BrowserGenerationSchema       = "vibe-vpn.private-server-generation.v2"
	browserLegacyGenerationSchema = "vibe-vpn.private-server-generation.v1"
)

// BrowserCatalog is private state. Servers contain runtime credentials and
// endpoints and must never be serialized to an operator-facing response.
type BrowserCatalog struct {
	Schema     string          `json:"schema"`
	Generation uint64          `json:"generation"`
	Servers    []BrowserRecord `json:"servers"`
	Integrity  string          `json:"integrity"`
}

type BrowserRecord struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Result      NodeResult `json:"result"`
}

// BrowserGenerationState is a private authenticated high-water mark. Revision
// advances for every catalog publication, including same-generation ping
// updates, so an older catalog/commit pair cannot be replayed after a newer
// publication. Legacy v1 markers have revision zero and are migrated under the
// state lock before use.
type BrowserGenerationState struct {
	Schema     string `json:"schema"`
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision,omitempty"`
	Integrity  string `json:"integrity"`
}

// BrowserServer is the only server representation permitted on the JSON CLI.
type BrowserServer struct {
	Availability    string   `json:"availability,omitempty"`
	PingStatus      string   `json:"ping_status,omitempty"`
	ServerID        string   `json:"server_id"`
	DisplayName     string   `json:"display_name"`
	Transport       string   `json:"transport"`
	Security        string   `json:"security"`
	Status          string   `json:"status"`
	DownloadMbps    *float64 `json:"download_mbps,omitempty"`
	DownloadSeconds *float64 `json:"download_seconds,omitempty"`
	DownloadedBytes *int64   `json:"downloaded_bytes,omitempty"`
	LatencyMS       *int     `json:"latency_ms,omitempty"`
	Selected        bool     `json:"selected"`
}

type BrowserResponse struct {
	Schema     string          `json:"schema"`
	Status     string          `json:"status"`
	Generation uint64          `json:"generation"`
	Servers    []BrowserServer `json:"servers,omitempty"`
	Server     *BrowserServer  `json:"server,omitempty"`
}

// NewBrowserCatalog assigns local-salt HMAC identities. Exact duplicate nodes
// are collapsed, while distinct nodes with duplicate safe human names receive
// deterministic display suffixes. Generic rows stay usable through opaque IDs.
func NewBrowserCatalog(generation uint64, salt []byte, results []NodeResult) BrowserCatalog {
	if generation == 0 {
		generation = 1
	}
	byID := make(map[string]BrowserRecord, len(results))
	for _, result := range results {
		id := BrowserServerID(salt, result)
		if id == "" {
			continue
		}
		if _, exists := byID[id]; exists {
			continue
		}
		byID[id] = BrowserRecord{ID: id, DisplayName: SafeDisplayName(result.Name), Result: result}
	}
	records := make([]BrowserRecord, 0, len(byID))
	for _, record := range byID {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })

	counts := make(map[string]int, len(records))
	for _, record := range records {
		counts[record.DisplayName]++
	}
	seen := make(map[string]int, len(counts))
	for i := range records {
		name := records[i].DisplayName
		if counts[name] > 1 && name != "Server" {
			seen[name]++
			records[i].DisplayName = name + " (" + smallDecimal(seen[name]) + ")"
		}
	}
	catalog := BrowserCatalog{Schema: BrowserCatalogSchema, Generation: generation, Servers: records}
	SealBrowserCatalog(salt, &catalog)
	return catalog
}

func (c BrowserCatalog) Valid() bool {
	return c.Schema == BrowserCatalogSchema && c.Generation > 0 && c.Integrity != ""
}

// SealBrowserCatalog authenticates all private catalog fields, including test
// readiness and display metadata. It detects corruption that could otherwise
// turn an untested server into a selectable one.
func SealBrowserCatalog(salt []byte, catalog *BrowserCatalog) {
	if catalog == nil || len(salt) < 16 {
		return
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(browserCatalogPayload(*catalog))
	catalog.Integrity = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func VerifyBrowserCatalog(salt []byte, catalog BrowserCatalog) bool {
	if !catalog.Valid() || len(salt) < 16 {
		return false
	}
	expected := catalog
	expected.Integrity = ""
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(browserCatalogPayload(expected))
	decoded, err := base64.RawURLEncoding.DecodeString(catalog.Integrity)
	return err == nil && hmac.Equal(decoded, mac.Sum(nil))
}

func NewBrowserGenerationState(generation uint64, salt []byte) BrowserGenerationState {
	return NewBrowserGenerationStateWithRevision(generation, generation, salt)
}

func NewBrowserGenerationStateWithRevision(generation, revision uint64, salt []byte) BrowserGenerationState {
	state := BrowserGenerationState{Schema: BrowserGenerationSchema, Generation: generation, Revision: revision}
	if generation == 0 || revision == 0 || len(salt) < 16 {
		return state
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(browserGenerationPayload(state))
	state.Integrity = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return state
}

func VerifyBrowserGenerationState(salt []byte, state BrowserGenerationState) bool {
	if state.Generation == 0 || state.Integrity == "" || len(salt) < 16 {
		return false
	}
	switch state.Schema {
	case BrowserGenerationSchema:
		if state.Revision == 0 {
			return false
		}
	case browserLegacyGenerationSchema:
		if state.Revision != 0 {
			return false
		}
	default:
		return false
	}
	expected := state
	expected.Integrity = ""
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(browserGenerationPayload(expected))
	decoded, err := base64.RawURLEncoding.DecodeString(state.Integrity)
	return err == nil && hmac.Equal(decoded, mac.Sum(nil))
}

func IsCurrentBrowserGenerationState(state BrowserGenerationState) bool {
	return state.Schema == BrowserGenerationSchema && state.Generation > 0 && state.Revision > 0
}

func browserGenerationPayload(state BrowserGenerationState) []byte {
	state.Integrity = ""
	body, _ := json.Marshal(state)
	return body
}

func browserCatalogPayload(catalog BrowserCatalog) []byte {
	catalog.Integrity = ""
	body, _ := json.Marshal(catalog)
	return body
}

func (c BrowserCatalog) Find(id string) (BrowserRecord, bool) {
	for _, record := range c.Servers {
		if hmac.Equal([]byte(record.ID), []byte(id)) {
			return record, true
		}
	}
	return BrowserRecord{}, false
}

func (c BrowserCatalog) SafeServers(selectedID string) []BrowserServer {
	out := make([]BrowserServer, 0, len(c.Servers))
	for _, record := range c.Servers {
		out = append(out, record.Safe(record.ID == selectedID))
	}
	return out
}

func (r BrowserRecord) Safe(selected bool) BrowserServer {
	status := "untested"
	if r.Result.OK {
		status = "ready"
	} else if r.Result.Error != "" || r.Result.Seconds > 0 {
		status = "failed"
	}
	var latency *int
	pingStatus := "untested"
	if r.Result.PingStatus == "ready" && r.Result.PingMS > 0 && r.Result.PingMS <= 60000 {
		latency = &r.Result.PingMS
		pingStatus = "ready"
	} else if r.Result.PingStatus == "failed" {
		pingStatus = "failed"
	}

	var speed, seconds *float64
	var size *int64
	if r.Result.OK && r.Result.Mbps > 0 && !math.IsNaN(r.Result.Mbps) && !math.IsInf(r.Result.Mbps, 0) && r.Result.Seconds > 0 && r.Result.Seconds <= 60000 && r.Result.Bytes > 0 {
		speed, seconds, size = &r.Result.Mbps, &r.Result.Seconds, &r.Result.Bytes
	}
	return BrowserServer{
		ServerID: r.ID, DisplayName: SafeDisplayName(r.DisplayName),
		Transport: safeTransport(r.Result.Network), Security: safeSecurity(r.Result.Security),
		Availability: safeAvailability(r.Result.Availability), PingStatus: pingStatus, Status: status, LatencyMS: latency, Selected: selected, DownloadMbps: speed, DownloadSeconds: seconds, DownloadedBytes: size,
	}
}

// BrowserServerID is stable for the same private node identity and local salt.
// HMAC prevents an observer from brute-forcing an ID from a raw endpoint alone.
func BrowserServerID(salt []byte, result NodeResult) string {
	if len(salt) < 16 {
		return ""
	}
	identity := struct {
		Link     string         `json:"link"`
		Host     string         `json:"host"`
		Port     int            `json:"port"`
		Network  string         `json:"network"`
		Security string         `json:"security"`
		Outbound map[string]any `json:"outbound"`
	}{
		Link: identityLink(result.Link), Host: strings.TrimSpace(strings.ToLower(result.Host)),
		Port: result.Port, Network: strings.TrimSpace(strings.ToLower(result.Network)),
		Security: strings.TrimSpace(strings.ToLower(result.Security)), Outbound: result.Outbound,
	}
	if identity.Link == "" && identity.Host == "" && len(identity.Outbound) == 0 {
		return ""
	}
	canonical, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(canonical)
	sum := mac.Sum(nil)
	return "srv_" + base64.RawURLEncoding.EncodeToString(sum[:20])
}

func identityLink(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = u.Query().Encode()
	return u.String()
}

var (
	domainLike = regexp.MustCompile(`(?i)(^|[^[:alnum:]])(?:[[:alnum:]-]+\.)+[[:alpha:]]{2,}([^[:alnum:]]|$)`)
	ipv4Like   = regexp.MustCompile(`(^|[^0-9])(?:[0-9]{1,3}\.){3}[0-9]{1,3}([^0-9]|$)`)
	portLike   = regexp.MustCompile(`:[0-9]{2,5}([^0-9]|$)`)
	uuidLike   = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	secretLike = regexp.MustCompile(`(?i)(secret|credential|password|passwd|token|auth|uuid|subscription|vless)`)
)

// SafeDisplayName treats subscription fragments as untrusted. Endpoint-like,
// credential-like, path-like, control-bearing, and arbitrary single-token names
// become the exact closed generic label "Server".
func SafeDisplayName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" || strings.Contains(name, "://") || strings.ContainsAny(name, "@/\\") ||
		domainLike.MatchString(name) || ipv4Like.MatchString(name) || portLike.MatchString(name) || uuidLike.MatchString(name) || secretLike.MatchString(name) || net.ParseIP(name) != nil {
		return "Server"
	}
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) {
			return "Server"
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) || strings.ContainsRune("-_()[]", r) || r >= 0x1F1E6 && r <= 0x1F1FF {
			b.WriteRune(r)
		}
		if len([]rune(b.String())) >= 48 {
			break
		}
	}
	name = strings.Join(strings.Fields(b.String()), " ")
	if len(strings.Fields(name)) < 2 {
		return "Server"
	}
	return name
}

func safeTransport(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "tcp", "ws", "grpc", "hysteria2", "quic":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "other"
	}
}

func safeSecurity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "tls", "reality", "none":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "other"
	}
}

func smallDecimal(n int) string {
	if n <= 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// SaltFingerprint is test/diagnostic support for comparing salts without ever
// exposing a salt in the public browser schema.
func SaltFingerprint(salt []byte) string {
	sum := sha256.Sum256(salt)
	return hex.EncodeToString(sum[:8])
}

func safeAvailability(value string) string {
	if value == "ready" || value == "failed" {
		return value
	}
	return "untested"
}
