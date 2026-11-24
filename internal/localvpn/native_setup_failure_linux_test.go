//go:build linux

package localvpn

import (
	"bytes"
	"errors"
	"testing"
)

func TestNativeSetupFailureReportsOnlyFixedStages(t *testing.T) {
	var out bytes.Buffer
	reportNativeSetupFailure(&out, errors.New("native services must be stopped before configuration"))
	reportNativeSetupFailure(&out, errors.New("https://secret.example/sub leaked"))
	reportNativeSetupFailure(&out, nil)
	want := "native_setup_failure=services-not-stopped\nnative_setup_failure=other\n"
	if out.String() != want {
		t.Fatalf("got %q", out.String())
	}
}

func TestNativeSetupFailureFilterDropsEverythingElse(t *testing.T) {
	var out bytes.Buffer
	f := &nativeSetupFailureFilter{out: &out}
	for _, chunk := range []string{"secret https://x\nnative_setup_", "failure=publish\nnative_setup_failure=BAD url\n"} {
		if n, err := f.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatal(n, err)
		}
	}
	if out.String() != "native_setup_failure=publish\n" {
		t.Fatalf("got %q", out.String())
	}
}
