//go:build linux

// The native VPN lab is test-only. It mutates networking only after proving it
// is running in an explicitly opted-in disposable Docker container.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/localvpn"
	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"github.com/blockedby/local-vpn-kde/internal/subscription"
	"github.com/blockedby/local-vpn-kde/internal/vless"
)

const (
	labOptIn                = "VPNKIT_NATIVE_GATEWAY_VPN_LAB"
	realLabOptIn            = "VPNKIT_NATIVE_GATEWAY_REAL_SUBSCRIPTION_LAB"
	fixtureRoot             = "/fixture"
	privateRoot             = "/run/native-vpn-lab"
	uplinkName              = "lab-uplink"
	wanPeerName             = "lab-wan0"
	wanNamespace            = "lab-wan"
	uplinkAddress           = "192.0.2.10/24"
	uplinkGateway           = "192.0.2.1"
	wanTarget               = "198.18.0.1"
	wanPort                 = 18080
	expectedBody            = "native-gateway-vpn-fixture"
	commandTimeout          = 8 * time.Second
	labModeEnv              = "VPNKIT_NATIVE_GATEWAY_VPN_LAB_MODE"
	rootMode                = "root"
	serviceMode             = "service"
	serviceUID              = 987
	serviceGID              = 987
	serviceCaps             = "0000000000003000"
	realBenchmarkMaxDefault = 50
	realBenchmarkMaxLimit   = 50
	realBenchmarkReadLimit  = 64 << 10
	realProbeHost           = "www.cloudflare.com" // legacy synthetic baseline only
	realProbePath           = "/cdn-cgi/trace"
)

var uplink = nativegateway.Uplink{Interface: uplinkName, AddressCIDR: uplinkAddress, Gateway: uplinkGateway}

