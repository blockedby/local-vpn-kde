//go:build linux

// The native-gateway lab is intentionally test-only. It exercises the real
// Controller and LinuxNetworkInspector only inside an explicitly opted-in
// disposable container.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

const (
	labOptIn        = "VPNKIT_NATIVE_GATEWAY_KERNEL_LAB"
	uplinkName      = "lab-uplink"
	uplinkAddress   = "192.0.2.10/24"
	uplinkGateway   = "192.0.2.1"
	unrelatedName   = "lab-unrelated"
	unrelatedCIDR   = "203.0.113.10/24"
	externalTarget  = "198.51.100.77"
	commandDeadline = 5 * time.Second
)

var uplink = nativegateway.Uplink{
	Interface: uplinkName, AddressCIDR: uplinkAddress, Gateway: uplinkGateway,
}

func main() {
	if err := requireDisposableLab(); err != nil {
		fatal(err)
	}
	if len(os.Args) == 2 && os.Args[1] == "--echo-child" {
		if err := echoOnce(); err != nil {
			fatal(fmt.Errorf("UDP echo child: %w", err))
		}
		return
	}
	if len(os.Args) != 1 {
		fatal(fmt.Errorf("unexpected arguments: %q", os.Args[1:]))
	}
	if err := runLab(); err != nil {
		fatal(err)
	}
	fmt.Println("native_gateway_kernel_lab=pass")
	fmt.Println("coverage=real FileJournal persistence/reopening, Controller and LinuxNetworkInspector start/idempotent-start/stop/idempotent-stop with kernel routing, nftables, namespace inspection, and loopback UDP")
	fmt.Println("limitations=fixture-only NetworkManager physical label; no full VPN or systemd lifecycle")
}

func requireDisposableLab() error {
	if os.Getenv(labOptIn) != "1" {
		return fmt.Errorf("refusing kernel mutation without %s=1", labOptIn)
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return fmt.Errorf("refusing kernel mutation without Docker container marker /.dockerenv: %w", err)
	}
	return nil
}

