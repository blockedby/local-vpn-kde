//go:build linux

package localvpn

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	autostartServiceStateTimeout    = 5 * time.Second
	autostartServiceStateMaxOutput  = 8 * 1024
	autostartServiceStateProperties = "LoadState,UnitFileState,ActiveState"
)

var (
	errAutostartServiceStateUnavailable = errors.New("autostart service state unavailable")
	errAutostartServiceStateMalformed   = errors.New("invalid autostart service state")
)

// AutostartServiceState is the actual state of the fixed user autostart unit.
// It is deliberately separate from AutostartOptions, which contains saved
// intent rather than the state reported by the user service manager.
type AutostartServiceState struct {
	Enabled string `json:"enabled"`
	Active  string `json:"active"`
}

type autostartServiceStatePropertiesValue struct {
	loadState     string
	unitFileState string
	activeState   string
}

type autostartServiceStateCallResult struct {
	data []byte
	err  error
}

// ServiceState reads only the fixed user service's state. It never consults
// saved autostart intent or issues a systemd mutation command.
func (a Autostart) ServiceState(parent context.Context) (AutostartServiceState, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, autostartServiceStateTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return AutostartServiceState{}, err
	}

	result := make(chan autostartServiceStateCallResult, 1)
	go func() {
		data, err := a.call(ctx, "systemctl", "--user", "show", autostartUnit, "--property="+autostartServiceStateProperties)
		result <- autostartServiceStateCallResult{data: data, err: err}
	}()
	var data []byte
	var callErr error
	select {
	case response := <-result:
		data, callErr = response.data, response.err
	case <-ctx.Done():
		return AutostartServiceState{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return AutostartServiceState{}, err
	}
	if callErr != nil && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)) {
		return AutostartServiceState{}, serviceStateContextError(ctx, callErr)
	}
	if len(data) > autostartServiceStateMaxOutput {
		return AutostartServiceState{}, errAutostartServiceStateUnavailable
	}

	properties, err := parseAutostartServiceStateProperties(data)
	if err != nil {
		return AutostartServiceState{}, err
	}
	missing := properties.loadState == "not-found" || properties.unitFileState == "not-found"
	if missing {
		if properties.activeState != "inactive" {
			return AutostartServiceState{}, errAutostartServiceStateMalformed
		}
		return AutostartServiceState{Enabled: "not-installed", Active: "inactive"}, nil
	}
	if callErr != nil {
		return AutostartServiceState{}, serviceStateContextError(ctx, callErr)
	}

	enabled, ok := autostartServiceEnabledState(properties.unitFileState)
	if !ok {
		return AutostartServiceState{}, errAutostartServiceStateMalformed
	}
	active, ok := autostartServiceActiveState(properties.activeState)
	if !ok {
		return AutostartServiceState{}, errAutostartServiceStateMalformed
	}
	return AutostartServiceState{Enabled: enabled, Active: active}, nil
}

func serviceStateContextError(ctx context.Context, callErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(callErr, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(callErr, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errAutostartServiceStateUnavailable
}

func parseAutostartServiceStateProperties(data []byte) (autostartServiceStatePropertiesValue, error) {
	var properties autostartServiceStatePropertiesValue
	if len(data) == 0 || len(data) > autostartServiceStateMaxOutput {
		return properties, errAutostartServiceStateMalformed
	}
	text := string(data)
	if strings.ContainsAny(text, "\x00\r") {
		return properties, errAutostartServiceStateMalformed
	}
	if strings.HasSuffix(text, "\n") {
		text = strings.TrimSuffix(text, "\n")
	}
	if text == "" || strings.HasSuffix(text, "\n") {
		return properties, errAutostartServiceStateMalformed
	}

	seen := make(map[string]bool, 3)
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.Count(line, "=") != 1 {
			return properties, errAutostartServiceStateMalformed
		}
		key, value, _ := strings.Cut(line, "=")
		if seen[key] {
			return properties, errAutostartServiceStateMalformed
		}
		seen[key] = true
		switch key {
		case "LoadState":
			if !autostartServiceLoadStateAllowed(value) {
				return properties, errAutostartServiceStateMalformed
			}
			properties.loadState = value
		case "UnitFileState":
			// systemd may leave this property empty for a unit that is not
			// installed. The missing-unit result below is fixed and safe.
			if value != "" && !autostartServiceUnitFileStateAllowed(value) {
				return properties, errAutostartServiceStateMalformed
			}
			properties.unitFileState = value
		case "ActiveState":
			if !autostartServiceActiveStateAllowed(value) {
				return properties, errAutostartServiceStateMalformed
			}
			properties.activeState = value
		default:
			return properties, errAutostartServiceStateMalformed
		}
	}
	if len(seen) != 3 || !seen["LoadState"] || !seen["UnitFileState"] || !seen["ActiveState"] {
		return properties, errAutostartServiceStateMalformed
	}
	if properties.unitFileState == "" && properties.loadState != "not-found" {
		return properties, errAutostartServiceStateMalformed
	}
	return properties, nil
}

func autostartServiceLoadStateAllowed(value string) bool {
	switch value {
	case "stub", "loaded", "not-found", "bad-setting", "error", "merged", "masked", "undergoing", "unknown":
		return true
	default:
		return false
	}
}

func autostartServiceUnitFileStateAllowed(value string) bool {
	switch value {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias", "masked", "masked-runtime", "static", "disabled", "indirect", "generated", "transient", "bad", "not-found", "unknown":
		return true
	default:
		return false
	}
}

func autostartServiceActiveStateAllowed(value string) bool {
	switch value {
	case "active", "inactive", "activating", "deactivating", "failed", "unknown", "reloading", "maintenance":
		return true
	default:
		return false
	}
}

func autostartServiceEnabledState(value string) (string, bool) {
	switch value {
	case "enabled", "enabled-runtime":
		return "enabled", true
	case "disabled":
		return "disabled", true
	case "masked", "masked-runtime":
		return "masked", true
	case "static":
		return "static", true
	case "indirect":
		return "indirect", true
	case "generated":
		return "generated", true
	case "linked", "linked-runtime":
		return "linked", true
	case "alias", "transient", "bad", "unknown":
		return "unknown", true
	default:
		return "", false
	}
}

func autostartServiceActiveState(value string) (string, bool) {
	switch value {
	case "active", "inactive", "activating", "deactivating", "failed", "unknown":
		return value, true
	case "reloading", "maintenance":
		return "unknown", true
	default:
		return "", false
	}
}
