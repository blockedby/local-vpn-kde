//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativeDesktopStatusLimit       = 16 * 1024
	nativeDesktopLockTimeout       = 5 * time.Second
	nativeDesktopCompensateTimeout = 30 * time.Second
)

var errNativeDesktopStatusLimit = errors.New("NetworkManager status exceeded limit")

type nativeDesktopService interface {
	Start(context.Context) error
	Stop(context.Context) error
	Status(context.Context) (GatewayStatus, error)
}

type nativeDesktopNetworkManager interface {
	Run(context.Context, string, bool, io.Writer) error
}

type nativeDesktopDependencies struct {
	service nativeDesktopService
	nm      nativeDesktopNetworkManager
}

// This private factory is the dependency seam used by unit tests. Production
// always receives the fixed native service client and the owned-UUID-aware
// NetworkManager adapter.
var newNativeDesktopDependencies = func(base string) nativeDesktopDependencies {
	return nativeDesktopDependencies{
		service: NativeService{},
		nm:      NetworkManager{Base: base},
	}
}

type nativeDesktopLimitWriter struct {
	buffer   bytes.Buffer
	remain   int
	overflow bool
}

func (w *nativeDesktopLimitWriter) Write(data []byte) (int, error) {
	if len(data) > w.remain {
		w.overflow = true
		if w.remain > 0 {
			_, _ = w.buffer.Write(data[:w.remain])
			w.remain = 0
		}
		return 0, errNativeDesktopStatusLimit
	}
	w.remain -= len(data)
	return w.buffer.Write(data)
}

func parseNativeDesktopNMStatus(data []byte) (configured, active string, err error) {
	if len(data) == 0 || len(data) > nativeDesktopStatusLimit || bytes.ContainsAny(data, "\x00\r") {
		return "", "", errors.New("invalid NetworkManager status")
	}
	allowed := map[string]bool{
		"command": true, "mutation": true, "connection": true,
		"configured": true, "active": true, "ownership": true,
		"device": true, "profile": true, "private_values": true,
	}
	seen := make(map[string]bool, len(allowed))
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" || len(value) > 4096 || !allowed[key] || seen[key] {
			return "", "", errors.New("invalid NetworkManager status")
		}
		seen[key] = true
		switch key {
		case "configured":
			configured = value
		case "active":
			active = value
		}
	}
	if !seen["configured"] || !seen["active"] || (configured != "yes" && configured != "no") || (active != "yes" && active != "no") {
		return "", "", errors.New("invalid NetworkManager status")
	}
	return configured, active, nil
}

func nativeDesktopGatewayState(status GatewayStatus) string {
	if status.Ready {
		return "healthy"
	}
	switch status.Gateway {
	case "absent":
		return "absent"
	case "inactive":
		return "inactive"
	case "activating":
		return "starting"
	case "failed", "active":
		return "unhealthy"
	default:
		return "unknown"
	}
}

func nativeDesktopPhase(output io.Writer, phase string) error {
	_, err := fmt.Fprintf(output, "phase=%s\n", phase)
	return err
}

func lockNativeDesktop(ctx context.Context, base string, create bool) (func(), error) {
	dir, err := directory(base, create, true)
	if err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, nativeDesktopLockTimeout)
	lock, err := lockFile(lockCtx, dir, ".native-lifecycle.lock")
	cancel()
	unix.Close(dir)
	if err != nil {
		return nil, err
	}
	return func() { _ = unix.Close(lock) }, nil
}

