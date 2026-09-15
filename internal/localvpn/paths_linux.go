package localvpn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// SecretRoot validates the local-only boundary without creating directories.
func SecretRoot(repo, requested string, fixture bool) (string, error) {
	if !filepath.IsAbs(repo) || requested == "" {
		return "", errors.New("invalid local root")
	}
	if !filepath.IsAbs(requested) {
		requested = repo + "/" + requested
	}
	for _, part := range strings.Split(requested, "/") {
		if part == "." || part == ".." {
			return "", errors.New("noncanonical local root")
		}
	}
	base := filepath.Clean(requested)
	for _, part := range strings.Split(base, "/") {
		part = strings.ToLower(part)
		if strings.Contains(part, "prod") || strings.Contains(part, "live") {
			return "", errors.New("production-like root refused")
		}
	}
	within := func(root string) bool { return base == root || strings.HasPrefix(base, root+"/") }
	if base == "/tmp" || base == "/var/tmp" || base == "/tmp/vpnkit-local-" || base == "/var/tmp/vpnkit-local-" {
		return "", errors.New("secret root is too broad")
	}
	local := filepath.Join(repo, "secrets/vpnkit-local")
	labs := filepath.Join(repo, "secrets/vpnkit-labs")
	allowed := within(local) || strings.HasPrefix(base, labs+"/")
	for _, tmp := range []string{"/tmp", "/var/tmp"} {
		if strings.HasPrefix(base, tmp+"/vpnkit-local-") && base != tmp+"/vpnkit-local-" {
			allowed = true
		}
		if fixture && strings.HasPrefix(base, tmp+"/") {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("secret root must stay in local or isolated fixture tree")
	}
	// Walk the existing prefix from root, never following links.
	fd, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return "", err
	}
	defer func() { unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(base, "/"), "/")
	for i, part := range parts {
		next, e := childDirectory(fd, part, false, i == len(parts)-1)
		if errors.Is(e, unix.ENOENT) {
			return base, nil
		}
		if e != nil {
			return "", errors.New("unsafe secret root")
		}
		unix.Close(fd)
		fd = next
	}
	return base, nil
}

func privateDirectory(fd int) error {
	if err := checkDirectory(fd, true); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&0077 != 0 {
		return errors.New("directory is not private")
	}
	return nil
}

func validatePrivateFile(fd int) error {
	st, err := privateInode(fd)
	if err != nil {
		return err
	}
	if st.Mode&0077 != 0 || st.Uid != uint32(os.Getuid()) {
		return errors.New("file is not private")
	}
	return nil
}
