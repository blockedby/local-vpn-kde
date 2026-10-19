//go:build linux

package localvpn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeConfigurationGeneratedContractsAgree(t *testing.T) {
	assets, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generateNativeConfigurationAt(context.Background(), "https://example.invalid/subscription", 1000, assets, t.TempDir())
	if err != nil {
		t.Fatal("generation failed:", err)
	}
	if _, _, err := prepareNativeConfigurationBundle(generated.Bundle, 987); err != nil {
		t.Fatal("publication contract rejected generated bundle:", err)
	}
	if err := validateNativeClientProfile(generated.ClientProfile); err != nil {
		t.Fatal("delivery contract rejected generated profile:", err)
	}
}

func TestNativeConfigurationRequestBoundaries(t *testing.T) {
	for _, input := range []string{`{}`, `{"subscription":"secret","subscription":"other","state_path":"/private"}`, `{"Subscription":"secret","state_path":"/private"}`, `{"subscription":"secret","state_path":"/private","owner_uid":1}`, `{"subscription":"secret","state_path":"/private"}{}`, strings.Repeat("x", (32<<10)+1)} {
		if _, err := readNativeConfigurationRequest(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid request accepted or exposed")
		}
	}
	if _, err := readNativeConfigurationRequest(strings.NewReader(`{"subscription":"https://example.invalid/x","state_path":"/private/local-vpn-kde"}`)); err != nil {
		t.Fatal(err)
	}
}

type nativeConfigurationTestProfileTransaction struct {
	calls                  *[]string
	commitErr, rollbackErr error
}

func (t *nativeConfigurationTestProfileTransaction) Commit() error {
	*t.calls = append(*t.calls, "profile-commit")
	return t.commitErr
}
func (t *nativeConfigurationTestProfileTransaction) Rollback() error {
	*t.calls = append(*t.calls, "profile-rollback")
	return t.rollbackErr
}

func TestNativeConfigurationSequence(t *testing.T) {
	for _, fail := range []string{"", "inactive", "generate", "profile", "publish", "select", "enroll", "desktop"} {
		t.Run(fail, func(t *testing.T) {
			var calls []string
			call := func(stage string) error {
				calls = append(calls, stage)
				if fail == stage {
					return errors.New("private-secret-detail")
				}
				return nil
			}
			operations := nativeConfigurationOperations{
				inactive: func(context.Context) error { return call("inactive") },
				generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
					return NativeGeneratedConfiguration{Bundle: NativeConfigurationBundle{OwnerUID: 1000}}, call("generate")
				},
				profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
					if err := call("profile"); err != nil {
						return nil, err
					}
					return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
				},
				publish:         func(context.Context, NativeConfigurationBundle) error { return call("publish") },
				selectServer:    func(context.Context) error { return call("select") },
				enroll:          func(context.Context) error { return call("enroll") },
				desktop:         func(context.Context) error { return call("desktop") },
				setupIncomplete: func() (bool, error) { calls = append(calls, "inhibitor-read"); return false, nil },
				setSetupIncomplete: func(value bool) error {
					if value {
						calls = append(calls, "inhibitor-set")
					} else {
						calls = append(calls, "inhibitor-clear")
					}
					return nil
				},
			}
			err := configureNativeInstallation(context.Background(), nativeConfigurationRequest{}, 1000, 1000, operations)
			if (err != nil) != (fail != "") {
				t.Fatalf("calls=%v error=%v", calls, err)
			}
			if len(calls) == 0 || calls[0] != "inactive" {
				t.Fatalf("unexpected initial calls: %v", calls)
			}
			if fail == "publish" && !reflect.DeepEqual(calls[len(calls)-2:], []string{"profile-rollback", "inhibitor-clear"}) {
				t.Fatalf("publication failure did not compensate: %v", calls)
			}
			if fail == "" && !reflect.DeepEqual(calls[len(calls)-5:], []string{"profile-commit", "select", "enroll", "desktop", "inhibitor-clear"}) {
				t.Fatalf("successful setup ordering is wrong: %v", calls)
			}
			if fail == "desktop" && calls[len(calls)-1] != "desktop" {
				t.Fatalf("desktop failure cleared inhibitor: %v", calls)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("private error escaped")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := configureNativeInstallation(ctx, nativeConfigurationRequest{}, 1000, 1000, nativeConfigurationOperations{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled request continued")
	}
}

func TestNativeConfigurationCommittedPublicationRetainsProfileAndInhibitor(t *testing.T) {
	var calls []string
	tx := &nativeConfigurationTestProfileTransaction{calls: &calls}
	operations := nativeConfigurationOperations{
		inactive: func(context.Context) error { return nil },
		generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
			return NativeGeneratedConfiguration{}, nil
		},
		profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
			return tx, nil
		},
		publish: func(context.Context, NativeConfigurationBundle) error {
			return nativeConfigurationCommittedError("committed")
		},
		setupIncomplete: func() (bool, error) { return false, nil },
		setSetupIncomplete: func(value bool) error {
			calls = append(calls, "inhibitor="+map[bool]string{true: "true", false: "false"}[value])
			return nil
		},
	}
	if err := configureNativeInstallation(context.Background(), nativeConfigurationRequest{}, 1000, 1000, operations); err == nil {
		t.Fatal("committed publication error was hidden")
	}
	if !reflect.DeepEqual(calls, []string{"inhibitor=true", "profile-commit"}) {
		t.Fatalf("new profile was not retained safely: %v", calls)
	}
}

func TestNativeConfigurationCancellationAfterStagingRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls []string
	operations := nativeConfigurationOperations{
		inactive: func(context.Context) error { return nil },
		generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
			return NativeGeneratedConfiguration{}, nil
		},
		profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
			return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
		},
		publish:         func(context.Context, NativeConfigurationBundle) error { cancel(); return context.Canceled },
		setupIncomplete: func() (bool, error) { return false, nil },
		setSetupIncomplete: func(value bool) error {
			calls = append(calls, "inhibitor="+map[bool]string{true: "true", false: "false"}[value])
			return nil
		},
	}
	if err := configureNativeInstallation(ctx, nativeConfigurationRequest{}, 1000, 1000, operations); err == nil {
		t.Fatal("cancellation was hidden")
	}
	if !reflect.DeepEqual(calls, []string{"inhibitor=true", "profile-rollback", "inhibitor=false"}) {
		t.Fatalf("cancellation did not compensate: %v", calls)
	}
}

