//go:build linux

package localvpn

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"golang.org/x/sys/unix"
)

const (
	nativeServiceUnitDirectory = nativegateway.UnitDirectory

	// hostCommand already imposes the same bound for production commands. Keep
	// the bound here as well so injected commands have the same contract.
	nativeServiceCommandTimeout   = 15 * time.Second
	nativeServiceLifecycleTimeout = 2 * time.Minute
	nativeServiceOutputLimit      = 64 * 1024
	nativeServiceUnitLimit        = 64 * 1024

	nativeServiceShowProperties = "LoadState,ActiveState,SubState,Type,DropInPaths,FragmentPath,NeedDaemonReload"
)

var errNativeServiceOutputLimit = errors.New("native systemd response exceeded limit")

// NativeService is the unprivileged client for the two fixed, already-installed
// native systemd units. The function fields are deliberately private so callers
// cannot replace the lifecycle or unit identity; tests can inject bounded
// command and file readers without invoking systemctl.
type NativeService struct {
	command  func(context.Context, string, ...string) ([]byte, error)
	readUnit func(string) ([]byte, error)
}

// GatewayStatus contains only the lifecycle state of the fixed systemd units.
// It does not describe NetworkManager, OpenVPN, or VPN connectivity.
type GatewayStatus struct {
	Gateway string `json:"gateway"`
	Network string `json:"network"`
	Ready   bool   `json:"ready"`
}

type nativeSystemdUnitState struct {
	LoadState        string
	ActiveState      string
	SubState         string
	Type             string
	DropInPaths      string
	FragmentPath     string
	NeedDaemonReload string
}

func nativeServiceContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, nativeServiceCommandTimeout)
}

func (s NativeService) call(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := nativeServiceCommandTimeout
	if name == "systemctl" && len(args) > 0 && (args[0] == "start" || args[0] == "stop") {
		limit = nativeServiceLifecycleTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return nil, err
	}

	var (
		data []byte
		err  error
	)
	if s.command != nil {
		data, err = s.command(bounded, name, args...)
	} else {
		data, err = runHostCommand(bounded, limit, false, name, args...)
	}
	if boundedErr := bounded.Err(); boundedErr != nil {
		return data, boundedErr
	}
	if err != nil {
		return data, err
	}
	if len(data) > nativeServiceOutputLimit {
		return nil, errNativeServiceOutputLimit
	}
	return data, nil
}

func nativeServiceCommandError(ctx context.Context, err error, message string) error {
	if err == nil {
		return nil
	}
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(message)
}

func nativeServiceUnitPath(name string) (string, bool) {
	switch name {
	case nativegateway.GatewayUnit, nativegateway.NetworkUnit:
		return filepath.Join(nativeServiceUnitDirectory, name), true
	default:
		return "", false
	}
}

// nativeServiceUnitLocation is an unexported test seam. Production always
// roots the fixed /usr/lib/systemd/system path at / and requires uid 0. Tests may
// root the same descriptor walk at a private temporary directory and use their
// test uid without writing the installed systemd directory.
type nativeServiceUnitLocation struct {
	rootPath     string
	relativePath string
	ownerUID     uint32
}

func productionNativeServiceUnitLocation(name string) (nativeServiceUnitLocation, bool) {
	path, ok := nativeServiceUnitPath(name)
	if !ok {
		return nativeServiceUnitLocation{}, false
	}
	return nativeServiceUnitLocation{
		rootPath:     "/",
		relativePath: strings.TrimPrefix(path, "/"),
		ownerUID:     0,
	}, true
}

func readInstalledNativeUnit(name string) ([]byte, error) {
	location, ok := productionNativeServiceUnitLocation(name)
	if !ok {
		return nil, errors.New("native systemd unit is not fixed")
	}
	return readInstalledNativeUnitAt(location)
}

