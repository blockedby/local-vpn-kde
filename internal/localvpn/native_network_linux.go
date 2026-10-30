//go:build linux

package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	_, exists, err := journal.Load(ctx)
	if err != nil {
		return err
	}
	if action == "stop" {
		if !exists {
			return nil
		}
		return stopNativeNetwork(ctx, controller, reads, nativeNamespacePresent)
	}
	return startNativeNetwork(ctx, controller, exists,
		func(ctx context.Context) (nativegateway.Uplink, error) {
			// Discover the current physical underlay even when an obsolete
			// journal exists (DHCP/interface changes are common across boots).
			// Controller still proves old resources absent before allowing an
			// epoch change, and validates a fresh snapshot before replacement.
			uplink, _, err := nativegateway.ProbeNetwork(ctx, reads)
			return uplink, err
		},
		func(ctx context.Context) error {
			return (nativeMutationCommands{}).Run(ctx, nativegateway.Command{Program: "ip", Args: []string{"netns", "exec", nativegateway.Namespace, nativegateway.LibexecDir + "/local-vpn-kde.bin", "native-namespace-init"}})
		}, func(ctx context.Context) error {
			return stopNativeNetwork(ctx, controller, reads, nativeNamespacePresent)
		})
}

type nativeNetworkLifecycleController interface {
	Start(context.Context, nativegateway.Uplink) error
	Stop(context.Context) error
}

func nativeNamespacePresent() (bool, error) {
	_, err := os.Lstat("/run/netns/" + nativegateway.Namespace)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func stopNativeNetwork(ctx context.Context, controller nativeNetworkLifecycleController, reads nativegateway.ReadOnlyRunner, namespacePresent func() (bool, error)) error {
	present, err := namespacePresent()
	if err != nil {
		return err
	}
	if present {
		// Never detach a named namespace held by a gateway or foreign
		// process. An absent marker goes to the controller's complete proof,
		// rather than treating ip netns pids ENOENT as a permanent failure.
		pids, err := reads.Run(ctx, "ip", "netns", "pids", nativegateway.Namespace)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(pids)) != "" {
			return ErrNativeNetworkBusy
		}
	}
	return controller.Stop(ctx)
}

func startNativeNetwork(ctx context.Context, controller nativeNetworkLifecycleController, existed bool, discoverUplink func(context.Context) (nativegateway.Uplink, error), initializeNamespace func(context.Context) error, cleanupNetwork func(context.Context) error) error {
	uplink, err := discoverUplink(ctx)
	if err != nil {
		return err
	}
	if err = controller.Start(ctx, uplink); err != nil {
		if !existed || !errors.Is(err, nativegateway.ErrNetworkNeedsCleanup) {
			return err
		}
		// A link flap can remove dedicated routes while leaving the durable
		// journal intact. Rebuild only after proving no namespace users and
		// letting Stop validate every remaining resource's ownership.
		if cleanupErr := cleanupNetwork(ctx); cleanupErr != nil {
			return fmt.Errorf("native network recovery blocked: %w", cleanupErr)
		}
		existed = false
		// Cleanup may take time; refresh DHCP/underlay information before
		// constructing the new plan.
		uplink, err = discoverUplink(ctx)
		if err != nil {
			return err
		}
		if err = controller.Start(ctx, uplink); err != nil {
			return err
		}
	}
	// Root sets namespace-only sysctls before the capability-restricted
	// gateway starts; initialization independently proves namespace identity.
	err = initializeNamespace(ctx)
	if err != nil && !existed {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, controller.Stop(cleanup))
	}
	return err
}