func runLab() (runErr error) {
	reads := readOnlyRunner{}
	inspector := nativegateway.NewLinuxNetworkInspector(reads)

	initial, err := inspectSnapshot(inspector)
	if err != nil {
		return fmt.Errorf("capture complete initial snapshot: %w", err)
	}
	if err := verifyFixtureSnapshot(initial); err != nil {
		return fmt.Errorf("initial fixture snapshot: %w", err)
	}
	if err := verifyOwnedResourcesAbsent(initial); err != nil {
		return fmt.Errorf("initial owned-resource state: %w", err)
	}

	mutations := &mutationRunner{}
	journal, err := openNetworkJournal()
	if err != nil {
		return err
	}
	controller := nativegateway.Controller{Runner: mutations, Journal: journal, Inspector: inspector}

	// Emergency cleanup is restricted to the controller's fixed resource names.
	// A successful run performs all cleanup through Controller.Stop instead.
	defer cleanupOwnedResources()
	defer func() {
		if journal == nil {
			return
		}
		if closeErr := journal.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close network journal: %w", closeErr))
		}
	}()

	if err := callController("first start", func(ctx context.Context) error {
		return controller.Start(ctx, uplink)
	}); err != nil {
		return err
	}
	firstStartMutations := mutations.count
	if firstStartMutations == 0 {
		return errors.New("first Controller.Start executed no mutations")
	}
	if err := verifyDedicatedRoute(); err != nil {
		return err
	}
	if err := verifyLoopbackUDP(); err != nil {
		return err
	}

	if err := reopenNetworkJournal(&journal); err != nil {
		return fmt.Errorf("reopen network journal before idempotent start: %w", err)
	}
	controller.Journal = journal
	if err := verifyControllerStatus("after first start and journal reopen", controller, true); err != nil {
		return err
	}

	if err := callController("idempotent start", func(ctx context.Context) error {
		return controller.Start(ctx, uplink)
	}); err != nil {
		return err
	}
	if mutations.count != firstStartMutations {
		return fmt.Errorf("second Controller.Start mutated kernel: command count changed from %d to %d", firstStartMutations, mutations.count)
	}
	if err := verifyDedicatedRoute(); err != nil {
		return fmt.Errorf("routing after idempotent start: %w", err)
	}
	if err := verifyLoopbackUDP(); err != nil {
		return fmt.Errorf("UDP after idempotent start: %w", err)
	}

	// Preserve the original fail-closed proof, then restore the exact route
	// owned by the journal before Stop so cleanup can safely prove ownership.
	if _, err := commandOutput(commandDeadline, nil, "ip", "route", "del", "default", "via", uplinkGateway, "dev", uplinkName, "table", strconv.Itoa(nativegateway.RoutingTable), "protocol", strconv.Itoa(nativegateway.RouteProtocol)); err != nil {
		return fmt.Errorf("remove dedicated-table default for fail-closed check: %w", err)
	}
	if err := verifyUnreachableWithoutDefault(); err != nil {
		return err
	}
	if _, err := commandOutput(commandDeadline, nil, "ip", "route", "add", "default", "via", uplinkGateway, "dev", uplinkName, "table", strconv.Itoa(nativegateway.RoutingTable), "protocol", strconv.Itoa(nativegateway.RouteProtocol)); err != nil {
		return fmt.Errorf("restore exact owned dedicated-table default before stop: %w", err)
	}
	if err := verifyDedicatedRoute(); err != nil {
		return fmt.Errorf("restored dedicated route: %w", err)
	}
	if err := verifyControllerStatus("before stop", controller, true); err != nil {
		return err
	}

	if err := callController("first stop", controller.Stop); err != nil {
		return err
	}
	firstStopMutations := mutations.count
	if firstStopMutations == firstStartMutations {
		return errors.New("first Controller.Stop executed no cleanup mutations")
	}
	if err := verifyControllerStatus("after first stop", controller, false); err != nil {
		return err
	}
	if exists, err := loadNetworkJournal(journal, "after first stop"); err != nil {
		return err
	} else if exists {
		return errors.New("Controller.Stop did not durably clear the ownership journal")
	}

	finalSnapshot, err := inspectSnapshot(inspector)
	if err != nil {
		return fmt.Errorf("capture complete post-stop snapshot: %w", err)
	}
	if err := verifyOwnedResourcesAbsent(finalSnapshot); err != nil {
		return fmt.Errorf("post-stop owned-resource state: %w", err)
	}
	if err := verifyFixtureSnapshot(finalSnapshot); err != nil {
		return fmt.Errorf("post-stop fixture preservation: %w", err)
	}
	if err := verifySnapshotRestored(initial, finalSnapshot); err != nil {
		return err
	}

	if err := reopenNetworkJournal(&journal); err != nil {
		return fmt.Errorf("reopen cleared network journal: %w", err)
	}
	controller.Journal = journal
	if err := verifyControllerStatus("after stop and journal reopen", controller, false); err != nil {
		return err
	}
	if exists, err := loadNetworkJournal(journal, "after stop and journal reopen"); err != nil {
		return err
	} else if exists {
		return errors.New("cleared ownership journal reappeared after reopen")
	}

	if err := callController("idempotent stop", controller.Stop); err != nil {
		return err
	}
	if mutations.count != firstStopMutations {
		return fmt.Errorf("second Controller.Stop mutated kernel: command count changed from %d to %d", firstStopMutations, mutations.count)
	}
	if err := verifyFixtureSnapshot(finalSnapshot); err != nil {
		return fmt.Errorf("fixtures after idempotent stop: %w", err)
	}
	return nil
}

// readOnlyRunner executes every inspector/probe read against the real kernel.
// Only nmcli is fixture-overridden because Docker's dummy lab-uplink cannot be
// NetworkManager-managed. Stdout is read separately, bounded while reading,
// and every process receives both the caller deadline and a local deadline.
type readOnlyRunner struct{}

func (readOnlyRunner) Run(ctx context.Context, program string, args ...string) ([]byte, error) {
	if program == "nmcli" {
		want := []string{"-t", "--escape", "yes", "--fields", "DEVICE,TYPE,STATE", "device", "status"}
		if !reflect.DeepEqual(args, want) {
			return nil, errors.New("unexpected nmcli inspection command")
		}
		return []byte(uplinkName + ":ethernet:connected\n"), nil
	}
	switch program {
	case "ip", "nft", "sysctl":
	default:
		return nil, fmt.Errorf("read-only runner rejected program %q", program)
	}
	return runBounded(ctx, commandDeadline, nil, program, args...)
}

type mutationRunner struct {
	count int
}

func (r *mutationRunner) Run(ctx context.Context, command nativegateway.Command) error {
	switch command.Program {
	case "ip", "nft", "sysctl":
	default:
		return fmt.Errorf("mutation runner rejected program %q", command.Program)
	}
	if len(command.Stdin) > 1<<20 {
		return errors.New("mutation command input exceeded limit")
	}
	if command.Stdin != "" && (command.Program != "nft" || !reflect.DeepEqual(command.Args, []string{"-f", "-"})) {
		return errors.New("mutation command input is not permitted")
	}
	r.count++
	_, err := runBounded(ctx, commandDeadline, strings.NewReader(command.Stdin), command.Program, command.Args...)
	return err
}