func main() {
	if err := requireDisposableLab(); err != nil {
		fatal("guard", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	realMax, realArgsOK := parseRealLabArgs(os.Args[1:])
	orchestrator := len(os.Args) == 1 || (realLabMode() && realArgsOK)
	switch {
	case len(os.Args) == 2 && os.Args[1] == "--namespace-init":
		err = localvpn.InitializeNativeNamespace(ctx)
	case len(os.Args) == 2 && os.Args[1] == "--serve":
		err = localvpn.ServeNativeGateway(ctx, newPhaseWriter(os.Stdout))
	case len(os.Args) == 2 && os.Args[1] == "--cap-status":
		err = reportCapabilityStatus()
	case len(os.Args) == 2 && os.Args[1] == "--wan":
		err = serveWAN(ctx)
	case !orchestrator:
		err = errors.New("unexpected lab arguments")
	case realLabMode():
		deadline, cancel := context.WithTimeout(ctx, 540*time.Second)
		defer cancel()
		err = runRealLab(deadline, realMax)
	default:
		deadline, cancel := context.WithTimeout(ctx, 100*time.Second)
		defer cancel()
		err = runLab(deadline)
	}
	if err != nil {
		fatal("runtime", err)
	}
	if orchestrator {
		mode := labMode()
		fmt.Println("native_gateway_vpn_lab=pass")
		if realLabMode() {
			fmt.Println("coverage=opt-in real subscription: actual eth0 native network plan, root namespace initialization, restricted service ServeNativeGateway, subscription-selected sing-box, localhost OpenVPN client, /1 full-tunnel routes, bounded public HTTPS tunnel bytes/path, and HTTP failure with client/routes retained after sing-box loss")
			fmt.Println("limitations=DNS tunneling, systemd, NetworkManager/KDE, autostart, a host kill switch after client exit, and host-ISP egress are not proven; the container eth0 underlay may traverse the host VPN")
		} else {
			fmt.Printf("coverage=real %s fixture: native network plan, root namespace initialization, ServeNativeGateway, sing-box/OpenVPN, localhost client profile, /1 full-tunnel routes, physical-source policy egress, and no fallback after sing-box loss\n", mode)
			fmt.Println("limitations=systemd, NetworkManager/KDE, autostart, real internet, DNS readiness, and a host kill switch after client exit are not proven")
		}
		if mode == serviceMode {
			fmt.Println("service_isolation=uid/gid 987, only CAP_NET_ADMIN and CAP_NET_RAW inheritable/permitted/effective/bounding/ambient, no-new-privileges, root-owned group-readable immutable configuration, and service-owned private state/runtime")
		}
	}
}

func realLabMode() bool { return os.Getenv(realLabOptIn) == "1" }

// The explicit real lab tests at most 50 production-filtered candidates. The
// optional flag only lowers that cap; subscriptions never travel via argv/env.
func parseRealLabArgs(args []string) (int, bool) {
	if len(args) == 0 {
		return realBenchmarkMaxDefault, true
	}
	if len(args) != 2 || args[0] != "--max-candidates" {
		return 0, false
	}
	value, err := strconv.Atoi(args[1])
	if err != nil || value < 1 || value > realBenchmarkMaxLimit {
		return 0, false
	}
	return value, true
}

func labMode() string {
	if os.Getenv(labModeEnv) == serviceMode {
		return serviceMode
	}
	return rootMode
}

func requireDisposableLab() error {
	if os.Getenv(labOptIn) != "1" {
		return fmt.Errorf("refusing mutation without %s=1", labOptIn)
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return errors.New("refusing mutation without Docker container marker")
	}
	mode := os.Getenv(labModeEnv)
	if mode != "" && mode != rootMode && mode != serviceMode {
		return errors.New("unsupported native VPN lab mode")
	}
	if realLabMode() && mode != serviceMode {
		return errors.New("real subscription lab requires restricted service mode")
	}
	serviceChild := labMode() == serviceMode && len(os.Args) == 2 && (os.Args[1] == "--serve" || os.Args[1] == "--cap-status")
	if serviceChild {
		if os.Geteuid() != serviceUID || os.Getegid() != serviceGID {
			return errors.New("service child does not have the dedicated uid/gid")
		}
	} else if os.Geteuid() != 0 {
		return errors.New("lab orchestrator requires root inside the disposable container")
	}
	return nil
}

func runLab(ctx context.Context) error {
	fmt.Println("lab_phase=prepare")
	if err := prepareFixture(); err != nil {
		return fmt.Errorf("prepare fixture: %w", err)
	}
	defer cleanupKernel()

	if err := createWAN(ctx); err != nil {
		return fmt.Errorf("create synthetic WAN: %w", err)
	}
	wan, err := startChild(ctx, "wan", "ip", "netns", "exec", wanNamespace, fixtureRoot+"/native-vpn-lab", "--wan")
	if err != nil {
		return err
	}
	defer wan.stop()
	if err := waitFor(ctx, 8*time.Second, func() bool {
		return commandOK(ctx, "ip", "netns", "exec", wanNamespace, "ss", "-H", "-ltn", "sport", "=", ":"+strconv.Itoa(wanPort))
	}); err != nil {
		return errors.New("synthetic WAN responder did not become ready")
	}

	if err := applyNativePlan(ctx); err != nil {
		return fmt.Errorf("apply native network plan: %w", err)
	}
	if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, fixtureRoot+"/native-vpn-lab", "--namespace-init"); err != nil {
		return fmt.Errorf("initialize native namespace: %w", err)
	}
	if err := verifyPhysicalPolicy(ctx); err != nil {
		return err
	}

	// runLab uses only synthetic direct outbounds and public fixtures. Never
	// forward checker output from the separate real-subscription lab paths.
	checkCtx, cancelCheck := context.WithTimeout(ctx, 10*time.Second)
	check := exec.CommandContext(checkCtx, nativegateway.LibexecDir+"/sing-box", "check", "--config", nativegateway.ConfigDir+"/sing-box/config.json")
	checkOutput, checkErr := check.CombinedOutput()
	cancelCheck()
	if checkErr != nil {
		if len(checkOutput) > 2048 {
			checkOutput = checkOutput[:2048]
		}
		return fmt.Errorf("synthetic sing-box configuration check failed (%v): %s", checkErr, strings.TrimSpace(string(checkOutput)))
	}
	fmt.Println("lab_phase=start-native-gateway")
	gatewayCommand := []string{"ip", "netns", "exec", nativegateway.Namespace}
	if labMode() == serviceMode {
		if err := verifyServiceCapabilities(ctx, gatewayCommand); err != nil {
			return err
		}
		gatewayCommand = append(gatewayCommand, serviceCommand("--serve")...)
	} else {
		gatewayCommand = append(gatewayCommand, fixtureRoot+"/native-vpn-lab", "--serve")
	}
	gateway, err := startChild(ctx, "gateway", gatewayCommand[0], gatewayCommand[1:]...)
	if err != nil {
		return err
	}
	defer gateway.stop()
	if err := gateway.waitPhase(ctx, "vpnkit_phase=runtime-ready", 40*time.Second); err != nil {
		return err
	}

	fmt.Println("lab_phase=start-localhost-openvpn-client")
	client, err := startChild(ctx, "client", "openvpn", "--config", privateRoot+"/secrets/openvpn/client/vpnkit-local.ovpn", "--dev", "tun-client0")
	if err != nil {
		return err
	}
	defer client.stop()
	if err := waitFor(ctx, 25*time.Second, func() bool {
		return commandOK(ctx, "ip", "link", "show", "tun-client0") && client.running()
	}); err != nil {
		return errors.New("localhost OpenVPN client did not establish its tunnel")
	}
	if err := verifyClientRoutes(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(privateRoot+"/tunnel-ready", []byte("ready\n"), 0600); err != nil {
		return fmt.Errorf("arm WAN response: %w", err)
	}

	fmt.Println("lab_phase=verify-full-tunnel")
	body, err := httpGet(ctx, 10*time.Second)
	if err != nil {
		return fmt.Errorf("HTTP through native gateway: %w", err)
	}
	if body != expectedBody {
		return errors.New("synthetic WAN returned an unexpected body")
	}

	// Real candidate retries stop the localhost client and gateway, remove the
	// synchronized runtime config, then start a fresh restricted service. Repeat
	// that exact lifecycle with public synthetic data so stale child, barrier, or
	// runtime state cannot make later candidates fail only in the private lab.
	if labMode() == serviceMode {
		for attempt := 2; attempt <= 3; attempt++ {
			fmt.Printf("lab_phase=synthetic-restart-cleanup attempt=%d\n", attempt)
			client.stop()
			gateway.stopWithGrace(12 * time.Second)
			if err := resetRealCandidateRuntime(nativegateway.StateDir + "/sing-box/config.json"); err != nil {
				return errors.New("synthetic-candidate-reset")
			}

			fmt.Printf("lab_phase=synthetic-restart-native-gateway attempt=%d\n", attempt)
			gateway, err = startChild(ctx, "gateway", gatewayCommand[0], gatewayCommand[1:]...)
			if err != nil {
				return err
			}
			defer gateway.stop()
			if err := gateway.waitPhase(ctx, "vpnkit_phase=runtime-ready", 40*time.Second); err != nil {
				classification := gateway.readinessFailureClassification()
				fmt.Printf("lab_observation=synthetic-gateway-readiness attempt=%d classification=%s\n", attempt, classification)
				return fmt.Errorf("synthetic gateway restart %d readiness (%s): %w", attempt, classification, err)
			}

			fmt.Printf("lab_phase=synthetic-restart-localhost-openvpn-client attempt=%d\n", attempt)
			client, err = startChild(ctx, "client", "openvpn", "--config", privateRoot+"/secrets/openvpn/client/vpnkit-local.ovpn", "--dev", "tun-client0")
			if err != nil {
				return err
			}
			defer client.stop()
			if err := waitFor(ctx, 25*time.Second, func() bool {
				return commandOK(ctx, "ip", "link", "show", "tun-client0") && client.running()
			}); err != nil {
				return fmt.Errorf("localhost OpenVPN client did not establish its tunnel on synthetic restart %d", attempt)
			}
			if err := verifyClientRoutes(ctx); err != nil {
				return fmt.Errorf("synthetic restart %d routes: %w", attempt, err)
			}
			body, err = httpGet(ctx, 10*time.Second)
			if err != nil {
				return fmt.Errorf("HTTP through native gateway on synthetic restart %d: %w", attempt, err)
			}
			if body != expectedBody {
				return errors.New("synthetic WAN returned an unexpected body after restart")
			}
		}
	}

	// Kill the real sing-box child, not the client or the supervisor. The
	// supervisor should fail and tear down its server while the persistent
	// localhost OpenVPN client remains alive with its pushed /1 routes.
	fmt.Println("lab_phase=verify-no-fallback")
	if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, "pkill", "-x", "sing-box"); err != nil {
		return errors.New("could not stop the native sing-box child")
	}
	if err := waitFor(ctx, 8*time.Second, func() bool { return !gateway.running() }); err != nil {
		return errors.New("native supervisor did not exit after sing-box loss")
	}
	if !client.running() {
		return errors.New("OpenVPN client exited before the no-fallback check")
	}
	if err := verifyClientRoutes(ctx); err != nil {
		return fmt.Errorf("OpenVPN client routes were not retained for no-fallback check: %w", err)
	}
	if body, err = httpGet(ctx, 2*time.Second); err == nil {
		return fmt.Errorf("traffic reached synthetic WAN after gateway loss: body=%q", body)
	}
	if !client.running() {
		return errors.New("OpenVPN client exited during the no-fallback check")
	}
	fmt.Println("lab_observation=after sing-box loss the OpenVPN client remained running with both /1 routes, and the tunneled HTTP request failed; this does not prove a host kill switch after client exit or route restoration")
	return nil
}

type realCandidate struct {
	ordinal  int
	link     string
	outbound map[string]any
}

type benchmarkTarget struct {
	URL      string
	Hostname string
	IP       string
}

type realBenchmark struct {
	configPath string
	stateDir   string
	target     benchmarkTarget
	backend    string
	max        int
}

var runBenchmarkProcess = func(ctx context.Context, path string, args []string) error {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func runRealLab(ctx context.Context, maxCandidates int) error {
	fmt.Println("lab_phase=real-prepare")
	if err := prepareFixtureMode(true); err != nil {
		return err
	}
	defer cleanupKernel()

	benchmark, err := prepareRealBenchmark(ctx, maxCandidates)
	if err != nil {
		return errors.New("real-stage-benchmark-config")
	}
	fmt.Printf("lab_phase=real-production-benchmark backend=%s max_candidates=%d\n", benchmark.backend, benchmark.max)
	winner, tested, successful, benchmarkErr := runRealBenchmark(ctx, benchmark)
	fmt.Printf("lab_observation=real-production-benchmark tested=%d successful=%d backend=%s\n", tested, successful, benchmark.backend)
	if benchmarkErr != nil {
		fmt.Printf("lab_observation=real-baseline-unavailable reason=missing-working-baseline tested=%d\n", tested)
		return errors.New("real-stage-missing-working-baseline")
	}
	candidate, err := candidateFromBenchmarkWinner(winner)
	if err != nil {
		return errors.New("real-stage-winner-prepare")
	}
	prepared, classification := prepareRealCandidate(ctx, candidate)
	if classification != "success" || prepared.link != winner.Link {
		fmt.Println("lab_observation=real-candidate classification=winner-prepare")
		return errors.New("real-stage-winner-prepare")
	}
	classification = runCandidateUnderlayControlTarget(ctx, prepared, benchmark.target)
	fmt.Println(candidateControlObservation(candidate.ordinal, classification))
	if classification != "success" {
		fmt.Printf("lab_observation=real-probe-target-mismatch classification=%s benchmark_successes=%d\n", classification, successful)
		return errors.New("real-stage-probe-target-mismatch")
	}

	actual, snapshot, err := realEth0Plan(ctx)
	if err != nil {
		return errors.New("real-stage-uplink")
	}
	uplink = actual
	if err := applyPlan(ctx, actual, snapshot); err != nil {
		return errors.New("real-stage-network-plan")
	}
	if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, fixtureRoot+"/native-vpn-lab", "--namespace-init"); err != nil {
		return errors.New("real-stage-namespace-init")
	}
	if err := verifyPhysicalPolicyTo(ctx, benchmark.target.IP, actual); err != nil {
		return errors.New("real-stage-egress-policy")
	}

	if err := installRealCandidate(ctx, prepared); err != nil {
		return errors.New("real-stage-winner-install")
	}
	if err := resetRealCandidateRuntime(nativegateway.StateDir + "/sing-box/config.json"); err != nil {
		return errors.New("real-stage-candidate-reset")
	}
	if !realCandidateConfigOK(ctx) {
		return errors.New("real-stage-candidate-config")
	}
	gatewayCommand := []string{"ip", "netns", "exec", nativegateway.Namespace}
	if err := verifyServiceCapabilities(ctx, gatewayCommand); err != nil {
		return errors.New("real-stage-service-isolation")
	}
	gatewayCommand = append(gatewayCommand, serviceCommand("--serve")...)
	fmt.Println("lab_phase=real-start-native-gateway")
	gateway, err := startChild(ctx, "gateway", gatewayCommand[0], gatewayCommand[1:]...)
	if err != nil {
		return errors.New("real-stage-gateway-start")
	}
	defer gateway.stopWithGrace(12 * time.Second)
	if err := gateway.waitPhase(ctx, "vpnkit_phase=runtime-ready", 45*time.Second); err != nil {
		fmt.Printf("lab_observation=real-gateway-readiness classification=%s\n", gateway.readinessFailureClassification())
		return errors.New("real-stage-gateway-readiness")
	}

	fmt.Println("lab_phase=real-start-localhost-openvpn-client")
	client, err := startChild(ctx, "client", "openvpn", "--config", privateRoot+"/secrets/openvpn/client/vpnkit-local.ovpn", "--dev", "tun-client0")
	if err != nil {
		return errors.New("real-stage-openvpn-client")
	}
	defer client.stop()
	if err := waitFor(ctx, 25*time.Second, func() bool {
		return commandOK(ctx, "ip", "link", "show", "tun-client0") && client.running()
	}); err != nil {
		return errors.New("real-stage-openvpn-client")
	}
	if err := verifyClientRoutesTo(ctx, benchmark.target.IP); err != nil {
		return errors.New("real-stage-client-routes")
	}

	fmt.Println("lab_phase=real-verify-public-tunnel")
	beforeRX, beforeTX, countErr := tunnelCounters()
	probe, requestErr := realHTTPSGetTarget(ctx, benchmark.target, 15*time.Second)
	afterRX, afterTX, afterErr := tunnelCounters()
	countersOK := countErr == nil && afterErr == nil
	fmt.Printf("lab_observation=real-https classification=%s status_category=%s body_bytes=%d tun_counters=%s tun_rx_delta=%d tun_tx_delta=%d\n",
		probe.classification, probe.statusCategory, probe.bodyBytes, availability(countersOK), counterDelta(beforeRX, afterRX, countersOK), counterDelta(beforeTX, afterTX, countersOK))
	if requestErr != nil || !countersOK || probe.bodyBytes == 0 || afterRX <= beforeRX || afterTX <= beforeTX {
		return errors.New("real-stage-native-probe")
	}

	fmt.Println("lab_phase=real-verify-no-fallback")
	if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, "pkill", "-x", "sing-box"); err != nil {
		return errors.New("real-stage-stop-sing-box")
	}
	if err := waitFor(ctx, 8*time.Second, func() bool { return !gateway.running() }); err != nil {
		return errors.New("real-stage-supervisor-loss")
	}
	if !client.running() || verifyClientRoutesTo(ctx, benchmark.target.IP) != nil {
		return errors.New("real-stage-retained-client-routes")
	}
	if _, err := realHTTPSGetTarget(ctx, benchmark.target, 4*time.Second); err == nil {
		return errors.New("real-stage-no-fallback")
	}
	if !client.running() || verifyClientRoutesTo(ctx, benchmark.target.IP) != nil {
		return errors.New("real-stage-retained-client-routes")
	}
	fmt.Println("lab_observation=bounded benchmark-target HTTPS transferred bytes over tun-client0; after sing-box loss HTTPS failed while the OpenVPN client and both /1 routes remained")
	return nil
}

