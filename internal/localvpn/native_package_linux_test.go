//go:build linux

package localvpn

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNativePackageCleanupOrderAndFailClosed(t *testing.T) {
	for _, failService := range []bool{false, true} {
		var calls []string
		err := prepareNativePackageRemoval(context.Background(), func(context.Context) error {
			calls = append(calls, "service")
			if failService {
				return errors.New("private detail")
			}
			return nil
		}, func(context.Context) error { calls = append(calls, "network"); return nil })
		want := []string{"service", "network"}
		if failService {
			want = []string{"service"}
		}
		if !reflect.DeepEqual(calls, want) || (err != nil) != failService {
			t.Fatalf("calls %v err %v", calls, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unexpected := func(context.Context) error { t.Fatal("cancelled transaction mutated state"); return nil }
	if err := prepareNativePackageRemoval(ctx, unexpected, unexpected); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
