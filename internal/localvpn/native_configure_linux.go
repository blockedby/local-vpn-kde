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
	"os/user"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type nativeConfigurationRequest struct {
	Subscription string `json:"subscription"`
	StatePath    string `json:"state_path"`
}

func readNativeConfigurationRequest(input io.Reader) (nativeConfigurationRequest, error) {
	var request nativeConfigurationRequest
	data, err := io.ReadAll(io.LimitReader(input, (32<<10)+1))
	if err != nil || len(data) > 32<<10 {
		return request, errors.New("invalid native configuration request")
	}
	// Decode from a bounded buffer without accepting additional objects/fields.
	decoder := json.NewDecoder(bytes.NewReader(data))
	begin, err := decoder.Token()
	if err != nil || begin != json.Delim('{') {
		return request, errors.New("invalid native configuration request")
	}
	seen := make(map[string]bool, 2)
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok || seen[key] || (key != "subscription" && key != "state_path") {
			return nativeConfigurationRequest{}, errors.New("invalid native configuration request")
		}
		seen[key] = true
		var value string
		if decoder.Decode(&value) != nil {
			return nativeConfigurationRequest{}, errors.New("invalid native configuration request")
		}
		if key == "subscription" {
			request.Subscription = value
		} else {
			request.StatePath = value
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nativeConfigurationRequest{}, errors.New("invalid native configuration request")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || request.Subscription == "" || request.StatePath == "" {
		return nativeConfigurationRequest{}, errors.New("invalid native configuration request")
	}
	return request, nil
}

func readNativeConfigurationRequestBounded(parent context.Context, input io.Reader) (nativeConfigurationRequest, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nativeConfigurationRequest{}, err
	}
	type result struct {
		request nativeConfigurationRequest
		err     error
	}
	done := make(chan result, 1)
	go func() { request, err := readNativeConfigurationRequest(input); done <- result{request, err} }()
	select {
	case result := <-done:
		return result.request, result.err
	case <-ctx.Done():
		return nativeConfigurationRequest{}, errors.New("native configuration input cancelled or timed out")
	}
}

// ConfigureNativeInstallation is the fixed pkexec entry point. Its input never
// contains executable paths, unit names, owner IDs, or caller-generated configs.
// It does not start services, connect NetworkManager, or enable autostart.
func ConfigureNativeInstallation(ctx context.Context, input io.Reader) error {
	return configureNativeInstallationProtocol(ctx, input, os.Stdout)
}

func configureNativeInstallationProtocol(ctx context.Context, input io.Reader, output io.Writer) error {
	binary, err := os.Executable()
	if err != nil || binary != InstalledCLI || os.Geteuid() != 0 {
		return errors.New("native configuration requires the authorized installed helper")
	}
	if err = bindNativeProcessToParent(); err != nil {
		return err
	}
	uidText := os.Getenv("PKEXEC_UID")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != uidText {
		return errors.New("native configuration caller is unavailable")
	}
	owner, err := user.LookupId(uidText)
	if err != nil {
		return errors.New("native configuration caller is unavailable")
	}
	gid, err := strconv.ParseUint(owner.Gid, 10, 32)
	if err != nil {
		return errors.New("native configuration caller is unavailable")
	}
	service, err := user.Lookup("local-vpn-kde")
	if err != nil {
		return errors.New("native service identity is unavailable")
	}
	serviceUID, uidErr := strconv.ParseUint(service.Uid, 10, 32)
	serviceGID, gidErr := strconv.ParseUint(service.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || serviceUID == 0 || serviceUID == uid {
		return errors.New("native service identity is unavailable")
	}
	if _, err = user.LookupGroup("local-vpn-kde-users"); err != nil {
		return errors.New("native authorization group is unavailable")
	}
	if err = ValidateInstalledAssets(); err != nil {
		return errors.New("installed native assets are unavailable")
	}
	protocol := newNativeConfigurationProtocol(input, output)
	request, err := protocol.readRequest(ctx)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Own all post-request input from this point forward. EOF or a malformed
	// frame cancels lock acquisition and root work immediately; a valid ACK is
	// retained until READY rather than starting its one-minute final wait early.
	acknowledgement := protocol.watchAcknowledgement(bounded, cancel)
	release, err := acquireNativeConfigurationLock(bounded, true)
	if err != nil {
		return errors.New("native setup lock unavailable")
	}
	defer release()
	blocked, err := nativePackageBlocked()
	if err != nil || blocked {
		return errors.New("native setup blocked by a package transaction")
	}
	dir, err := directory("/run/local-vpn-kde-install", true, true)
	if err != nil {
		return errors.New("native setup lock unavailable")
	}
	defer unix.Close(dir)
	lock, err := lockFile(bounded, dir, "configure.lock")
	if err != nil {
		return errors.New("native setup lock unavailable")
	}
	defer unix.Close(lock)
	return configureNativeInstallation(bounded, request, uint32(uid), uint32(gid), nativeConfigurationOperations{
		inactive:           nativeConfigurationInactive,
		generate:           GenerateNativeConfiguration,
		profile:            startNativeProfileTransaction,
		publish:            PublishNativeConfiguration,
		setupIncomplete:    nativeSetupIncomplete,
		setSetupIncomplete: setNativeSetupIncomplete,
		selectServer: func(ctx context.Context) error {
			return nativeConfigurationCommand(ctx, "/usr/bin/setpriv", "--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--reuid", strconv.FormatUint(serviceUID, 10), "--regid", strconv.FormatUint(serviceGID, 10), "--clear-groups", "--", "/usr/libexec/local-vpn-kde/vibe-vpn", "--config", "/etc/local-vpn-kde/vibe-vpn/config.yaml", "native-initialize-selection")
		},
		enroll: func(ctx context.Context) error {
			deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			usermod, err := nativeSystemExecutable("usermod")
			if err != nil {
				return err
			}
			return nativeConfigurationCommand(deadline, usermod, "--append", "--groups", "local-vpn-kde-users", "--", owner.Username)
		},
		desktop: func(ctx context.Context) error {
			return protocol.readyAndWait(ctx, acknowledgement)
		},
	})
}

func bindNativeProcessToParent() error {
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0) != nil || os.Getppid() != parent {
		return errors.New("native configuration caller disappeared")
	}
	return nil
}

