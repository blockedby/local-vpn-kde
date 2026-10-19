//go:build linux

package nativegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// ProbeMaxOutputBytes is the largest result ProbeNetwork will accept from one
// command. ReadOnlyRunner implementations must enforce this limit while
// reading: on overflow they must discard the result and return an error, never
// return a silently truncated (and potentially still valid) prefix. The
// production bounded process runner is deliberately owned by the caller.
const ProbeMaxOutputBytes = 4 << 20

// ReadOnlyRunner is the only host-access seam used by ProbeNetwork. Run must
// execute the exact program and arguments it receives without a shell, honor
// ctx, bound stdout to ProbeMaxOutputBytes as described above, and include a
// non-nil error for non-zero exit, unavailable commands, or output overflow.
// Implementations must not merge stderr into the returned stdout.
type ReadOnlyRunner interface {
	Run(ctx context.Context, program string, args ...string) ([]byte, error)
}

type probeCommand struct {
	program string
	args    []string
}

var probeCommands = []probeCommand{
	{program: "ip", args: []string{"-json", "link", "show"}},
	{program: "ip", args: []string{"-json", "address", "show"}},
	{program: "ip", args: []string{"-json", "-4", "route", "show", "table", "all"}},
	{program: "ip", args: []string{"-json", "-4", "rule", "show"}},
	{program: "ip", args: []string{"netns", "list"}},
	{program: "nft", args: []string{"-j", "list", "tables"}},
	{program: "sysctl", args: []string{"-n", "net.ipv4.ip_forward"}},
	{program: "nmcli", args: []string{"-t", "--escape", "yes", "--fields", "DEVICE,TYPE,STATE", "device", "status"}},
}

// ProbeNetwork obtains the complete IPv4 planning snapshot using a fixed set
// of read-only commands and selects one NetworkManager-proven Ethernet or Wi-Fi
// underlay. It performs no mutation and never invokes a container runtime.
// The returned snapshot must still be passed to PlanNetwork, which owns the
// reserved-range and resource-collision checks.
func ProbeNetwork(ctx context.Context, runner ReadOnlyRunner) (Uplink, Snapshot, error) {
	if runner == nil {
		return Uplink{}, Snapshot{}, errors.New("network probe runner is required")
	}
	outputs := make([][]byte, len(probeCommands))
	for i, command := range probeCommands {
		if err := ctx.Err(); err != nil {
			return Uplink{}, Snapshot{}, err
		}
		output, err := runner.Run(ctx, command.program, command.args...)
		if err != nil {
			return Uplink{}, Snapshot{}, fmt.Errorf("read-only network probe %s failed: %w", command.program, err)
		}
		if len(output) > ProbeMaxOutputBytes {
			return Uplink{}, Snapshot{}, fmt.Errorf("read-only network probe %s exceeded output limit", command.program)
		}
		outputs[i] = output
	}

	interfaces, addresses, err := parseInterfaces(outputs[0], outputs[1])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	routes, routeFacts, err := parseRoutes(outputs[2], interfaces)
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	rules, err := parseRules(outputs[3])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	namespaces, err := parseNamespaces(outputs[4])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	tables, err := parseNFTables(outputs[5])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	forwarding, err := parseForwarding(outputs[6])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	devices, err := parseNMDevices(outputs[7])
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	uplink, err := selectUplink(routeFacts, addresses, devices)
	if err != nil {
		return Uplink{}, Snapshot{}, err
	}
	if _, _, _, err := validateUplink(uplink); err != nil {
		return Uplink{}, Snapshot{}, fmt.Errorf("invalid discovered uplink: %w", err)
	}

	return uplink, Snapshot{
		Interfaces:     interfaces,
		Namespaces:     namespaces,
		Routes:         routes,
		Rules:          rules,
		NFTables:       tables,
		IPv4Forwarding: forwarding,
	}, nil
}

func decodeJSON(data []byte, destination any, label string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("malformed or truncated %s JSON: %w", label, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("malformed %s JSON: trailing value", label)
		}
		return fmt.Errorf("malformed %s JSON: %w", label, err)
	}
	return nil
}

