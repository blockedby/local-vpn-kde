//go:build linux

package localvpn

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	nativeGenerationMaxSubscriptionURL = 8192
	nativeGenerationTempParent         = "/tmp"
	nativeGenerationTempPrefix         = "local-vpn-kde-native-"
)

// NativeGeneratedConfiguration is the in-memory result of native configuration
// generation. Publication, ownership changes, and client-profile delivery are
// deliberately outside this operation.
type NativeGeneratedConfiguration struct {
	Bundle        NativeConfigurationBundle
	ClientProfile []byte
}

// GenerateNativeConfiguration validates the fixed installed package, stages a
// fresh native configuration, and returns it without publishing or starting
// anything. It must be called by the root production entrypoint. The caller is
// responsible for authentication, stopping the gateway, calling
// PublishNativeConfiguration, applying ownership/group grants, delivering
// ClientProfile, and importing it into NetworkManager.
func GenerateNativeConfiguration(ctx context.Context, subscriptionURL string, ownerUID uint32) (NativeGeneratedConfiguration, error) {
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}
	if os.Geteuid() != 0 {
		return NativeGeneratedConfiguration{}, errors.New("native configuration generation requires root")
	}
	if err := validateNativeGenerationSubscriptionURL(subscriptionURL); err != nil {
		return NativeGeneratedConfiguration{}, err
	}
	if err := ValidateInstalledAssets(); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native installed assets are unavailable")
	}
	return generateNativeConfigurationStaged(ctx, subscriptionURL, ownerUID, InstalledAssetRoot, nativeGenerationTempParent, false)
}

// generateNativeConfigurationAt is a non-production test seam. Production
// callers cannot select either argument: GenerateNativeConfiguration fixes the
// installed asset root and the /tmp staging parent before entering the shared
// implementation.
func generateNativeConfigurationAt(ctx context.Context, subscriptionURL string, ownerUID uint32, assetsRoot, tempParent string) (NativeGeneratedConfiguration, error) {
	return generateNativeConfigurationStaged(ctx, subscriptionURL, ownerUID, assetsRoot, tempParent, true)
}

func generateNativeConfigurationStaged(ctx context.Context, subscriptionURL string, ownerUID uint32, assetsRoot, tempParent string, validateAssets bool) (NativeGeneratedConfiguration, error) {
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}
	if err := validateNativeGenerationSubscriptionURL(subscriptionURL); err != nil {
		return NativeGeneratedConfiguration{}, err
	}
	if err := validateAbsoluteLayoutPath(assetsRoot); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native installed assets are unavailable")
	}
	if validateAssets {
		if err := validateNativeGenerationAssetsAt(assetsRoot); err != nil {
			return NativeGeneratedConfiguration{}, errors.New("native installed assets are unavailable")
		}
	}
	if err := validateNativeGenerationTempParent(tempParent); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native staging parent is unavailable")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}

	stage, err := os.MkdirTemp(tempParent, nativeGenerationTempPrefix)
	if err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native staging directory creation failed")
	}
	defer func() {
		_ = os.RemoveAll(stage)
	}()
	if err := protectNativeGenerationStage(stage); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native staging directory is unsafe")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}

	if err := writeNativeGenerationSubscription(stage, []byte(subscriptionURL+"\n")); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native subscription staging failed")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}

	if err := Assets(AssetOptions{
		Base:            stage,
		TemplateDir:     filepath.Join(assetsRoot, "config/openvpn"),
		Runtime:         "native",
		Endpoint:        "127.0.0.1",
		Port:            31194,
		DNS:             "8.8.8.8",
		CertificateDays: 825,
	}); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native asset generation failed")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}

	if err := Render(RenderOptions{
		Template: filepath.Join(assetsRoot, "config/sing-box/config.tun.json.template"),
		Base:     stage,
		Runtime:  "native",
		Policy:   "strict",
		RuleSets: "remote",
		Outbound: "subscription",
	}); err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native configuration rendering failed")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}

	bundle := NativeConfigurationBundle{OwnerUID: ownerUID, Files: make(map[string][]byte)}
	var bundleTotal uint64
	addBundleFile := func(name string, data []byte) error {
		if len(data) == 0 || len(data) > nativeConfigurationMaxFile || bundleTotal+uint64(len(data)) > nativeConfigurationMaxTotal {
			return errors.New("native configuration output is too large")
		}
		bundleTotal += uint64(len(data))
		bundle.Files[name] = data
		return nil
	}
	for _, name := range nativeConfigurationRequiredFiles {
		stagedName := name
		switch name {
		case "sing-box/config.json":
			stagedName = "rendered/sing-box/config.json"
		case "vibe-vpn/config.yaml":
			stagedName = "rendered/vibe-vpn/config.yaml"
		case "openvpn/server.conf":
			stagedName = "rendered/openvpn/server.conf"
		}
		data, readErr := readNativeGeneratedFile(stage, stagedName)
		if readErr != nil {
			return NativeGeneratedConfiguration{}, errors.New("native configuration output is incomplete")
		}
		if err := addBundleFile(name, data); err != nil {
			return NativeGeneratedConfiguration{}, err
		}
	}
	for _, name := range nativeConfigurationOptionalFiles {
		data, readErr := readNativeGeneratedFile(stage, filepath.Join("rendered", filepath.FromSlash(name)))
		if errors.Is(readErr, unix.ENOENT) {
			continue
		}
		if readErr != nil {
			return NativeGeneratedConfiguration{}, errors.New("native configuration output is unsafe")
		}
		if err := addBundleFile(name, data); err != nil {
			return NativeGeneratedConfiguration{}, err
		}
	}
	profile, err := readNativeGeneratedFile(stage, "openvpn/client/vpnkit-local.ovpn")
	if err != nil {
		return NativeGeneratedConfiguration{}, errors.New("native client profile is unavailable")
	}
	if err := nativeGenerationContextErr(ctx); err != nil {
		return NativeGeneratedConfiguration{}, err
	}
	return NativeGeneratedConfiguration{Bundle: bundle, ClientProfile: profile}, nil
}