func TestNativeConfigurationPriorInhibitorSurvivesFullRollback(t *testing.T) {
	var calls []string
	operations := nativeConfigurationOperations{
		inactive: func(context.Context) error { return nil },
		generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
			return NativeGeneratedConfiguration{}, nil
		},
		profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
			return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
		},
		publish:         func(context.Context, NativeConfigurationBundle) error { return errors.New("failed") },
		setupIncomplete: func() (bool, error) { return true, nil },
		setSetupIncomplete: func(value bool) error {
			calls = append(calls, "inhibitor="+map[bool]string{true: "true", false: "false"}[value])
			return nil
		},
	}
	if err := configureNativeInstallation(context.Background(), nativeConfigurationRequest{}, 1000, 1000, operations); err == nil {
		t.Fatal("publication failure was hidden")
	}
	if !reflect.DeepEqual(calls, []string{"inhibitor=true", "profile-rollback"}) {
		t.Fatalf("prior inhibitor was cleared: %v", calls)
	}
}

func TestNativeConfigurationDesktopAckControlsInhibitorClear(t *testing.T) {
	for _, test := range []struct {
		name string
		ack  bool
	}{
		{name: "ack clears inhibitor", ack: true},
		{name: "eof retains inhibitor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootInput, clientOutput := io.Pipe()
			clientInput, rootOutput := io.Pipe()
			defer rootInput.Close()
			defer clientInput.Close()
			protocol := newNativeConfigurationProtocol(rootInput, rootOutput)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			acknowledgement := protocol.watchAcknowledgement(ctx, cancel)
			var calls []string
			operations := nativeConfigurationOperations{
				inactive: func(context.Context) error { return nil },
				generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
					return NativeGeneratedConfiguration{}, nil
				},
				profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
					return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
				},
				publish:      func(context.Context, NativeConfigurationBundle) error { return nil },
				selectServer: func(context.Context) error { return nil },
				enroll:       func(context.Context) error { return nil },
				desktop: func(ctx context.Context) error {
					return protocol.readyAndWait(ctx, acknowledgement)
				},
				setupIncomplete: func() (bool, error) { return false, nil },
				setSetupIncomplete: func(value bool) error {
					if value {
						calls = append(calls, "inhibitor-set")
					} else {
						calls = append(calls, "inhibitor-clear")
					}
					return nil
				},
			}
			result := make(chan error, 1)
			go func() {
				result <- configureNativeInstallation(ctx, nativeConfigurationRequest{}, 1000, 1000, operations)
			}()
			line, err := bufio.NewReader(clientInput).ReadString('\n')
			if err != nil || line != string(nativeReadyFrame) {
				t.Fatalf("ready frame=%q error=%v", line, err)
			}
			if strings.Contains(strings.Join(calls, ","), "inhibitor-clear") {
				t.Fatalf("inhibitor cleared before ack: %v", calls)
			}
			if test.ack {
				if _, err = clientOutput.Write(nativeAckFrame); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = clientOutput.Close()
			}
			select {
			case err = <-result:
			case <-time.After(time.Second):
				t.Fatal("coordinator did not finish")
			}
			cleared := strings.Contains(strings.Join(calls, ","), "inhibitor-clear")
			if test.ack && (err != nil || !cleared) {
				t.Fatalf("ack did not permit clear: calls=%v err=%v", calls, err)
			}
			if !test.ack && (err == nil || cleared) {
				t.Fatalf("EOF did not retain inhibitor: calls=%v err=%v", calls, err)
			}
		})
	}
}

