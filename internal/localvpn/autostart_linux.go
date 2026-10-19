package localvpn

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const autostartUnit = "local-vpn-kde-autostart.service"
const autostartMarker = "# Managed by local-vpn-kde autostart\n"

type AutostartOptions struct {
	Gateway    bool   `json:"gateway"`
	Connect    bool   `json:"connect"`
	Runtime    string `json:"runtime,omitempty"`
	LastResult string `json:"last_result,omitempty"`
}

// Autostart is opt-in at user login. It never enables linger or changes routing
// policy, and disabling it never disconnects a currently running VPN.
// Native autostart follows graphical-session.target, not a lingering user
// manager at boot. Legacy Docker target semantics remain unchanged.
type Autostart struct {
	Repo, Base, ConfigHome string
	Runtime                string
	Installed              bool
	command                func(context.Context, string, ...string) ([]byte, error)
	lifecycle              func(context.Context, []string) error
	wait                   func(context.Context) error
	nativeStatus           func(context.Context) (GatewayStatus, error)
	nativeStart            func(context.Context) error
	nativeConnect          func(context.Context) error
}

func (a Autostart) call(ctx context.Context, name string, args ...string) ([]byte, error) {
	if a.command != nil {
		return a.command(ctx, name, args...)
	}
	return hostCommand(ctx, name, args...)
}
func (a Autostart) configDir() (int, error) {
	home := a.ConfigHome
	if home == "" {
		var err error
		home, err = os.UserConfigDir()
		if err != nil {
			return -1, err
		}
	}
	return directory(filepath.Join(home, "systemd/user"), true, true)
}
func autostartRuntime(value string) (string, error) {
	switch value {
	case "", "docker":
		return "docker", nil
	case "native":
		return "native", nil
	default:
		return "", errors.New("invalid autostart runtime")
	}
}

func (a Autostart) runtime() (string, error) {
	if a.Installed && a.Runtime == "" {
		return "native", nil
	}
	return autostartRuntime(a.Runtime)
}

func (a Autostart) Status(ctx context.Context) (AutostartOptions, error) {
	requested, err := a.runtime()
	if err != nil {
		return AutostartOptions{}, err
	}
	options := AutostartOptions{Runtime: a.Runtime}
	if a.Installed {
		options.Runtime = requested
	}
	fd, err := directory(a.Base, false, true)
	if errors.Is(err, unix.ENOENT) {
		return options, nil
	}
	if err != nil {
		return options, err
	}
	defer unix.Close(fd)
	data, err := readAt(fd, "autostart.json")
	if errors.Is(err, unix.ENOENT) {
		return options, nil
	}
	if err != nil {
		return options, err
	}
	// A legacy file with no runtime means Docker, not the requested runtime.
	options = AutostartOptions{}
	if err = json.Unmarshal(data, &options); err != nil {
		return options, errors.New("invalid autostart settings")
	}
	saved, runtimeErr := autostartRuntime(options.Runtime)
	if runtimeErr != nil {
		return options, runtimeErr
	}
	if options.Gateway && saved != requested {
		return options, errors.New("enabled autostart runtime does not match requested runtime")
	}
	if !options.Gateway {
		// Disabled intent is safe to adopt for the caller's selected runtime.
		options.Runtime = a.Runtime
	}
	if options.Connect && !options.Gateway {
		return options, errors.New("autoconnect requires gateway autostart")
	}
	options.LastResult = ""
	result, resultErr := readAt(fd, "autostart-result.json")
	if resultErr == nil {
		var state struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(result, &state) != nil || !autostartResultAllowed(state.Result) {
			return options, errors.New("invalid autostart result")
		}
		options.LastResult = state.Result
	} else if !errors.Is(resultErr, unix.ENOENT) {
		return options, resultErr
	}
	return options, nil
}
func autostartResultAllowed(result string) bool {
	switch result {
	case "ready", "disabled", "failed", "prerequisites-unavailable", "cancelled", "running":
		return true
	}
	return false
}
func (a Autostart) recordResult(result string) error {
	if !autostartResultAllowed(result) {
		return errors.New("invalid autostart result")
	}
	fd, err := directory(a.Base, true, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	data, _ := json.Marshal(map[string]string{"result": result})
	return atomicWrite(fd, "autostart-result.json", data)
}
func (a Autostart) validateBase() error {
	if _, err := a.unit(); err != nil {
		return err
	}
	if a.Installed {
		_, err := ResolveApplicationLayout(InstalledCLI, a.Repo, a.Base, a.Runtime)
		return err
	}
	if a.Base != filepath.Join(a.Repo, "secrets/vpnkit-local") {
		return errors.New("autostart requires the installation's default private directory")
	}
	return nil
}
func systemdArg(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "$", "$$")
	return strconv.Quote(value)
}
func (a Autostart) marker() string {
	return autostartMarker + fmt.Sprintf("# Installation: %x\n", sha256.Sum256([]byte(a.Repo)))
}