func readInstalledNativeUnitAt(location nativeServiceUnitLocation) ([]byte, error) {
	parent, base, err := openNativeServiceUnitParent(location)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)

	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("native systemd unit is unavailable")
	}
	file := os.NewFile(uintptr(fd), "native systemd unit")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("native systemd unit is unavailable")
	}
	defer file.Close()

	before, err := validateNativeServiceUnitFile(fd, location.ownerUID)
	if err != nil {
		return nil, err
	}
	if before.Size > nativeServiceUnitLimit {
		return nil, errNativeServiceOutputLimit
	}

	data, err := io.ReadAll(io.LimitReader(file, int64(nativeServiceUnitLimit)+1))
	if err != nil {
		return nil, errors.New("native systemd unit could not be read")
	}
	if len(data) > nativeServiceUnitLimit {
		return nil, errNativeServiceOutputLimit
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || !sameNativeServiceUnitStat(before, after) || after.Size != int64(len(data)) {
		return nil, errors.New("native systemd unit changed while being read")
	}
	if _, err := validateNativeServiceUnitFile(fd, location.ownerUID); err != nil {
		return nil, err
	}
	return data, nil
}

func openNativeServiceUnitParent(location nativeServiceUnitLocation) (int, string, error) {
	if location.rootPath == "" || !filepath.IsAbs(location.rootPath) {
		return -1, "", errors.New("invalid native systemd unit path")
	}
	parts := strings.Split(location.relativePath, "/")
	if len(parts) < 2 {
		return -1, "", errors.New("invalid native systemd unit path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, "", errors.New("invalid native systemd unit path")
		}
	}

	fd, err := unix.Open(location.rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", errors.New("native systemd unit root unavailable")
	}
	if err := validateNativeServiceDirectory(fd, location.ownerUID); err != nil {
		_ = unix.Close(fd)
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = unix.Close(fd)
			return -1, "", errors.New("native systemd unit directory unavailable")
		}
		if err := validateNativeServiceDirectory(next, location.ownerUID); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, "", err
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func validateNativeServiceDirectory(fd int, ownerUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return errors.New("native systemd unit directory identity unavailable")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != ownerUID || stat.Mode&0022 != 0 {
		return errors.New("native systemd unit directory is unsafe")
	}
	return nil
}

func validateNativeServiceUnitFile(fd int, ownerUID uint32) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return stat, errors.New("native systemd unit identity unavailable")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != ownerUID || stat.Mode&0022 != 0 || stat.Nlink != 1 {
		return stat, errors.New("native systemd unit has unsafe ownership, mode, or links")
	}
	return stat, nil
}

func sameNativeServiceUnitStat(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev &&
		a.Ino == b.Ino &&
		a.Size == b.Size &&
		a.Mode == b.Mode &&
		a.Uid == b.Uid &&
		a.Nlink == b.Nlink
}

func nativeServiceExpectedUnit(name string) (string, bool) {
	if name != nativegateway.GatewayUnit && name != nativegateway.NetworkUnit {
		return "", false
	}
	contents, ok := nativegateway.UnitFiles()[name]
	return contents, ok
}

func (s NativeService) verifyInstalledUnits(ctx context.Context) error {
	// Verify every unit before issuing any lifecycle command. Network is first
	// because the gateway's fixed unit Requires it, but neither is started here.
	for _, name := range []string{nativegateway.NetworkUnit, nativegateway.GatewayUnit} {
		if err := ctx.Err(); err != nil {
			return err
		}
		expected, ok := nativeServiceExpectedUnit(name)
		if !ok {
			return errors.New("native systemd unit is not fixed")
		}
		var (
			data []byte
			err  error
		)
		if s.readUnit != nil {
			data, err = s.readUnit(name)
		} else {
			data, err = readInstalledNativeUnit(name)
		}
		if err != nil {
			return errors.New("native systemd unit is not installed or owned")
		}
		if len(data) > nativeServiceUnitLimit || string(data) != expected {
			return errors.New("native systemd unit is not installed or owned")
		}
	}
	return nil
}