func nativeConfigurationCommand(ctx context.Context, program string, args ...string) error {
	command := exec.CommandContext(ctx, program, args...)
	command.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "HOME=/var/lib/local-vpn-kde", "LANG=C.UTF-8"}
	command.Dir = "/"
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 5 * time.Second
	// Benchmark output contains private endpoints; never forward or persist it.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func nativeConfigurationInactive(ctx context.Context) error {
	service := NativeService{}
	if err := service.mutationPreflight(ctx); err != nil {
		return err
	}
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	if status.Gateway != "inactive" || status.Network != "inactive" {
		return errors.New("native services must be stopped before configuration")
	}
	return nil
}

type nativeConfigurationOperations struct {
	inactive           func(context.Context) error
	generate           func(context.Context, string, uint32) (NativeGeneratedConfiguration, error)
	profile            func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error)
	publish            func(context.Context, NativeConfigurationBundle) error
	selectServer       func(context.Context) error
	enroll             func(context.Context) error
	desktop            func(context.Context) error
	setupIncomplete    func() (bool, error)
	setSetupIncomplete func(bool) error
}

func configureNativeInstallation(ctx context.Context, request nativeConfigurationRequest, uid, gid uint32, operations nativeConfigurationOperations) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if operations.inactive(ctx) != nil {
		return errors.New("native services must be stopped before configuration")
	}
	generated, err := operations.generate(ctx, request.Subscription, uid)
	if err != nil {
		return errors.New("native configuration generation failed")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if operations.inactive(ctx) != nil {
		return errors.New("native services must be stopped before configuration")
	}

	priorIncomplete, err := operations.setupIncomplete()
	if err != nil {
		return errors.New("native setup inhibitor state is unavailable")
	}
	if err = operations.setSetupIncomplete(true); err != nil {
		return errors.New("native setup inhibitor could not be installed")
	}
	clearAfterRollback := func(tx nativeProfileTransaction) bool {
		if tx != nil && tx.Rollback() != nil {
			return false
		}
		if !priorIncomplete && operations.setSetupIncomplete(false) != nil {
			return false
		}
		return true
	}

	tx, err := operations.profile(ctx, uid, gid, request.StatePath, generated.ClientProfile)
	if err != nil {
		// The child may have lost its final rollback acknowledgement. Conservatively
		// retain the inhibitor; a retry is the recovery mechanism.
		return errors.New("native client profile staging failed; setup retained for retry")
	}
	if err = ctx.Err(); err != nil {
		if !clearAfterRollback(tx) {
			return errors.New("native setup canceled and profile rollback is uncertain")
		}
		return err
	}
	publishErr := operations.publish(ctx, generated.Bundle)
	if publishErr != nil {
		var committed *NativeConfigurationCommittedError
		if errors.As(publishErr, &committed) {
			if tx.Commit() != nil {
				return errors.New("native configuration committed but profile commit is uncertain; setup retained for retry")
			}
			return errors.New("native configuration was committed with a durability or cleanup error; setup retained for retry")
		}
		if !clearAfterRollback(tx) {
			return errors.New("native configuration publication failed and profile rollback is uncertain")
		}
		return errors.New("native configuration publication failed")
	}
	if tx.Commit() != nil {
		return errors.New("native client profile commit failed; configuration retained for retry")
	}
	if operations.selectServer(ctx) != nil {
		return errors.New("native server selection failed; configuration retained for retry")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if operations.enroll(ctx) != nil {
		return errors.New("native authorization enrollment failed; configuration retained for retry")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if operations.desktop == nil || operations.desktop(ctx) != nil {
		return errors.New("native desktop finalization failed; configuration retained for retry")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if operations.setSetupIncomplete(false) != nil {
		return errors.New("native setup completed but inhibitor cleanup failed")
	}
	return nil
}
