package localvpn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var uuidInText = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
var fingerprintPattern = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)
var tunnelDevice = regexp.MustCompile(`^tun[A-Za-z0-9_.-]{0,12}$`)

type nmCapability struct {
	UUID, Fingerprint string
	Legacy            bool
}
type nmConnection struct{ Name, UUID, Type, Service, Data, Device string }
type NetworkManager struct {
	Base          string
	MigrationBase string
	Timeout       time.Duration
	command       func(context.Context, ...string) (string, error)
	persist       func(int, map[string][]byte) error
}

var ErrForeignNMProfile = errors.New("existing vpnkit-local profile belongs to another installation; ownership migration could not be verified")
var ErrActiveNMMigration = errors.New("disconnect the previous local VPN before migrating its profile")

// migrateOwnership reads the previous installation without changing it. A name
// alone never authorizes adoption: require its private capability, matching
// source fingerprint, the exact live UUID and a proven local OpenVPN profile.
func (n NetworkManager) migrateOwnership(ctx context.Context) error {
	if n.MigrationBase == "" || n.MigrationBase == n.Base {
		return ErrForeignNMProfile
	}
	if err := validateSecretTree(n.MigrationBase, nil); err != nil {
		return ErrForeignNMProfile
	}
	previous := NetworkManager{Base: n.MigrationBase, command: n.command}
	cap, err := previous.readCapability()
	if err != nil || cap.UUID == "" {
		return ErrForeignNMProfile
	}
	fingerprint, err := previous.profileFingerprint()
	if err != nil || cap.Fingerprint != fingerprint {
		return ErrForeignNMProfile
	}
	profile, err := n.connection(ctx, cap.UUID)
	if err != nil || profile.Name != "vpnkit-local" {
		return ErrForeignNMProfile
	}
	if err := n.namedAllowlist(ctx, cap.UUID, ""); err != nil {
		return ErrForeignNMProfile
	}
	active, err := n.active(ctx, cap.UUID)
	if err != nil {
		return ErrForeignNMProfile
	}
	if active {
		return ErrActiveNMMigration
	}
	// Recheck the read-only evidence before publishing the new capability.
	current, err := previous.readCapability()
	if err != nil || current != cap {
		return ErrForeignNMProfile
	}
	if fingerprint, err := previous.profileFingerprint(); err != nil || fingerprint != cap.Fingerprint {
		return ErrForeignNMProfile
	}
	cap.Legacy = false
	return n.writeCapability(cap)
}

