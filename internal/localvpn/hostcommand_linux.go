package localvpn

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// hostCommand is for non-forking host utilities (nmcli and ip), not daemons or
// lifecycle scripts. Keep them in the caller's supervised process group so a
// forced cancellation of the whole operation cannot leave a detached mutation.
func hostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output := &boundedOutput{limit: 1024 * 1024, cancel: cancel}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	data, overflow := output.result()
	if err != nil || overflow {
		return data, errors.New("host command failed")
	}
	return data, nil
}