func parseInterfaces(linkData, addressData []byte) ([]InterfaceState, map[string][]netip.Prefix, error) {
	var links []struct {
		Name string `json:"ifname"`
	}
	if err := decodeJSON(linkData, &links, "ip link"); err != nil {
		return nil, nil, err
	}
	if links == nil {
		return nil, nil, errors.New("malformed ip link JSON: expected array")
	}
	states := make([]InterfaceState, 0, len(links))
	indexes := make(map[string]int, len(links))
	for _, link := range links {
		if !safeInterfaceName(link.Name) {
			return nil, nil, fmt.Errorf("invalid interface name in ip link snapshot: %q", link.Name)
		}
		if _, exists := indexes[link.Name]; exists {
			return nil, nil, fmt.Errorf("duplicate interface in ip link snapshot: %q", link.Name)
		}
		indexes[link.Name] = len(states)
		states = append(states, InterfaceState{Name: link.Name})
	}

	var rows []struct {
		Name     string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := decodeJSON(addressData, &rows, "ip address"); err != nil {
		return nil, nil, err
	}
	if rows == nil {
		return nil, nil, errors.New("malformed ip address JSON: expected array")
	}
	parsed := make(map[string][]netip.Prefix, len(rows))
	seenRows := make(map[string]bool, len(rows))
	for _, row := range rows {
		index, exists := indexes[row.Name]
		if !exists || seenRows[row.Name] {
			return nil, nil, fmt.Errorf("inconsistent ip address snapshot for interface %q", row.Name)
		}
		seenRows[row.Name] = true
		for _, info := range row.AddrInfo {
			address, err := netip.ParseAddr(info.Local)
			if err != nil || (info.Family != "inet" && info.Family != "inet6") ||
				(info.Family == "inet") != address.Is4() || info.PrefixLen < 0 || info.PrefixLen > address.BitLen() {
				return nil, nil, fmt.Errorf("invalid address in ip address snapshot for interface %q", row.Name)
			}
			prefix := netip.PrefixFrom(address, info.PrefixLen)
			states[index].Addresses = append(states[index].Addresses, prefix.String())
			parsed[row.Name] = append(parsed[row.Name], prefix)
		}
	}
	return states, parsed, nil
}

type routeFact struct {
	destination string
	gateway     string
	device      string
	preferred   string
	routeType   string
}

func parseRoutes(data []byte, interfaces []InterfaceState) ([]RouteState, []routeFact, error) {
	var rows []map[string]json.RawMessage
	if err := decodeJSON(data, &rows, "ip route"); err != nil {
		return nil, nil, err
	}
	if rows == nil {
		return nil, nil, errors.New("malformed ip route JSON: expected array")
	}
	knownInterfaces := make(map[string]bool, len(interfaces))
	for _, iface := range interfaces {
		knownInterfaces[iface.Name] = true
	}
	states := make([]RouteState, 0, len(rows))
	facts := make([]routeFact, 0, len(rows))
	for _, row := range rows {
		destination, err := optionalString(row, "dst")
		if err != nil {
			return nil, nil, fmt.Errorf("invalid route destination: %w", err)
		}
		if destination == "" {
			destination = "default"
		}
		if destination != "default" {
			prefix, parseErr := netip.ParsePrefix(destination)
			if parseErr != nil {
				// iproute2 omits /32 for host routes, including local-table entries.
				address, addressErr := netip.ParseAddr(destination)
				if addressErr != nil || !address.Is4() {
					return nil, nil, fmt.Errorf("invalid IPv4 route destination %q", destination)
				}
				prefix = netip.PrefixFrom(address, 32)
			}
			if !prefix.Addr().Is4() {
				return nil, nil, fmt.Errorf("invalid IPv4 route destination %q", destination)
			}
			destination = prefix.Masked().String()
		}
		gateway, err := optionalString(row, "gateway")
		if err != nil {
			return nil, nil, fmt.Errorf("invalid route gateway: %w", err)
		}
		if gateway != "" {
			address, parseErr := netip.ParseAddr(gateway)
			if parseErr != nil || !address.Is4() {
				return nil, nil, fmt.Errorf("invalid IPv4 route gateway %q", gateway)
			}
		}
		device, err := optionalString(row, "dev")
		if err != nil || (device != "" && !knownInterfaces[device]) {
			return nil, nil, fmt.Errorf("inconsistent route interface %q", device)
		}
		table, err := numericName(row["table"], map[string]int{"unspec": 0, "default": 253, "main": 254, "local": 255}, 254)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid route table: %w", err)
		}
		protocol, err := numericName(row["protocol"], protocolNumbers, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid route protocol: %w", err)
		}
		preferred, err := optionalString(row, "prefsrc")
		if err != nil {
			return nil, nil, fmt.Errorf("invalid preferred route source: %w", err)
		}
		routeType, err := optionalString(row, "type")
		if err != nil {
			return nil, nil, fmt.Errorf("invalid route type: %w", err)
		}
		states = append(states, RouteState{Table: table, Destination: destination, Gateway: gateway, Interface: device, Protocol: protocol})
		facts = append(facts, routeFact{destination: destination, gateway: gateway, device: device, preferred: preferred, routeType: routeType})
	}
	return states, facts, nil
}

