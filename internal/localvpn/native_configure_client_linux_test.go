//go:build linux

package localvpn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

type nativeTestConfigurationProcess struct {
	clientInput  *io.PipeWriter
	clientOutput *io.PipeReader
	helperInput  *io.PipeReader
	helperOutput *io.PipeWriter
	handler      func(io.Reader, io.Writer) error
	done         chan error
	started      chan struct{}
	waited       chan struct{}
	waitOnce     sync.Once
}

func newNativeTestConfigurationProcess(handler func(io.Reader, io.Writer) error) *nativeTestConfigurationProcess {
	helperInput, clientInput := io.Pipe()
	clientOutput, helperOutput := io.Pipe()
	return &nativeTestConfigurationProcess{
		clientInput: clientInput, clientOutput: clientOutput,
		helperInput: helperInput, helperOutput: helperOutput,
		handler: handler, done: make(chan error, 1), started: make(chan struct{}), waited: make(chan struct{}),
	}
}

func (p *nativeTestConfigurationProcess) StdinPipe() (io.WriteCloser, error) {
	return p.clientInput, nil
}
func (p *nativeTestConfigurationProcess) StdoutPipe() (io.ReadCloser, error) {
	return p.clientOutput, nil
}
func (p *nativeTestConfigurationProcess) Start() error {
	close(p.started)
	go func() {
		err := p.handler(p.helperInput, p.helperOutput)
		_ = p.helperInput.Close()
		_ = p.helperOutput.Close()
		p.done <- err
	}()
	return nil
}
func (p *nativeTestConfigurationProcess) Wait() error {
	err := <-p.done
	p.waitOnce.Do(func() { close(p.waited) })
	return err
}
func (p *nativeTestConfigurationProcess) Signal(os.Signal) error {
	_ = p.helperInput.CloseWithError(syscall.EPIPE)
	_ = p.helperOutput.CloseWithError(syscall.EPIPE)
	return nil
}
func (p *nativeTestConfigurationProcess) Kill() error { return p.Signal(os.Kill) }

func TestNativeDesktopProtocolAcknowledgesOnlyAfterFinalization(t *testing.T) {
	for _, test := range []struct {
		name        string
		finalizeErr error
		wantAck     bool
	}{
		{name: "profile import error", finalizeErr: errors.New("import failed")},
		{name: "subscription error", finalizeErr: errors.New("subscription failed")},
		{name: "success", wantAck: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ack := make(chan bool, 1)
			process := newNativeTestConfigurationProcess(func(input io.Reader, output io.Writer) error {
				reader := bufio.NewReader(input)
				if _, err := reader.ReadString('\n'); err != nil {
					ack <- false
					return err
				}
				if _, err := output.Write(nativeReadyFrame); err != nil {
					ack <- false
					return err
				}
				line, err := reader.ReadString('\n')
				ack <- err == nil && line == string(nativeAckFrame)
				return err
			})
			err := runNativeDesktopProtocol(context.Background(), process, nativeConfigurationRequest{
				Subscription: "https://example.invalid/subscription", StatePath: "/private/state",
			}, func(context.Context) error { return test.finalizeErr })
			if (err == nil) != test.wantAck {
				t.Fatalf("error=%v wantAck=%v", err, test.wantAck)
			}
			select {
			case got := <-ack:
				if got != test.wantAck {
					t.Fatalf("ack=%v want=%v", got, test.wantAck)
				}
			case <-time.After(time.Second):
				t.Fatal("helper was not released")
			}
			select {
			case <-process.waited:
			case <-time.After(time.Second):
				t.Fatal("pkexec process was not reaped")
			}
		})
	}
}

func TestNativeDesktopProtocolCancellationAfterSpawnClosesPipesAndReaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handlerDone := make(chan struct{})
	process := newNativeTestConfigurationProcess(func(input io.Reader, _ io.Writer) error {
		defer close(handlerDone)
		_, err := io.Copy(io.Discard, input)
		return err
	})
	result := make(chan error, 1)
	go func() {
		result <- runNativeDesktopProtocol(ctx, process, nativeConfigurationRequest{
			Subscription: "https://example.invalid/subscription", StatePath: "/private/state",
		}, func(context.Context) error { return nil })
	}()
	<-process.started
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancellation was ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop supervisor")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("helper goroutine leaked")
	}
	select {
	case <-process.waited:
	case <-time.After(time.Second):
		t.Fatal("pkexec process was not reaped")
	}
}

type nativeUnsignalableConfigurationProcess struct {
	release chan struct{}
	waited  chan struct{}
}

func (p *nativeUnsignalableConfigurationProcess) StdinPipe() (io.WriteCloser, error) {
	return nil, errors.New("unused")
}
func (p *nativeUnsignalableConfigurationProcess) StdoutPipe() (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (p *nativeUnsignalableConfigurationProcess) Start() error { return errors.New("unused") }
func (p *nativeUnsignalableConfigurationProcess) Wait() error {
	<-p.release
	close(p.waited)
	return nil
}
func (p *nativeUnsignalableConfigurationProcess) Signal(os.Signal) error { return syscall.EPERM }
func (p *nativeUnsignalableConfigurationProcess) Kill() error            { return syscall.EPERM }

func TestWaitNativeConfigurationProcessIsBoundedWhenSignalsFail(t *testing.T) {
	process := &nativeUnsignalableConfigurationProcess{release: make(chan struct{}), waited: make(chan struct{})}
	started := time.Now()
	err := waitNativeConfigurationProcess(context.Background(), process, 10*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("unreaped process was reported as successful")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("unsignalable process wait was not bounded: %v", elapsed)
	}
	close(process.release)
	select {
	case <-process.waited:
	case <-time.After(time.Second):
		t.Fatal("released Wait goroutine did not finish")
	}
}

func TestNativeDesktopProtocolCancellationAfterReadySendsNoAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ack := make(chan bool, 1)
	process := newNativeTestConfigurationProcess(func(input io.Reader, output io.Writer) error {
		reader := bufio.NewReader(input)
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		if _, err := output.Write(nativeReadyFrame); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		ack <- err == nil && line == string(nativeAckFrame)
		return err
	})
	err := runNativeDesktopProtocol(ctx, process, nativeConfigurationRequest{
		Subscription: "https://example.invalid/subscription", StatePath: "/private/state",
	}, func(context.Context) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("cancellation was ignored")
	}
	select {
	case got := <-ack:
		if got {
			t.Fatal("ack sent after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not observe EOF")
	}
}