func openNetworkJournal() (*nativegateway.FileJournal, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	journal, err := nativegateway.OpenNetworkJournal(ctx)
	if err != nil {
		return nil, fmt.Errorf("open network journal: %w", err)
	}
	return journal, nil
}

func reopenNetworkJournal(journal **nativegateway.FileJournal) error {
	if journal == nil {
		return errors.New("network journal holder is nil")
	}
	if *journal != nil {
		if err := (*journal).Close(); err != nil {
			return fmt.Errorf("close network journal before reopen: %w", err)
		}
		*journal = nil
	}
	reopened, err := openNetworkJournal()
	if err != nil {
		return err
	}
	*journal = reopened
	return nil
}

func verifyControllerStatus(label string, controller nativegateway.Controller, want bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ready, err := controller.Status(ctx)
	if err != nil {
		return fmt.Errorf("Controller.Status %s: %w", label, err)
	}
	if ready != want {
		return fmt.Errorf("Controller.Status %s returned %t, want %t", label, ready, want)
	}
	return nil
}

func loadNetworkJournal(journal *nativegateway.FileJournal, label string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, exists, err := journal.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("load network journal %s: %w", label, err)
	}
	return exists, nil
}

func callController(label string, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := call(ctx); err != nil {
		return fmt.Errorf("Controller %s: %w", label, err)
	}
	return nil
}

func inspectSnapshot(inspector *nativegateway.LinuxNetworkInspector) (nativegateway.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return inspector.Snapshot(ctx)
}

func verifyFixtureSnapshot(snapshot nativegateway.Snapshot) error {
	fixtures := map[string]string{uplinkName: uplinkAddress, unrelatedName: unrelatedCIDR}
	for name, cidr := range fixtures {
		found := false
		for _, state := range snapshot.Interfaces {
			if state.Name != name {
				continue
			}
			for _, address := range state.Addresses {
				if address == cidr {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("fixture interface %s with address %s is absent", name, cidr)
		}
	}
	for _, route := range snapshot.Routes {
		if route.Table == 254 && route.Destination == "default" && route.Gateway == uplinkGateway && route.Interface == uplinkName {
			return nil
		}
	}
	return errors.New("lab-uplink default route is absent")
}

func verifyOwnedResourcesAbsent(snapshot nativegateway.Snapshot) error {
	for _, state := range snapshot.Interfaces {
		if state.Name == nativegateway.HostInterface || state.Name == nativegateway.GuestInterface {
			return fmt.Errorf("owned interface %s remains", state.Name)
		}
	}
	for _, namespace := range snapshot.Namespaces {
		if namespace == nativegateway.Namespace {
			return errors.New("owned namespace remains")
		}
	}
	for _, route := range snapshot.Routes {
		if route.Table == nativegateway.RoutingTable {
			return errors.New("owned routing table remains populated")
		}
	}
	for _, rule := range snapshot.Rules {
		if rule.Priority == nativegateway.EgressRulePriority || rule.Priority == nativegateway.UnreachableRulePriority || rule.Table == nativegateway.RoutingTable {
			return errors.New("owned policy rule remains")
		}
	}
	for _, table := range snapshot.NFTables {
		if table.Family == nativegateway.NFTFamily && table.Name == nativegateway.NFTTable {
			return errors.New("owned nftables table remains")
		}
	}
	if _, err := os.Stat("/run/netns/" + nativegateway.Namespace); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("owned namespace marker remains")
		}
		return fmt.Errorf("cannot prove owned namespace marker absent: %w", err)
	}
	return nil
}

func verifySnapshotRestored(initial, final nativegateway.Snapshot) error {
	// Controller deliberately relinquishes global IPv4 forwarding instead of
	// disabling a setting another service may have begun using.
	final.IPv4Forwarding = initial.IPv4Forwarding
	if !reflect.DeepEqual(initial, final) {
		return errors.New("complete post-stop snapshot differs from initial snapshot apart from intentionally relinquished IPv4 forwarding")
	}
	return nil
}

func verifyDedicatedRoute() error {
	output, err := routeGet()
	if err != nil {
		return fmt.Errorf("guest-source route lookup: %w", err)
	}
	text := string(output)
	for _, fragment := range []string{"via " + uplinkGateway, "dev " + uplinkName, "table " + strconv.Itoa(nativegateway.RoutingTable)} {
		if !strings.Contains(text, fragment) {
			return fmt.Errorf("guest-source route did not use dedicated uplink table; missing %q in %q", fragment, strings.TrimSpace(text))
		}
	}
	return nil
}

func routeGet() ([]byte, error) {
	return commandOutput(3*time.Second, nil, "ip", "-4", "route", "get", externalTarget,
		"from", nativegateway.GuestAddress, "iif", nativegateway.HostInterface)
}