func validateNativeGenerationSubscriptionURL(value string) error {
	if value == "" || len(value) > nativeGenerationMaxSubscriptionURL {
		return errors.New("invalid subscription URL")
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return errors.New("invalid subscription URL")
		}
	}
	lower := strings.ToLower(value)
	for _, encodedControl := range []string{"%00", "%0a", "%0d"} {
		if strings.Contains(lower, encodedControl) {
			return errors.New("invalid subscription URL")
		}
	}
	if strings.ContainsRune(value, '#') {
		return errors.New("invalid subscription URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return errors.New("invalid subscription URL")
	}
	return nil
}

func validateNativeGenerationAssetsAt(assetRoot string) error {
	if err := validateAbsoluteLayoutPath(assetRoot); err != nil {
		return err
	}
	uid := uint32(os.Getuid())
	for _, relative := range installedAssetFiles {
		executable := relative == "scripts/vpnkit/vpnkit-native.sh"
		if err := validateTrustedRegularFile(filepath.Join(assetRoot, relative), uid, assetRoot, executable); err != nil {
			return err
		}
	}
	return nil
}

func validateNativeGenerationTempParent(parent string) error {
	if err := validateAbsoluteLayoutPath(parent); err != nil {
		return err
	}
	fd, err := unix.Open(parent, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("invalid staging parent")
	}
	return nil
}

func protectNativeGenerationStage(path string) error {
	fd, err := unix.Open(path, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchmod(fd, 0700); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) || stat.Mode&0777 != 0700 {
		return errors.New("unsafe staging directory")
	}
	return nil
}

func writeNativeGenerationSubscription(base string, data []byte) error {
	root, err := directory(base, false, true)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	vibe, err := childDirectory(root, "vibe-vpn", true, true)
	if err != nil {
		return err
	}
	defer unix.Close(vibe)
	return atomicWrite(vibe, "sub_url", data)
}

func readNativeGeneratedFile(base, name string) ([]byte, error) {
	parts := strings.Split(filepath.ToSlash(name), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, errors.New("invalid generated file")
	}
	fd, err := directory(base, false, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	for index, part := range parts {
		if !component(part) {
			return nil, errors.New("invalid generated file")
		}
		if index == len(parts)-1 {
			return readNativeGeneratedFileAt(fd, part)
		}
		child, openErr := childDirectory(fd, part, false, true)
		if openErr != nil {
			return nil, openErr
		}
		unix.Close(fd)
		fd = child
	}
	return nil, errors.New("invalid generated file")
}

func readNativeGeneratedFileAt(parent int, name string) ([]byte, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "native-generated-file")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Size < 0 || stat.Size > int64(nativeConfigurationMaxFile) {
		return nil, errors.New("unsafe generated file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(nativeConfigurationMaxFile)+1))
	if err != nil || len(data) > nativeConfigurationMaxFile {
		return nil, errors.New("generated file is too large")
	}
	return data, nil
}

func nativeGenerationContextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("native configuration generation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