func runNativeDesktopStatus(ctx context.Context, base string, output io.Writer, dependencies nativeDesktopDependencies, baseMissing bool) error {
	gateway, err := dependencies.service.Status(ctx)
	if err != nil {
		return fmt.Errorf("native gateway status failed: %w", err)
	}
	configured, active := "no", "no"
	if !baseMissing {
		capture := &nativeDesktopLimitWriter{remain: nativeDesktopStatusLimit}
		if err = dependencies.nm.Run(ctx, "status", true, capture); err != nil {
			return fmt.Errorf("NetworkManager status failed: %w", err)
		}
		if capture.overflow {
			return errNativeDesktopStatusLimit
		}
		configured, active, err = parseNativeDesktopNMStatus(capture.buffer.Bytes())
		if err != nil {
			return err
		}
	}
	policy := "strict"
	if executable, _ := os.Executable(); executable == InstalledCLI {
		var data bytes.Buffer
		if NativeOperationsClient(ctx, []string{"policy-status"}, &data) == nil {
			var result struct {
				Policy string `json:"routing_policy"`
			}
			if json.Unmarshal(data.Bytes(), &result) == nil && result.Policy == "smart" {
				policy = "smart"
			}
		}
	}
	status := struct {
		Container                string `json:"container"`
		NetworkManagerConfigured string `json:"networkmanager_configured"`
		NetworkManagerActive     string `json:"networkmanager_active"`
		Runtime                  string `json:"runtime"`
		RoutingPolicy            string `json:"routing_policy"`
	}{
		Container:                nativeDesktopGatewayState(gateway),
		NetworkManagerConfigured: configured,
		NetworkManagerActive:     active,
		Runtime:                  "native",
		RoutingPolicy:            policy,
	}
	if err = json.NewEncoder(output).Encode(status); err != nil {
		return fmt.Errorf("write native desktop status: %w", err)
	}
	return nil
}

// RunNativeDesktop coordinates the fixed native gateway lifecycle with the
// owned NetworkManager profile. Mutations require explicit authorization via
// yes; action is one of status, backend-start, start, disconnect, or stop.
func RunNativeDesktop(ctx context.Context, base string, action string, yes bool, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		return errors.New("native desktop output is required")
	}
	switch action {
	case "status":
	case "backend-start", "start", "disconnect", "stop":
		if !yes {
			return errors.New("native desktop mutation requires explicit authorization (--yes)")
		}
	default:
		return errors.New("invalid native desktop action")
	}

	dependencies := newNativeDesktopDependencies(base)
	if dependencies.service == nil || dependencies.nm == nil {
		return errors.New("native desktop dependencies are unavailable")
	}

	release, err := lockNativeDesktop(ctx, base, action != "status")
	baseMissing := false
	if action == "status" && errors.Is(err, unix.ENOENT) {
		baseMissing = true
	} else if err != nil {
		return fmt.Errorf("native desktop lifecycle lock failed: %w", err)
	} else {
		defer release()
	}

	switch action {
	case "status":
		return runNativeDesktopStatus(ctx, base, output, dependencies, baseMissing)
	case "backend-start":
		if err = nativeDesktopPhase(output, "gateway-start"); err != nil {
			return err
		}
		return dependencies.service.Start(ctx)
	case "start":
		if err = nativeDesktopPhase(output, "gateway-status"); err != nil {
			return err
		}
		before, statusErr := dependencies.service.Status(ctx)
		if statusErr != nil {
			return fmt.Errorf("native gateway readiness capture failed: %w", statusErr)
		}
		if err = nativeDesktopPhase(output, "gateway-start"); err != nil {
			return err
		}
		if err = dependencies.service.Start(ctx); err != nil {
			return err
		}
		if err = nativeDesktopPhase(output, "networkmanager-connect"); err != nil {
			return err
		}
		if err = dependencies.nm.Run(ctx, "connect", true, io.Discard); err != nil {
			if before.Ready {
				return err
			}
			_ = nativeDesktopPhase(output, "gateway-stop-compensation")
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nativeDesktopCompensateTimeout)
			stopErr := dependencies.service.Stop(rollbackCtx)
			cancel()
			if stopErr != nil {
				return errors.Join(err, fmt.Errorf("native gateway compensation failed: %w", stopErr))
			}
			return err
		}
		return nil
	case "disconnect":
		if err = nativeDesktopPhase(output, "networkmanager-disconnect"); err != nil {
			return err
		}
		return dependencies.nm.Run(ctx, "disconnect", true, io.Discard)
	case "stop":
		if err = nativeDesktopPhase(output, "networkmanager-disconnect"); err != nil {
			return err
		}
		if err = dependencies.nm.Run(ctx, "disconnect", true, io.Discard); err != nil {
			return err
		}
		if err = nativeDesktopPhase(output, "gateway-stop"); err != nil {
			return err
		}
		return dependencies.service.Stop(ctx)
	}
	return errors.New("invalid native desktop action")
}