func (a Autostart) unit() (string, error) {
	if !filepath.IsAbs(a.Repo) || filepath.Clean(a.Repo) != a.Repo || a.Repo == "/" || strings.ContainsAny(a.Repo, "\n\r\x00") {
		return "", errors.New("invalid repository path")
	}
	executable := filepath.Join(a.Repo, ".build/local-vpn-kde.bin")
	extra, session, target := "", "", "default.target"
	stateEnvironment := ""
	if runtime, _ := a.runtime(); runtime == "native" {
		extra = " --runtime " + systemdArg("native")
		target = "graphical-session.target"
		session = "PartOf=graphical-session.target\nAfter=graphical-session-pre.target\n"
	}
	if a.Installed {
		layout, err := ResolveApplicationLayout(InstalledCLI, a.Repo, a.Base, a.Runtime)
		if err != nil {
			return "", err
		}
		executable = layout.Executable
		session += "ConditionPathIsExecutable=" + InstalledCLI + "\n"
		extra += " --secrets-dir " + systemdArg(layout.State)
		// A shell's XDG override may not be imported into the user manager.
		// Environment= expands specifiers, but unlike ExecStart not dollar signs.
		stateEnvironment = "Environment=" + strconv.Quote(strings.ReplaceAll("XDG_STATE_HOME="+filepath.Dir(layout.State), "%", "%%")) + "\n"
	}
	return a.marker() + fmt.Sprintf(`[Unit]
Description=Local VPN at desktop login
%s
[Service]
Type=oneshot
%sExecStart=%s autostart --repo %s%s --action run
TimeoutStartSec=6min
RemainAfterExit=yes
StandardOutput=null
StandardError=null

[Install]
WantedBy=%s
`, session, stateEnvironment, systemdArg(executable), systemdArg(a.Repo), extra, target), nil
}
func (a Autostart) Configure(ctx context.Context, options AutostartOptions) error {
	requested, err := a.runtime()
	if err != nil {
		return err
	}
	selected, err := autostartRuntime(options.Runtime)
	if err != nil {
		return err
	}
	if selected != requested {
		return errors.New("autostart runtime does not match requested runtime")
	}
	if options.Runtime == "" {
		options.Runtime = a.Runtime
	}
	if err := a.validateBase(); err != nil {
		return err
	}
	options.LastResult = ""
	if options.Connect && !options.Gateway {
		return errors.New("autoconnect requires gateway autostart")
	}
	unit, err := a.unit()
	if err != nil {
		return err
	}
	fd, err := directory(a.Base, true, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	lock, err := lockFile(ctx, fd, "autostart.lock")
	if err != nil {
		return err
	}
	defer unix.Close(lock)
	dir, err := a.configDir()
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	old, err := readAt(dir, autostartUnit)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err == nil && !strings.HasPrefix(string(old), a.marker()) {
		return errors.New("autostart unit is not owned")
	}
	// Publish disabled intent first so failed systemctl operations cannot leave
	// an old enabled unit unexpectedly connecting on the next login.
	data, _ := json.Marshal(AutostartOptions{Runtime: options.Runtime})
	if err = atomicWrite(fd, "autostart.json", data); err != nil {
		return err
	}
	if options.Runtime == "native" && len(old) != 0 && strings.Contains(string(old), "WantedBy=default.target") {
		// Remove the old boot/user-manager link without stopping an active VPN.
		if _, err = a.call(ctx, "systemctl", "--user", "disable", autostartUnit); err != nil {
			return errors.New("could not migrate login autostart target")
		}
	}
	if err = atomicWrite(dir, autostartUnit, []byte(unit)); err != nil {
		return err
	}
	if _, err = a.call(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return errors.New("user service manager unavailable")
	}
	action := "disable"
	if options.Gateway {
		action = "enable"
	}
	if _, err = a.call(ctx, "systemctl", "--user", action, autostartUnit); err != nil {
		return errors.New("could not configure login autostart")
	}
	data, _ = json.Marshal(options)
	return atomicWrite(fd, "autostart.json", data)
}
func nativeAutostartServiceAvailable(status GatewayStatus) bool {
	available := func(state string) bool {
		switch state {
		case "active", "inactive", "failed", "activating", "deactivating", "reloading":
			return true
		default:
			return false
		}
	}
	return available(status.Gateway) && available(status.Network)
}

func (a Autostart) Run(ctx context.Context) (runErr error) {
	runtime, err := a.runtime()
	if err != nil {
		return err
	}
	if err := a.validateBase(); err != nil {
		return err
	}
	result := "failed"
	defer func() {
		if errors.Is(runErr, context.Canceled) {
			result = "cancelled"
		} else if errors.Is(runErr, context.DeadlineExceeded) {
			result = "failed"
		}
		if err := a.recordResult(result); runErr == nil && err != nil {
			runErr = err
		}
	}()
	options, err := a.Status(ctx)
	if err != nil {
		return err
	}
	if !options.Gateway {
		result = "disabled"
		return nil
	}
	if err := a.recordResult("running"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ready := false
	result = "failed"
	for attempt := 0; attempt < 12; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		gatewayErr := error(nil)
		if runtime == "native" {
			status := GatewayStatus{}
			if a.nativeStatus != nil {
				status, gatewayErr = a.nativeStatus(ctx)
			} else {
				status, gatewayErr = (NativeService{}).Status(ctx)
			}
			if gatewayErr == nil && !nativeAutostartServiceAvailable(status) {
				gatewayErr = errors.New("native gateway unavailable")
			}
		} else {
			_, gatewayErr = a.call(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		}
		networkErr := error(nil)
		if options.Connect {
			var state []byte
			state, networkErr = a.call(ctx, "nmcli", "-t", "-f", "STATE", "general")
			if networkErr == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "connected") {
				networkErr = errors.New("network unavailable")
			}
		}
		if gatewayErr != nil || networkErr != nil {
			result = "prerequisites-unavailable"
		}
		if gatewayErr == nil && networkErr == nil {
			ready = true
			break
		}
		if attempt == 11 {
			break
		}
		if a.wait != nil {
			err = a.wait(ctx)
		} else {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if err != nil {
			return err
		}
	}
	if !ready {
		return errors.New("autostart prerequisites unavailable after bounded retries")
	}
	// Re-read intent after waiting, so disabling during readiness wins.
	current, err := a.Status(ctx)
	if err != nil {
		return err
	}
	if !current.Gateway {
		result = "disabled"
		return nil
	}
	result = "failed"
	if runtime == "native" {
		start := a.nativeStart
		if start == nil {
			start = (NativeService{}).Start
		}
		if err = start(ctx); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return errors.New("native gateway autostart failed")
		}
		// Starting the notify service can take time. Re-read immediately before
		// the optional desktop VPN mutation so a concurrent disable wins.
		current, err = a.Status(ctx)
		if err != nil {
			return err
		}
		if !current.Gateway {
			result = "disabled"
			return nil
		}
		if current.Connect {
			connect := a.nativeConnect
			if connect == nil {
				connect = func(ctx context.Context) error {
					return (NetworkManager{Base: a.Base}).Run(ctx, "connect", true, io.Discard)
				}
			}
			if err = connect(ctx); err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				return errors.New("NetworkManager autoconnect failed")
			}
		}
	} else {
		run := a.lifecycle
		if run == nil {
			run = func(ctx context.Context, args []string) error {
				b := &Bridge{options: BridgeOptions{Base: a.Base, Executable: filepath.Join(a.Repo, "scripts/vpnkit/vpnkit-local.sh"), Grace: 31 * time.Second}}
				result, _ := b.lifecycle(ctx, "autostart", args, func(string) {})
				if result.Reason != "ok" {
					return errors.New("autostart lifecycle failed; inspect private diagnostics")
				}
				return nil
			}
		}
		if err = run(ctx, []string{"backend", "start"}); err != nil {
			return err
		}
		current, err = a.Status(ctx)
		if err != nil {
			return err
		}
		if current.Connect {
			result = "failed"
			if err = run(ctx, []string{"start"}); err != nil {
				return err
			}
		}
	}
	result = "ready"
	return nil
}
