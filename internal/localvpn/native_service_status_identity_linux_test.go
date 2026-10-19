//go:build linux

package localvpn

import (
	"context"
	"testing"
)

func TestNativeServiceStatusRejectsForeignUnitContents(t *testing.T) {
	service, _ := newNativeServiceTestFixture(t, nativeServiceReadyTestStates())
	service.readUnit = func(string) ([]byte, error) { return []byte("[Service]\nExecStart=/foreign\n"), nil }
	status, err := service.Status(context.Background())
	if err == nil || status.Ready {
		t.Fatal("foreign unit content was reported ready")
	}
}
