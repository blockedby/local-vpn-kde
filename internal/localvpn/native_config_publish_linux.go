//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	nativeConfigurationRoot     = "/etc"
	nativeConfigurationTarget   = "local-vpn-kde"
	nativeConfigurationManifest = ".native-configuration-manifest.json"
	nativeConfigurationLayout   = "local-vpn-kde/native-configuration/v1"
	nativeConfigurationVersion  = 1
	nativeConfigurationMaxFile  = 16 << 20
	nativeConfigurationMaxTotal = 32 << 20
	nativeConfigurationService  = "local-vpn-kde"
	nativeConfigurationLock     = ".local-vpn-kde.publish.lock"
)

var nativeConfigurationRequiredFiles = []string{
	"sing-box/config.json",
	"vibe-vpn/config.yaml",
	"vibe-vpn/sub_url",
	"openvpn/server.conf",
	"openvpn/pki/ca.crt",
	"openvpn/pki/server.crt",
	"openvpn/pki/server.key",
	"openvpn/pki/ta.key",
}

var nativeConfigurationOptionalFiles = []string{
	"sing-box/rule-sets/vpnkit-adblock.json",
	"sing-box/rule-sets/vpnkit-dev-direct.json",
	"sing-box/rule-sets/geoip-ru.json",
	"sing-box/rule-sets/geosite-category-ru.json",
}

// NativeConfigurationBundle is a complete, root-generated native gateway
// configuration. Files are relative names from the fixed allowlist; callers
// cannot select the publication destination.
type NativeConfigurationBundle struct {
	OwnerUID uint32
	Files    map[string][]byte
}

// PublishNativeConfiguration atomically publishes bundle at
// /etc/local-vpn-kde. It must be called by root. The caller remains responsible
// for authenticating the request, generating the bundle from trusted inputs,
// serializing the wider setup operation, and keeping the gateway inactive until
// publication returns.
func PublishNativeConfiguration(ctx context.Context, bundle NativeConfigurationBundle) error {
	if os.Geteuid() != 0 {
		return errors.New("native configuration publication requires root")
	}
	group, err := user.LookupGroup(nativeConfigurationService)
	if err != nil {
		return errors.New("native configuration service identity is unavailable")
	}
	serviceUser, err := user.Lookup(nativeConfigurationService)
	if err != nil {
		return errors.New("native configuration service identity is unavailable")
	}
	gid, err := parseNativeConfigurationID(group.Gid)
	if err != nil {
		return errors.New("native configuration service identity is invalid")
	}
	serviceUID, err := parseNativeConfigurationID(serviceUser.Uid)
	if err != nil {
		return errors.New("native configuration service identity is invalid")
	}
	return publishNativeConfiguration(ctx, bundle, nativeConfigurationPublisher{
		rootDir:    nativeConfigurationRoot,
		rootUID:    0,
		serviceUID: serviceUID,
		serviceGID: gid,
	})
}

// NativeConfigurationCommittedError reports a publication failure that
// occurred after the new root configuration was atomically installed. Callers
// must not roll a paired user profile back when this error is returned.
type NativeConfigurationCommittedError struct{ cause error }

func (e *NativeConfigurationCommittedError) Error() string { return e.cause.Error() }
func (e *NativeConfigurationCommittedError) Unwrap() error { return e.cause }

func nativeConfigurationCommittedError(message string) error {
	return &NativeConfigurationCommittedError{cause: errors.New(message)}
}

type nativeConfigurationPublisher struct {
	rootDir    string
	rootUID    uint32
	serviceUID uint32
	serviceGID uint32
}

type nativeConfigurationManifestData struct {
	Version  int               `json:"version"`
	Layout   string            `json:"layout"`
	OwnerUID uint32            `json:"ownerUID"`
	Files    map[string]string `json:"files"`
}

type nativeConfigurationPrior struct {
	exists bool
	files  map[string]string
}

func parseNativeConfigurationID(value string) (uint32, error) {
	id, err := strconv.ParseUint(value, 10, 32)
	return uint32(id), err
}