var protocolNumbers = map[string]int{
	"unspec": 0, "redirect": 1, "kernel": 2, "boot": 3, "static": 4,
	"gated": 8, "ra": 9, "mrt": 10, "zebra": 11, "bird": 12,
	"dnrouted": 13, "xorp": 14, "ntk": 15, "dhcp": 16, "mrouted": 17,
	"keepalived": 18, "babel": 42, "bgp": 186, "isis": 187, "ospf": 188,
	"rip": 189, "eigrp": 192,
}

func parseRules(data []byte) ([]RuleState, error) {
	var rows []map[string]json.RawMessage
	if err := decodeJSON(data, &rows, "ip rule"); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, errors.New("malformed ip rule JSON: expected array")
	}
	states := make([]RuleState, 0, len(rows))
	guest := netip.MustParseAddr(GuestAddress)
	for _, row := range rows {
		priority, err := requiredInt(row["priority"])
		if err != nil || priority < 0 {
			return nil, errors.New("invalid priority in IPv4 rule snapshot")
		}
		from, err := ruleSourcePrefix(row)
		if err != nil {
			return nil, fmt.Errorf("invalid source in IPv4 rule snapshot: %w", err)
		}
		if from == "" {
			from = "all"
		}
		iif, err := optionalString(row, "iif")
		if err != nil {
			return nil, fmt.Errorf("invalid input interface in IPv4 rule snapshot: %w", err)
		}
		table, err := numericName(row["table"], map[string]int{"unspec": 0, "default": 253, "main": 254, "local": 255}, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid table in IPv4 rule snapshot: %w", err)
		}
		action, err := optionalString(row, "action")
		if err != nil {
			return nil, fmt.Errorf("invalid action in IPv4 rule snapshot: %w", err)
		}
		if action == "" && table != 0 {
			action = "lookup"
		}
		protocol, err := numericName(row["protocol"], protocolNumbers, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid protocol in IPv4 rule snapshot: %w", err)
		}
		state := RuleState{Priority: priority, From: from, IncomingInterface: iif, Table: table, Action: action, Protocol: protocol}
		states = append(states, state)

		if priority < EgressRulePriority && (ruleMayAffectGuest(from, iif, guest) || ruleMayInvertSelection(row)) && !isStandardLocalRule(row, state) {
			return nil, fmt.Errorf("IPv4 rule priority %d may affect native guest routing before priority %d", priority, EgressRulePriority)
		}
	}
	return states, nil
}

func ruleMayAffectGuest(from, iif string, guest netip.Addr) bool {
	if iif != "" && iif != HostInterface {
		return false
	}
	if from == "all" || from == "0.0.0.0/0" {
		return true
	}
	prefix, err := netip.ParsePrefix(from)
	return err != nil || (prefix.Addr().Is4() && prefix.Contains(guest))
}

