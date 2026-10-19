//go:build linux

package localvpn

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type nativeStubbornProtocolReader struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (r *nativeStubbornProtocolReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	close(r.finished)
	return 0, io.EOF
}

// Close deliberately does not release Read.
func (r *nativeStubbornProtocolReader) Close() error { return nil }

type nativeStubbornProtocolWriter struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (w *nativeStubbornProtocolWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	close(w.finished)
	return 0, io.ErrClosedPipe
}

// Close deliberately does not release Write.
func (w *nativeStubbornProtocolWriter) Close() error { return nil }

func TestNativeProtocolTimeoutDoesNotDependOnCloseUnblockingIO(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		reader := &nativeStubbornProtocolReader{
			started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
		}
		protocol := newNativeConfigurationProtocol(reader, io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := protocol.readFrame(ctx, time.Minute)
			result <- err
		}()
		<-reader.started
		cancel()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("cancelled read succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("read teardown exceeded its bounded grace")
		}
		close(reader.release)
		select {
		case <-reader.finished:
		case <-time.After(time.Second):
			t.Fatal("released read goroutine did not finish")
		}
	})

	t.Run("write", func(t *testing.T) {
		writer := &nativeStubbornProtocolWriter{
			started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
		}
		protocol := newNativeConfigurationProtocol(nil, writer)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- protocol.writeFrame(ctx, []byte("frame\n"), time.Minute) }()
		<-writer.started
		cancel()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("cancelled write succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("write teardown exceeded its bounded grace")
		}
		close(writer.release)
		select {
		case <-writer.finished:
		case <-time.After(time.Second):
			t.Fatal("released write goroutine did not finish")
		}
	})
}
