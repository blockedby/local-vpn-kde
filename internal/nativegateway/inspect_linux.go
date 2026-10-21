//go:build linux

package nativegateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// LinuxInspectorFilesystem is the read-only filesystem seam used by the
// inspector. StatNamespace implementations must inspect an opened descriptor,
// not follow a pathname and then stat it.
type LinuxInspectorFilesystem interface {
	ReadFile(string) ([]byte, error)
	StatNamespace(string) (LinuxNamespaceStat, error)
}

// LinuxNamespaceStat contains the attributes needed to prove that a netns
// marker is a root-owned NSFS mount and to retain its immutable identity.
type LinuxNamespaceStat struct {
	Device uint64
	Inode  uint64
	UID    uint32
	FSType int64
}

type hostInspectorFilesystem struct{}

func (hostInspectorFilesystem) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (hostInspectorFilesystem) StatNamespace(path string) (LinuxNamespaceStat, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return LinuxNamespaceStat{}, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	var statfs unix.Statfs_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return LinuxNamespaceStat{}, err
	}
	if err := unix.Fstatfs(fd, &statfs); err != nil {
		return LinuxNamespaceStat{}, err
	}
	return LinuxNamespaceStat{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid, FSType: statfs.Type}, nil
}

// LinuxNetworkInspector compares the complete reserved kernel state with the
// fixed PlanNetwork contract. It executes only fixed, read-only commands.
// Individual command/read failures produce Unverifiable observations; malformed
// successful output is an inspection error because no safe interpretation is
// possible.
//
// Inspection is a sequence of kernel reads, not an atomic kernel snapshot.
// Callers must serialize transaction mutations. Immutable namespace/interface
// identities expose replacements between inspections, but a concurrent actor
// can still create a TOCTOU inconsistency between two reads in one inspection;
// such concurrent administration is outside the transaction ownership model.
type LinuxNetworkInspector struct {
	runner ReadOnlyRunner
	files  LinuxInspectorFilesystem
}

// NewLinuxNetworkInspector constructs the production inspector. The optional
// filesystem argument exists for deterministic tests; production callers omit
// it and receive the descriptor-based implementation above.
func NewLinuxNetworkInspector(runner ReadOnlyRunner, files ...LinuxInspectorFilesystem) *LinuxNetworkInspector {
	var filesystem LinuxInspectorFilesystem = hostInspectorFilesystem{}
	if len(files) == 1 && files[0] != nil {
		filesystem = files[0]
	}
	return &LinuxNetworkInspector{runner: runner, files: filesystem}
}

func (i *LinuxNetworkInspector) Snapshot(ctx context.Context) (Snapshot, error) {
	if i == nil || i.runner == nil {
		return Snapshot{}, errors.New("network inspector runner is required")
	}
	_, snapshot, err := ProbeNetwork(ctx, i.runner)
	return snapshot, err
}

type inspectLink struct {
	Name      string
	Kind      string
	Index     int
	PeerIndex int
	Up        bool
}

type inspectAddress struct {
	Name  string
	CIDRs []string
}

type inspectRoute struct {
	Dst, Gateway, Dev, Preferred, Scope, Type string
	Table, Protocol                           int
	raw                                       map[string]json.RawMessage
}

type inspectRule struct {
	Priority, Table, Protocol int
	From, IIF, Action         string
	raw                       map[string]json.RawMessage
}

type inspectFacts struct {
	forwardOK      bool
	forward        string
	namespaceState ObservationState
	namespace      NamespaceIdentity
	rootLinksOK    bool
	rootLinks      map[string]inspectLink
	rootAddrsOK    bool
	rootAddrs      map[string]inspectAddress
	guestLinksOK   bool
	guestLinks     map[string]inspectLink
	guestAddrsOK   bool
	guestAddrs     map[string]inspectAddress
	routesOK       bool
	routes         []inspectRoute
	guestRoutesOK  bool
	guestRoutes    []inspectRoute
	rulesOK        bool
	rules          []inspectRule
	nftOK          bool
	nft            any
	files          map[string]fileFact
}

type fileFact struct {
	ok    bool
	value string
}