func readNativeTestProtocolRequest(t *testing.T, protocol *nativeConfigurationProtocol, clientOutput io.Writer) nativeConfigurationRequest {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(clientOutput, `{"subscription":"https://example.invalid/subscription","state_path":"/private/state"}`+"\n")
		written <- err
	}()
	request, err := protocol.readRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = <-written; err != nil {
		t.Fatal(err)
	}
	return request
}

func TestNativeConfigurationProtocolDisconnectCancelsRootWork(t *testing.T) {
	for _, test := range []struct {
		name, stage, frame string
	}{
		{name: "EOF during generation", stage: "generation"},
		{name: "EOF during benchmark", stage: "benchmark"},
		{name: "malformed frame during generation", stage: "generation", frame: "{\"ack\":\"WRONG\"}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootInput, clientOutput := io.Pipe()
			clientInput, rootOutput := io.Pipe()
			defer rootInput.Close()
			defer clientOutput.Close()
			defer clientInput.Close()
			protocol := newNativeConfigurationProtocol(rootInput, rootOutput)
			request := readNativeTestProtocolRequest(t, protocol, clientOutput)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			acknowledgement := protocol.watchAcknowledgement(ctx, cancel)
			started := make(chan struct{})
			var calls []string
			operations := nativeConfigurationOperations{
				inactive: func(context.Context) error { return nil },
				generate: func(ctx context.Context, _ string, _ uint32) (NativeGeneratedConfiguration, error) {
					if test.stage == "generation" {
						close(started)
						<-ctx.Done()
						return NativeGeneratedConfiguration{}, ctx.Err()
					}
					return NativeGeneratedConfiguration{}, nil
				},
				profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
					return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
				},
				publish: func(context.Context, NativeConfigurationBundle) error { return nil },
				selectServer: func(ctx context.Context) error {
					close(started)
					<-ctx.Done()
					return ctx.Err()
				},
				enroll: func(context.Context) error { return nil },
				desktop: func(ctx context.Context) error {
					return protocol.readyAndWait(ctx, acknowledgement)
				},
				setupIncomplete: func() (bool, error) { return false, nil },
				setSetupIncomplete: func(value bool) error {
					if value {
						calls = append(calls, "inhibitor-set")
					} else {
						calls = append(calls, "inhibitor-clear")
					}
					return nil
				},
			}
			result := make(chan error, 1)
			go func() { result <- configureNativeInstallation(ctx, request, 1000, 1000, operations) }()
			<-started
			closedAt := time.Now()
			if test.frame == "" {
				_ = clientOutput.Close()
			} else if _, err := io.WriteString(clientOutput, test.frame); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("desktop EOF did not fail root work")
				}
			case <-time.After(time.Second):
				t.Fatal("desktop EOF did not promptly cancel root work")
			}
			if elapsed := time.Since(closedAt); elapsed > time.Second {
				t.Fatalf("root cancellation was not prompt: %v", elapsed)
			}
			if strings.Contains(strings.Join(calls, ","), "inhibitor-clear") {
				t.Fatalf("disconnect cleared inhibitor: %v", calls)
			}
		})
	}
}