// iproute2 may emit the prefix length separately from the source address.
func ruleSourcePrefix(row map[string]json.RawMessage) (string, error) {
	from, err := optionalStringAlias(row, "src", "from")
	if err != nil {
		return "", err
	}
	raw, hasLength := row["srclen"]
	if from == "" || from == "all" {
		if hasLength {
			n, err := requiredInt(raw)
			if err != nil || n != 0 {
				return "", errors.New("invalid rule source length")
			}
		}
		return "all", nil
	}
	var prefix netip.Prefix
	if strings.Contains(from, "/") {
		prefix, err = netip.ParsePrefix(from)
	} else {
		var address netip.Addr
		address, err = netip.ParseAddr(from)
		if err == nil {
			prefix = netip.PrefixFrom(address, 32)
		}
	}
	if err != nil || !prefix.IsValid() || !prefix.Addr().Is4() {
		return "", errors.New("invalid IPv4 rule source")
	}
	if hasLength {
		n, err := requiredInt(raw)
		if err != nil || n < 0 || n > 32 || (strings.Contains(from, "/") && n != prefix.Bits()) {
			return "", errors.New("invalid rule source length")
		}
		prefix = netip.PrefixFrom(prefix.Addr(), n)
	}
	return prefix.Masked().String(), nil
}

func ruleMayInvertSelection(raw map[string]json.RawMessage) bool {
	value, exists := raw["not"]
	if !exists {
		return false
	}
	var inverted bool
	// An unrecognized representation is conservatively treated as active.
	return json.Unmarshal(value, &inverted) != nil || inverted
}

func isStandardLocalRule(raw map[string]json.RawMessage, rule RuleState) bool {
	if rule.Priority != 0 || rule.From != "all" || rule.IncomingInterface != "" || rule.Table != 255 || (rule.Action != "lookup" && rule.Action != "to_tbl") {
		return false
	}
	for key := range raw {
		if key != "priority" && key != "src" && key != "from" && key != "table" && key != "action" && key != "protocol" {
			return false
		}
	}
	return rule.Protocol == 0 || rule.Protocol == 2
}

func parseNamespaces(data []byte) ([]string, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, nil
	}
	result := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "(") || strings.ContainsAny(fields[0], "\x00/ ") || seen[fields[0]] {
			return nil, errors.New("malformed ip netns list output")
		}
		seen[fields[0]] = true
		result = append(result, fields[0])
	}
	return result, nil
}

func parseNFTables(data []byte) ([]NFTTableState, error) {
	var document struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := decodeJSON(data, &document, "nft list tables"); err != nil {
		return nil, err
	}
	if document.NFTables == nil {
		return nil, errors.New("malformed nft list tables JSON: expected nftables array")
	}
	result := make([]NFTTableState, 0, len(document.NFTables))
	for _, entry := range document.NFTables {
		raw, table := entry["table"]
		if !table {
			if _, metadata := entry["metainfo"]; metadata {
				continue
			}
			return nil, errors.New("unexpected object in nft list tables output")
		}
		var value struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || value.Family == "" || value.Name == "" {
			return nil, errors.New("malformed table in nft list tables output")
		}
		result = append(result, NFTTableState{Family: value.Family, Name: value.Name})
	}
	return result, nil
}

func parseForwarding(data []byte) (bool, error) {
	switch strings.TrimSpace(string(data)) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("malformed net.ipv4.ip_forward value")
	}
}

type nmDevice struct {
	physical  bool
	connected bool
}

func parseNMDevices(data []byte) (map[string]nmDevice, error) {
	result := make(map[string]nmDevice)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields, err := splitEscaped(line, ':')
		if err != nil || len(fields) != 3 || !safeInterfaceName(fields[0]) {
			return nil, errors.New("malformed nmcli device snapshot")
		}
		typeName := strings.ToLower(fields[1])
		physical := typeName == "ethernet" || typeName == "wifi" || typeName == "802-3-ethernet" || typeName == "802-11-wireless"
		state := nmDevice{physical: physical, connected: strings.HasPrefix(strings.ToLower(fields[2]), "connected")}
		if old, exists := result[fields[0]]; exists && old != state {
			return nil, fmt.Errorf("conflicting nmcli rows for interface %q", fields[0])
		}
		result[fields[0]] = state
	}
	return result, nil
}