func (i *LinuxNetworkInspector) Inspect(ctx context.Context, expected NetworkExpectation) (NetworkInspection, error) {
	var result NetworkInspection
	if i == nil || i.runner == nil || i.files == nil {
		return result, errors.New("network inspector dependencies are required")
	}
	if err := validateInspectionExpectation(expected); err != nil {
		return result, err
	}
	facts, err := i.readFacts(ctx)
	if err != nil {
		return result, err
	}
	for n := 0; n < int(expected.EffectCount); n++ {
		result.Effects[n] = observeEffect(expected.Effects[n], expected.Uplink, facts)
	}
	return result, nil
}

func validateInspectionExpectation(expected NetworkExpectation) error {
	if _, _, _, err := validateUplink(expected.Uplink); err != nil {
		return fmt.Errorf("invalid inspection uplink: %w", err)
	}
	commands, err := PlanNetwork(expected.Uplink, Snapshot{IPv4Forwarding: expected.InitialIPv4Forwarding})
	if err != nil {
		return err
	}
	kinds, err := effectKinds(commands, expected.InitialIPv4Forwarding)
	if err != nil || int(expected.EffectCount) != len(kinds) {
		return errors.New("invalid inspection effect set")
	}
	for n, kind := range kinds {
		if expected.Effects[n] != kind {
			return errors.New("invalid inspection effect order")
		}
	}
	for n := len(kinds); n < MaxNetworkEffects; n++ {
		if expected.Effects[n] != 0 {
			return errors.New("invalid trailing inspection effect")
		}
	}
	return nil
}

func (i *LinuxNetworkInspector) readFacts(ctx context.Context) (inspectFacts, error) {
	facts := inspectFacts{files: make(map[string]fileFact)}
	stat, err := i.files.StatNamespace("/run/netns/" + Namespace)
	switch {
	case err == nil && stat.UID == 0 && stat.FSType == unix.NSFS_MAGIC && stat.Device != 0 && stat.Inode != 0:
		facts.namespaceState = ObservationExact
		facts.namespace = NamespaceIdentity{Device: stat.Device, Inode: stat.Inode}
	case err == nil:
		facts.namespaceState = ObservationConflict
	case errors.Is(err, fs.ErrNotExist):
		facts.namespaceState = ObservationAbsent
	default:
		facts.namespaceState = ObservationUnverifiable
	}

	forward, forwardOK := i.run(ctx, "iptables", "-w", "5", "-S", "FORWARD")
	facts.forward, facts.forwardOK = string(forward), forwardOK
	var parseErr error
	facts.rootLinks, facts.rootLinksOK, parseErr = i.readLinks(ctx, "ip", "-json", "-details", "link", "show")
	if parseErr != nil {
		return facts, parseErr
	}
	facts.rootAddrs, facts.rootAddrsOK, parseErr = i.readAddresses(ctx, "ip", "-json", "address", "show")
	if parseErr != nil {
		return facts, parseErr
	}
	facts.routes, facts.routesOK, parseErr = i.readInspectRoutes(ctx, "ip", "-json", "-4", "route", "show", "table", strconv.Itoa(RoutingTable))
	if parseErr != nil {
		return facts, parseErr
	}
	if !facts.routesOK {
		// A never-created routing table is not an empty successful scoped
		// query: iproute2 reports "FIB table does not exist" with nonzero
		// status after reboot. Prove presence/absence from a successful
		// complete dump instead of trusting the error text or assuming empty.
		facts.routes, facts.routesOK, parseErr = i.readInspectRoutes(ctx, "ip", "-json", "-4", "route", "show", "table", "all")
		if parseErr != nil {
			return facts, parseErr
		}
	}
	facts.rules, facts.rulesOK, parseErr = i.readInspectRules(ctx, "ip", "-json", "-4", "rule", "show")
	if parseErr != nil {
		return facts, parseErr
	}

	if facts.namespaceState == ObservationExact {
		facts.guestLinks, facts.guestLinksOK, parseErr = i.readLinks(ctx, "ip", "-n", Namespace, "-json", "-details", "link", "show")
		if parseErr != nil {
			return facts, parseErr
		}
		facts.guestAddrs, facts.guestAddrsOK, parseErr = i.readAddresses(ctx, "ip", "-n", Namespace, "-json", "address", "show")
		if parseErr != nil {
			return facts, parseErr
		}
		facts.guestRoutes, facts.guestRoutesOK, parseErr = i.readInspectRoutes(ctx, "ip", "-n", Namespace, "-json", "-4", "route", "show", "table", "main")
		if parseErr != nil {
			return facts, parseErr
		}
		for _, key := range []string{"net.ipv6.conf.all.disable_ipv6", "net.ipv6.conf.default.disable_ipv6"} {
			out, ok := i.run(ctx, "ip", "netns", "exec", Namespace, "sysctl", "-n", key)
			facts.files["netns:"+key] = fileFact{ok: ok, value: strings.TrimSpace(string(out))}
		}
	}

	out, ok := i.run(ctx, "nft", "-j", "list", "tables")
	if ok {
		tables, err := parseNFTables(out)
		if err != nil {
			return facts, err
		}
		facts.nftOK = true
		present := false
		for _, table := range tables {
			if table.Family == NFTFamily && table.Name == NFTTable {
				present = true
			}
		}
		if present {
			out, facts.nftOK = i.run(ctx, "nft", "-j", "list", "table", NFTFamily, NFTTable)
			if facts.nftOK {
				if err := decodeJSON(out, &facts.nft, "nft table"); err != nil {
					return facts, err
				}
			}
		}
	}
	for _, path := range []string{
		"/proc/sys/net/ipv4/conf/" + HostInterface + "/route_localnet",
		"/proc/sys/net/ipv4/ip_forward",
	} {
		data, readErr := i.files.ReadFile(path)
		facts.files[path] = fileFact{ok: readErr == nil, value: strings.TrimSpace(string(data))}
	}
	return facts, nil
}