func parseNativeSystemdUnitState(data []byte) (nativeSystemdUnitState, error) {
	var state nativeSystemdUnitState
	if len(data) == 0 || len(data) > nativeServiceOutputLimit {
		return state, errors.New("invalid native systemd status")
	}
	text := string(data)
	if strings.ContainsAny(text, "\x00\r") {
		return state, errors.New("invalid native systemd status")
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	seen := make(map[string]bool, 7)
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" && key == "" || seen[key] {
			return state, errors.New("invalid native systemd status")
		}
		if len(value) > 1024 {
			return state, errors.New("invalid native systemd status")
		}
		seen[key] = true
		switch key {
		case "LoadState":
			state.LoadState = value
		case "ActiveState":
			state.ActiveState = value
		case "SubState":
			state.SubState = value
		case "Type":
			state.Type = value
		case "DropInPaths":
			state.DropInPaths = value
		case "FragmentPath":
			state.FragmentPath = value
		case "NeedDaemonReload":
			state.NeedDaemonReload = value
		default:
			return state, errors.New("invalid native systemd status")
		}
	}
	if len(seen) != 7 || !seen["LoadState"] || !seen["ActiveState"] || !seen["SubState"] || !seen["Type"] || !seen["DropInPaths"] || !seen["FragmentPath"] || !seen["NeedDaemonReload"] {
		return state, errors.New("invalid native systemd status")
	}
	return state, nil
}

func (s NativeService) queryUnit(ctx context.Context, name string) (nativeSystemdUnitState, error) {
	var empty nativeSystemdUnitState
	if name != nativegateway.GatewayUnit && name != nativegateway.NetworkUnit {
		return empty, errors.New("native systemd unit is not fixed")
	}
	data, err := s.call(ctx, "systemctl", "show", "--no-pager", "--plain", "--property="+nativeServiceShowProperties, name)
	state, parseErr := parseNativeSystemdUnitState(data)
	if err != nil {
		// systemctl normally reports a missing unit through LoadState=not-found.
		// Accept that bounded state even if a test double also returns a nonzero
		// status, but never turn an arbitrary command failure into "absent".
		if parseErr == nil && state.LoadState == "not-found" && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return state, nil
		}
		return empty, nativeServiceCommandError(ctx, err, "native systemd status query failed")
	}
	if parseErr != nil {
		return empty, errors.New("native systemd status query returned invalid data")
	}
	return state, nil
}

func nativeSystemdStateLabel(state nativeSystemdUnitState) string {
	if state.LoadState == "not-found" {
		return "absent"
	}
	switch state.ActiveState {
	case "active", "inactive", "failed", "activating", "deactivating", "reloading":
		return state.ActiveState
	default:
		return "unknown"
	}
}

func nativeGatewayReady(gateway, network nativeSystemdUnitState) bool {
	return nativeServiceStateMatches(nativegateway.GatewayUnit, gateway, "notify", "running") &&
		nativeServiceStateMatches(nativegateway.NetworkUnit, network, "oneshot", "exited")
}

func nativeServiceStateMatches(name string, state nativeSystemdUnitState, expectedType, expectedSubState string) bool {
	expectedFragment, ok := nativeServiceUnitPath(name)
	return ok &&
		state.LoadState == "loaded" &&
		state.ActiveState == "active" &&
		state.SubState == expectedSubState &&
		state.Type == expectedType &&
		state.DropInPaths == "" &&
		state.FragmentPath == expectedFragment &&
		state.NeedDaemonReload == "no"
}

func (s NativeService) statusWithContext(ctx context.Context) (GatewayStatus, error) {
	gateway, err := s.queryUnit(ctx, nativegateway.GatewayUnit)
	if err != nil {
		return GatewayStatus{}, err
	}
	network, err := s.queryUnit(ctx, nativegateway.NetworkUnit)
	if err != nil {
		return GatewayStatus{}, err
	}
	status := GatewayStatus{
		Gateway: nativeSystemdStateLabel(gateway),
		Network: nativeSystemdStateLabel(network),
		Ready:   nativeGatewayReady(gateway, network),
	}
	if status.Ready {
		if err := s.verifyInstalledUnits(ctx); err != nil {
			status.Ready = false
			return status, err
		}
	}
	return status, nil
}

// Status is read-only. In particular, it never starts, enables, reloads, or
// inspects desktop VPN/NetworkManager state.
func (s NativeService) Status(parent context.Context) (GatewayStatus, error) {
	ctx, cancel := nativeServiceContext(parent)
	defer cancel()
	return s.statusWithContext(ctx)
}