func prepareRealBenchmark(ctx context.Context, maxCandidates int) (realBenchmark, error) {
	if maxCandidates < 1 || maxCandidates > realBenchmarkMaxLimit {
		return realBenchmark{}, errors.New("invalid benchmark candidate cap")
	}
	nativeConfig, err := config.Load(nativegateway.ConfigDir + "/vibe-vpn/config.yaml")
	if err != nil || nativeConfig.SubscriptionFile != nativegateway.ConfigDir+"/vibe-vpn/sub_url" {
		return realBenchmark{}, errors.New("invalid native benchmark config")
	}
	target, err := resolveBenchmarkTarget(ctx, nativeConfig.TestURL)
	if err != nil {
		return realBenchmark{}, err
	}
	root := privateRoot + "/benchmark"
	stateDir := root + "/state"
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return realBenchmark{}, err
	}
	subscriptionPath := root + "/input/sub_url"
	if err := copyFile(nativeConfig.SubscriptionFile, subscriptionPath, 0600); err != nil {
		return realBenchmark{}, err
	}
	benchmarkConfig := nativeConfig
	benchmarkConfig.SubscriptionFile = subscriptionPath
	benchmarkConfig.StateDir = stateDir
	benchmarkConfig.SingBoxBin = nativegateway.LibexecDir + "/sing-box"
	benchmarkConfig.SingBoxConfig = root + "/unused/runtime-config.json"
	benchmarkConfig.SingBoxRestartFile = root + "/unused/restart"
	benchmarkConfig.SingBoxRestartAckGenerationFile = root + "/unused/generation"
	benchmarkConfig.SingBoxRestartAckFile = root + "/unused/generation.ack"
	benchmarkConfig.ProductionSocks = "127.0.0.1:21803"
	benchmarkConfig.TestSocks = "127.0.0.1:21802"
	benchmarkConfig.TestLimitKiB = realBenchmarkReadLimit >> 10
	benchmarkConfig.TestDurationSeconds = 0
	body, err := json.MarshalIndent(benchmarkConfig, "", "  ")
	if err != nil {
		return realBenchmark{}, err
	}
	configPath := root + "/config.json"
	if err := os.WriteFile(configPath, append(body, '\n'), 0600); err != nil {
		return realBenchmark{}, err
	}
	return realBenchmark{configPath: configPath, stateDir: stateDir, target: target, backend: benchmarkBackendClass(benchmarkConfig.Runtime), max: maxCandidates}, nil
}

