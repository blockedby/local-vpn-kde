package main

import (
	"errors"
	"reflect"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRequireNativeInitialParentRejectsAbsentParent(t *testing.T) {
	oldGetPPID, oldPrctl := nativeInitialGetPPID, nativeInitialPrctl
	t.Cleanup(func() {
		nativeInitialGetPPID, nativeInitialPrctl = oldGetPPID, oldPrctl
	})

	var calls []string
	nativeInitialGetPPID = func() int {
		calls = append(calls, "getppid")
		return 1
	}
	nativeInitialPrctl = func(int, uintptr, uintptr, uintptr, uintptr) error {
		calls = append(calls, "prctl")
		return nil
	}

	if err := requireNativeInitialParent(); err == nil {
		t.Fatal("absent parent accepted")
	}
	if !reflect.DeepEqual(calls, []string{"getppid"}) {
		t.Fatalf("parent-death signal was armed before absent-parent rejection: %v", calls)
	}
}

func TestRequireNativeInitialParentRejectsParentChange(t *testing.T) {
	oldGetPPID, oldPrctl := nativeInitialGetPPID, nativeInitialPrctl
	t.Cleanup(func() {
		nativeInitialGetPPID, nativeInitialPrctl = oldGetPPID, oldPrctl
	})

	parents := []int{24101, 24102}
	index := 0
	var calls []string
	nativeInitialGetPPID = func() int {
		calls = append(calls, "getppid")
		parent := parents[index]
		index++
		return parent
	}
	var option int
	var signal uintptr
	nativeInitialPrctl = func(gotOption int, gotSignal, _, _, _ uintptr) error {
		calls = append(calls, "prctl")
		option, signal = gotOption, gotSignal
		return nil
	}

	if err := requireNativeInitialParent(); err == nil {
		t.Fatal("parent change accepted")
	}
	if !reflect.DeepEqual(calls, []string{"getppid", "prctl", "getppid"}) {
		t.Fatalf("unexpected parent guard order: %v", calls)
	}
	if option != unix.PR_SET_PDEATHSIG || signal != uintptr(syscall.SIGTERM) {
		t.Fatalf("unexpected prctl request: option=%d signal=%d", option, signal)
	}
}

func TestRequireNativeInitialParentRejectsArmFailure(t *testing.T) {
	oldGetPPID, oldPrctl := nativeInitialGetPPID, nativeInitialPrctl
	t.Cleanup(func() {
		nativeInitialGetPPID, nativeInitialPrctl = oldGetPPID, oldPrctl
	})

	var calls []string
	armErr := errors.New("injected prctl failure")
	nativeInitialGetPPID = func() int {
		calls = append(calls, "getppid")
		return 24103
	}
	nativeInitialPrctl = func(int, uintptr, uintptr, uintptr, uintptr) error {
		calls = append(calls, "prctl")
		return armErr
	}

	err := requireNativeInitialParent()
	if !errors.Is(err, armErr) {
		t.Fatalf("arm failure was not returned: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"getppid", "prctl"}) {
		t.Fatalf("parent was rechecked after arm failure: %v", calls)
	}
}

func TestRequireNativeInitialParentSuccessArmsBeforeRecheck(t *testing.T) {
	oldGetPPID, oldPrctl := nativeInitialGetPPID, nativeInitialPrctl
	t.Cleanup(func() {
		nativeInitialGetPPID, nativeInitialPrctl = oldGetPPID, oldPrctl
	})

	var calls []string
	nativeInitialGetPPID = func() int {
		calls = append(calls, "getppid")
		return 24104
	}
	var option int
	var signal uintptr
	nativeInitialPrctl = func(gotOption int, gotSignal, _, _, _ uintptr) error {
		calls = append(calls, "prctl")
		option, signal = gotOption, gotSignal
		return nil
	}

	if err := requireNativeInitialParent(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"getppid", "prctl", "getppid"}) {
		t.Fatalf("unexpected successful parent guard order: %v", calls)
	}
	if option != unix.PR_SET_PDEATHSIG || signal != uintptr(syscall.SIGTERM) {
		t.Fatalf("unexpected prctl request: option=%d signal=%d", option, signal)
	}
}
