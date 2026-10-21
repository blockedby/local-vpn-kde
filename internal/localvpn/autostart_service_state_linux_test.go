//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type autostartServiceStateCall struct {
	name string
	args []string
}

func autostartServiceStateFixture(output []byte, callErr error) (Autostart, *[]autostartServiceStateCall) {
	calls := []autostartServiceStateCall{}
	a := Autostart{
		Base: "/does/not/exist",
		command: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, autostartServiceStateCall{name: name, args: append([]string(nil), args...)})
			return append([]byte(nil), output...), callErr
		},
	}
	return a, &calls
}

func TestAutostartServiceStateReportsActiveButDisabled(t *testing.T) {
	a, calls := autostartServiceStateFixture([]byte("LoadState=loaded\nUnitFileState=disabled\nActiveState=active\n"), nil)
	got, err := a.ServiceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (AutostartServiceState{Enabled: "disabled", Active: "active"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %#v", *calls)
	}
}

func TestAutostartServiceStateReportsEnabledButFailed(t *testing.T) {
	a, _ := autostartServiceStateFixture([]byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=failed\n"), nil)
	got, err := a.ServiceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (AutostartServiceState{Enabled: "enabled", Active: "failed"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestAutostartServiceStateReportsMissingUnitWithoutMutation(t *testing.T) {
	a, calls := autostartServiceStateFixture([]byte("LoadState=not-found\nUnitFileState=not-found\nActiveState=inactive\n"), errors.New("systemctl exited with status 4"))
	got, err := a.ServiceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (AutostartServiceState{Enabled: "not-installed", Active: "inactive"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	if len(*calls) != 1 || (*calls)[0].args[1] != "show" {
		t.Fatalf("calls = %#v", *calls)
	}
}

func TestAutostartServiceStateBusUnavailableIsGeneric(t *testing.T) {
	secret := "private-bus-diagnostic"
	a, _ := autostartServiceStateFixture(nil, errors.New("Failed to connect to bus: "+secret))
	got, err := a.ServiceState(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("state = %#v, err = %v", got, err)
	}
	if got != (AutostartServiceState{}) {
		t.Fatalf("state = %#v", got)
	}
}

func TestAutostartServiceStateRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{name: "missing property", data: "LoadState=loaded\nActiveState=inactive\n"},
		{name: "duplicate property", data: "LoadState=loaded\nLoadState=loaded\nUnitFileState=disabled\nActiveState=inactive\n"},
		{name: "extra property", data: "LoadState=loaded\nUnitFileState=disabled\nActiveState=inactive\nDescription=secret\n"},
		{name: "trailing garbage", data: "LoadState=loaded\nUnitFileState=disabled\nActiveState=inactive\ntrailing\n"},
		{name: "invalid enabled enum", data: "LoadState=loaded\nUnitFileState=custom-state\nActiveState=inactive\n"},
		{name: "invalid active enum", data: "LoadState=loaded\nUnitFileState=disabled\nActiveState=custom-state\n"},
		{name: "carriage return", data: "LoadState=loaded\r\nUnitFileState=disabled\nActiveState=inactive\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := autostartServiceStateFixture([]byte(tc.data), nil)
			got, err := a.ServiceState(context.Background())
			if err == nil || got != (AutostartServiceState{}) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("state = %#v, err = %v", got, err)
			}
		})
	}
}

func TestAutostartServiceStateRejectsOversizedResponse(t *testing.T) {
	secret := []byte("private-output-marker")
	data := bytes.Repeat(secret, (autostartServiceStateMaxOutput/len(secret))+1)
	a, _ := autostartServiceStateFixture(data, nil)
	got, err := a.ServiceState(context.Background())
	if err == nil || got != (AutostartServiceState{}) || strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("state = %#v, err = %v", got, err)
	}
}

func TestAutostartServiceStateDeadlineIsBounded(t *testing.T) {
	a := Autostart{command: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := a.ServiceState(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("state = %#v, err = %v", got, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("state query was not bounded: %s", elapsed)
	}
}

func TestAutostartServiceStateUsesFixedReadOnlyCommand(t *testing.T) {
	data := []byte("LoadState=loaded\nUnitFileState=static\nActiveState=activating\n")
	a, calls := autostartServiceStateFixture(data, nil)
	if _, err := a.ServiceState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %#v", *calls)
	}
	want := []string{"--user", "show", autostartUnit, "--property=LoadState,UnitFileState,ActiveState"}
	if (*calls)[0].name != "systemctl" || !reflect.DeepEqual((*calls)[0].args, want) {
		t.Fatalf("call = %#v, want systemctl %v", (*calls)[0], want)
	}
	for _, arg := range (*calls)[0].args {
		if arg == "enable" || arg == "start" || arg == "reload" || arg == "daemon-reload" {
			t.Fatalf("read-only query issued mutation argument: %#v", (*calls)[0])
		}
	}
}