func publishNativeConfiguration(ctx context.Context, bundle NativeConfigurationBundle, p nativeConfigurationPublisher) error {
	manifest, manifestBytes, err := prepareNativeConfigurationBundle(bundle, p.serviceUID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.New("native configuration publication canceled")
	}

	parentFD, err := unix.Open(p.rootDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("native configuration parent is unavailable")
	}
	defer unix.Close(parentFD)
	if err := validateNativeConfigurationDirectoryFD(parentFD, p.rootUID, nil); err != nil {
		return errors.New("native configuration parent is not protected")
	}

	lockFD, err := openNativeConfigurationLock(parentFD, p.rootUID)
	if err != nil {
		return err
	}
	defer unix.Close(lockFD)
	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		return errors.New("native configuration publication lock failed")
	}
	defer unix.Flock(lockFD, unix.LOCK_UN) //nolint:errcheck -- best effort while closing the descriptor

	prior, err := inspectNativeConfigurationTarget(parentFD, bundle.OwnerUID, p)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.New("native configuration publication canceled")
	}

	stageName, stageFD, err := createNativeConfigurationStage(parentFD, p)
	if err != nil {
		return err
	}
	stageOpen := true
	committed := false
	defer func() {
		if stageOpen {
			unix.Close(stageFD) //nolint:errcheck -- cleanup path
		}
		if !committed {
			cleanupNativeConfigurationTree(parentFD, stageName, manifest.Files, p) //nolint:errcheck -- original error wins
		}
	}()

	if err := populateNativeConfigurationStage(stageFD, bundle.Files, manifestBytes, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.New("native configuration publication canceled")
	}
	if err := unix.Fsync(stageFD); err != nil {
		return errors.New("native configuration staging sync failed")
	}
	if err := unix.Close(stageFD); err != nil {
		stageOpen = false
		return errors.New("native configuration staging close failed")
	}
	stageOpen = false

	if prior.exists {
		err = unix.Renameat2(parentFD, stageName, parentFD, nativeConfigurationTarget, unix.RENAME_EXCHANGE)
	} else {
		err = unix.Renameat2(parentFD, stageName, parentFD, nativeConfigurationTarget, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		return errors.New("native configuration atomic commit failed")
	}
	committed = true
	if err := unix.Fsync(parentFD); err != nil {
		return nativeConfigurationCommittedError("native configuration was committed but parent sync failed")
	}

	if prior.exists {
		if err := cleanupNativeConfigurationTree(parentFD, stageName, prior.files, p); err != nil {
			return nativeConfigurationCommittedError("native configuration was committed but previous-tree cleanup failed")
		}
		if err := unix.Fsync(parentFD); err != nil {
			return nativeConfigurationCommittedError("native configuration was committed but cleanup sync failed")
		}
	}
	return nil
}

func prepareNativeConfigurationBundle(bundle NativeConfigurationBundle, serviceUID uint32) (nativeConfigurationManifestData, []byte, error) {
	if bundle.OwnerUID == 0 || bundle.OwnerUID == serviceUID {
		return nativeConfigurationManifestData{}, nil, errors.New("native configuration owner is invalid")
	}
	allowed := nativeConfigurationAllowedFiles()
	for _, name := range nativeConfigurationRequiredFiles {
		if _, ok := bundle.Files[name]; !ok {
			return nativeConfigurationManifestData{}, nil, errors.New("native configuration is incomplete")
		}
	}
	var total uint64
	digests := make(map[string]string, len(bundle.Files))
	for name, data := range bundle.Files {
		if _, ok := allowed[name]; !ok || name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") {
			return nativeConfigurationManifestData{}, nil, errors.New("native configuration contains an unsupported file")
		}
		if len(data) == 0 || len(data) > nativeConfigurationMaxFile {
			return nativeConfigurationManifestData{}, nil, errors.New("native configuration file size is invalid")
		}
		total += uint64(len(data))
		if total > nativeConfigurationMaxTotal {
			return nativeConfigurationManifestData{}, nil, errors.New("native configuration total size is invalid")
		}
		if strings.HasSuffix(name, ".json") && !validNativeConfigurationJSONObject(data) {
			return nativeConfigurationManifestData{}, nil, errors.New("native configuration JSON is invalid")
		}
		sum := sha256.Sum256(data)
		digests[name] = hex.EncodeToString(sum[:])
	}
	manifest := nativeConfigurationManifestData{
		Version: nativeConfigurationVersion, Layout: nativeConfigurationLayout,
		OwnerUID: bundle.OwnerUID, Files: digests,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nativeConfigurationManifestData{}, nil, errors.New("native configuration manifest generation failed")
	}
	return manifest, append(encoded, '\n'), nil
}

