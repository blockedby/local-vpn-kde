//go:build linux

package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

// RunNativeNetwork is the fixed privileged service entrypoint. It is not a
// desktop authorization boundary: only the installed system unit should call
// it. No paths, interfaces, addresses, or commands are accepted from callers.
func RunNativeNetwork(ctx context.Context, action string, output io.Writer) error {
	switch action {
	case "check", "start", "stop", "status":
	default:
		return errors.New("invalid native network action")
	}
	if os.Geteuid() != 0 {
		return errors.New("native networking requires the installed privileged service")
	}
	if action == "start" {
		release, err := acquireNativeConfigurationLock(ctx, false)
		if err != nil {
			return err
		}
		defer release()
		blocked, err := nativePackageBlocked()
		if err != nil {
			return err
		}
		if blocked {
			return errors.New("native start blocked by a package transaction")
		}
		incomplete, err := nativeSetupIncomplete()
		if err != nil {
			return err
		}
		if incomplete {
			return errors.New("native start blocked: retry incomplete native setup")
		}
	}
	reads := nativeReadCommands{}
	if action == "check" {
		uplink, snapshot, err := nativegateway.ProbeNetwork(ctx, reads)
		if err != nil {
			return err
		}
		if _, err = nativegateway.PlanNetwork(uplink, snapshot); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]bool{"preflight_ready": true})
	}
	journal, err := nativegateway.OpenNetworkJournal(ctx)
	if err != nil {
		return err
	}
	defer journal.Close()
	controller := nativegateway.Controller{Runner: nativeMutationCommands{}, Journal: journal, Inspector: nativegateway.NewLinuxNetworkInspector(reads)}
	if action == "status" {
		ready, err := controller.Status(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]bool{"network_ready": ready})
	}
	previous, exists, err := journal.Load(ctx)
	if err != nil {
		return err
	}
	if action == "stop" {
		if !exists {
			return nil
		}
		// Never detach the named namespace while a gateway or foreign process
		// still holds it. systemd orders gateway shutdown before network shutdown.
		pids, err := reads.Run(ctx, "ip", "netns", "pids", nativegateway.Namespace)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(pids)) != "" {
			return errors.New("native namespace still has running processes; stop the gateway first")
		}
		return controller.Stop(ctx)
	}
	uplink := previous.Uplink
	if !exists {
		var snapshot nativegateway.Snapshot
		uplink, snapshot, err = nativegateway.ProbeNetwork(ctx, reads)
		if err != nil {
			return err
		}
		if _, err = nativegateway.PlanNetwork(uplink, snapshot); err != nil {
			return err
		}
	}
	if err = controller.Start(ctx, uplink); err != nil {
		return err
	}
	// Root sets namespace-only sysctls before the capability-restricted gateway
	// starts. The nested command independently verifies its namespace identity.
	err = nativeMutationCommands{}.Run(ctx, nativegateway.Command{Program: "ip", Args: []string{"netns", "exec", nativegateway.Namespace, nativegateway.LibexecDir + "/local-vpn-kde.bin", "native-namespace-init"}})
	if err != nil && !exists {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, controller.Stop(cleanup))
	}
	return err
}
