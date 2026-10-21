//go:build linux

package nativegateway

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// VerifyNamespace fails closed unless this process has already been placed in
// the installed native network namespace. It never enters a namespace itself.
// This check must precede any gateway routing or child-process startup.
func VerifyNamespace() error {
	target, targetStat, err := openNamespaceTarget()
	if err != nil {
		return err
	}
	defer unix.Close(target)

	current, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return errors.New("current namespace identity unavailable")
	}
	defer current.Close()
	var currentStat unix.Stat_t
	if err := unix.Fstat(int(current.Fd()), &currentStat); err != nil {
		return errors.New("current namespace identity unavailable")
	}

	if os.Geteuid() == 0 {
		initial, err := os.Open("/proc/1/ns/net")
		if err != nil {
			return errors.New("initial namespace identity unavailable")
		}
		defer initial.Close()
		var initialStat unix.Stat_t
		if err := unix.Fstat(int(initial.Fd()), &initialStat); err != nil {
			return errors.New("initial namespace identity unavailable")
		}
		return verifyIdentity(targetStat, currentStat, initialStat)
	}

	proof, err := readNamespaceIdentityProof(productionNamespaceIdentityLocation())
	if err != nil {
		return err
	}
	return verifyIdentityProof(targetStat, currentStat, proof)
}

// PublishNamespaceIdentity publishes the root-issued namespace proof consumed
// by a restricted gateway service. The root namespace initializer must call it
// only after the fixed namespace sysctls have been applied.
func PublishNamespaceIdentity() error {
	if os.Geteuid() != 0 {
		return errors.New("publishing namespace identity requires root")
	}
	target, targetStat, err := openNamespaceTarget()
	if err != nil {
		return err
	}
	defer unix.Close(target)

	current, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return errors.New("current namespace identity unavailable")
	}
	defer current.Close()
	initial, err := os.Open("/proc/1/ns/net")
	if err != nil {
		return errors.New("initial namespace identity unavailable")
	}
	defer initial.Close()

	var currentStat, initialStat unix.Stat_t
	if unix.Fstat(int(current.Fd()), &currentStat) != nil || unix.Fstat(int(initial.Fd()), &initialStat) != nil {
		return errors.New("network namespace identity unavailable")
	}
	if err := verifyIdentity(targetStat, currentStat, initialStat); err != nil {
		return err
	}
	return publishNamespaceIdentityProof(productionNamespaceIdentityLocation(), identityFromStats(targetStat, initialStat))
}

func openNamespaceTarget() (int, unix.Stat_t, error) {
	target, err := unix.Open("/run/netns/"+Namespace, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, errors.New("native network namespace unavailable")
	}
	var targetStat unix.Stat_t
	if err := unix.Fstat(target, &targetStat); err != nil {
		unix.Close(target)
		return -1, unix.Stat_t{}, errors.New("native namespace identity unavailable")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(target, &fs); err != nil || fs.Type != unix.NSFS_MAGIC || targetStat.Uid != 0 {
		unix.Close(target)
		return -1, unix.Stat_t{}, errors.New("native namespace must be a root-owned namespace mount")
	}
	return target, targetStat, nil
}

func sameNamespace(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

func verifyIdentity(target, current, initial unix.Stat_t) error {
	if sameNamespace(current, initial) {
		return errors.New("refusing native gateway in the initial network namespace")
	}
	if !sameNamespace(current, target) {
		return fmt.Errorf("native gateway must run in namespace %s", Namespace)
	}
	return nil
}