func benchmarkBackendClass(runtime string) string {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "singbox", "sing-box", "":
		return "singbox"
	case "xray":
		return "xray"
	default:
		return "unsupported"
	}
}

func benchmarkCommandArgs(configPath string, maxCandidates int) []string {
	return []string{"test", "--config", configPath, "--max", strconv.Itoa(maxCandidates), "--limit-kib", strconv.Itoa(realBenchmarkReadLimit >> 10), "--duration-sec", "0"}
}

func candidateFromBenchmarkWinner(winner picker.NodeResult) (realCandidate, error) {
	if !winner.OK || winner.Excluded || winner.Index < 1 || winner.Link == "" {
		return realCandidate{}, errors.New("invalid benchmark winner")
	}
	if _, err := vless.Parse(winner.Link); err != nil {
		return realCandidate{}, errors.New("invalid benchmark winner")
	}
	return realCandidate{ordinal: winner.Index, link: winner.Link}, nil
}

func runRealBenchmark(ctx context.Context, benchmark realBenchmark) (picker.NodeResult, int, int, error) {
	processErr := runBenchmarkProcess(ctx, fixtureRoot+"/vibe-vpn", benchmarkCommandArgs(benchmark.configPath, benchmark.max))
	results, loadErr := loadBenchmarkResults(ctx, benchmark.stateDir)
	if loadErr != nil {
		return picker.NodeResult{}, 0, 0, errors.New("benchmark results unavailable")
	}
	tested, successful := 0, 0
	for _, result := range results {
		if result.Link != "" || result.Error != "" || result.OK {
			tested++
		}
		if result.OK && !result.Excluded {
			successful++
		}
	}
	winner := picker.BestFiltered(results)
	if processErr != nil || winner == nil || winner.Link == "" || winner.Index < 1 {
		return picker.NodeResult{}, tested, successful, errors.New("missing working baseline")
	}
	if _, err := candidateFromBenchmarkWinner(*winner); err != nil {
		return picker.NodeResult{}, tested, successful, err
	}
	return *winner, tested, successful, nil
}

func loadBenchmarkResults(ctx context.Context, stateDir string) ([]picker.NodeResult, error) {
	lock, err := state.AcquireLock(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	body, _, err := state.LoadPrivateFileLocked(stateDir, "last-results.json", 8<<20)
	if err != nil {
		return nil, err
	}
	var results []picker.NodeResult
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&results); err != nil || len(results) == 0 {
		return nil, errors.New("invalid private benchmark results")
	}
	return results, nil
}

func resolveBenchmarkTarget(ctx context.Context, raw string) (benchmarkTarget, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") || net.ParseIP(parsed.Hostname()) != nil {
		return benchmarkTarget{}, errors.New("benchmark target must be a public HTTPS hostname")
	}
	lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookup, parsed.Hostname())
	if err != nil {
		return benchmarkTarget{}, err
	}
	for _, address := range addresses {
		if ip := address.IP.To4(); isPublicIPv4(ip) {
			return benchmarkTarget{URL: parsed.String(), Hostname: parsed.Hostname(), IP: ip.String()}, nil
		}
	}
	return benchmarkTarget{}, errors.New("benchmark target has no public IPv4 address")
}

func isPublicIPv4(ip net.IP) bool {
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func runLegacyRealLab(ctx context.Context) error {
	fmt.Println("lab_phase=real-prepare")
	if err := prepareFixtureMode(true); err != nil {
		return err
	}
	defer cleanupKernel()

	candidates, err := loadRealCandidates()
	if err != nil {
		return err
	}
	probeIP, err := resolveRealProbe(ctx)
	if err != nil {
		return errors.New("real-stage-public-resolution")
	}

	// Establish the A side before creating the native namespace or changing any
	// routes. Each successful candidate carries the exact prepared outbound into
	// the B side; failed controls are never interpreted as native failures.
	controlled := make([]realCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		prepared, classification := prepareRealCandidate(ctx, candidate)
		if classification == "success" {
			classification = runCandidateUnderlayControl(ctx, prepared, probeIP)
		}
		fmt.Println(candidateControlObservation(candidate.ordinal, classification))
		if classification == "success" {
			controlled = append(controlled, prepared)
		}
	}
	if len(controlled) == 0 {
		fmt.Println("lab_observation=real-underlay-control no_usable_control=true")
		return errors.New("real-stage-no-usable-underlay-control")
	}

	actual, snapshot, err := realEth0Plan(ctx)
	if err != nil {
		return errors.New("real-stage-uplink")
	}
	uplink = actual
	if err := applyPlan(ctx, actual, snapshot); err != nil {
		return errors.New("real-stage-network-plan")
	}
	if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, fixtureRoot+"/native-vpn-lab", "--namespace-init"); err != nil {
		return errors.New("real-stage-namespace-init")
	}
	if err := verifyPhysicalPolicyTo(ctx, realProbeHostRouteTarget, actual); err != nil {
		return errors.New("real-stage-egress-policy")
	}

	gatewayCommand := []string{"ip", "netns", "exec", nativegateway.Namespace}
	if err := verifyServiceCapabilities(ctx, gatewayCommand); err != nil {
		return errors.New("real-stage-service-isolation")
	}
	gatewayCommand = append(gatewayCommand, serviceCommand("--serve")...)

	for range controlled {
		candidate := controlled[0]
		controlled = controlled[1:]
		if err := installRealCandidate(ctx, candidate); err != nil {
			fmt.Println("lab_observation=real-candidate-preparation-failed")
			continue
		}
		// The gateway sync intentionally preserves the selected outbound from an
		// existing runtime config. A retry must therefore remove the prior runtime
		// copy or every later candidate silently reuses the first candidate.
		if err := resetRealCandidateRuntime(nativegateway.StateDir + "/sing-box/config.json"); err != nil {
			return errors.New("real-stage-candidate-reset")
		}
		if !realCandidateConfigOK(ctx) {
			fmt.Println("lab_observation=real-candidate classification=config-check")
			continue
		}
		fmt.Println("lab_phase=real-start-native-gateway")
		gateway, startErr := startChild(ctx, "gateway", gatewayCommand[0], gatewayCommand[1:]...)
		if startErr != nil {
			continue
		}
		if err := gateway.waitPhase(ctx, "vpnkit_phase=runtime-ready", 45*time.Second); err != nil {
			classification := gateway.readinessFailureClassification()
			fmt.Printf("lab_observation=real-gateway-readiness classification=%s\n", classification)
			gateway.stopWithGrace(12 * time.Second)
			continue
		}

		fmt.Println("lab_phase=real-start-localhost-openvpn-client")
		client, clientErr := startChild(ctx, "client", "openvpn", "--config", privateRoot+"/secrets/openvpn/client/vpnkit-local.ovpn", "--dev", "tun-client0")
		if clientErr != nil {
			gateway.stop()
			return errors.New("real-stage-openvpn-client")
		}
		if err := waitFor(ctx, 25*time.Second, func() bool {
			return commandOK(ctx, "ip", "link", "show", "tun-client0") && client.running()
		}); err != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-openvpn-client")
		}
		if err := verifyClientRoutesTo(ctx, probeIP); err != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-client-routes")
		}

		fmt.Println("lab_phase=real-verify-public-tunnel")
		beforeRX, beforeTX, countErr := tunnelCounters()
		probe, requestErr := realHTTPSGet(ctx, probeIP, 15*time.Second)
		afterRX, afterTX, afterErr := tunnelCounters()
		countersOK := countErr == nil && afterErr == nil
		fmt.Printf("lab_observation=real-https classification=%s status_category=%s body_bytes=%d tun_counters=%s tun_rx_delta=%d tun_tx_delta=%d\n",
			probe.classification, probe.statusCategory, probe.bodyBytes, availability(countersOK), counterDelta(beforeRX, afterRX, countersOK), counterDelta(beforeTX, afterTX, countersOK))
		if requestErr != nil || !countersOK || probe.bodyBytes == 0 || afterRX <= beforeRX || afterTX <= beforeTX {
			client.stop()
			// Native gateway shutdown can spend up to ten seconds installing and
			// removing its fail-closed barrier. Do not kill it at the generic
			// two-second child deadline and leave retry state half-cleaned.
			gateway.stopWithGrace(12 * time.Second)
			continue
		}

		fmt.Println("lab_phase=real-verify-no-fallback")
		if _, err := runCommand(ctx, commandTimeout, nil, "ip", "netns", "exec", nativegateway.Namespace, "pkill", "-x", "sing-box"); err != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-stop-sing-box")
		}
		if err := waitFor(ctx, 8*time.Second, func() bool { return !gateway.running() }); err != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-supervisor-loss")
		}
		if !client.running() || verifyClientRoutesTo(ctx, probeIP) != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-retained-client-routes")
		}
		if _, err := realHTTPSGet(ctx, probeIP, 4*time.Second); err == nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-no-fallback")
		}
		if !client.running() || verifyClientRoutesTo(ctx, probeIP) != nil {
			client.stop()
			gateway.stop()
			return errors.New("real-stage-retained-client-routes")
		}
		fmt.Println("lab_observation=bounded public HTTPS transferred bytes over tun-client0; after sing-box loss HTTPS failed while the OpenVPN client and both /1 routes remained")
		client.stop()
		gateway.stop()
		return nil
	}
	return errors.New("real-stage-candidates-exhausted")
}

