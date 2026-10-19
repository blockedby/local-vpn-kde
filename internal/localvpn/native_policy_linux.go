//go:build linux

package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"golang.org/x/sys/unix"
)

func loadNativePolicyBundle(uid uint32) (NativeConfigurationBundle, error) {
	bundle := NativeConfigurationBundle{OwnerUID: uid, Files: map[string][]byte{}}
	identity, err := user.Lookup(nativegateway.ServiceUser)
	if err != nil {
		return bundle, err
	}
	gid, err := parseNativeConfigurationID(identity.Gid)
	if err != nil {
		return bundle, err
	}
	serviceUID, err := parseNativeConfigurationID(identity.Uid)
	if err != nil {
		return bundle, err
	}
	publisher := nativeConfigurationPublisher{rootDir: "/etc", rootUID: 0, serviceUID: serviceUID, serviceGID: gid}
	parent, err := unix.Open("/etc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return bundle, err
	}
	defer unix.Close(parent)
	prior, err := inspectNativeConfigurationTarget(parent, uid, publisher)
	if err != nil {
		return bundle, err
	}
	if !prior.exists || len(prior.files) == 0 {
		return bundle, errors.New("native configuration missing")
	}
	fd, err := unix.Openat(parent, nativeConfigurationTarget, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return bundle, err
	}
	defer unix.Close(fd)
	for name := range prior.files {
		data, err := readNativeConfigurationFile(fd, name, 0, gid, 0640, nativeConfigurationMaxFile)
		if err != nil {
			return bundle, err
		}
		bundle.Files[name] = data
	}
	return bundle, nil
}
func policyFromNativeConfig(data []byte) (string, error) {
	var config struct {
		Route struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if json.Unmarshal(data, &config) != nil {
		return "", errors.New("invalid native policy")
	}
	for _, rule := range config.Route.Rules {
		if rule["outbound"] == "direct-out" {
			return "smart", nil
		}
	}
	return "strict", nil
}
func nativeInstalledPolicy(uid uint32) (string, error) {
	bundle, err := loadNativePolicyBundle(uid)
	if err != nil {
		return "", err
	}
	return policyFromNativeConfig(bundle.Files["sing-box/config.json"])
}
func toggleNativePolicy(ctx context.Context, uid uint32, output io.Writer) error {
	service := NativeService{}
	if err := service.mutationPreflight(ctx); err != nil {
		return err
	}
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	if status.Gateway != "inactive" && !status.Ready {
		return errors.New("native gateway must be ready or stopped")
	}
	bundle, err := loadNativePolicyBundle(uid)
	if err != nil {
		return err
	}
	old := bundle.Files["sing-box/config.json"]
	policy, err := policyFromNativeConfig(old)
	if err != nil {
		return err
	}
	if policy == "strict" {
		policy = "smart"
	} else {
		policy = "strict"
	}
	template, err := os.ReadFile(InstalledAssetRoot + "/config/sing-box/config.tun.json.template")
	if err != nil {
		return err
	}
	next, err := buildConfig(template, RenderOptions{Policy: policy, RuleSets: "remote", Outbound: "subscription"})
	if err != nil {
		return err
	}
	next, err = nativeSingBoxConfig(next)
	if err != nil {
		return err
	}
	// Keep the trusted source outbound; runtime synchronization separately
	// preserves the user-selected outbound under the existing state lock.
	var previous, replacement map[string]any
	if json.Unmarshal(old, &previous) != nil || json.Unmarshal(next, &replacement) != nil {
		return errors.New("invalid native policy source")
	}
	replacement["outbounds"] = previous["outbounds"]
	next, err = json.MarshalIndent(replacement, "", "  ")
	if err != nil {
		return err
	}
	stop := func(ctx context.Context) error {
		_, err := service.call(ctx, "systemctl", "stop", nativegateway.GatewayUnit)
		return err
	}
	start := func(ctx context.Context) error { return service.Start(ctx) }
	if status.Ready {
		if err = stop(ctx); err != nil {
			return err
		}
	}
	restore := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = stop(cleanup)
		bundle.Files["sing-box/config.json"] = old
		if PublishNativeConfiguration(cleanup, bundle) == nil && status.Ready {
			_ = start(cleanup)
		}
	}
	bundle.Files["sing-box/config.json"] = append(next, '\n')
	if err = PublishNativeConfiguration(ctx, bundle); err != nil {
		restore()
		return err
	}
	if status.Ready {
		if err = start(ctx); err != nil {
			restore()
			return err
		}
	}
	return json.NewEncoder(output).Encode(map[string]string{"routing_policy": policy})
}