func verifyUnreachableWithoutDefault() error {
	output, err := routeGet()
	if err == nil {
		return fmt.Errorf("guest-source route remained reachable after dedicated default removal: %q", strings.TrimSpace(string(output)))
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unreachable") {
		return fmt.Errorf("guest-source fail-closed lookup returned unexpected kernel error: %w", err)
	}
	return nil
}

func verifyLoopbackUDP() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lab executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "ip", "netns", "exec", nativegateway.Namespace, executable, "--echo-child")
	var childLog bytes.Buffer
	child.Stdout = &childLog
	child.Stderr = &childLog
	if err := child.Start(); err != nil {
		return fmt.Errorf("start namespace UDP echo child: %w", err)
	}
	waited := false
	defer func() {
		if !waited && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	connection, err := net.DialTimeout("udp4", fmt.Sprintf("127.0.0.1:%d", nativegateway.LoopbackPort), time.Second)
	if err != nil {
		return fmt.Errorf("open loopback UDP client: %w", err)
	}
	defer connection.Close()
	payload := []byte("native-gateway-loopback")
	buffer := make([]byte, 128)
	var exchangeErr error
	exchanged := false
	for attempt := 0; attempt < 12; attempt++ {
		_ = connection.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err = connection.Write(payload); err == nil {
			var count int
			count, err = connection.Read(buffer)
			if err == nil && bytes.Equal(buffer[:count], payload) {
				exchanged = true
				break
			}
			if err == nil {
				err = fmt.Errorf("echo payload mismatch: got %q", buffer[:count])
			}
		}
		exchangeErr = err
		select {
		case <-ctx.Done():
			attempt = 12
		case <-time.After(75 * time.Millisecond):
		}
	}
	if !exchanged {
		if exchangeErr == nil {
			exchangeErr = errors.New("no echoed datagram received")
		}
		return fmt.Errorf("127.0.0.1:%d -> namespace %s:%d UDP exchange failed: %w; child output: %s",
			nativegateway.LoopbackPort, nativegateway.GuestAddress, nativegateway.GatewayPort, exchangeErr, strings.TrimSpace(childLog.String()))
	}
	if err := child.Wait(); err != nil {
		waited = true
		return fmt.Errorf("namespace UDP echo child exited unsuccessfully: %w; output: %s", err, strings.TrimSpace(childLog.String()))
	}
	waited = true
	return nil
}

func echoOnce() error {
	address := fmt.Sprintf("%s:%d", nativegateway.GuestAddress, nativegateway.GatewayPort)
	listener, err := net.ListenPacket("udp4", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		return err
	}
	buffer := make([]byte, 128)
	count, peer, err := listener.ReadFrom(buffer)
	if err != nil {
		return err
	}
	_, err = listener.WriteTo(buffer[:count], peer)
	return err
}

type boundedLabStderr struct{ bytes.Buffer }

func (b *boundedLabStderr) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 4096 - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func runBounded(parent context.Context, timeout time.Duration, stdin io.Reader, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Stdin = stdin
	var stderr boundedLabStderr
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare %s: %w", program, err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", program, err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(nativegateway.ProbeMaxOutputBytes)+1))
	if len(output) > nativegateway.ProbeMaxOutputBytes {
		cancel()
		_ = command.Wait()
		return nil, fmt.Errorf("%s output exceeded %d bytes", program, nativegateway.ProbeMaxOutputBytes)
	}
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s timed out: %w", program, ctx.Err())
	}
	if readErr != nil {
		return nil, fmt.Errorf("read %s output: %w", program, readErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("%s exited unsuccessfully: %w; stderr: %s", program, waitErr, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func commandOutput(timeout time.Duration, stdin io.Reader, program string, args ...string) ([]byte, error) {
	return runBounded(context.Background(), timeout, stdin, program, args...)
}

func cleanupOwnedResources() {
	commands := [][]string{
		{"nft", "delete", "table", nativegateway.NFTFamily, nativegateway.NFTTable},
		{"ip", "rule", "del", "priority", strconv.Itoa(nativegateway.EgressRulePriority)},
		{"ip", "rule", "del", "priority", strconv.Itoa(nativegateway.UnreachableRulePriority)},
		{"ip", "route", "flush", "table", strconv.Itoa(nativegateway.RoutingTable)},
		{"ip", "netns", "del", nativegateway.Namespace},
		{"ip", "link", "del", nativegateway.HostInterface},
	}
	for _, item := range commands {
		_, _ = commandOutput(2*time.Second, nil, item[0], item[1:]...)
	}
}

func fatal(err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(os.Stderr, "native gateway kernel lab timed out:", err)
	} else {
		fmt.Fprintln(os.Stderr, "native gateway kernel lab failed:", err)
	}
	os.Exit(1)
}