const realProbeHostRouteTarget = "1.1.1.1"

func loadRealCandidates() ([]realCandidate, error) {
	info, err := os.Lstat(fixtureRoot + "/sub_url")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16384 {
		return nil, errors.New("real-stage-subscription-input")
	}
	settings, err := config.Load(nativegateway.ConfigDir + "/vibe-vpn/config.yaml")
	if err != nil || settings.SubscriptionFile != nativegateway.ConfigDir+"/vibe-vpn/sub_url" || settings.TimeoutSeconds < 1 || settings.TimeoutSeconds > 20 {
		return nil, errors.New("real-stage-native-config")
	}
	data, err := os.ReadFile(settings.SubscriptionFile)
	if err != nil {
		return nil, errors.New("real-stage-subscription-input")
	}
	urls := subscription.URLList(string(data))
	if len(urls) == 0 || len(urls) > 3 {
		return nil, errors.New("real-stage-subscription-input")
	}
	links := make([]string, 0)
	for _, raw := range urls {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return nil, errors.New("real-stage-subscription-input")
		}
		fetched, fetchErr := subscription.Fetch(raw, time.Duration(settings.TimeoutSeconds)*time.Second)
		if fetchErr == nil {
			links = append(links, fetched...)
		}
	}
	filter := picker.FilterOptions{DefaultExclude: true}
	seen := make(map[string]bool)
	candidates := make([]realCandidate, 0, 3)
	for index, link := range links {
		node, parseErr := vless.Parse(link)
		if parseErr != nil || seen[link] {
			continue
		}
		result := picker.NodeResult{Index: index + 1, Name: node.Name, Host: node.Host, Port: node.Port, Network: node.Network, Security: node.Security, Link: link}
		if ok, _ := filter.MatchResult(result); !ok {
			continue
		}
		if _, supportedErr := vless.SingBoxOutbound(link); supportedErr != nil {
			continue
		}
		seen[link] = true
		candidates = append(candidates, realCandidate{ordinal: len(candidates) + 1, link: link})
		if len(candidates) == 3 {
			break
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("real-stage-no-supported-candidates")
	}
	return candidates, nil
}

func installRealCandidate(_ context.Context, candidate realCandidate) error {
	if candidate.outbound == nil {
		return errors.New("candidate was not underlay-controlled")
	}
	outbound := make(map[string]any, len(candidate.outbound)+1)
	for key, value := range candidate.outbound {
		outbound[key] = value
	}
	outbound["tag"] = "selected-native-out"
	path := nativegateway.ConfigDir + "/sing-box/config.json"
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return errors.New("candidate config unavailable")
	}
	var config map[string]any
	if json.Unmarshal(data, &config) != nil {
		return errors.New("candidate config invalid")
	}
	values, ok := config["outbounds"].([]any)
	if !ok {
		return errors.New("candidate config invalid")
	}
	found := 0
	for index, value := range values {
		entry, entryOK := value.(map[string]any)
		if entryOK && entry["tag"] == "selected-native-out" {
			values[index] = outbound
			found++
		}
	}
	if found != 1 {
		return errors.New("candidate config invalid")
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return errors.New("candidate config invalid")
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0640); err != nil || os.Chown(path, 0, serviceGID) != nil || os.Chmod(path, 0640) != nil {
		return errors.New("candidate config install failed")
	}
	return nil
}

func resetRealCandidateRuntime(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func realCandidateConfigOK(ctx context.Context) bool {
	command := []string{"ip", "netns", "exec", nativegateway.Namespace,
		"setpriv", "--reuid", strconv.Itoa(serviceUID), "--regid", strconv.Itoa(serviceGID), "--clear-groups",
		"--inh-caps", "+net_admin,+net_raw", "--ambient-caps", "+net_admin,+net_raw",
		"--bounding-set", "-all,+net_admin,+net_raw", "--no-new-privs",
		nativegateway.LibexecDir + "/sing-box", "check", "-c", nativegateway.ConfigDir + "/sing-box/config.json"}
	_, err := runCommand(ctx, commandTimeout, nil, command[0], command[1:]...)
	return err == nil
}

func realEth0Plan(ctx context.Context) (nativegateway.Uplink, nativegateway.Snapshot, error) {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return nativegateway.Uplink{}, nativegateway.Snapshot{}, err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nativegateway.Uplink{}, nativegateway.Snapshot{}, err
	}
	addressCIDR := ""
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil {
			if addressCIDR != "" {
				return nativegateway.Uplink{}, nativegateway.Snapshot{}, errors.New("ambiguous eth0 IPv4")
			}
			addressCIDR = address.String()
		}
	}
	if addressCIDR == "" {
		return nativegateway.Uplink{}, nativegateway.Snapshot{}, errors.New("missing eth0 IPv4")
	}
	output, err := runCommand(ctx, commandTimeout, nil, "ip", "-json", "-4", "route", "show", "default")
	if err != nil {
		return nativegateway.Uplink{}, nativegateway.Snapshot{}, err
	}
	gateway, err := parseEth0Default(output)
	if err != nil {
		return nativegateway.Uplink{}, nativegateway.Snapshot{}, err
	}
	u := nativegateway.Uplink{Interface: "eth0", AddressCIDR: addressCIDR, Gateway: gateway}
	s := nativegateway.Snapshot{
		Interfaces:     []nativegateway.InterfaceState{{Name: "eth0", Addresses: []string{addressCIDR}}},
		Routes:         []nativegateway.RouteState{{Table: 254, Destination: "default", Gateway: u.Gateway, Interface: "eth0"}},
		IPv4Forwarding: readForwarding(),
	}
	return u, s, nil
}