func validNativeConfigurationJSONObject(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func nativeConfigurationAllowedFiles() map[string]struct{} {
	allowed := make(map[string]struct{}, len(nativeConfigurationRequiredFiles)+len(nativeConfigurationOptionalFiles))
	for _, name := range nativeConfigurationRequiredFiles {
		allowed[name] = struct{}{}
	}
	for _, name := range nativeConfigurationOptionalFiles {
		allowed[name] = struct{}{}
	}
	return allowed
}

func nativeConfigurationAllowedDirectories() map[string]struct{} {
	return map[string]struct{}{
		"sing-box": {}, "sing-box/rule-sets": {}, "vibe-vpn": {},
		"openvpn": {}, "openvpn/pki": {},
	}
}

func openNativeConfigurationLock(parentFD int, rootUID uint32) (int, error) {
	fd, err := unix.Openat(parentFD, nativeConfigurationLock, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return -1, errors.New("native configuration publication lock is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != rootUID || stat.Mode&0777 != 0600 {
		unix.Close(fd)
		return -1, errors.New("native configuration publication lock is not protected")
	}
	return fd, nil
}

func inspectNativeConfigurationTarget(parentFD int, ownerUID uint32, p nativeConfigurationPublisher) (nativeConfigurationPrior, error) {
	fd, err := unix.Openat(parentFD, nativeConfigurationTarget, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nativeConfigurationPrior{}, nil
	}
	if err != nil {
		return nativeConfigurationPrior{}, errors.New("existing native configuration is unsafe")
	}
	defer unix.Close(fd)
	if err := validateNativeConfigurationDirectoryFD(fd, p.rootUID, &p.serviceGID); err != nil {
		return nativeConfigurationPrior{}, errors.New("existing native configuration is unsafe")
	}

	names, err := nativeConfigurationDirectoryNames(fd)
	if err != nil {
		return nativeConfigurationPrior{}, errors.New("existing native configuration cannot be inspected")
	}
	if len(names) == 0 {
		return nativeConfigurationPrior{exists: true, files: map[string]string{}}, nil
	}
	if err := unix.Fstatat(fd, nativeConfigurationManifest, &unix.Stat_t{}, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		if err := validateEmptyNativeConfigurationTree(fd, p); err != nil {
			return nativeConfigurationPrior{}, errors.New("existing native configuration is not an empty protected tree")
		}
		return nativeConfigurationPrior{exists: true, files: map[string]string{}}, nil
	}

	manifestBytes, err := readNativeConfigurationFile(fd, nativeConfigurationManifest, p.rootUID, p.serviceGID, 0600, 1<<20)
	if err != nil {
		return nativeConfigurationPrior{}, errors.New("existing native configuration manifest is unsafe")
	}
	var manifest nativeConfigurationManifestData
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(&struct{}{}) != io.EOF || manifest.Version != nativeConfigurationVersion || manifest.Layout != nativeConfigurationLayout || manifest.OwnerUID != ownerUID {
		return nativeConfigurationPrior{}, errors.New("existing native configuration manifest is invalid")
	}
	if err := validateNativeConfigurationManifestFiles(manifest.Files); err != nil {
		return nativeConfigurationPrior{}, errors.New("existing native configuration manifest is invalid")
	}
	if err := validateNativeConfigurationTree(fd, manifest.Files, p); err != nil {
		return nativeConfigurationPrior{}, err
	}
	return nativeConfigurationPrior{exists: true, files: manifest.Files}, nil
}

func validateNativeConfigurationManifestFiles(files map[string]string) error {
	allowed := nativeConfigurationAllowedFiles()
	for _, name := range nativeConfigurationRequiredFiles {
		if _, ok := files[name]; !ok {
			return errors.New("missing file")
		}
	}
	for name, digest := range files {
		decoded, err := hex.DecodeString(digest)
		if _, ok := allowed[name]; !ok || err != nil || len(decoded) != sha256.Size {
			return errors.New("invalid file")
		}
	}
	return nil
}

func validateNativeConfigurationTree(rootFD int, files map[string]string, p nativeConfigurationPublisher) error {
	expectedDirs := map[string]struct{}{}
	for name := range files {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			expectedDirs[dir] = struct{}{}
		}
	}
	if err := validateNativeConfigurationEntries(rootFD, "", files, expectedDirs, p); err != nil {
		return errors.New("existing native configuration tree is unsafe")
	}
	for name, want := range files {
		data, err := readNativeConfigurationFile(rootFD, name, p.rootUID, p.serviceGID, 0640, nativeConfigurationMaxFile)
		if err != nil {
			return errors.New("existing native configuration file is unsafe")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return errors.New("existing native configuration digest mismatch")
		}
	}
	return nil
}

func validateNativeConfigurationEntries(dirFD int, prefix string, files map[string]string, dirs map[string]struct{}, p nativeConfigurationPublisher) error {
	names, err := nativeConfigurationDirectoryNames(dirFD)
	if err != nil {
		return err
	}
	for _, base := range names {
		name := base
		if prefix != "" {
			name = prefix + "/" + base
		}
		if name == nativeConfigurationManifest && prefix == "" {
			continue
		}
		if _, ok := files[name]; ok {
			var stat unix.Stat_t
			if err := unix.Fstatat(dirFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || !validNativeConfigurationFileStat(&stat, p.rootUID, p.serviceGID, 0640) {
				return errors.New("unsafe file")
			}
			continue
		}
		if _, ok := dirs[name]; !ok {
			return errors.New("foreign entry")
		}
		childFD, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil || validateNativeConfigurationDirectoryFD(childFD, p.rootUID, &p.serviceGID) != nil {
			if childFD >= 0 {
				unix.Close(childFD)
			}
			return errors.New("unsafe directory")
		}
		err = validateNativeConfigurationEntries(childFD, name, files, dirs, p)
		unix.Close(childFD)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateEmptyNativeConfigurationTree(rootFD int, p nativeConfigurationPublisher) error {
	allowed := nativeConfigurationAllowedDirectories()
	var walk func(int, string) error
	walk = func(dirFD int, prefix string) error {
		names, err := nativeConfigurationDirectoryNames(dirFD)
		if err != nil {
			return err
		}
		for _, base := range names {
			name := base
			if prefix != "" {
				name = prefix + "/" + base
			}
			if _, ok := allowed[name]; !ok {
				return errors.New("foreign entry")
			}
			childFD, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil || validateNativeConfigurationDirectoryFD(childFD, p.rootUID, &p.serviceGID) != nil {
				if childFD >= 0 {
					unix.Close(childFD)
				}
				return errors.New("unsafe directory")
			}
			err = walk(childFD, name)
			unix.Close(childFD)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walk(rootFD, "")
}

func createNativeConfigurationStage(parentFD int, p nativeConfigurationPublisher) (string, int, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", -1, errors.New("native configuration staging failed")
	}
	name := ".local-vpn-kde.new-" + hex.EncodeToString(random[:])
	if err := unix.Mkdirat(parentFD, name, 0750); err != nil {
		return "", -1, errors.New("native configuration staging failed")
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR) //nolint:errcheck -- best effort
		return "", -1, errors.New("native configuration staging failed")
	}
	if err := unix.Fchown(fd, int(p.rootUID), int(p.serviceGID)); err != nil || unix.Fchmod(fd, 0750) != nil {
		unix.Close(fd)
		unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR) //nolint:errcheck -- best effort
		return "", -1, errors.New("native configuration staging ownership failed")
	}
	return name, fd, nil
}

func populateNativeConfigurationStage(rootFD int, files map[string][]byte, manifest []byte, p nativeConfigurationPublisher) error {
	for _, dir := range []string{"sing-box", "sing-box/rule-sets", "vibe-vpn", "openvpn", "openvpn/pki"} {
		needed := false
		for name := range files {
			if strings.HasPrefix(name, dir+"/") {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		if err := mkdirNativeConfigurationPath(rootFD, dir, p); err != nil {
			return err
		}
	}
	for name, data := range files {
		if err := writeNativeConfigurationFile(rootFD, name, data, 0640, p); err != nil {
			return err
		}
	}
	if err := writeNativeConfigurationFile(rootFD, nativeConfigurationManifest, manifest, 0600, p); err != nil {
		return err
	}
	return syncNativeConfigurationDirectories(rootFD, files)
}

func mkdirNativeConfigurationPath(rootFD int, name string, p nativeConfigurationPublisher) error {
	parts := strings.Split(name, "/")
	fd := rootFD
	ownedFD := -1
	for _, part := range parts {
		if err := unix.Mkdirat(fd, part, 0750); err != nil && !errors.Is(err, unix.EEXIST) {
			if ownedFD >= 0 {
				unix.Close(ownedFD)
			}
			return errors.New("native configuration directory creation failed")
		}
		child, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil || unix.Fchown(child, int(p.rootUID), int(p.serviceGID)) != nil || unix.Fchmod(child, 0750) != nil {
			if child >= 0 {
				unix.Close(child)
			}
			if ownedFD >= 0 {
				unix.Close(ownedFD)
			}
			return errors.New("native configuration directory protection failed")
		}
		if ownedFD >= 0 {
			unix.Close(ownedFD)
		}
		ownedFD = child
		fd = child
	}
	if ownedFD >= 0 {
		unix.Close(ownedFD)
	}
	return nil
}

func writeNativeConfigurationFile(rootFD int, name string, data []byte, mode uint32, p nativeConfigurationPublisher) error {
	fd, err := unix.Openat(rootFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return errors.New("native configuration file creation failed")
	}
	ok := false
	defer func() {
		unix.Close(fd) //nolint:errcheck -- error is returned explicitly where material
		if !ok {
			unix.Unlinkat(rootFD, name, 0)
		} //nolint:errcheck -- bounded best effort
	}()
	if err := unix.Fchown(fd, int(p.rootUID), int(p.serviceGID)); err != nil || unix.Fchmod(fd, mode) != nil {
		return errors.New("native configuration file ownership failed")
	}
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil || n == 0 {
			return errors.New("native configuration file write failed")
		}
		data = data[n:]
	}
	if err := unix.Fsync(fd); err != nil {
		return errors.New("native configuration file sync failed")
	}
	ok = true
	return nil
}

func syncNativeConfigurationDirectories(rootFD int, files map[string][]byte) error {
	dirSet := map[string]struct{}{".": {}}
	for name := range files {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			dirSet[dir] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		leftDepth, rightDepth := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if dirs[i] == "." {
			leftDepth = -1
		}
		if dirs[j] == "." {
			rightDepth = -1
		}
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return dirs[i] < dirs[j]
	})
	for _, dir := range dirs {
		fd := rootFD
		if dir != "." {
			var err error
			fd, err = unix.Openat(rootFD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return errors.New("native configuration directory sync failed")
			}
		}
		if err := unix.Fsync(fd); err != nil {
			if dir != "." {
				unix.Close(fd)
			}
			return errors.New("native configuration directory sync failed")
		}
		if dir != "." {
			unix.Close(fd)
		}
	}
	return nil
}

func cleanupNativeConfigurationTree(parentFD int, name string, files map[string]string, p nativeConfigurationPublisher) error {
	rootFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil || validateNativeConfigurationDirectoryFD(rootFD, p.rootUID, &p.serviceGID) != nil {
		if rootFD >= 0 {
			unix.Close(rootFD)
		}
		return errors.New("unsafe cleanup tree")
	}
	defer unix.Close(rootFD)
	for file := range files {
		if err := unix.Unlinkat(rootFD, file, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	if err := unix.Unlinkat(rootFD, nativeConfigurationManifest, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	for _, dir := range []string{"sing-box/rule-sets", "openvpn/pki", "sing-box", "vibe-vpn", "openvpn"} {
		if err := unix.Unlinkat(rootFD, dir, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return nil
}

func readNativeConfigurationFile(rootFD int, name string, uid, gid uint32, mode uint32, limit int) ([]byte, error) {
	fd, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "native-configuration-file")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || !validNativeConfigurationFileStat(&stat, uid, gid, mode) || stat.Size < 0 || stat.Size > int64(limit) {
		return nil, errors.New("unsafe file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("file read failed")
	}
	return data, nil
}

func validNativeConfigurationFileStat(stat *unix.Stat_t, uid, gid uint32, mode uint32) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && stat.Uid == uid && stat.Gid == gid && stat.Mode&0777 == mode
}

func validateNativeConfigurationDirectoryFD(fd int, uid uint32, gid *uint32) error {
	if fd < 0 {
		return errors.New("invalid directory")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uid {
		return errors.New("invalid directory")
	}
	if gid == nil {
		if stat.Mode&0022 != 0 {
			return errors.New("writable directory")
		}
		return nil
	}
	if stat.Gid != *gid || stat.Mode&0777 != 0750 {
		return errors.New("unprotected directory")
	}
	return nil
}

func nativeConfigurationDirectoryNames(fd int) ([]string, error) {
	independent, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(independent), "native-configuration-directory")
	defer file.Close()
	return file.Readdirnames(-1)
}