func (n NetworkManager) profilePath() string {
	return filepath.Join(n.Base, "openvpn/client/vpnkit-local.ovpn")
}
func (n NetworkManager) call(ctx context.Context, args ...string) (string, error) {
	if n.command != nil {
		return n.command(ctx, args...)
	}
	data, err := hostCommand(ctx, "nmcli", args...)
	if err != nil {
		return string(data), errors.New("NetworkManager command failed")
	}
	return string(data), nil
}
func (n NetworkManager) stateDir(create bool) (int, error) {
	root, err := directory(n.Base, false, true)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	return childDirectory(root, "state", create, true)
}
func (n NetworkManager) readCapability() (nmCapability, error) {
	var result nmCapability
	dir, err := n.stateDir(false)
	if errors.Is(err, unix.ENOENT) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer unix.Close(dir)
	data, err := readAt(dir, "networkmanager-state")
	if errors.Is(err, unix.ENOENT) {
		uuid, e1 := readAt(dir, "networkmanager-uuid")
		fingerprint, e2 := readAt(dir, "networkmanager-profile-fingerprint")
		if errors.Is(e1, unix.ENOENT) && errors.Is(e2, unix.ENOENT) {
			return result, nil
		}
		if e1 != nil || e2 != nil {
			return result, errors.New("incomplete NetworkManager capability")
		}
		data = []byte(strings.TrimSpace(string(uuid)) + " " + strings.TrimSpace(string(fingerprint)))
		result.Legacy = true
	} else if err != nil {
		return result, err
	}
	text := strings.TrimRight(string(data), "\n")
	fields := strings.Fields(text)
	if strings.ContainsAny(text, "\r\n") || len(fields) != 2 || !uuidPattern.MatchString(fields[0]) || !fingerprintPattern.MatchString(fields[1]) {
		return result, errors.New("invalid NetworkManager capability")
	}
	result.UUID = strings.ToLower(fields[0])
	result.Fingerprint = strings.ToLower(fields[1])
	return result, nil
}
func (n NetworkManager) writeCapability(cap nmCapability) error {
	dir, err := n.stateDir(true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err = unix.Fchmod(dir, 0700); err != nil {
		return err
	}
	values := map[string][]byte{"networkmanager-state": nil, "networkmanager-uuid": nil, "networkmanager-profile-fingerprint": nil}
	if cap.UUID != "" {
		if !uuidPattern.MatchString(cap.UUID) || !fingerprintPattern.MatchString(cap.Fingerprint) {
			return errors.New("invalid NetworkManager capability")
		}
		values["networkmanager-state"] = []byte(cap.UUID + " " + cap.Fingerprint + "\n")
		values["networkmanager-uuid"] = []byte(cap.UUID + "\n")
		values["networkmanager-profile-fingerprint"] = []byte(cap.Fingerprint + "\n")
	}
	if n.persist != nil {
		return n.persist(dir, values)
	}
	return writeBundle(dir, values)
}
func (n NetworkManager) profileFingerprint() (string, error) {
	data, err := runtimeRead(n.profilePath())
	if err != nil {
		return "", err
	}
	client, device, proto, remotes := false, false, false, 0
	inline := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if inline != "" {
			if line == "</"+inline+">" {
				inline = ""
			}
			continue
		}
		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
			inline = strings.Trim(line, "<>")
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "client":
			client = len(fields) == 1
		case "dev":
			device = len(fields) == 2 && (fields[1] == "tun" || fields[1] == "tun0")
		case "proto":
			proto = len(fields) == 2 && (fields[1] == "udp" || fields[1] == "udp4")
		case "remote-random":
			return "", errors.New("random remote refused")
		case "remote":
			remotes++
			if len(fields) < 2 || len(fields) > 3 {
				return "", errors.New("invalid local remote")
			}
			host := strings.Trim(fields[1], "[]")
			if host != "localhost" && host != "127.0.0.1" {
				return "", errors.New("foreign profile endpoint")
			}
			if len(fields) == 3 {
				port, e := strconv.Atoi(fields[2])
				if e != nil || port < 1 || port > 65535 {
					return "", errors.New("invalid local port")
				}
			}
		}
	}
	if !client || !device || !proto || remotes != 1 || inline != "" {
		return "", errors.New("invalid local OpenVPN profile")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func (n NetworkManager) inventory(ctx context.Context, active bool) ([]nmConnection, error) {
	args := []string{"-t", "-f", "NAME,UUID,TYPE", "connection", "show"}
	if active {
		args[2] = "NAME,UUID,TYPE,DEVICE"
		args = append(args, "--active")
	}
	text, err := n.call(ctx, args...)
	if err != nil {
		return nil, err
	}
	result := []nmConnection{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		fields := splitNMTerse(line)
		if len(fields) < 3 {
			return nil, errors.New("invalid NetworkManager inventory")
		}
		row := nmConnection{Name: fields[0], UUID: strings.ToLower(fields[1]), Type: fields[2]}
		if !uuidPattern.MatchString(row.UUID) {
			return nil, errors.New("invalid NetworkManager inventory UUID")
		}
		if active {
			if len(fields) != 4 {
				return nil, errors.New("invalid active inventory")
			}
			row.Device = fields[3]
		}
		result = append(result, row)
	}
	return result, nil
}
func splitNMTerse(line string) []string {
	fields := []string{}
	var value strings.Builder
	escaped := false
	for _, r := range strings.TrimSuffix(line, "\r") {
		if escaped {
			value.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == ':' {
			fields = append(fields, value.String())
			value.Reset()
		} else {
			value.WriteRune(r)
		}
	}
	if escaped {
		value.WriteRune('\\')
	}
	return append(fields, value.String())
}
func (n NetworkManager) connection(ctx context.Context, uuid string) (nmConnection, error) {
	result := nmConnection{}
	if !uuidPattern.MatchString(uuid) {
		return result, errors.New("invalid UUID")
	}
	text, err := n.call(ctx, "-t", "-f", "connection.id,connection.uuid,connection.type,vpn.service-type,vpn.data", "connection", "show", "uuid", uuid)
	if err != nil {
		return result, err
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var values []string
	if !strings.Contains(lines[0], ":") {
		values = lines
	} else if strings.HasPrefix(lines[0], "connection.") {
		fields := map[string]string{}
		for _, line := range lines {
			key, value, ok := strings.Cut(line, ":")
			if !ok || fields[key] != "" {
				return result, errors.New("invalid connection fields")
			}
			fields[key] = value
		}
		values = []string{fields["connection.id"], fields["connection.uuid"], fields["connection.type"], fields["vpn.service-type"], fields["vpn.data"]}
	} else {
		values = strings.SplitN(lines[0], ":", 5)
	}
	if len(values) != 5 {
		return result, errors.New("invalid NetworkManager profile")
	}
	result = nmConnection{Name: values[0], UUID: strings.ToLower(values[1]), Type: values[2], Service: values[3], Data: strings.ReplaceAll(values[4], `\:`, ":")}
	if result.UUID != strings.ToLower(uuid) || result.Name == "" || result.Type != "vpn" || result.Service != "org.freedesktop.NetworkManager.openvpn" {
		return result, errors.New("not the expected OpenVPN profile")
	}
	remotes := 0
	for _, entry := range strings.Split(result.Data, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "remote-random" {
			return result, errors.New("random remote refused")
		}
		if key == "remote" {
			remotes++
			host := strings.Trim(strings.SplitN(value, ":", 2)[0], "[]")
			if host != "127.0.0.1" && host != "localhost" {
				return result, errors.New("foreign NetworkManager endpoint")
			}
		}
	}
	if remotes != 1 {
		return result, errors.New("invalid NetworkManager remote")
	}
	return result, nil
}
func (n NetworkManager) assess(ctx context.Context, checkSource bool) (nmCapability, string, error) {
	cap, err := n.readCapability()
	if err != nil {
		return cap, "state-invalid", err
	}
	rows, err := n.inventory(ctx, false)
	if err != nil {
		return cap, "unknown", err
	}
	named := []nmConnection{}
	present := false
	for _, row := range rows {
		if row.Name == "vpnkit-local" {
			named = append(named, row)
		}
		if row.UUID == cap.UUID {
			present = true
		}
	}
	if cap.UUID == "" {
		switch len(named) {
		case 0:
			return cap, "missing", nil
		case 1:
			return cap, "foreign", nil
		default:
			return cap, "duplicate", nil
		}
	}
	if !present {
		if cap.Legacy {
			return cap, "stale", errors.New("legacy ownership stale")
		}
		if len(named) == 0 {
			return cap, "stale", nil
		}
		return cap, "foreign-collision", nil
	}
	profile, err := n.connection(ctx, cap.UUID)
	if err != nil || profile.Name != "vpnkit-local" {
		if cap.Legacy {
			return cap, "invalid", errors.New("legacy ownership no longer local")
		}
		return cap, "invalid", nil
	}
	for _, row := range named {
		if row.UUID != cap.UUID {
			return cap, "foreign-collision", nil
		}
	}
	if len(named) != 1 {
		return cap, "duplicate", nil
	}
	if cap.Legacy || checkSource {
		fingerprint, err := n.profileFingerprint()
		if err != nil {
			return cap, "source-invalid", err
		}
		if fingerprint != cap.Fingerprint {
			if cap.Legacy {
				return cap, "drift", errors.New("legacy ownership fingerprint changed")
			}
			return cap, "drift", nil
		}
	}
	if cap.Legacy {
		dir, err := n.stateDir(false)
		if err != nil {
			return cap, "state-invalid", err
		}
		defer unix.Close(dir)
		persist := n.persist
		if persist == nil {
			persist = writeBundle
		}
		if err = persist(dir, map[string][]byte{"networkmanager-state": []byte(cap.UUID + " " + cap.Fingerprint + "\n")}); err != nil {
			return cap, "state-invalid", err
		}
		cap.Legacy = false
	}
	return cap, "owned", nil
}
func (n NetworkManager) active(ctx context.Context, uuid string) (bool, error) {
	rows, err := n.inventory(ctx, true)
	if err != nil {
		return false, err
	}
	found := false
	for _, row := range rows {
		if row.UUID != uuid {
			continue
		}
		if row.Type != "vpn" || found {
			return false, errors.New("ambiguous active UUID")
		}
		found = true
	}
	return found, nil
}
func (n NetworkManager) namedAllowlist(ctx context.Context, old, new string) error {
	rows, err := n.inventory(ctx, false)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Name == "vpnkit-local" && row.UUID != old && row.UUID != new {
			return errors.New("foreign same-name profile")
		}
	}
	return nil
}
func (n NetworkManager) harden(ctx context.Context, uuid, name string) error {
	_, err := n.call(ctx, "connection", "modify", "uuid", uuid, "connection.id", name, "connection.autoconnect", "no", "ipv4.route-metric", "50", "ipv6.method", "disabled")
	return err
}
func (n NetworkManager) delete(ctx context.Context, uuid string) error {
	if !uuidPattern.MatchString(uuid) {
		return errors.New("invalid owned UUID")
	}
	_, err := n.call(ctx, "connection", "delete", "uuid", uuid)
	return err
}

func (n NetworkManager) importProfile(ctx context.Context, cap nmCapability, ownership string) (resultErr error) {
	fingerprint, err := n.profileFingerprint()
	if err != nil {
		return err
	}
	if ownership == "owned" && cap.Fingerprint == fingerprint {
		return nil
	}
	old := ""
	if ownership == "owned" {
		old = cap.UUID
	} else if ownership != "missing" && ownership != "stale" {
		return errors.New("refusing foreign or invalid NetworkManager profile")
	}
	before, err := n.inventory(ctx, false)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, row := range before {
		known[row.UUID] = true
	}
	raw, importErr := n.call(ctx, "connection", "import", "type", "openvpn", "file", n.profilePath())
	matches := map[string]bool{}
	for _, id := range uuidInText.FindAllString(raw, -1) {
		matches[strings.ToLower(id)] = true
	}
	imported := ""
	if len(matches) == 1 {
		for id := range matches {
			imported = id
		}
	}
	if importErr != nil && imported == "" {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer done()
		after, e := n.inventory(cleanup, false)
		if e == nil {
			newIDs := []string{}
			for _, row := range after {
				if !known[row.UUID] {
					newIDs = append(newIDs, row.UUID)
				}
			}
			if len(newIDs) == 1 {
				imported = newIDs[0]
			}
		}
	}
	if imported == "" || known[imported] || imported == old {
		return errors.New("import result is not a unique new UUID")
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			failed := false
			if _, e := n.connection(cleanup, imported); e == nil {
				_ = n.harden(cleanup, imported, "vpnkit-local-import-"+imported)
				if e = n.delete(cleanup, imported); e != nil {
					failed = true
				}
			} else {
				failed = true
			}
			if e := n.writeCapability(cap); e != nil {
				failed = true
			}
			if failed {
				resultErr = errors.New("NetworkManager rollback incomplete")
			} else {
				resultErr = errors.New("NetworkManager operation failed; previous profile and ownership were restored")
			}
		}
	}()
	if _, err = n.connection(ctx, imported); err != nil {
		return errors.New("imported profile could not be proven local")
	}
	if importErr != nil {
		return errors.New("NetworkManager import failed after creating a profile")
	}
	for _, name := range []string{"vpnkit-local-import-" + imported, "vpnkit-local"} {
		if err = n.harden(ctx, imported, name); err != nil {
			return err
		}
		profile, e := n.connection(ctx, imported)
		if e != nil || profile.Name != name {
			return errors.New("profile hardening could not be verified")
		}
		if err = n.namedAllowlist(ctx, old, imported); err != nil {
			return err
		}
	}
	if err = n.writeCapability(nmCapability{UUID: imported, Fingerprint: fingerprint}); err != nil {
		return err
	}
	if old != "" {
		deleteErr := n.delete(ctx, old)
		// A failed reply can follow a successful deletion. Once the old UUID
		// may be gone, restoring its capability would discard the usable new
		// profile and leave ownership pointing at a nonexistent connection.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		rows, e := n.inventory(check, false)
		if e != nil {
			committed = true
			return errors.New("new NetworkManager profile retained; old profile cleanup could not be verified")
		}
		for _, row := range rows {
			if row.UUID == old {
				if deleteErr != nil {
					return deleteErr
				}
				return errors.New("old profile still present")
			}
		}
	}
	committed = true
	return nil
}

func (n NetworkManager) Run(ctx context.Context, action string, yes bool, output io.Writer) error {
	switch action {
	case "plan", "status", "verify":
	case "import", "connect", "disconnect", "remove":
		if !yes {
			return errors.New("mutation requires --yes")
		}
	default:
		return errors.New("invalid NetworkManager action")
	}
	if n.Timeout == 0 {
		n.Timeout = 30 * time.Second
	}
	if n.Timeout < time.Second || n.Timeout > 120*time.Second {
		return errors.New("invalid NetworkManager timeout")
	}
	if err := validateSecretTree(n.Base, nil); err != nil {
		return err
	}
	dir, lockErr := n.stateDir(true)
	if lockErr != nil {
		return lockErr
	}
	defer unix.Close(dir)
	lockCtx, lockCancel := context.WithTimeout(ctx, 5*time.Second)
	defer lockCancel()
	lock, lockErr := lockFile(lockCtx, dir, ".networkmanager.lock")
	if lockErr != nil {
		return lockErr
	}
	defer unix.Close(lock)
	checkSource := action != "import"
	cap, ownership, err := n.assess(ctx, checkSource)
	if err != nil {
		return err
	}
	if action == "import" && ownership == "foreign" {
		if err = n.migrateOwnership(ctx); err != nil {
			return err
		}
		cap, ownership, err = n.assess(ctx, false)
		if err != nil {
			return err
		}
	}
	if action == "plan" || action == "status" {
		configured, active, device := "no", "no", "none"
		profile := "missing"
		if _, e := n.profileFingerprint(); e == nil {
			profile = "ready"
		} else if _, e = runtimeRead(n.profilePath()); e == nil {
			profile = "invalid"
		}
		if ownership == "owned" {
			configured = "yes"
			up, e := n.active(ctx, cap.UUID)
			if e != nil {
				return e
			}
			if up {
				device, e = n.activeDevice(ctx, cap.UUID)
				if errors.Is(e, errNMNotReady) {
					device = "none"
				} else if e != nil {
					return e
				} else {
					active = "yes"
				}
			}
		}
		key := "configured"
		if action == "plan" {
			key = "existing_connection"
		}
		fmt.Fprintf(output, "command=%s\nmutation=none\nconnection=vpnkit-local\n%s=%s\nactive=%s\nownership=%s\ndevice=%s\nprofile=%s\nprivate_values=not_printed\n", action, key, configured, active, ownership, device, profile)
		return nil
	}
	if action == "import" {
		if err = n.importProfile(ctx, cap, ownership); err != nil {
			return err
		}
		result := "ok"
		if fingerprint, e := n.profileFingerprint(); e == nil && ownership == "owned" && cap.Fingerprint == fingerprint {
			result = "already-configured"
		}
		fmt.Fprintf(output, "networkmanager_import=%s\nconnection=vpnkit-local\n", result)
		return nil
	}
	if ownership == "missing" && (action == "disconnect" || action == "remove") {
		fmt.Fprintf(output, "networkmanager_%s=not-configured\nconnection=vpnkit-local\n", action)
		return nil
	}
	if ownership != "owned" {
		return errors.New("refusing unowned or changed NetworkManager profile")
	}
	switch action {
	case "verify":
		device, e := n.activeDevice(ctx, cap.UUID)
		if e != nil {
			return e
		}
		fmt.Fprintf(output, "networkmanager_mapping=pass\nowned_uuid_ip4_tun=pass\nopenvpn_handshake=pass\ndevice=%s\n", device)
		return nil
	case "connect":
		if _, err = n.call(ctx, "--wait", "0", "connection", "up", "uuid", cap.UUID); err != nil {
			return err
		}
		timeout, cancel := context.WithTimeout(ctx, n.Timeout)
		defer cancel()
		for {
			device, e := n.activeDevice(timeout, cap.UUID)
			if e == nil {
				e = hostRoutes(timeout, device)
				if e == nil {
					fmt.Fprintf(output, "networkmanager_connect=ok\nconnection=vpnkit-local\ndevice=%s\n", device)
					return nil
				}
			}
			if e != nil && !errors.Is(e, errNMNotReady) {
				return e
			}
			select {
			case <-timeout.Done():
				return errors.New("NetworkManager connect timed out before the owned tunnel and full-tunnel routes became ready")
			case <-time.After(100 * time.Millisecond):
			}
		}
	case "disconnect", "remove":
		up, e := n.active(ctx, cap.UUID)
		if e != nil {
			return e
		}
		if up {
			if _, err = n.call(ctx, "connection", "down", "uuid", cap.UUID); err != nil {
				return err
			}
		}
		if action == "remove" {
			if err = n.delete(ctx, cap.UUID); err != nil {
				return err
			}
			if err = n.writeCapability(nmCapability{}); err != nil {
				return err
			}
		}
		fmt.Fprintf(output, "networkmanager_%s=ok\nconnection=vpnkit-local\n", action)
		return nil
	}
	return errors.New("unsupported NetworkManager action")
}
