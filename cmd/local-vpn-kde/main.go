//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/localvpn"
	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func run(args []string) error {
	if len(args) > 0 && (args[0] == "native-operation" || args[0] == "native-operation-client") {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		if args[0] == "native-operation" {
			if len(args) != 1 {
				return fmt.Errorf("native operation takes protocol input")
			}
			return localvpn.RunNativeOperation(ctx, os.Stdin, os.Stdout)
		}
		return localvpn.NativeOperationsClient(ctx, args[1:], os.Stdout)
	}

	if len(args) > 0 && (args[0] == "native-configure" || args[0] == "native-configure-client" || args[0] == "native-profile-transaction") {
		if len(args) != 1 {
			return fmt.Errorf("native configuration helper accepts no arguments")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		if args[0] == "native-profile-transaction" {
			return localvpn.RunNativeProfileTransaction(ctx, os.Stdin, os.Stdout)
		}
		if args[0] == "native-configure" {
			return localvpn.ConfigureNativeInstallation(ctx, os.Stdin)
		}
		return localvpn.RunNativeConfigurationClient(ctx, os.Stdin)
	}
	if len(args) > 0 && args[0] == "native-package" {
		if len(args) != 3 || args[1] != "--action" || (args[2] != "stage-runtime" && args[2] != "pre-upgrade" && args[2] != "pre-remove" && args[2] != "post-change" && args[2] != "recover") {
			return fmt.Errorf("native-package accepts --action pre-remove, pre-upgrade, stage-runtime, post-change or recover")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		if args[2] == "stage-runtime" {
			return localvpn.StageNativeRuntime(ctx)
		}
		if args[2] == "pre-upgrade" {
			return localvpn.PrepareNativePackageUpgrade(ctx)
		}
		if args[2] == "recover" {
			return localvpn.RecoverNativePackageChange(ctx)
		}
		if args[2] == "post-change" {
			return localvpn.CompleteNativePackageChange(ctx)
		}
		return localvpn.PrepareNativePackageRemoval(ctx)
	}
	if len(args) > 0 && args[0] == "native-network" {
		flags := flag.NewFlagSet("native-network", flag.ContinueOnError)
		action := flags.String("action", "status", "check, start, stop, or status")
		if err := flags.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("native-network accepts only --action")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		return localvpn.RunNativeNetwork(ctx, *action, os.Stdout)
	}
	if len(args) > 0 && (args[0] == "native-serve" || args[0] == "native-namespace-init") {
		if len(args) != 1 {
			return fmt.Errorf("%s takes no arguments; configuration is fixed by the installed service", args[0])
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		if args[0] == "native-namespace-init" {
			return localvpn.InitializeNativeNamespace(ctx)
		}
		return localvpn.ServeNativeGateway(ctx, os.Stdout)
	}
	if len(args) > 0 && args[0] == "host-smoke" {
		if len(args) != 1 {
			return fmt.Errorf("host-smoke takes no arguments")
		}
		opts, err := localvpn.SmokeEnvironment()
		if err != nil {
			fmt.Fprintln(os.Stdout, "host_smoke_failed_check=inputs")
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		return localvpn.HostSmoke(ctx, opts, os.Stdout)
	}
	if len(args) > 0 && args[0] == "ui" {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		root := filepath.Dir(filepath.Dir(binary))
		if binary == localvpn.InstalledCLI {
			root = localvpn.InstalledAssetRoot
		}
		for _, arg := range args[1:] {
			if arg == "--status-json" || arg == "--test" || arg == "--help" || arg == "-h" {
				if err := localvpn.ValidateUI(root); err != nil {
					return err
				}
				forwarded := append([]string{"bridge", "--repo", root}, args[1:]...)
				if arg == "--test" {
					forwarded = append(forwarded, "--status-json")
				}
				return run(forwarded)
			}
		}
		return localvpn.ExecUI(root, args[1:])
	}
	if len(args) > 0 {
		switch args[0] {
		case "container", "routing", "health", "dns-switch":
			return runContainer(args)
		}
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: local-vpn-kde render|assets|prepare|bridge")
	}
	flags := flag.NewFlagSet("local-vpn-kde", flag.ContinueOnError)
	action := flags.String("action", "status", "NetworkManager action")
	yes := flags.Bool("yes", false, "authorize the requested local mutation")
	repo := flags.String("repo", ".", "repository root")
	adapter := flags.String("lifecycle-executable", env("VPNKIT_TUI_LIFECYCLE_EXECUTABLE", ""), "fixed lifecycle adapter")
	mock := flags.Bool("test", false, "mock transport")
	mode := flags.String("mode", "strict", "initial interface mode")
	statusOnly := flags.Bool("status-json", false, "read-only interface status")
	base := flags.String("secrets-dir", env("VPNKIT_LOCAL_SECRETS_DIR", "secrets/vpnkit-local"), "private local state directory")
	runtimeMode := flags.String("runtime", "", "runtime: docker for checkout, native for installed package")
	policy := flags.String("policy", env("VPNKIT_LOCAL_POLICY", "strict"), "strict or smart")
	rules := flags.String("rulesets", env("VPNKIT_RULESET_SOURCE_MODE", "remote"), "remote or local-fixture")
	outbound := flags.String("outbound", env("VPNKIT_SELECTED_OUTBOUND_MODE", "subscription"), "subscription or direct-fixture")
	allowValue := strings.ToLower(env("VPNKIT_LOCAL_ALLOW_MISSING_SUBSCRIPTION", "false"))
	allow := flags.Bool("allow-missing-subscription", allowValue == "true" || allowValue == "1" || allowValue == "yes" || allowValue == "on", "allow absent subscription for preparation")
	endpoint := flags.String("endpoint", env("VPNKIT_LOCAL_ENDPOINT", env("VPNKIT_LOCAL_HOST", "127.0.0.1")), "loopback endpoint")
	portValue, err := strconv.Atoi(env("VPNKIT_LOCAL_PORT", env("VPNKIT_LOCAL_OPENVPN_PORT", "1194")))
	if err != nil {
		return fmt.Errorf("invalid local port")
	}
	port := flags.Int("port", portValue, "local UDP port")
	dns := flags.String("push-dns", env("VPNKIT_LOCAL_OPENVPN_PUSH_DNS", env("VPNKIT_OPENVPN_PUSH_DNS", "8.8.8.8")), "pushed Google DNS")
	daysValue, err := strconv.Atoi(env("VPNKIT_LOCAL_CERT_DAYS", "825"))
	if err != nil {
		return fmt.Errorf("invalid certificate lifetime")
	}
	days := flags.Int("cert-days", daysValue, "certificate lifetime")
	if err = flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	executablePath, executableErr := os.Executable()
	if executableErr != nil {
		return executableErr
	}
	installed := executablePath == localvpn.InstalledCLI
	if *runtimeMode == "" {
		*runtimeMode = "docker"
		if installed {
			*runtimeMode = "native"
		}
	}
	if *runtimeMode != "docker" && *runtimeMode != "native" {
		return fmt.Errorf("runtime must be docker or native")
	}
	if *runtimeMode == "native" {
		switch args[0] {
		case "render", "assets", "prepare", "bridge", "autostart", "native-desktop", "networkmanager":
		default:
			return fmt.Errorf("native runtime is unsupported for this command")
		}
		if *adapter != "" {
			return fmt.Errorf("native runtime does not accept lifecycle adapter overrides")
		}
	}
	if *runtimeMode == "native" && os.Getenv("VPNKIT_LOCAL_PORT") == "" && os.Getenv("VPNKIT_LOCAL_OPENVPN_PORT") == "" {
		explicitPort := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "port" {
				explicitPort = true
			}
		})
		if !explicitPort {
			*port = 31194
		}
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	var selected string
	if installed {
		layout, layoutErr := localvpn.ResolveApplicationLayout(executablePath, *repo, *base, *runtimeMode)
		if layoutErr != nil {
			return layoutErr
		}
		if err := localvpn.ValidateInstalledAssets(); err != nil {
			return err
		}
		root, selected = layout.Assets, layout.State
	} else {
		selected, err = localvpn.SecretRoot(root, *base, os.Getenv("VPNKIT_LOCAL_TEST_FIXTURE") == "1")
		if err != nil {
			return err
		}
	}
	switch args[0] {
	case "native-desktop":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		return localvpn.RunNativeDesktop(ctx, selected, *action, *yes, os.Stdout)
	case "autostart":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		a := localvpn.Autostart{Repo: root, Base: selected, Runtime: *runtimeMode, Installed: installed}
		var opts localvpn.AutostartOptions
		switch *action {
		case "status":
			opts, err = a.Status(ctx)
		case "gateway", "connect", "off":
			opts = localvpn.AutostartOptions{Gateway: *action != "off", Connect: *action == "connect"}
			if *runtimeMode == "native" {
				opts.Runtime = "native"
			}
			err = a.Configure(ctx, opts)
		case "run":
			return a.Run(ctx)
		default:
			return fmt.Errorf("invalid autostart action")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(opts)
	case "setup":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		return localvpn.RunSetup(ctx, root, *action, os.Stdout)
	case "networkmanager":
		if env("VPNKIT_LOCAL_NM_CONNECTION", "vpnkit-local") != "vpnkit-local" {
			return fmt.Errorf("connection name must be vpnkit-local")
		}
		expected := filepath.Join(selected, "openvpn/client/vpnkit-local.ovpn")
		if path := os.Getenv("VPNKIT_LOCAL_PROFILE"); path != "" {
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			if path != expected {
				return fmt.Errorf("profile must remain canonical")
			}
		}
		seconds, e := strconv.Atoi(env("VPNKIT_LOCAL_NM_CONNECT_TIMEOUT_SECONDS", "30"))
		if e != nil {
			return fmt.Errorf("invalid NetworkManager timeout")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		manager := localvpn.NetworkManager{Base: selected, Native: *runtimeMode == "native", Timeout: time.Duration(seconds) * time.Second}
		if !installed && filepath.Base(root) == "local-vpn-kde" {
			previous := filepath.Join(filepath.Dir(root), "vibe-practicum-vpn")
			if path, err := localvpn.SecretRoot(previous, "secrets/vpnkit-local", false); err == nil {
				manager.MigrationBase = path
			}
		}
		return manager.Run(ctx, *action, *yes, os.Stdout)
	case "bridge":
		if installed && !*statusOnly && !*mock {
			if err := localvpn.EnsureInstalledUserState(selected); err != nil {
				return err
			}
		}
		executable := *adapter
		if executable == "" {
			executable = filepath.Join(root, "scripts/vpnkit/vpnkit-local.sh")
			if *runtimeMode == "native" {
				executable = filepath.Join(root, "scripts/vpnkit/vpnkit-native.sh")
			}
		}
		grace := 31 * time.Second
		if seconds, e := strconv.ParseFloat(os.Getenv("VPNKIT_LOCAL_COMPENSATION_TIMEOUT_SECONDS"), 64); e == nil && seconds >= 1 && seconds <= 3600 {
			grace = time.Duration((seconds + 1) * float64(time.Second))
		}
		bridge, e := localvpn.NewBridge(localvpn.BridgeOptions{Repo: root, Base: selected, Executable: executable, Mode: *mode, Mock: *mock, Grace: grace, Runtime: *runtimeMode})
		if e != nil {
			return e
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		signals := make(chan os.Signal, 8)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1)
		defer signal.Stop(signals)
		go func() {
			for {
				select {
				case sig := <-signals:
					if sig == syscall.SIGUSR1 {
						bridge.Cancel()
					} else {
						cancel()
						_ = os.Stdin.Close()
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
		var input = os.Stdin
		if *statusOnly {
			return json.NewEncoder(os.Stdout).Encode(bridge.QueryStatus(ctx))
		}
		return bridge.Serve(ctx, input, os.Stdout)
	case "render":
		err = localvpn.Render(localvpn.RenderOptions{Template: filepath.Join(root, "config/sing-box/config.tun.json.template"), Base: selected, Runtime: *runtimeMode, Policy: *policy, RuleSets: *rules, Outbound: *outbound, AllowMissingSubscription: *allow, Fixture: os.Getenv("VPNKIT_LOCAL_TEST_FIXTURE") == "1"})
		if err == nil {
			watchdog := "compose_local_watchdog"
			if *runtimeMode == "native" {
				watchdog = "disabled"
			}
			fmt.Printf("vpnkit_local_render=ok\npolicy=%s\nruleset_mode=%s\ndns_failover=%s\nsecret_material=not_printed\n", *policy, *rules, watchdog)
		}
	case "assets", "prepare":
		err = localvpn.Assets(localvpn.AssetOptions{Base: selected, Runtime: *runtimeMode, TemplateDir: filepath.Join(root, "config/openvpn"), Endpoint: *endpoint, Port: *port, DNS: *dns, CertificateDays: *days})
		if err == nil && args[0] == "prepare" {
			err = localvpn.Render(localvpn.RenderOptions{Template: filepath.Join(root, "config/sing-box/config.tun.json.template"), Base: selected, Runtime: *runtimeMode, Policy: *policy, RuleSets: *rules, Outbound: *outbound, AllowMissingSubscription: true, Fixture: os.Getenv("VPNKIT_LOCAL_TEST_FIXTURE") == "1"})
		}
		if err == nil {
			fmt.Println("vpnkit_local_assets=ok\npermissions=directories_700_files_600\nsecret_material=not_printed\nsubscription_input=not_read\nnetworkmanager_import=not_run")
		}
	default:
		return fmt.Errorf("unknown command")
	}
	return err
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		if len(os.Args) > 1 && os.Args[1] == "native-network" && errors.Is(err, localvpn.ErrNativeNetworkBusy) {
			fmt.Fprintln(os.Stderr, "native namespace is held by another process; cleanup was not performed")
			os.Exit(localvpn.NativeNetworkBusyExitCode)
		}
		if len(os.Args) > 1 && os.Args[1] == "native-network" && errors.Is(err, nativegateway.ErrNetworkNeedsCleanup) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(localvpn.NativeNetworkChangedExitCode)
		}
		if errors.Is(err, localvpn.ErrOtherVPNActive) {
			fmt.Fprintln(os.Stderr, "native-other-vpn-active")
			os.Exit(20)
		}
		if len(os.Args) > 1 && os.Args[1] == "networkmanager" {
			if errors.Is(err, localvpn.ErrForeignNMProfile) {
				fmt.Fprintln(os.Stderr, "networkmanager_failure=foreign-profile")
			}
			if errors.Is(err, localvpn.ErrActiveNMMigration) {
				fmt.Fprintln(os.Stderr, "networkmanager_failure=previous-profile-active")
			}
			fmt.Fprintln(os.Stderr, "NetworkManager:", err)
		} else if len(os.Args) > 1 && (os.Args[1] == "host-smoke" || os.Args[1] == "native-network" || os.Args[1] == "native-desktop") {
			// Preserve network preflight and lifecycle reasons in the service
			// journal; these commands never include subscription configuration.
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, "local VPN operation failed closed")
		}
		os.Exit(20)
	}
}

func runContainer(args []string) error {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		if _, err = os.Stat("/run/.containerenv"); err != nil {
			return fmt.Errorf("container-only operation")
		}
	}
	c, err := localvpn.ContainerEnvironment()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	switch args[0] {
	case "container":
		if len(args) != 1 {
			return fmt.Errorf("unexpected runtime arguments")
		}
		return c.ServeContainer(ctx, os.Stdout)
	case "health":
		if len(args) != 1 {
			return fmt.Errorf("unexpected health arguments")
		}
		return c.Health(ctx)
	case "routing":
		if len(args) == 1 {
			return c.ApplyRouting(ctx)
		}
		if len(args) == 2 {
			switch args[1] {
			case "--install-fail-closed-barrier":
				return c.InstallBarrier(ctx)
			case "--remove-fail-closed-barrier":
				return c.RemoveBarrier(ctx)
			}
		}
	case "dns-switch":
		if len(args) == 2 {
			return c.SwitchDNS(ctx, args[1])
		}
	}
	return fmt.Errorf("invalid runtime arguments")
}
