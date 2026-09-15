package localvpn

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// validateSecretTree is a read-only preflight. Actual reads and writes still
// use held no-follow directory descriptors; this scan does not replace them.
func validateSecretTree(base string, outputLink func(string) bool) error {
	fd, err := directory(base, false, true)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	remaining := 100000
	var walk func(int, string, int) error
	walk = func(fd int, prefix string, depth int) error {
		file := os.NewFile(uintptr(fd), "private-directory")
		defer file.Close()
		if depth > 128 {
			return errors.New("secret tree is too deep")
		}
		for {
			names, err := file.Readdirnames(128)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, name := range names {
				remaining--
				if remaining < 0 || !component(name) {
					return errors.New("secret tree is too large or invalid")
				}
				relative := filepath.Join(prefix, name)
				var stat unix.Stat_t
				if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return err
				}
				switch stat.Mode & unix.S_IFMT {
				case unix.S_IFDIR:
					child, err := childDirectory(fd, name, false, true)
					if err != nil {
						return err
					}
					if err = walk(child, relative, depth+1); err != nil {
						return err
					}
				case unix.S_IFREG:
					if stat.Nlink != 1 {
						return errors.New("secret tree files must not be hard-linked")
					}
				case unix.S_IFLNK:
					if outputLink == nil || !outputLink(relative) {
						return errors.New("secret tree must not contain symlink entries")
					}
				default:
					return errors.New("secret tree contains a non-regular entry")
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
		}
	}
	return walk(fd, "", 0)
}

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