func (i *LinuxNetworkInspector) run(ctx context.Context, program string, args ...string) ([]byte, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	out, err := i.runner.Run(ctx, program, args...)
	if err != nil || len(out) > ProbeMaxOutputBytes {
		return nil, false
	}
	return out, true
}

func (i *LinuxNetworkInspector) readLinks(ctx context.Context, program string, args ...string) (map[string]inspectLink, bool, error) {
	data, ok := i.run(ctx, program, args...)
	if !ok {
		return nil, false, nil
	}
	var rows []struct {
		Name      string   `json:"ifname"`
		Index     int      `json:"ifindex"`
		PeerIndex int      `json:"link_index"`
		PeerName  string   `json:"link"`
		Flags     []string `json:"flags"`
		LinkInfo  struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	if err := decodeJSON(data, &rows, "ip link"); err != nil {
		return nil, false, err
	}
	if rows == nil {
		return nil, false, errors.New("malformed ip link JSON: expected array")
	}
	result := make(map[string]inspectLink, len(rows))
	for _, row := range rows {
		if !safeInterfaceName(row.Name) || row.Index <= 0 {
			return nil, false, errors.New("invalid interface in ip link JSON")
		}
		if _, exists := result[row.Name]; exists {
			return nil, false, errors.New("duplicate interface in ip link JSON")
		}
		link := inspectLink{Name: row.Name, Kind: row.LinkInfo.Kind, Index: row.Index, PeerIndex: row.PeerIndex}
		for _, flag := range row.Flags {
			if flag == "UP" {
				link.Up = true
			}
		}
		result[row.Name] = link
	}
	// For peers in this namespace iproute2 emits a name, not link_index.
	// Resolve only against this complete snapshot, never against a guessed ID.
	for _, row := range rows {
		if row.PeerName == "" {
			continue
		}
		peer, exists := result[row.PeerName]
		if !exists {
			return nil, false, errors.New("unresolved interface peer in ip link JSON")
		}
		link := result[row.Name]
		if link.PeerIndex != 0 && link.PeerIndex != peer.Index {
			return nil, false, errors.New("inconsistent interface peer in ip link JSON")
		}
		link.PeerIndex = peer.Index
		result[row.Name] = link
	}
	return result, true, nil
}

func (i *LinuxNetworkInspector) readAddresses(ctx context.Context, program string, args ...string) (map[string]inspectAddress, bool, error) {
	data, ok := i.run(ctx, program, args...)
	if !ok {
		return nil, false, nil
	}
	var rows []struct {
		Name string `json:"ifname"`
		Info []struct {
			Family, Local string
			PrefixLen     int `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := decodeJSON(data, &rows, "ip address"); err != nil {
		return nil, false, err
	}
	if rows == nil {
		return nil, false, errors.New("malformed ip address JSON: expected array")
	}
	result := make(map[string]inspectAddress, len(rows))
	for _, row := range rows {
		if !safeInterfaceName(row.Name) {
			return nil, false, errors.New("invalid interface in ip address JSON")
		}
		if _, exists := result[row.Name]; exists {
			return nil, false, errors.New("duplicate interface in ip address JSON")
		}
		entry := inspectAddress{Name: row.Name}
		for _, info := range row.Info {
			addr, err := netip.ParseAddr(info.Local)
			if err != nil || info.PrefixLen < 0 || info.PrefixLen > addr.BitLen() || (info.Family == "inet") != addr.Is4() || (info.Family != "inet" && info.Family != "inet6") {
				return nil, false, errors.New("invalid address in ip address JSON")
			}
			entry.CIDRs = append(entry.CIDRs, netip.PrefixFrom(addr, info.PrefixLen).String())
		}
		result[row.Name] = entry
	}
	return result, true, nil
}

func (i *LinuxNetworkInspector) readInspectRoutes(ctx context.Context, program string, args ...string) ([]inspectRoute, bool, error) {
	data, ok := i.run(ctx, program, args...)
	if !ok {
		return nil, false, nil
	}
	var rows []map[string]json.RawMessage
	if err := decodeJSON(data, &rows, "ip route"); err != nil {
		return nil, false, err
	}
	if rows == nil {
		return nil, false, errors.New("malformed ip route JSON: expected array")
	}
	result := make([]inspectRoute, 0, len(rows))
	for _, raw := range rows {
		dst, err := optionalString(raw, "dst")
		if err != nil {
			return nil, false, err
		}
		if dst == "" {
			dst = "default"
		}
		gateway, err := optionalString(raw, "gateway")
		if err != nil {
			return nil, false, err
		}
		dev, err := optionalString(raw, "dev")
		if err != nil {
			return nil, false, err
		}
		preferred, err := optionalString(raw, "prefsrc")
		if err != nil {
			return nil, false, err
		}
		scope, err := optionalString(raw, "scope")
		if err != nil {
			return nil, false, err
		}
		typeName, err := optionalString(raw, "type")
		if err != nil {
			return nil, false, err
		}
		if typeName == "" {
			typeName = "unicast"
		}
		// A table-scoped iproute2 query omits the table field in each row.
		// Preserve the fixed query's context rather than assuming main.
		queryTable := 254
		for n := 0; n+1 < len(args); n++ {
			if args[n] == "table" && args[n+1] == strconv.Itoa(RoutingTable) {
				queryTable = RoutingTable
			}
		}
		table, err := numericName(raw["table"], map[string]int{"main": 254, "local": 255, "default": 253, "unspec": 0}, queryTable)
		if err != nil {
			return nil, false, err
		}
		// iproute2's non-detailed route output elides the default boot protocol.
		protocol, err := numericName(raw["protocol"], protocolNumbers, 3)
		if err != nil {
			return nil, false, err
		}
		result = append(result, inspectRoute{Dst: dst, Gateway: gateway, Dev: dev, Preferred: preferred, Scope: scope, Type: typeName, Table: table, Protocol: protocol, raw: raw})
	}
	return result, true, nil
}

func (i *LinuxNetworkInspector) readInspectRules(ctx context.Context, program string, args ...string) ([]inspectRule, bool, error) {
	data, ok := i.run(ctx, program, args...)
	if !ok {
		return nil, false, nil
	}
	var rows []map[string]json.RawMessage
	if err := decodeJSON(data, &rows, "ip rule"); err != nil {
		return nil, false, err
	}
	if rows == nil {
		return nil, false, errors.New("malformed ip rule JSON: expected array")
	}
	result := make([]inspectRule, 0, len(rows))
	for _, raw := range rows {
		priority, err := requiredInt(raw["priority"])
		if err != nil {
			return nil, false, err
		}
		from, err := ruleSourcePrefix(raw)
		if err != nil {
			return nil, false, err
		}
		if from == "" {
			from = "all"
		}
		iif, err := optionalString(raw, "iif")
		if err != nil {
			return nil, false, err
		}
		table, err := numericName(raw["table"], map[string]int{"main": 254, "local": 255, "default": 253, "unspec": 0}, 0)
		if err != nil {
			return nil, false, err
		}
		action, err := optionalStringAlias(raw, "action", "type")
		if err != nil {
			return nil, false, err
		}
		if action == "" && table != 0 {
			action = "lookup"
		}
		if action == "to_tbl" {
			action = "lookup"
		}
		if address, parseErr := netip.ParseAddr(from); parseErr == nil && address.Is4() {
			from = netip.PrefixFrom(address, 32).String()
		}
		protocol, err := numericName(raw["protocol"], protocolNumbers, 0)
		if err != nil {
			return nil, false, err
		}
		result = append(result, inspectRule{Priority: priority, Table: table, Protocol: protocol, From: from, IIF: iif, Action: action, raw: raw})
	}
	return result, true, nil
}

func observeEffect(kind EffectKind, uplink Uplink, f inspectFacts) EffectObservation {
	nsID := CreatedIdentity{Namespace: f.namespace}
	host, hostExists := f.rootLinks[HostInterface]
	guestRoot, guestRootExists := f.rootLinks[GuestInterface]
	guestNS, guestNSExists := f.guestLinks[GuestInterface]
	vethID := CreatedIdentity{Veth: VethIdentity{HostIfIndex: host.Index}}
	if guestNSExists {
		vethID.Veth.GuestIfIndex = guestNS.Index
	} else if guestRootExists {
		vethID.Veth.GuestIfIndex = guestRoot.Index
	}
	pairExact := hostExists && host.Kind == "veth" && vethID.Veth.GuestIfIndex > 0 && host.PeerIndex == vethID.Veth.GuestIfIndex
	if pairExact {
		peer := guestRoot
		if guestNSExists {
			peer = guestNS
		}
		pairExact = peer.Kind == "veth" && (peer.PeerIndex == 0 || peer.PeerIndex == host.Index)
	}
	movedID := CreatedIdentity{Namespace: f.namespace, Veth: vethID.Veth}

	switch kind {
	case EffectNamespace:
		return EffectObservation{State: f.namespaceState, Identity: nsID}
	case EffectVeth:
		if !f.rootLinksOK {
			return observation(ObservationUnverifiable)
		}
		// Before the move, the complete pair is provable entirely in root.
		if pairExact && guestRootExists {
			return EffectObservation{State: ObservationExact, Identity: vethID}
		}
		if f.namespaceState == ObservationExact {
			if !f.guestLinksOK {
				return observation(ObservationUnverifiable)
			}
			if pairExact && guestNSExists {
				return EffectObservation{State: ObservationExact, Identity: vethID}
			}
			if !hostExists && !guestRootExists && !guestNSExists {
				return observation(ObservationAbsent)
			}
			return observation(ObservationConflict)
		}
		if f.namespaceState == ObservationAbsent && !hostExists && !guestRootExists {
			return observation(ObservationAbsent)
		}
		if f.namespaceState == ObservationConflict || f.namespaceState == ObservationUnverifiable {
			return observation(ObservationUnverifiable)
		}
		return observation(ObservationConflict)
	case EffectGuestMoved:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact || !f.rootLinksOK || !f.guestLinksOK {
			return observation(ObservationUnverifiable)
		}
		if pairExact && guestNSExists && !guestRootExists {
			return EffectObservation{State: ObservationExact, Identity: movedID}
		}
		if pairExact && guestRootExists && !guestNSExists {
			return observation(ObservationAbsent)
		}
		if !guestNSExists && !guestRootExists && !hostExists {
			return observation(ObservationAbsent)
		}
		return observation(ObservationConflict)
	case EffectHostAddress:
		return observeAddress(f.rootAddrs, f.rootAddrsOK, HostInterface, HostAddress+"/30", !hostExists && f.rootLinksOK)
	case EffectGuestAddress:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		return observeAddress(f.guestAddrs, f.guestAddrsOK, GuestInterface, GuestAddress+"/30", !guestNSExists && f.guestLinksOK)
	case EffectConnectedRoute:
		address, prefix, _, _ := validateUplink(uplink)
		want := inspectRoute{Dst: prefix.String(), Dev: uplink.Interface, Preferred: address.String(), Scope: "link", Type: "unicast", Table: RoutingTable, Protocol: RouteProtocol}
		other := inspectRoute{Dst: "default", Gateway: uplink.Gateway, Dev: uplink.Interface, Type: "unicast", Table: RoutingTable, Protocol: RouteProtocol}
		return observeOwnedRoute(f.routes, f.routesOK, want, &other, false)
	case EffectDefaultUplinkRoute:
		want := inspectRoute{Dst: "default", Gateway: uplink.Gateway, Dev: uplink.Interface, Type: "unicast", Table: RoutingTable, Protocol: RouteProtocol}
		address, prefix, _, _ := validateUplink(uplink)
		other := inspectRoute{Dst: prefix.String(), Dev: uplink.Interface, Preferred: address.String(), Scope: "link", Type: "unicast", Table: RoutingTable, Protocol: RouteProtocol}
		return observeOwnedRoute(f.routes, f.routesOK, want, &other, false)
	case EffectGuestDefaultRoute:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		want := inspectRoute{Dst: "default", Gateway: HostAddress, Dev: GuestInterface, Type: "unicast", Table: 254, Protocol: 3}
		return observeOwnedRoute(f.guestRoutes, f.guestRoutesOK, want, nil, true)
	case EffectUnreachableRule:
		want := inspectRule{Priority: UnreachableRulePriority, From: GuestAddress + "/32", Action: "unreachable", Protocol: RouteProtocol}
		return observeOwnedRule(f.rules, f.rulesOK, want)
	case EffectEgressRule:
		want := inspectRule{Priority: EgressRulePriority, From: GuestAddress + "/32", IIF: HostInterface, Table: RoutingTable, Action: "lookup", Protocol: RouteProtocol}
		return observeOwnedRule(f.rules, f.rulesOK, want)
	case EffectForwardEgress, EffectForwardReturn:
		if !f.forwardOK {
			return observation(ObservationUnverifiable)
		}
		return observation(observeForwardCompatibility(f.forward, uplink.Interface, kind == EffectForwardReturn))
	case EffectNFTTable:
		if !f.nftOK {
			return observation(ObservationUnverifiable)
		}
		if f.nft == nil {
			return observation(ObservationAbsent)
		}
		if reflect.DeepEqual(normalizeNFT(f.nft), normalizeNFT(expectedNFT(uplink.Interface))) {
			return observation(ObservationExact)
		}
		return observation(ObservationConflict)
	case EffectRouteLocalnet:
		if !hostExists && f.rootLinksOK {
			return observation(ObservationAbsent)
		}
		return observeBooleanFile(f.files["/proc/sys/net/ipv4/conf/"+HostInterface+"/route_localnet"])
	case EffectIPv4Forwarding:
		return observeBooleanFile(f.files["/proc/sys/net/ipv4/ip_forward"])
	case EffectDisableIPv6All:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		return observeBooleanFile(f.files["netns:net.ipv6.conf.all.disable_ipv6"])
	case EffectDisableIPv6Default:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		return observeBooleanFile(f.files["netns:net.ipv6.conf.default.disable_ipv6"])
	case EffectHostUp:
		return observeUp(host, hostExists, f.rootLinksOK)
	case EffectLoopbackUp:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		link, exists := f.guestLinks["lo"]
		return observeUp(link, exists, f.guestLinksOK)
	case EffectGuestUp:
		if f.namespaceState == ObservationAbsent {
			return observation(ObservationAbsent)
		}
		if f.namespaceState != ObservationExact {
			return observation(ObservationUnverifiable)
		}
		return observeUp(guestNS, guestNSExists, f.guestLinksOK)
	default:
		return observation(ObservationUnverifiable)
	}
}

func observation(state ObservationState) EffectObservation { return EffectObservation{State: state} }

func observeUp(link inspectLink, exists, available bool) EffectObservation {
	if !available {
		return observation(ObservationUnverifiable)
	}
	if !exists || !link.Up {
		return observation(ObservationAbsent)
	}
	return observation(ObservationExact)
}

func observeAddress(all map[string]inspectAddress, available bool, name, want string, interfaceAbsent bool) EffectObservation {
	if !available {
		return observation(ObservationUnverifiable)
	}
	entry, exists := all[name]
	if !exists {
		if interfaceAbsent {
			return observation(ObservationAbsent)
		}
		return observation(ObservationUnverifiable)
	}
	found := false
	for _, cidr := range entry.CIDRs {
		if cidr == want {
			found = true
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Is4() {
			return observation(ObservationConflict)
		}
	}
	if found {
		return observation(ObservationExact)
	}
	return observation(ObservationAbsent)
}

func routeEqual(a, b inspectRoute) bool {
	return a.Dst == b.Dst && a.Gateway == b.Gateway && a.Dev == b.Dev && a.Preferred == b.Preferred && a.Scope == b.Scope && a.Type == b.Type && a.Table == b.Table && a.Protocol == b.Protocol &&
		onlyJSONFields(a.raw, "dst", "gateway", "dev", "prefsrc", "scope", "type", "table", "protocol", "flags")
}

func observeOwnedRoute(routes []inspectRoute, available bool, want inspectRoute, other *inspectRoute, guest bool) EffectObservation {
	if !available {
		return observation(ObservationUnverifiable)
	}
	found := false
	for _, route := range routes {
		owned := route.Table == RoutingTable
		if guest {
			owned = route.Dst == "default"
		}
		if !owned {
			continue
		}
		if routeEqual(route, want) {
			found = true
			continue
		}
		// The other exact planned host-table route is allowed during partial install.
		if !guest && other != nil && routeEqual(route, *other) {
			continue
		}
		return observation(ObservationConflict)
	}
	if found {
		return observation(ObservationExact)
	}
	return observation(ObservationAbsent)
}

func ruleEqual(a, b inspectRule) bool {
	return a.Priority == b.Priority && a.Table == b.Table && a.Protocol == b.Protocol && a.From == b.From && a.IIF == b.IIF && a.Action == b.Action &&
		onlyJSONFields(a.raw, "priority", "src", "from", "srclen", "iif", "table", "action", "type", "protocol", "flags")
}

func onlyJSONFields(raw map[string]json.RawMessage, allowed ...string) bool {
	if raw == nil { // Expected values are constructed rather than decoded.
		return true
	}
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key, value := range raw {
		if !set[key] {
			return false
		}
		if key == "flags" && string(value) != "[]" && string(value) != "null" {
			return false
		}
	}
	return true
}

func observeOwnedRule(rules []inspectRule, available bool, want inspectRule) EffectObservation {
	if !available {
		return observation(ObservationUnverifiable)
	}
	found := false
	for _, rule := range rules {
		if rule.Priority == want.Priority {
			if ruleEqual(rule, want) {
				found = true
				continue
			}
			return observation(ObservationConflict)
		}
		if rule.Priority != EgressRulePriority && rule.Priority != UnreachableRulePriority && rule.Table != RoutingTable {
			continue
		}
		otherPlanned := (ruleEqual(rule, inspectRule{Priority: EgressRulePriority, From: GuestAddress + "/32", IIF: HostInterface, Table: RoutingTable, Action: "lookup", Protocol: RouteProtocol}) ||
			ruleEqual(rule, inspectRule{Priority: UnreachableRulePriority, From: GuestAddress + "/32", Action: "unreachable", Protocol: RouteProtocol}))
		if !otherPlanned {
			return observation(ObservationConflict)
		}
	}
	if found {
		return observation(ObservationExact)
	}
	return observation(ObservationAbsent)
}

func observeBooleanFile(value fileFact) EffectObservation {
	if !value.ok {
		return observation(ObservationUnverifiable)
	}
	switch value.value {
	case "1":
		return observation(ObservationExact)
	case "0":
		return observation(ObservationAbsent)
	default:
		return observation(ObservationUnverifiable)
	}
}

// normalizeNFT strips only kernel-assigned handles, counters and metainfo.
// Every other object, selector, expression and ordering remains significant.
func normalizeNFT(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "handle" || key == "metainfo" {
				continue
			}
			if key == "counter" {
				continue
			}
			result[key] = normalizeNFT(child)
		}
		if raw, exists := result["set"]; exists {
			if members, ok := raw.([]any); ok {
				slices.SortFunc(members, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
				result["set"] = members
			}
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			if object, ok := child.(map[string]any); ok {
				if _, counter := object["counter"]; counter && len(object) == 1 {
					continue
				}
				if _, metadata := object["metainfo"]; metadata && len(object) == 1 {
					continue
				}
			}
			result = append(result, normalizeNFT(child))
		}
		return result
	default:
		return value
	}
}

func expectedNFT(uplink string) any {
	match := func(left, right any) any {
		return map[string]any{"match": map[string]any{"op": "==", "left": left, "right": right}}
	}
	meta := func(key string) any { return map[string]any{"meta": map[string]any{"key": key}} }
	payload := func(field string) any {
		return map[string]any{"payload": map[string]any{"protocol": "ip", "field": field}}
	}
	udp := func(field string) any {
		return map[string]any{"payload": map[string]any{"protocol": "udp", "field": field}}
	}
	ct := map[string]any{"ct": map[string]any{"key": "state"}}
	member := func(left any, right ...any) any {
		return map[string]any{"match": map[string]any{"op": "in", "left": left, "right": right}}
	}
	rule := func(chain string, expr ...any) any {
		return map[string]any{"rule": map[string]any{"family": NFTFamily, "table": NFTTable, "chain": chain, "expr": expr}}
	}
	// nft's list-table JSON emits all chain declarations before the rules and
	// canonicalizes membership tests to the "in" operator with an array.
	objects := []any{
		map[string]any{"table": map[string]any{"family": NFTFamily, "name": NFTTable}},
		map[string]any{"chain": map[string]any{"family": NFTFamily, "table": NFTTable, "name": "output_nat", "type": "nat", "hook": "output", "prio": float64(-100), "policy": "accept"}},
		map[string]any{"chain": map[string]any{"family": NFTFamily, "table": NFTTable, "name": "postrouting_nat", "type": "nat", "hook": "postrouting", "prio": float64(100), "policy": "accept"}},
		map[string]any{"chain": map[string]any{"family": NFTFamily, "table": NFTTable, "name": "forward", "type": "filter", "hook": "forward", "prio": float64(0), "policy": "accept"}},
		rule("output_nat", match(meta("oifname"), "lo"), match(payload("saddr"), "127.0.0.1"), match(payload("daddr"), "127.0.0.1"), match(udp("dport"), float64(LoopbackPort)), map[string]any{"dnat": map[string]any{"family": "ip", "addr": GuestAddress, "port": float64(GatewayPort)}}),
		rule("postrouting_nat", match(meta("oifname"), HostInterface), match(payload("saddr"), "127.0.0.1"), match(payload("daddr"), GuestAddress), match(udp("dport"), float64(GatewayPort)), map[string]any{"snat": map[string]any{"family": "ip", "addr": HostAddress}}),
		rule("postrouting_nat", match(meta("iifname"), HostInterface), match(meta("oifname"), uplink), match(payload("saddr"), GuestAddress), map[string]any{"masquerade": nil}),
		rule("forward", match(meta("iifname"), HostInterface), match(meta("oifname"), uplink), match(payload("saddr"), GuestAddress), member(ct, "established", "related", "new"), map[string]any{"accept": nil}),
		rule("forward", match(meta("iifname"), uplink), match(meta("oifname"), HostInterface), match(payload("daddr"), GuestAddress), member(ct, "established", "related"), map[string]any{"accept": nil}),
		rule("forward", match(meta("iifname"), HostInterface), map[string]any{"drop": nil}),
		rule("forward", match(meta("oifname"), HostInterface), map[string]any{"drop": nil}),
	}
	return map[string]any{"nftables": objects}
}