func validateNativeMutationState(name string, state nativeSystemdUnitState) error {
	expectedFragment, ok := nativeServiceUnitPath(name)
	if !ok {
		return errors.New("native systemd unit is not fixed")
	}
	if state.LoadState != "loaded" {
		return errors.New("native systemd unit is not loaded")
	}
	if state.FragmentPath != expectedFragment {
		return errors.New("native systemd unit fragment is not fixed")
	}
	if state.NeedDaemonReload != "no" {
		return errors.New("native systemd unit needs daemon reload")
	}
	if state.DropInPaths != "" {
		return errors.New("native systemd unit has drop-ins")
	}
	expectedType := ""
	switch name {
	case nativegateway.GatewayUnit:
		expectedType = "notify"
	case nativegateway.NetworkUnit:
		expectedType = "oneshot"
	}
	if state.Type != expectedType {
		return errors.New("native systemd unit type is invalid")
	}
	return nil
}

func (s NativeService) mutationPreflight(ctx context.Context) error {
	if err := s.verifyInstalledUnits(ctx); err != nil {
		return err
	}
	gateway, err := s.queryUnit(ctx, nativegateway.GatewayUnit)
	if err != nil {
		return err
	}
	network, err := s.queryUnit(ctx, nativegateway.NetworkUnit)
	if err != nil {
		return err
	}
	if err := validateNativeMutationState(nativegateway.GatewayUnit, gateway); err != nil {
		return err
	}
	if err := validateNativeMutationState(nativegateway.NetworkUnit, network); err != nil {
		return err
	}
	return nil
}

// Start asks systemd to start only the fixed gateway unit. Its Requires= edge
// starts the fixed network unit, and Type=notify makes this call wait until the
// supervisor has sent READY=1. Interactive Polkit authorization is allowed
// so newly enrolled users do not have to restart their desktop session. A final read-only status check is still made.
func (s NativeService) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, nativeServiceLifecycleTimeout)
	defer cancel()
	if err := s.mutationPreflight(ctx); err != nil {
		return err
	}
	beforeInvocation, _ := s.networkInvocation(ctx)
	if _, err := s.call(ctx, "systemctl", "start", nativegateway.GatewayUnit); err != nil {
		if afterInvocation, invocationErr := s.networkInvocation(ctx); invocationErr == nil && beforeInvocation != "" && afterInvocation != "not-started" && afterInvocation != beforeInvocation && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			data, queryErr := s.call(ctx, "systemctl", "show", "--property=ExecMainStatus", "--value", nativegateway.NetworkUnit)
			if queryErr == nil {
				switch strings.TrimSpace(string(data)) {
				case strconv.Itoa(NativeNetworkChangedExitCode):
					return ErrNativeNetworkChanged
				case strconv.Itoa(NativeNetworkBusyExitCode):
					return ErrNativeNetworkBusy
				}
			}
		}
		return nativeServiceCommandError(ctx, err, "native gateway start failed")
	}
	status, err := s.statusWithContext(ctx)
	if err != nil {
		return err
	}
	if !status.Ready {
		return errors.New("native gateway did not become ready")
	}
	return nil
}

// A prior failed invocation must not mask a new authorization, cancellation,
// start-limit, or gateway error with a stale network exit status.
func (s NativeService) networkInvocation(ctx context.Context) (string, error) {
	data, err := s.call(ctx, "systemctl", "show", "--property=InvocationID", "--value", nativegateway.NetworkUnit)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "not-started", nil
	}
	if len(id) != 32 {
		return "", errors.New("invalid network invocation identity")
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return "", errors.New("invalid network invocation identity")
		}
	}
	return id, nil
}

// Stop preserves systemd dependency order: the gateway is stopped before its
// network namespace/service is stopped. It never calls sudo or changes enable
// state.
func (s NativeService) Stop(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, nativeServiceLifecycleTimeout)
	defer cancel()
	if err := s.mutationPreflight(ctx); err != nil {
		return err
	}
	if _, err := s.call(ctx, "systemctl", "stop", nativegateway.GatewayUnit); err != nil {
		return nativeServiceCommandError(ctx, err, "native gateway stop failed")
	}
	if _, err := s.call(ctx, "systemctl", "stop", nativegateway.NetworkUnit); err != nil {
		return nativeServiceCommandError(ctx, err, "native network stop failed")
	}
	return nil
}