// Query defaults without a dev filter: iproute2 omits dev from filtered JSON.
func parseEth0Default(output []byte) (string, error) {
	var routes []struct {
		Destination string `json:"dst"`
		Gateway     string `json:"gateway"`
		Device      string `json:"dev"`
	}
	if json.Unmarshal(output, &routes) != nil || len(routes) != 1 || routes[0].Destination != "default" || net.ParseIP(routes[0].Gateway).To4() == nil || routes[0].Device != "eth0" {
		return "", errors.New("ambiguous eth0 default")
	}
	return routes[0].Gateway, nil
}

func applyPlan(ctx context.Context, selected nativegateway.Uplink, snapshot nativegateway.Snapshot) error {
	plan, err := nativegateway.PlanNetwork(selected, snapshot)
	if err != nil {
		return err
	}
	for _, command := range plan {
		if _, err := runCommand(ctx, commandTimeout, strings.NewReader(command.Stdin), command.Program, command.Args...); err != nil {
			return err
		}
	}
	return nil
}

func resolveRealProbe(ctx context.Context) (string, error) {
	lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookup, realProbeHost)
	if err != nil {
		return "", err
	}
	for _, address := range addresses {
		if ip := address.IP.To4(); ip != nil {
			return ip.String(), nil
		}
	}
	return "", errors.New("public probe has no IPv4 address")
}

type realHTTPSResult struct {
	classification string
	statusCategory string
	bodyBytes      int64
}

func realHTTPSGet(parent context.Context, targetIP string, timeout time.Duration) (realHTTPSResult, error) {
	return realHTTPSGetTarget(parent, benchmarkTarget{URL: "https://" + realProbeHost + realProbePath, Hostname: realProbeHost, IP: targetIP}, timeout)
}

func realHTTPSGetTarget(parent context.Context, target benchmarkTarget, timeout time.Duration) (realHTTPSResult, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return realHTTPSGetTargetWithDial(parent, target, timeout, func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(target.IP, "443"))
	})
}

func realHTTPSGetWithDial(parent context.Context, timeout time.Duration, dialContext func(context.Context, string, string) (net.Conn, error)) (realHTTPSResult, error) {
	return realHTTPSGetTargetWithDial(parent, benchmarkTarget{URL: "https://" + realProbeHost + realProbePath, Hostname: realProbeHost}, timeout, dialContext)
}

func realHTTPSGetTargetWithDial(parent context.Context, target benchmarkTarget, timeout time.Duration, dialContext func(context.Context, string, string) (net.Conn, error)) (realHTTPSResult, error) {
	result := realHTTPSResult{classification: "request-build", statusCategory: "none"}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target.Hostname},
		DialContext:     dialContext,
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return result, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		result.classification = classifyHTTPSFailure(err)
		return result, err
	}
	defer response.Body.Close()
	result.statusCategory = strconv.Itoa(response.StatusCode/100) + "xx"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.classification = "http-status"
		return result, errors.New("public HTTPS status")
	}
	result.bodyBytes, err = io.Copy(io.Discard, io.LimitReader(response.Body, realBenchmarkReadLimit))
	if err != nil {
		result.classification = "body-read"
		return result, err
	}
	result.classification = "success"
	return result, nil
}

func classifyHTTPSFailure(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verification) {
		return "tls-validation"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "dial-refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection-reset"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return "dial-unreachable"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "tls-eof"
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return "dial-timeout"
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return "tls-protocol"
	}
	return "transport"
}

func availability(ok bool) string {
	if ok {
		return "available"
	}
	return "unavailable"
}

func counterDelta(before, after uint64, ok bool) uint64 {
	if !ok || after < before {
		return 0
	}
	return after - before
}

func tunnelCounters() (uint64, uint64, error) {
	read := func(name string) (uint64, error) {
		data, err := os.ReadFile("/sys/class/net/tun-client0/statistics/" + name)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	}
	rx, err := read("rx_bytes")
	if err != nil {
		return 0, 0, err
	}
	tx, err := read("tx_bytes")
	return rx, tx, err
}

func prepareFixture() error { return prepareFixtureMode(false) }

func prepareFixtureMode(real bool) error {
	if err := os.MkdirAll(privateRoot, 0700); err != nil {
		return err
	}
	if err := os.Chmod(privateRoot, 0700); err != nil {
		return err
	}
	// Minimal Arch images do not ship /usr/libexec. Create its public parent
	// separately so MkdirAll below cannot accidentally make it root-only.
	if err := os.MkdirAll(filepath.Dir(nativegateway.LibexecDir), 0755); err != nil {
		return err
	}
	for _, dir := range []string{
		nativegateway.LibexecDir,
		nativegateway.ConfigDir + "/sing-box",
		nativegateway.ConfigDir + "/openvpn/pki",
		nativegateway.ConfigDir + "/vibe-vpn",
		nativegateway.StateDir + "/sing-box",
		nativegateway.StateDir + "/vibe-vpn",
		"/run/local-vpn-kde",
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
	}
	singBoxBinary, err := exec.LookPath("sing-box")
	if err != nil {
		return errors.New("fixture sing-box binary is unavailable")
	}
	if err := copyFile(singBoxBinary, nativegateway.LibexecDir+"/sing-box", 0555); err != nil {
		return fmt.Errorf("install fixture sing-box: %w", err)
	}
	if err := copyFile(fixtureRoot+"/vibe-vpn", nativegateway.LibexecDir+"/vibe-vpn", 0555); err != nil {
		return fmt.Errorf("install fixture helper: %w", err)
	}
	// Production runs this root-only ExecStartPre before starting the restricted
	// gateway service. Model it in the lab too: runtime restarts execute the
	// pinned binaries rather than mutable package paths.
	if err := localvpn.StageNativeRuntime(context.Background()); err != nil {
		return fmt.Errorf("stage fixture runtime: %w", err)
	}

	base := privateRoot + "/secrets"
	if err := localvpn.Assets(localvpn.AssetOptions{
		Base: base, Runtime: "native", TemplateDir: fixtureRoot + "/config/openvpn", Endpoint: "127.0.0.1",
		Port: nativegateway.LoopbackPort, DNS: "8.8.8.8", CertificateDays: 1,
	}); err != nil {
		return err
	}
	render := localvpn.RenderOptions{
		Template: fixtureRoot + "/config/sing-box/config.tun.json.template", Base: base,
		Policy: "strict", RuleSets: "local-fixture", Outbound: "direct-fixture",
		Runtime: "native", Fixture: true, AllowMissingSubscription: true,
	}
	if real {
		if err := copyFile(fixtureRoot+"/sub_url", base+"/vibe-vpn/sub_url", 0600); err != nil {
			return errors.New("real-stage-subscription-input")
		}
		render.RuleSets = "remote"
		render.Outbound = "subscription"
		render.AllowMissingSubscription = false
	}
	if err := localvpn.Render(render); err != nil {
		if real {
			return errors.New("real-stage-native-config")
		}
		return err
	}
	if err := copyTree(base+"/rendered/sing-box", nativegateway.ConfigDir+"/sing-box"); err != nil {
		return err
	}
	if err := copyTree(base+"/rendered/vibe-vpn", nativegateway.ConfigDir+"/vibe-vpn"); err != nil {
		return err
	}
	server, err := os.ReadFile(base + "/rendered/openvpn/server.conf")
	if err != nil {
		return err
	}
	server = bytes.ReplaceAll(server, []byte("/etc/openvpn/pki/"), []byte(nativegateway.ConfigDir+"/openvpn/pki/"))
	if bytes.Contains(server, []byte("/etc/openvpn/pki/")) {
		return errors.New("fixture OpenVPN path rewrite incomplete")
	}
	if err := os.WriteFile(nativegateway.ConfigDir+"/openvpn/server.conf", server, 0600); err != nil {
		return err
	}
	for _, name := range []string{"ca.crt", "server.crt", "server.key", "ta.key"} {
		if err := copyFile(base+"/rendered/openvpn/pki/"+name, nativegateway.ConfigDir+"/openvpn/pki/"+name, 0600); err != nil {
			return err
		}
	}
	if labMode() == serviceMode {
		if err := setTreeOwnership(nativegateway.ConfigDir, 0, serviceGID, 0750, 0640); err != nil {
			return fmt.Errorf("secure immutable service configuration: %w", err)
		}
		if err := setTreeOwnership(nativegateway.LibexecDir, 0, serviceGID, 0750, 0550); err != nil {
			return fmt.Errorf("secure service executables: %w", err)
		}
		for _, dir := range []string{nativegateway.StateDir, "/run/local-vpn-kde"} {
			if err := setTreeOwnership(dir, serviceUID, serviceGID, 0700, 0600); err != nil {
				return fmt.Errorf("secure service writable directory: %w", err)
			}
		}
	}
	return nil
}

