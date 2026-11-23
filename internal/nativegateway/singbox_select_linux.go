//go:build linux

package nativegateway

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	BundledSingBox = LibexecDir + "/sing-box"
	SystemSingBox  = "/usr/bin/sing-box"
)

type SingBoxChoice struct {
	Path           string // BundledSingBox or SystemSingBox
	Source         string // "bundled" or "system"
	Version        string // e.g. "1.14.4"
	BundledProblem string // "" if bundled was usable, else "missing", "cannot-run" or "unsupported-version"
}

var ErrNoUsableSingBox = errors.New("no usable sing-box: neither the bundled nor the system binary runs a supported version (1.13-1.14)")

var singBoxVersionPattern = regexp.MustCompile(`^sing-box version (\d+)\.(\d+)\.(\d+)(\S*)$`)

type singBoxVersion struct {
	parts  [3]string
	suffix string
	text   string
}

func parseSingBoxVersion(output string) (singBoxVersion, bool) {
	line, _, _ := strings.Cut(output, "\n")
	match := singBoxVersionPattern.FindStringSubmatch(line)
	if match == nil {
		return singBoxVersion{}, false
	}
	// SemVer build metadata ("+...") does not affect precedence.
	suffix, _, _ := strings.Cut(match[4], "+")
	v := singBoxVersion{suffix: suffix, text: match[1] + "." + match[2] + "." + match[3] + match[4]}
	for i := range v.parts {
		v.parts[i] = strings.TrimLeft(match[i+1], "0")
		if v.parts[i] == "" {
			v.parts[i] = "0"
		}
	}
	return v, true
}

func compareSingBoxVersions(a, b singBoxVersion) int {
	for i := range a.parts {
		if len(a.parts[i]) != len(b.parts[i]) {
			if len(a.parts[i]) < len(b.parts[i]) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.parts[i], b.parts[i]); c != 0 {
			return c
		}
	}
	if a.suffix == b.suffix {
		return 0
	}
	if a.suffix == "" {
		return 1
	}
	if b.suffix == "" {
		return -1
	}
	return compareSingBoxSuffixes(a.suffix, b.suffix)
}

func compareSingBoxSuffixes(a, b string) int {
	// Compare prerelease identifiers numerically where both are numeric.
	as, bs := strings.Split(strings.TrimPrefix(a, "-"), "."), strings.Split(strings.TrimPrefix(b, "-"), ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, bn := numericIdentifier(as[i]), numericIdentifier(bs[i])
		if an && bn {
			x, y := strings.TrimLeft(as[i], "0"), strings.TrimLeft(bs[i], "0")
			if len(x) != len(y) {
				if len(x) < len(y) {
					return -1
				}
				return 1
			}
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		} else if an != bn {
			if an {
				return -1
			}
			return 1
		} else if c := strings.Compare(as[i], bs[i]); c != 0 {
			return c
		}
	}
	if len(as) < len(bs) {
		return -1
	}
	if len(as) > len(bs) {
		return 1
	}
	return 0
}

func numericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func selectSingBox(ctx context.Context, probe func(context.Context, string) (string, error), stat func(string) error) (SingBoxChoice, error) {
	var choice SingBoxChoice
	var best singBoxVersion
	for _, candidate := range []struct{ path, source string }{{BundledSingBox, "bundled"}, {SystemSingBox, "system"}} {
		problem := ""
		var version singBoxVersion
		if err := stat(candidate.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				problem = "missing"
			} else {
				problem = "cannot-run"
			}
		} else if output, err := probe(ctx, candidate.path); err != nil {
			problem = "cannot-run"
		} else if parsed, ok := parseSingBoxVersion(output); !ok {
			problem = "cannot-run"
		} else {
			version = parsed
			min := singBoxVersion{parts: [3]string{"1", "13", "0"}}
			max := singBoxVersion{parts: [3]string{"1", "15", "0"}}
			if compareSingBoxVersions(version, min) < 0 || compareSingBoxVersions(version, max) >= 0 {
				problem = "unsupported-version"
			}
		}
		if candidate.path == BundledSingBox {
			choice.BundledProblem = problem
		}
		if problem == "" && (choice.Path == "" || compareSingBoxVersions(version, best) > 0) {
			choice.Path, choice.Source, choice.Version = candidate.path, candidate.source, version.text
			best = version
		}
	}
	if choice.Path == "" {
		return SingBoxChoice{}, ErrNoUsableSingBox
	}
	return choice, nil
}

// cappedOutput retains at most limit bytes while allowing the process to finish.
type cappedOutput struct {
	strings.Builder
	overflow bool
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	const limit = 4096
	n := len(p)
	remaining := limit - w.Len()
	if len(p) > remaining {
		w.overflow = true
		p = p[:remaining]
	}
	_, _ = w.Builder.Write(p)
	return n, nil
}

func probeSingBox(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	var output cappedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if output.overflow && !strings.Contains(output.String(), "\n") {
		return "", errors.New("sing-box version line exceeds limit")
	}
	return output.String(), nil
}

func SelectSingBox(ctx context.Context) (SingBoxChoice, error) {
	return selectSingBox(ctx, probeSingBox, trustedSingBoxFile)
}

// trustedSingBoxFile rejects a candidate before it is ever executed (the gateway
// probes as root): it must be a root-owned regular file, not a symlink, in a
// root-owned directory, with neither writable by group or others.
func trustedSingBoxFile(path string) error {
	var dir, file unix.Stat_t
	if err := unix.Lstat(filepath.Dir(path), &dir); err != nil {
		return err
	}
	if dir.Mode&unix.S_IFMT != unix.S_IFDIR || dir.Uid != 0 || dir.Mode&0o022 != 0 {
		return errUntrustedSingBox
	}
	if err := unix.Lstat(path, &file); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return os.ErrNotExist
		}
		return err
	}
	if file.Mode&unix.S_IFMT != unix.S_IFREG || file.Uid != 0 || file.Mode&0o022 != 0 {
		return errUntrustedSingBox
	}
	return nil
}

var errUntrustedSingBox = errors.New("untrusted sing-box candidate")

var (
	singBoxCacheMu     sync.Mutex
	singBoxCacheTime   time.Time
	singBoxCacheChoice SingBoxChoice
	singBoxCacheErr    error
	singBoxCacheValid  bool
	singBoxNow         = time.Now
	singBoxSelector    = SelectSingBox
)

func EffectiveSingBoxBin(configured string) string {
	if configured != BundledSingBox {
		return configured
	}
	singBoxCacheMu.Lock()
	defer singBoxCacheMu.Unlock()
	now := singBoxNow()
	if !singBoxCacheValid || now.Before(singBoxCacheTime) || now.Sub(singBoxCacheTime) >= 30*time.Second {
		singBoxCacheChoice, singBoxCacheErr = singBoxSelector(context.Background())
		singBoxCacheTime = singBoxNow()
		singBoxCacheValid = true
	}
	if singBoxCacheErr != nil {
		return configured
	}
	return singBoxCacheChoice.Path
}
