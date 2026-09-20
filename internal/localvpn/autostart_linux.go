package localvpn

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
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
	LastResult string `json:"last_result,omitempty"`
}

// Autostart is opt-in at user login. It never enables linger or changes routing
// policy, and disabling it never disconnects a currently running VPN.
// default.target follows the user manager, including an existing linger policy;
// this is not graphical-session.target and does not create a new boot policy.
type Autostart struct {
	Repo, Base, ConfigHome string
	command                func(context.Context, string, ...string) ([]byte, error)
	lifecycle              func(context.Context, []string) error
	wait                   func(context.Context) error
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
func (a Autostart) Status(ctx context.Context) (AutostartOptions, error) {
	var options AutostartOptions
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
	if err = json.Unmarshal(data, &options); err != nil {
		return options, errors.New("invalid autostart settings")
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
	return a.marker() + fmt.Sprintf(`[Unit]
Description=Local VPN at desktop login

[Service]
Type=oneshot
ExecStart=%s autostart --repo %s --action run
TimeoutStartSec=6min
RemainAfterExit=yes
StandardOutput=null
StandardError=null

[Install]
WantedBy=default.target
`, systemdArg(filepath.Join(a.Repo, ".build/local-vpn-kde.bin")), systemdArg(a.Repo)), nil
}
func (a Autostart) Configure(ctx context.Context, options AutostartOptions) error {
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
	data, _ := json.Marshal(AutostartOptions{})
	if err = atomicWrite(fd, "autostart.json", data); err != nil {
		return err
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
func (a Autostart) Run(ctx context.Context) (runErr error) {
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
		_, dockerErr := a.call(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		networkErr := error(nil)
		if options.Connect {
			var state []byte
			state, networkErr = a.call(ctx, "nmcli", "-t", "-f", "STATE", "general")
			if networkErr == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "connected") {
				networkErr = errors.New("network unavailable")
			}
		}
		if dockerErr != nil {
			result = "prerequisites-unavailable"
		} else if networkErr != nil {
			result = "prerequisites-unavailable"
		}
		if dockerErr == nil && networkErr == nil {
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
	run := a.lifecycle
	if run == nil {
		run = func(ctx context.Context, args []string) error {
			b := &Bridge{options: BridgeOptions{Base: a.Base, Executable: filepath.Join(a.Repo, "scripts/vpnkit/vpnkit-local.sh"), Grace: 5 * time.Second}}
			result, _ := b.lifecycle(ctx, "autostart", args, func(string) {})
			if result.Reason != "ok" {
				return errors.New("autostart lifecycle failed; inspect private diagnostics")
			}
			return nil
		}
	}
	// Re-read intent after waiting, so disabling during network readiness wins.
	current, err := a.Status(ctx)
	if err != nil {
		return err
	}
	if !current.Gateway {
		result = "disabled"
		return nil
	}
	result = "failed"
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
	result = "ready"
	return nil
}