func createWAN(ctx context.Context) error {
	commands := [][]string{
		{"ip", "netns", "add", wanNamespace},
		{"ip", "link", "add", uplinkName, "type", "veth", "peer", "name", wanPeerName},
		{"ip", "link", "set", wanPeerName, "netns", wanNamespace},
		{"ip", "addr", "add", uplinkAddress, "dev", uplinkName},
		{"ip", "link", "set", uplinkName, "up"},
		{"ip", "-n", wanNamespace, "addr", "add", uplinkGateway + "/24", "dev", wanPeerName},
		{"ip", "-n", wanNamespace, "addr", "add", wanTarget + "/32", "dev", "lo"},
		{"ip", "-n", wanNamespace, "link", "set", "lo", "up"},
		{"ip", "-n", wanNamespace, "link", "set", wanPeerName, "up"},
		{"ip", "route", "add", "default", "via", uplinkGateway, "dev", uplinkName},
	}
	for _, command := range commands {
		if _, err := runCommand(ctx, commandTimeout, nil, command[0], command[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(command[:min(4, len(command))], " "), err)
		}
	}
	return nil
}

func applyNativePlan(ctx context.Context) error {
	return applyPlan(ctx, uplink, nativegateway.Snapshot{
		Interfaces:     []nativegateway.InterfaceState{{Name: uplinkName, Addresses: []string{uplinkAddress}}},
		Namespaces:     []string{wanNamespace},
		Routes:         []nativegateway.RouteState{{Table: 254, Destination: "default", Gateway: uplinkGateway, Interface: uplinkName}},
		IPv4Forwarding: readForwarding(),
	})
}

func readForwarding() bool {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	return err == nil && strings.TrimSpace(string(data)) == "1"
}

func verifyPhysicalPolicy(ctx context.Context) error {
	return verifyPhysicalPolicyTo(ctx, wanTarget, uplink)
}

func verifyPhysicalPolicyTo(ctx context.Context, target string, selected nativegateway.Uplink) error {
	output, err := runCommand(ctx, commandTimeout, nil, "ip", "-4", "route", "get", target,
		"from", nativegateway.GuestAddress, "iif", nativegateway.HostInterface)
	if err != nil {
		return errors.New("native source-policy lookup failed")
	}
	text := string(output)
	for _, want := range []string{"via " + selected.Gateway, "dev " + selected.Interface, "table " + strconv.Itoa(nativegateway.RoutingTable)} {
		if !strings.Contains(text, want) {
			return errors.New("native source-policy lookup incomplete")
		}
	}
	return nil
}

func verifyClientRoutes(ctx context.Context) error { return verifyClientRoutesTo(ctx, wanTarget) }

func verifyClientRoutesTo(ctx context.Context, target string) error {
	if output, err := runCommand(ctx, commandTimeout, nil, "ip", "-4", "route", "show", target+"/32"); err != nil || strings.TrimSpace(string(output)) != "" {
		return errors.New("an explicit root route to the target exists")
	}
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		output, err := runCommand(ctx, commandTimeout, nil, "ip", "-4", "route", "show", prefix)
		if err != nil || !strings.Contains(string(output), "dev tun-client0") {
			return fmt.Errorf("pushed route %s does not use the client tunnel", prefix)
		}
	}
	output, err := runCommand(ctx, commandTimeout, nil, "ip", "-4", "route", "get", target)
	if err != nil || !strings.Contains(string(output), "dev tun-client0") {
		return errors.New("target lookup does not use the OpenVPN client tunnel")
	}
	return nil
}

func serveWAN(ctx context.Context) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort(wanTarget, strconv.Itoa(wanPort)))
	if err != nil {
		return errors.New("WAN listener failed")
	}
	defer listener.Close()
	server := &http.Server{ReadHeaderTimeout: 2 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
		if splitErr != nil || host != "192.0.2.10" {
			http.Error(w, "unexpected source", http.StatusForbidden)
			return
		}
		if _, statErr := os.Stat(privateRoot + "/tunnel-ready"); statErr != nil {
			http.Error(w, "tunnel not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, expectedBody)
	})}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("WAN responder stopped unexpectedly")
	}
	return nil
}

func httpGet(parent context.Context, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/", wanTarget, wanPort), nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	return string(body), nil
}

type childProcess struct {
	name       string
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	done       chan error
	phases     chan string
	once       sync.Once
	diagnostic string
	reason     string
	mu         sync.Mutex
}

func startChild(parent context.Context, name, program string, args ...string) (*childProcess, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prepare %s child", name)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prepare %s child", name)
	}
	child := &childProcess{name: name, cmd: cmd, cancel: cancel, done: make(chan error, 1), phases: make(chan string, 16)}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s child: %w", name, err)
	}
	var consumers sync.WaitGroup
	consumers.Add(2)
	consume := func(reader io.Reader) {
		defer consumers.Done()
		scanner := bufio.NewScanner(io.LimitReader(reader, 1<<20))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "vpnkit_phase=") {
				select {
				case child.phases <- line:
				default:
				}
			}
			child.mu.Lock()
			switch {
			case strings.HasPrefix(line, "native_gateway_vpn_lab_error="):
				child.diagnostic = safeText(line)
			case strings.HasPrefix(line, "vpnkit_reason=") && child.reason == "":
				child.reason = strings.TrimPrefix(line, "vpnkit_reason=")
			}
			child.mu.Unlock()
		}
	}
	go consume(stdout)
	go consume(stderr)
	go func() {
		waitErr := cmd.Wait()
		consumers.Wait()
		child.done <- waitErr
		close(child.done)
	}()
	return child, nil
}

func (c *childProcess) running() bool {
	if c == nil || c.cmd.Process == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return c.cmd.Process.Signal(syscall.Signal(0)) == nil
	}
}

func (c *childProcess) readinessFailureClassification() string {
	if c.running() {
		return "timeout"
	}
	c.mu.Lock()
	diagnostic, reason := c.diagnostic, c.reason
	c.mu.Unlock()
	if reason != "" {
		return reason
	}
	for _, match := range []struct{ text, classification string }{
		{"configuration synchronization", "config-sync"},
		{"runtime child exited before readiness", "runtime-child"},
		{"runtime readiness timed out", "runtime-timeout"},
		{"owned native gateway child is not running", "owned-child"},
		{"exited unsuccessfully", "runtime-command"},
		{"barrier", "fail-closed-barrier"},
		{"route", "routing"},
		{"TUN", "routing"},
		{"namespace", "namespace"},
	} {
		if strings.Contains(diagnostic, match.text) {
			return match.classification
		}
	}
	return "exit-other"
}