func selectUplink(routes []routeFact, addresses map[string][]netip.Prefix, devices map[string]nmDevice) (Uplink, error) {
	type candidate struct {
		device  string
		gateway netip.Addr
		address netip.Prefix
	}
	candidates := make(map[string]candidate)
	for _, route := range routes {
		broad := route.destination == "default"
		if !broad {
			if prefix, err := netip.ParsePrefix(route.destination); err == nil {
				broad = prefix.Bits() <= 1
			}
		}
		if !broad || route.routeType == "unreachable" || route.routeType == "blackhole" || route.routeType == "prohibit" || route.routeType == "throw" {
			continue
		}
		device := devices[route.device]
		if !device.physical {
			return Uplink{}, fmt.Errorf("non-physical or VPN default route on interface %q", route.device)
		}
		if !device.connected || route.destination != "default" {
			return Uplink{}, fmt.Errorf("physical underlay interface %q is not an unambiguous connected default", route.device)
		}
		gateway, err := netip.ParseAddr(route.gateway)
		if err != nil || !gateway.Is4() {
			return Uplink{}, fmt.Errorf("physical default route on %q has no valid IPv4 gateway", route.device)
		}
		matching := make([]netip.Prefix, 0, 1)
		for _, prefix := range addresses[route.device] {
			if prefix.Addr().Is4() && prefix.Contains(gateway) && usableHostInPrefix(prefix.Masked(), prefix.Addr()) {
				if route.preferred == "" || route.preferred == prefix.Addr().String() {
					matching = append(matching, prefix)
				}
			}
		}
		if len(matching) != 1 {
			return Uplink{}, fmt.Errorf("physical default route on %q has ambiguous or missing IPv4 address", route.device)
		}
		key := route.device + "\x00" + gateway.String() + "\x00" + matching[0].String()
		candidates[key] = candidate{device: route.device, gateway: gateway, address: matching[0]}
	}
	if len(candidates) != 1 {
		return Uplink{}, fmt.Errorf("expected exactly one physical Ethernet/Wi-Fi uplink, found %d", len(candidates))
	}
	for _, value := range candidates {
		return Uplink{Interface: value.device, AddressCIDR: value.address.String(), Gateway: value.gateway.String()}, nil
	}
	panic("unreachable")
}

func optionalString(object map[string]json.RawMessage, key string) (string, error) {
	raw, exists := object[key]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s is not a string", key)
	}
	return value, nil
}

func optionalStringAlias(object map[string]json.RawMessage, first, second string) (string, error) {
	one, err := optionalString(object, first)
	if err != nil {
		return "", err
	}
	two, err := optionalString(object, second)
	if err != nil {
		return "", err
	}
	if one != "" && two != "" && one != two {
		return "", errors.New("conflicting aliases")
	}
	if one != "" {
		return one, nil
	}
	return two, nil
}

func requiredInt(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing integer")
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("not an integer")
	}
	return value, nil
}

func numericName(raw json.RawMessage, names map[string]int, defaultValue int) (int, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return defaultValue, nil
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return 0, errors.New("value is neither a number nor a name")
	}
	if value, exists := names[name]; exists {
		return value, nil
	}
	value, err := strconv.Atoi(name)
	if err != nil {
		return 0, fmt.Errorf("unknown name %q", name)
	}
	return value, nil
}

func splitEscaped(value string, separator byte) ([]string, error) {
	var fields []string
	var field strings.Builder
	escaped := false
	for i := 0; i < len(value); i++ {
		character := value[i]
		if escaped {
			field.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == separator {
			fields = append(fields, field.String())
			field.Reset()
			continue
		}
		field.WriteByte(character)
	}
	if escaped {
		return nil, errors.New("trailing escape")
	}
	return append(fields, field.String()), nil
}
