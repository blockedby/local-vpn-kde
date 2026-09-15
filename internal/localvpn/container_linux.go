package localvpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type runtimeChild struct {
	cancel context.CancelFunc
	done   chan struct{}
	result ProcessResult
}

func startRuntimeChild(ctx context.Context, name string, args []string, output io.Writer) *runtimeChild {
	childCtx, cancel := context.WithCancel(ctx)
	child := &runtimeChild{cancel: cancel, done: make(chan struct{})}
	go func() {
		child.result = RunProcess(childCtx, name, args, os.Environ(), output, output, time.Second)
		close(child.done)
	}()
	return child
}
func (child *runtimeChild) stopped() bool {
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}
func (child *runtimeChild) stop() {
	if child == nil {
		return
	}
	child.cancel()
	<-child.done
}
func waitRuntime(ctx context.Context, child *runtimeChild, ready func() bool) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		if child.stopped() {
			return errors.New("runtime child exited before readiness")
		}
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-child.done:
			return errors.New("runtime child exited before readiness")
		case <-timer.C:
			return errors.New("runtime readiness timed out")
		case <-ticker.C:
		}
	}
}

// ServeContainer replaces shell process supervision. It serves restart tokens
// while bootstrap/recovery wait for acknowledgements under their state lock.
func (c ContainerConfig) ServeContainer(parent context.Context, output io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	processCtx, cancelProcesses := context.WithCancel(context.WithoutCancel(parent))
	defer cancelProcesses()
	// A runtime invocation is explicitly container-only; never route the host.
	if _, err := os.Stat("/.dockerenv"); err != nil {
		if _, podmanErr := os.Stat("/run/.containerenv"); podmanErr != nil {
			return errors.New("container runtime marker missing")
		}
	}
	for key, value := range map[string]string{"SINGBOX_RESTART_FILE": c.Request, "SINGBOX_GENERATION_FILE": c.Generation, "SINGBOX_ACK_FILE": c.Ack} {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	env := append(os.Environ(), "VIBE_VPN_DEFER_TRANSACTION_RECOVERY=1")
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	result := RunProcess(syncCtx, "vibe-vpn", []string{"sync-sing-box-config", "--source", c.Source, "--runtime", c.Runtime}, env, output, output, time.Second)
	syncCancel()
	if result.Reason != "ok" {
		return errors.New("runtime configuration synchronization failed")
	}
	if _, err := runtimeCommand(ctx, c.Binary, "check", "-c", c.Runtime); err != nil {
		return err
	}
	var singbox, openvpn, recovery, bootstrap *runtimeChild
	var dnsDone chan struct{}
	var bootstrapLog *os.File
	var bootstrapCapture *attemptOutput
	defer func() {
		barrierCtx, barrierDone := context.WithTimeout(context.Background(), 10*time.Second)
		barrierErr := c.InstallBarrier(barrierCtx)
		barrierDone()
		if barrierErr != nil {
			openvpn.stop()
		}
		cancelProcesses()
		cancel()
		if dnsDone != nil {
			<-dnsDone
		}
		bootstrap.stop()
		recovery.stop()
		openvpn.stop()
		singbox.stop()
		if bootstrapLog != nil {
			if bootstrapCapture != nil && bootstrapCapture.truncated {
				_, _ = bootstrapLog.Write(bootstrapCapture.tail)
			}
			_ = bootstrapLog.Sync()
			_ = bootstrapLog.Close()
		}
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = c.RemoveBarrier(cleanup)
	}()
	startSingbox := func() error {
		singbox = startRuntimeChild(processCtx, c.Binary, []string{"run", "-c", c.Runtime}, output)
		return waitRuntime(ctx, singbox, func() bool { _, err := runtimeCommand(ctx, "ip", "link", "show", c.Interface); return err == nil })
	}
	if err := startSingbox(); err != nil {
		return err
	}
	if err := c.InstallBarrier(ctx); err != nil {
		return err
	}
	openvpn = startRuntimeChild(processCtx, "openvpn", []string{"--config", c.OpenVPN}, output)
	if err := waitRuntime(ctx, openvpn, func() bool { _, e := runtimeCommand(ctx, "ip", "link", "show", "tun0"); return e == nil }); err != nil {
		return err
	}
	if err := c.ApplyRouting(ctx); err != nil {
		return err
	}
	if err := c.Health(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "vpnkit_phase=runtime-ready")
	if _, err := runtimeRead(c.Vibe); err == nil {
		recovery = startRuntimeChild(processCtx, "vibe-vpn", []string{"--config", c.Vibe, "recover-transactions"}, io.Discard)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	bootstrapSetting := strings.ToLower(os.Getenv("VPNKIT_BOOTSTRAP_PICK_ON_START"))
	switch bootstrapSetting {
	case "1", "true", "yes", "on":
		max := os.Getenv("VPNKIT_BOOTSTRAP_MAX_NODES")
		if max == "" {
			max = "50"
		}
		n, err := strconv.Atoi(max)
		if err != nil || n < 1 || n > 1000 {
			return errors.New("invalid bootstrap node limit")
		}
		// Detailed selection output is bounded by the same private capture used by
		// host operations; stdout carries phase information only.
		logDir, err := directory("/var/log/vibe-vpn", true, true)
		if err != nil {
			return err
		}
		defer unix.Close(logDir)
		logFD, err := unix.Openat(logDir, "bootstrap-selection.log", unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return err
		}
		log := os.NewFile(uintptr(logFD), "bootstrap-selection.log")
		bootstrapLog = log
		if err = validatePrivateFile(logFD); err != nil {
			return err
		}
		if err = log.Truncate(0); err != nil {
			return err
		}
		capture := &attemptOutput{file: log}
		bootstrapCapture = capture
		bootstrap = startRuntimeChild(processCtx, "vibe-vpn", []string{"--config", c.Vibe, "pick", "--max", max}, capture)
	case "", "0", "false", "no", "off":
	default:
		return errors.New("invalid bootstrap setting")
	}
	dnsDone = make(chan struct{})
	go func() { defer close(dnsDone); c.watchDNS(ctx, output) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		select {
		case <-dnsDone:
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("DNS watchdog exited")
		default:
		}
		if singbox.stopped() || openvpn.stopped() {
			return errors.New("VPN runtime process exited")
		}
		if recovery != nil && recovery.stopped() {
			if recovery.result.Reason != "ok" {
				return errors.New("runtime transaction recovery failed")
			}
			recovery = nil
		}
		if bootstrap != nil && bootstrap.stopped() {
			if bootstrap.result.Reason != "ok" {
				return errors.New("bootstrap selection failed")
			}
			bootstrap = nil
			fmt.Fprintln(output, "bootstrap selection complete")
		}
		data, err := runtimeRead(c.Request)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		token := strings.TrimRight(string(data), "\r\n")
		if token != "" && token != last {
			if len(token) > 512 || strings.ContainsAny(token, "\r\n\x00") {
				return errors.New("invalid restart token")
			}
			last = token
			before, err := runtimeRead(c.Generation)
			if err != nil {
				return err
			}
			if err = c.InstallBarrier(ctx); err != nil {
				return err
			}
			if _, err = runtimeCommand(ctx, c.Binary, "check", "-c", c.Runtime); err != nil {
				return err
			}
			singbox.stop()
			if err = startSingbox(); err != nil {
				return err
			}
			if err = c.ApplyRouting(ctx); err != nil {
				return err
			}
			if err = c.Health(ctx); err != nil {
				return err
			}
			after, err := runtimeRead(c.Generation)
			if err != nil {
				return err
			}
			if string(after) == string(before) {
				return errors.New("restart generation did not advance")
			}
			if err = runtimeWrite(c.Ack, []byte("token="+token+"\ngeneration="+strings.TrimSpace(string(after))+"\nhealth=healthy\n")); err != nil {
				return err
			}
			current, err := runtimeRead(c.Request)
			if err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
			if strings.TrimRight(string(current), "\r\n") == token {
				if err = runtimeRemove(c.Request); err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
