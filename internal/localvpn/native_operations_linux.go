//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"github.com/blockedby/local-vpn-kde/internal/profiles"
)

// NativeOperationsClient retains the protocol pipe until the privileged worker
// finishes. Cancellation closes it so cleanup does not depend on signalling a
// privileged process from the desktop UID.
func NativeOperationsClient(ctx context.Context, args []string, output io.Writer) error {
	if err := validateNativeOperation(args); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return errors.New("native operation client requires desktop user")
	}
	command := exec.Command("/usr/bin/pkexec", "--disable-internal-agent", InstalledCLI, "native-operation")
	command.Stdout = output
	command.Stderr = io.Discard
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		input.Close()
		return err
	}
	defer input.Close()
	if err = json.NewEncoder(input).Encode(args); err != nil {
		input.Close()
		_ = command.Wait()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		input.Close()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
		return ctx.Err()
	}
}

func validateNativeOperation(args []string) error {
	bad := errors.New("unsupported native operation")
	if len(args) == 2 && args[0] == "profiles" {
		_, err := profiles.DecodeRequest(strings.NewReader(args[1]))
		return err
	}
	if len(args) == 1 {
		switch args[0] {
		case "diagnostics", "policy-status", "toggle-mode":
			return nil
		}
	}
	if len(args) < 2 || args[0] != "servers" {
		return bad
	}
	var value string
	switch args[1] {
	case "list", "refresh", "current":
		if len(args) != 2 {
			return bad
		}
	case "select", "ping":
		if len(args) != 3 {
			return bad
		}
		value = args[2]
	case "speed":
		if len(args) != 3 && !(len(args) == 4 && args[3] == "--progress") {
			return bad
		}
		value = args[2]
	case "availability":
		if len(args) != 4 {
			return bad
		}
		b, _ := json.Marshal(map[string]string{"ID": args[2], "URL": args[3]})
		value = string(b)
	case "check-batch", "speed-batch":
		if len(args) != 4 {
			return bad
		}
		b, _ := json.Marshal(map[string]any{"IDs": strings.Split(args[2], ","), "URL": args[3]})
		value = string(b)
	default:
		return bad
	}
	_, _, err := serverArguments("servers/"+args[1], value)
	return err
}

// Progress is a fixed run option, never part of the persisted operation identity.
// The caller validates args before constructing either supervisor request.
func nativeOperationRequests(token string, args []string) (prepare, run []string) {
	operation := append([]string{token}, args[1:]...)
	prepared := operation
	if len(args) == 4 && args[1] == "speed" && args[3] == "--progress" {
		prepared = operation[:len(operation)-1]
	}
	return append([]string{"prepare"}, prepared...), append([]string{"run"}, operation...)
}

// RunNativeOperation accepts only fixed application operations after Polkit
// authorization. It never accepts executable, config, namespace or UID paths.
func RunNativeOperation(parent context.Context, input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("native operation requires authorization")
	}
	if err := bindNativeProcessToParent(); err != nil {
		return err
	}
	uidText := os.Getenv("PKEXEC_UID")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 {
		return errors.New("native caller unavailable")
	}
	caller, err := user.LookupId(uidText)
	if err != nil {
		return err
	}
	group, err := user.LookupGroup("local-vpn-kde-users")
	if err != nil {
		return err
	}
	groups, err := caller.GroupIds()
	if err != nil {
		return err
	}
	allowed := false
	for _, g := range groups {
		if g == group.Gid {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("native caller is not enrolled")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	protocol := newNativeConfigurationProtocol(input, output)
	frame, err := protocol.readFrame(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	var args []string
	if json.Unmarshal(frame, &args) != nil {
		return errors.New("invalid native request")
	}
	if err = validateNativeOperation(args); err != nil {
		return err
	}
	go func() { _, _ = protocol.reader.ReadByte(); cancel() }()
	exclusive := args[0] == "toggle-mode" || args[0] == "profiles"
	release, err := acquireNativeConfigurationLock(ctx, exclusive)
	if err != nil {
		return err
	}
	defer release()
	if blocked, err := nativePackageBlocked(); err != nil || blocked {
		return errors.New("native package transaction in progress")
	}
	if args[0] == "policy-status" {
		policy, err := nativeInstalledPolicy(uint32(uid))
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]string{"routing_policy": policy})
	}
	if args[0] == "toggle-mode" {
		return toggleNativePolicy(ctx, uint32(uid), output)
	}
	if args[0] == "profiles" {
		identity, err := user.Lookup(nativegateway.ServiceUser)
		if err != nil {
			return err
		}
		command := []string{"--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--reuid", identity.Uid, "--regid", identity.Gid, "--clear-groups", "--", nativegateway.LibexecDir + "/vibe-vpn", "--config", "/etc/local-vpn-kde/vibe-vpn/config.yaml", "profiles"}
		result := runProcessInput(ctx, "/usr/bin/setpriv", command, []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LC_ALL=C", "HOME=/var/lib/local-vpn-kde"}, bytes.NewBufferString(args[1]), output, io.Discard, time.Second, true)
		if result.Reason != "ok" {
			return errors.New("native profile operation failed")
		}
		return nil
	}
	service := NativeService{}
	if err = service.mutationPreflight(ctx); err != nil {
		return err
	}
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	if args[0] == "diagnostics" {
		return json.NewEncoder(output).Encode(status)
	}
	if !status.Ready {
		return errors.New("native gateway is not ready")
	}
	if incomplete, err := nativeSetupIncomplete(); err != nil || incomplete {
		return errors.New("native setup incomplete")
	}
	// ip netns exec supplies this package-owned resolver to workers and their
	// probe children. Host loopback DNS stubs are unreachable in our namespace.
	if err = validateTrustedRegularFile("/etc/netns/"+nativegateway.Namespace+"/resolv.conf", 0, "/", false); err != nil {
		return errors.New("native namespace resolver unavailable")
	}
	identity, err := user.Lookup(nativegateway.ServiceUser)
	if err != nil {
		return err
	}
	worker := func(ctx context.Context, operation []string, out io.Writer) error {
		command := []string{"netns", "exec", nativegateway.Namespace, "/usr/bin/setpriv", "--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--reuid", identity.Uid, "--regid", identity.Gid, "--clear-groups", "--", nativegateway.LibexecDir + "/vibe-vpn-server-op-supervisor"}
		command = append(command, operation...)
		result := RunProcess(ctx, "/usr/bin/ip", command, []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LC_ALL=C", "HOME=/var/lib/local-vpn-kde"}, out, io.Discard, time.Second)
		if result.Reason != "ok" {
			return errors.New("native server operation failed")
		}
		return nil
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	token := "op_" + base64.RawURLEncoding.EncodeToString(random[:])
	prepare, run := nativeOperationRequests(token, args)
	if err = worker(ctx, prepare, io.Discard); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = worker(cleanup, []string{"cancel", token}, io.Discard)
		_ = worker(cleanup, []string{"verify", token}, io.Discard)
	}()
	return worker(ctx, run, output)
}
