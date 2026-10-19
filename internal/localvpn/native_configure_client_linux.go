//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureNativeDesktop requests explicit administrator authorization, then
// imports the owned profile as the desktop user. It never starts a service or
// connects a VPN, and does not change either autostart preference.
func ConfigureNativeDesktop(ctx context.Context, base, subscription string) error {
	binary, err := os.Executable()
	if err != nil || binary != InstalledCLI || os.Geteuid() == 0 {
		return errors.New("native setup requires the installed desktop application")
	}
	request, err := json.Marshal(nativeConfigurationRequest{Subscription: subscription, StatePath: base})
	if err != nil {
		return errors.New("invalid native configuration request")
	}
	// Keep an unprivileged supervisor between the bridge and pkexec. Cancelling
	// it is permitted; the supervisor closes the protocol pipes and makes a
	// bounded attempt to reap pkexec.
	command := exec.CommandContext(ctx, InstalledCLI, "native-configure-client")
	command.Stdin = bytes.NewReader(request)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 3 * time.Second
	if err = command.Run(); err != nil {
		return errors.New("native setup incomplete; check authorization, stopped services and subscription")
	}
	return nil
}

func RunNativeConfigurationClient(ctx context.Context, input io.Reader) error {
	// Bind to the original bridge parent before parsing input or starting pkexec.
	// The PPID recheck closes the race where the parent exits during prctl.
	if err := bindNativeProcessToParent(); err != nil {
		return err
	}
	request, err := readNativeConfigurationRequestBounded(ctx, input)
	if err != nil {
		return err
	}
	return configureNativeDesktop(ctx, request.StatePath, request.Subscription)
}

func configureNativeDesktop(ctx context.Context, base, subscription string) error {
	binary, err := os.Executable()
	if err != nil || binary != InstalledCLI || os.Geteuid() == 0 {
		return errors.New("native setup requires the installed desktop application")
	}
	if err = validateNativeGenerationSubscriptionURL(subscription); err != nil {
		return errors.New("invalid subscription URL")
	}
	if _, err = ResolveApplicationLayout(binary, InstalledAssetRoot, base, "native"); err != nil {
		return errors.New("invalid native state directory")
	}
	if err = ValidateInstalledAssets(); err != nil {
		return errors.New("installed native assets unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	release, err := lockNativeDesktop(bounded, base, true)
	if err != nil {
		return err
	}
	defer release()

	command := exec.CommandContext(bounded, "/usr/bin/pkexec", "--disable-internal-agent", InstalledCLI, "native-configure")
	command.Env = os.Environ()
	command.Stderr = io.Discard
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 2 * time.Second
	process := &nativeExecConfigurationProcess{command: command}
	return runNativeDesktopProtocol(bounded, process, nativeConfigurationRequest{Subscription: subscription, StatePath: base}, func(finalize context.Context) error {
		// These are intentionally unprivileged operations on the caller-owned
		// profile and subscription copy. They grant no authority to the helper.
		manager := NetworkManager{Base: base, Timeout: 30 * time.Second}
		if err := manager.Run(finalize, "import", true, io.Discard); err != nil {
			return errors.New("native configuration installed but profile import failed")
		}
		if err := WriteSubscription(base, subscription); err != nil {
			return errors.New("native configuration installed but private subscription copy failed")
		}
		return nil
	})
}

const (
	nativeProcessTerminateGrace = 500 * time.Millisecond
	nativeProcessKillGrace      = 500 * time.Millisecond
)

type nativeConfigurationProcess interface {
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
	Signal(os.Signal) error
	Kill() error
}

type nativeExecConfigurationProcess struct {
	command *exec.Cmd
}

func (p *nativeExecConfigurationProcess) StdinPipe() (io.WriteCloser, error) {
	return p.command.StdinPipe()
}
func (p *nativeExecConfigurationProcess) StdoutPipe() (io.ReadCloser, error) {
	return p.command.StdoutPipe()
}
func (p *nativeExecConfigurationProcess) Start() error { return p.command.Start() }
func (p *nativeExecConfigurationProcess) Wait() error  { return p.command.Wait() }
func (p *nativeExecConfigurationProcess) Signal(signal os.Signal) error {
	if p.command.Process == nil {
		return os.ErrProcessDone
	}
	return p.command.Process.Signal(signal)
}
func (p *nativeExecConfigurationProcess) Kill() error {
	if p.command.Process == nil {
		return os.ErrProcessDone
	}
	return p.command.Process.Kill()
}

func runNativeDesktopProtocol(ctx context.Context, process nativeConfigurationProcess, request nativeConfigurationRequest, finalize func(context.Context) error) (returnErr error) {
	requestData, err := json.Marshal(request)
	if err != nil || len(requestData)+1 > nativeConfigurationFrameLimit {
		return errors.New("invalid native configuration request")
	}
	requestData = append(requestData, '\n')
	stdin, err := process.StdinPipe()
	if err != nil {
		return errors.New("native setup authorization unavailable")
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return errors.New("native setup authorization unavailable")
	}
	if err = process.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return errors.New("native setup authorization unavailable or denied")
	}

	waited := false
	defer func() {
		if waited {
			return
		}
		// Closing both pipes is the fail-closed path: normally the root helper
		// observes EOF. If the privileged descendant cannot be signalled or
		// reaped, this supervisor must still exit so its PDEATHSIG can take effect.
		_ = stdin.Close()
		_ = stdout.Close()
		_ = process.Signal(syscall.SIGTERM)
		_ = waitNativeConfigurationProcess(context.Background(), process, 2*time.Second)
	}()

	protocol := newNativeConfigurationProtocol(stdout, stdin)
	if err = protocol.writeFrame(ctx, requestData, nativeProtocolIOTimeout); err != nil {
		return errors.New("native setup request failed")
	}
	if err = protocol.waitReady(ctx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.New("native setup interrupted; configuration retained for retry")
	}
	if err = finalize(ctx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.New("native setup interrupted; configuration retained for retry")
	}
	if err = protocol.acknowledge(ctx); err != nil {
		return err
	}
	_ = stdin.Close()
	if err = waitNativeConfigurationProcess(ctx, process, 10*time.Second); err != nil {
		waited = true
		return errors.New("native setup failed after desktop finalization")
	}
	waited = true
	return nil
}

func waitNativeConfigurationProcess(ctx context.Context, process nativeConfigurationProcess, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	case <-timer.C:
	}
	_ = process.Signal(syscall.SIGTERM)
	terminateGrace := time.NewTimer(nativeProcessTerminateGrace)
	defer terminateGrace.Stop()
	select {
	case err := <-done:
		return err
	case <-terminateGrace.C:
	}
	_ = process.Kill()
	killGrace := time.NewTimer(nativeProcessKillGrace)
	defer killGrace.Stop()
	select {
	case err := <-done:
		return err
	case <-killGrace.C:
		return errors.New("native configuration process could not be reaped")
	}
}