func (c *childProcess) waitPhase(parent context.Context, want string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		select {
		case phase := <-c.phases:
			fmt.Println(phase)
			if phase == want {
				return nil
			}
		case <-c.done:
			c.mu.Lock()
			diagnostic := c.diagnostic
			c.mu.Unlock()
			if diagnostic != "" {
				return fmt.Errorf("%s child exited before readiness: %s", c.name, diagnostic)
			}
			return fmt.Errorf("%s child exited before readiness without a controlled diagnostic", c.name)
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s readiness", c.name)
		}
	}
}

func (c *childProcess) stop() { c.stopWithGrace(2 * time.Second) }

func (c *childProcess) stopWithGrace(grace time.Duration) {
	if c == nil {
		return
	}
	c.once.Do(func() {
		// Signal before cancelling the CommandContext. Cancel kills the leader
		// immediately, bypassing its signal handler and graceful child cleanup.
		// That previously left the first gateway's sing-box TUN behind, so every
		// later synthetic candidate failed with sing-box-tun-busy.
		defer c.cancel()
		if c.cmd.Process != nil {
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		}
		select {
		case <-c.done:
		case <-time.After(grace):
			c.cancel()
			if c.cmd.Process != nil {
				_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			}
			select {
			case <-c.done:
			case <-time.After(time.Second):
			}
		}
	})
}

func waitFor(parent context.Context, timeout time.Duration, predicate func() bool) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runCommand(parent context.Context, timeout time.Duration, stdin io.Reader, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Stdin = stdin
	var stdout, stderr limitedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s exited unsuccessfully", filepath.Base(program))
	}
	return stdout.Bytes(), nil
}

func commandOK(ctx context.Context, program string, args ...string) bool {
	_, err := runCommand(ctx, 2*time.Second, nil, program, args...)
	return err == nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

type phaseWriter struct {
	out     io.Writer
	mu      sync.Mutex
	pending string
}

func newPhaseWriter(out io.Writer) *phaseWriter { return &phaseWriter{out: out} }

func (w *phaseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += string(p)
	for {
		end := strings.IndexByte(w.pending, '\n')
		if end < 0 {
			// Runtime output is private. Keep only a bounded fragment while waiting
			// for a complete line, and never forward the fragment itself.
			if len(w.pending) > 4096 {
				w.pending = w.pending[len(w.pending)-4096:]
			}
			return len(p), nil
		}
		line := strings.TrimSpace(w.pending[:end])
		w.pending = w.pending[end+1:]
		if strings.HasPrefix(line, "vpnkit_phase=") {
			_, _ = fmt.Fprintln(w.out, line)
		} else if reason := syntheticRuntimeReason(line); reason != "" {
			_, _ = fmt.Fprintln(w.out, "vpnkit_reason="+reason)
		}
	}
}

// syntheticRuntimeReason turns only fixed, public fixture failure patterns into
// bounded classes. Raw runtime output can contain subscription material and is
// never forwarded by the lab.
func syntheticRuntimeReason(line string) string {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "device or resource busy") {
		switch {
		case strings.Contains(lower, "cannot ioctl tunsetiff"):
			return "openvpn-tun-busy"
		case strings.Contains(lower, "inbound/tun"), strings.Contains(lower, "configure tun interface"):
			return "sing-box-tun-busy"
		default:
			return "runtime-resource-busy"
		}
	}
	for _, match := range []struct{ text, reason string }{
		{"address already in use", "runtime-address-in-use"},
		{"cannot open tun/tap dev", "runtime-tun-unavailable"},
		{"operation not permitted", "runtime-permission"},
		{"permission denied", "runtime-permission"},
	} {
		if strings.Contains(lower, match.text) {
			return match.reason
		}
	}
	return ""
}

func serviceCommand(argument string) []string {
	return []string{
		"setpriv", "--reuid", strconv.Itoa(serviceUID), "--regid", strconv.Itoa(serviceGID), "--clear-groups",
		"--inh-caps", "+net_admin,+net_raw", "--ambient-caps", "+net_admin,+net_raw",
		"--bounding-set", "-all,+net_admin,+net_raw", "--no-new-privs",
		fixtureRoot + "/native-vpn-lab", argument,
	}
}

func verifyServiceCapabilities(ctx context.Context, namespaceCommand []string) error {
	command := append(append([]string(nil), namespaceCommand...), serviceCommand("--cap-status")...)
	output, err := runCommand(ctx, commandTimeout, nil, command[0], command[1:]...)
	if err != nil {
		return fmt.Errorf("service capability probe: %w", err)
	}
	status := string(output)
	for _, want := range []string{
		"Uid:\t987\t987\t987\t987", "Gid:\t987\t987\t987\t987",
		"CapInh:\t" + serviceCaps, "CapPrm:\t" + serviceCaps, "CapEff:\t" + serviceCaps,
		"CapBnd:\t" + serviceCaps, "CapAmb:\t" + serviceCaps, "NoNewPrivs:\t1",
	} {
		if !strings.Contains(status, want) {
			return fmt.Errorf("service capability probe omitted required status field %q", want)
		}
	}
	fmt.Print(status)
	return nil
}

func reportCapabilityStatus() error {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return errors.New("capability status unavailable")
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, prefix := range []string{"Uid:", "Gid:", "CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:", "NoNewPrivs:"} {
			if strings.HasPrefix(line, prefix) {
				fmt.Println(line)
			}
		}
	}
	return nil
}

func setTreeOwnership(root string, uid, gid int, directoryMode, fileMode os.FileMode) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("unexpected non-regular fixture path")
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, directoryMode)
		}
		return os.Chmod(path, fileMode)
	})
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			return os.Chmod(target, 0700)
		}
		if !info.Mode().IsRegular() {
			return errors.New("non-regular fixture file")
		}
		return copyFile(path, target, 0600)
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	limit := int64(64 << 20)
	if mode.Perm()&0111 != 0 {
		limit = 256 << 20
	}
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("fixture file is non-regular or exceeds copy limit")
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, limit+1))
	if copyErr == nil && (written > limit || written != info.Size()) {
		copyErr = errors.New("fixture file changed or exceeded copy limit")
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(destination, mode)
}

func cleanupKernel() {
	commands := [][]string{
		{"nft", "delete", "table", nativegateway.NFTFamily, nativegateway.NFTTable},
		{"ip", "rule", "del", "priority", strconv.Itoa(nativegateway.EgressRulePriority)},
		{"ip", "rule", "del", "priority", strconv.Itoa(nativegateway.UnreachableRulePriority)},
		{"ip", "route", "flush", "table", strconv.Itoa(nativegateway.RoutingTable)},
		{"ip", "netns", "del", nativegateway.Namespace},
		{"ip", "link", "del", nativegateway.HostInterface},
		{"ip", "netns", "del", wanNamespace},
		{"ip", "link", "del", uplinkName},
	}
	for _, command := range commands {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = runCommand(cleanup, 2*time.Second, nil, command[0], command[1:]...)
		cancel()
	}
}

func fatal(phase string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "native_gateway_vpn_lab_error=%s timed out\n", phase)
	} else {
		fmt.Fprintf(os.Stderr, "native_gateway_vpn_lab_error=%s: %s\n", phase, safeError(err))
	}
	os.Exit(1)
}

func safeError(err error) string {
	return safeText(err.Error())
}

func safeText(text string) string {
	text = strings.ReplaceAll(text, privateRoot, "<private>")
	text = strings.ReplaceAll(text, fixtureRoot, "<fixture>")
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}
