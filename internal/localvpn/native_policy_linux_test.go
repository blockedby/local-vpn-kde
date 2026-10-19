//go:build linux

package localvpn

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNativePolicyFailedRuleDownloadRestoresPreviousPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	current := "strict"
	starts := 0
	err := applyNativePolicy(ctx, []byte("strict"), []byte("smart"), true,
		func(ctx context.Context, b []byte) error {
			if ctx.Err() != nil {
				t.Fatal("rollback inherited canceled UI context")
			}
			current = string(b)
			calls = append(calls, "publish:"+current)
			return nil
		},
		func(context.Context) error { calls = append(calls, "stop"); return nil },
		func(context.Context) error {
			starts++
			calls = append(calls, "start:"+current)
			if starts == 1 {
				cancel()
				return errors.New("rule download failed")
			}
			return nil
		})
	if err == nil || current != "strict" {
		t.Fatalf("failure did not restore policy: %v %s", err, current)
	}
	want := []string{"publish:smart", "start:smart", "stop", "publish:strict", "start:strict"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestNativePolicyRollbackFailureIsReported(t *testing.T) {
	failure := errors.New("recovery unavailable")
	starts := 0
	err := applyNativePolicy(context.Background(), []byte("strict"), []byte("smart"), true,
		func(context.Context, []byte) error { return nil }, func(context.Context) error { return nil },
		func(context.Context) error {
			starts++
			if starts == 1 {
				return errors.New("candidate failed")
			}
			return failure
		})
	if !errors.Is(err, failure) {
		t.Fatal("rollback failure swallowed")
	}
}

func TestNativePolicyStoppedServiceStaysStopped(t *testing.T) {
	for _, pair := range [][2]string{{"strict", "smart"}, {"smart", "strict"}} {
		var actual string
		unexpected := func(context.Context) error { t.Fatal("stopped gateway was started"); return nil }
		if err := applyNativePolicy(context.Background(), []byte(pair[0]), []byte(pair[1]), false,
			func(_ context.Context, b []byte) error { actual = string(b); return nil }, unexpected, unexpected); err != nil || actual != pair[1] {
			t.Fatal(err)
		}
	}
}
