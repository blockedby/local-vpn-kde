package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// RunSetup exposes only fixed stages and classified outcomes to OpenTUI.
// Process output stays in the same bounded private attempt logs as the app.
func RunSetup(ctx context.Context, repo, action string, output io.Writer) error {
	if os.Geteuid() == 0 {
		return errors.New("run setup as the desktop user")
	}
	if action != "image" && action != "install" {
		return errors.New("invalid setup action")
	}
	base, err := SecretRoot(repo, "secrets/vpnkit-local", false)
	if err != nil {
		return err
	}
	if err = validateSecretTree(base, nil); err != nil {
		return err
	}
	fd, err := directory(base, true, true)
	if err != nil {
		return err
	}
	if err = unix.Fchmod(fd, 0700); err != nil {
		unix.Close(fd)
		return err
	}
	unix.Close(fd)
	executable := filepath.Join(repo, "scripts/vpnkit/vpnkit-local-install.sh")
	args := []string{"--non-interactive"}
	if action == "image" {
		executable, err = exec.LookPath("docker")
		if err != nil {
			return errors.New("Docker is unavailable")
		}
		args = []string{"compose", "--project-directory", repo, "-p", "vpnkit-local", "-f", filepath.Join(repo, "docker-compose.yml"), "-f", filepath.Join(repo, "compose.local.yaml"), "build", "vpnkit"}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	encoder := json.NewEncoder(output)
	// Isolate cancellation by process group while retaining the controlling
	// terminal identity used by sudo's per-terminal authentication timestamp.
	b := &Bridge{options: BridgeOptions{Base: base, Executable: executable, Grace: 31 * time.Second, keepTerminal: true}}
	result, attempt := b.lifecycle(ctx, "setup-"+action, args, func(phase string) {
		if err := encoder.Encode(map[string]string{"event": "progress", "phase": phase}); err != nil {
			cancel()
		}
	})
	return encoder.Encode(map[string]any{"event": "result", "ok": result.Reason == "ok", "reason": result.Reason, "code": result.Code, "attempt": attempt})
}