func TestNativeConfigurationProtocolRequestReadyAndAckSucceeds(t *testing.T) {
	rootInput, clientOutput := io.Pipe()
	clientInput, rootOutput := io.Pipe()
	defer rootInput.Close()
	defer clientOutput.Close()
	defer clientInput.Close()
	protocol := newNativeConfigurationProtocol(rootInput, rootOutput)
	request := readNativeTestProtocolRequest(t, protocol, clientOutput)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	acknowledgement := protocol.watchAcknowledgement(ctx, cancel)
	var calls []string
	operations := nativeConfigurationOperations{
		inactive: func(context.Context) error { return nil },
		generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
			return NativeGeneratedConfiguration{}, nil
		},
		profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
			return &nativeConfigurationTestProfileTransaction{calls: &calls}, nil
		},
		publish:      func(context.Context, NativeConfigurationBundle) error { return nil },
		selectServer: func(context.Context) error { return nil },
		enroll:       func(context.Context) error { return nil },
		desktop: func(ctx context.Context) error {
			return protocol.readyAndWait(ctx, acknowledgement)
		},
		setupIncomplete: func() (bool, error) { return false, nil },
		setSetupIncomplete: func(value bool) error {
			if value {
				calls = append(calls, "inhibitor-set")
			} else {
				calls = append(calls, "inhibitor-clear")
			}
			return nil
		},
	}
	result := make(chan error, 1)
	go func() { result <- configureNativeInstallation(ctx, request, 1000, 1000, operations) }()
	line, err := bufio.NewReader(clientInput).ReadString('\n')
	if err != nil || line != string(nativeReadyFrame) {
		t.Fatalf("ready frame=%q error=%v", line, err)
	}
	if _, err = clientOutput.Write(nativeAckFrame); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("acknowledged protocol did not finish")
	}
	if !strings.Contains(strings.Join(calls, ","), "inhibitor-clear") {
		t.Fatalf("successful protocol retained inhibitor: %v", calls)
	}
}

type nativeBlockingProtocolWriter struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *nativeBlockingProtocolWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *nativeBlockingProtocolWriter) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
}

func TestNativeConfigurationProtocolRejectsUnboundedOrCancelledFrames(t *testing.T) {
	oversized := strings.Repeat("x", nativeConfigurationFrameLimit+1) + "\n"
	protocol := newNativeConfigurationProtocol(strings.NewReader(oversized), io.Discard)
	if _, err := protocol.readRequest(context.Background()); err == nil {
		t.Fatal("oversized initial request accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	protocol = newNativeConfigurationProtocol(strings.NewReader(""), io.Discard)
	if _, err := protocol.readRequest(ctx); err == nil {
		t.Fatal("cancelled initial read accepted")
	}

	input, output := io.Pipe()
	defer input.Close()
	protocol = newNativeConfigurationProtocol(input, io.Discard)
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := protocol.readyAndWait(ctx, make(chan error)); err == nil {
		t.Fatal("cancelled final wait accepted")
	}
	_ = output.Close()

	writer := &nativeBlockingProtocolWriter{started: make(chan struct{}), closed: make(chan struct{})}
	protocol = newNativeConfigurationProtocol(strings.NewReader(""), writer)
	ctx, cancel = context.WithCancel(context.Background())
	writeDone := make(chan error, 1)
	go func() { writeDone <- protocol.acknowledge(ctx) }()
	<-writer.started
	cancel()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("cancelled blocked write accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled blocked write leaked")
	}
}

func TestNativeConfigurationRollbackUncertaintyRetainsInhibitor(t *testing.T) {
	var calls []string
	operations := nativeConfigurationOperations{
		inactive: func(context.Context) error { return nil },
		generate: func(context.Context, string, uint32) (NativeGeneratedConfiguration, error) {
			return NativeGeneratedConfiguration{}, nil
		},
		profile: func(context.Context, uint32, uint32, string, []byte) (nativeProfileTransaction, error) {
			return &nativeConfigurationTestProfileTransaction{calls: &calls, rollbackErr: errors.New("changed")}, nil
		},
		publish:         func(context.Context, NativeConfigurationBundle) error { return errors.New("failed") },
		setupIncomplete: func() (bool, error) { return false, nil },
		setSetupIncomplete: func(value bool) error {
			calls = append(calls, "inhibitor="+map[bool]string{true: "true", false: "false"}[value])
			return nil
		},
	}
	if err := configureNativeInstallation(context.Background(), nativeConfigurationRequest{}, 1000, 1000, operations); err == nil || strings.Contains(strings.Join(calls, ","), "inhibitor=false") {
		t.Fatalf("rollback uncertainty did not retain inhibitor: calls=%v err=%v", calls, err)
	}
}
