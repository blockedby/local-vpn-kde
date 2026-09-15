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
	return runHostCommand(ctx, 15*time.Second, true, name, args...)
}

func runHostCommand(ctx context.Context, limit time.Duration, mergeErrors bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	output := &boundedOutput{limit: 1024 * 1024, cancel: cancel}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout = output
	if mergeErrors {
		cmd.Stderr = output
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	data, overflow := output.result()
	if overflow {
		return data, errors.New("host command output exceeded limit")
	}
	if ctx.Err() != nil {
		return data, ctx.Err()
	}
	return data, err
}
