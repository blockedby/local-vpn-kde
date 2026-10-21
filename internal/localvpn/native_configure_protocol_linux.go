//go:build linux

package localvpn

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"time"
)

const (
	nativeConfigurationFrameLimit = 32 << 10
	nativeProtocolIOTimeout       = 5 * time.Second
	nativeProtocolReadyTimeout    = 10 * time.Minute
	nativeProtocolAckTimeout      = time.Minute
	nativeProtocolCloseGrace      = 100 * time.Millisecond
)

var (
	nativeReadyFrame = []byte("{\"status\":\"READY_FOR_DESKTOP\"}\n")
	nativeAckFrame   = []byte("{\"ack\":\"DESKTOP_COMPLETE\"}\n")
)

type nativeConfigurationProtocol struct {
	input  io.Reader
	reader *bufio.Reader
	output io.Writer
}

func newNativeConfigurationProtocol(input io.Reader, output io.Writer) *nativeConfigurationProtocol {
	return &nativeConfigurationProtocol{input: input, reader: bufio.NewReaderSize(input, 4096), output: output}
}

func (p *nativeConfigurationProtocol) readRequest(ctx context.Context) (nativeConfigurationRequest, error) {
	line, err := p.readFrame(ctx, nativeProtocolIOTimeout)
	if err != nil {
		return nativeConfigurationRequest{}, errors.New("invalid native configuration request")
	}
	return readNativeConfigurationRequest(bytes.NewReader(line))
}

// watchAcknowledgement is the single owner of protocol input after the request.
// It intentionally uses the root operation deadline rather than the final ACK
// timeout: the authenticated desktop user may queue its ACK before READY while
// long-running generation and selection work is still in progress.
func (p *nativeConfigurationProtocol) watchAcknowledgement(ctx context.Context, cancel context.CancelFunc) <-chan error {
	result := make(chan error, 1)
	go func() {
		line, err := p.readFrame(ctx, nativeProtocolReadyTimeout)
		if err == nil && !bytes.Equal(line, nativeAckFrame) {
			err = errors.New("invalid native configuration acknowledgement")
		}
		if err != nil {
			cancel()
		}
		result <- err
	}()
	return result
}

func (p *nativeConfigurationProtocol) readyAndWait(ctx context.Context, acknowledgement <-chan error) error {
	// Prefer a queued protocol failure or cancellation over emitting READY.
	select {
	case err := <-acknowledgement:
		if err != nil {
			return errors.New("native desktop finalization was not acknowledged")
		}
		acknowledgement = nil
	default:
		if ctx.Err() != nil {
			return errors.New("native desktop finalization was not acknowledged")
		}
	}
	if err := p.writeFrame(ctx, nativeReadyFrame, nativeProtocolIOTimeout); err != nil {
		return errors.New("native desktop supervisor unavailable")
	}
	if acknowledgement == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, nativeProtocolAckTimeout)
	defer cancel()
	select {
	case err := <-acknowledgement:
		if err != nil {
			return errors.New("native desktop finalization was not acknowledged")
		}
		return nil
	case <-ctx.Done():
		return errors.New("native desktop finalization was not acknowledged")
	}
}

func (p *nativeConfigurationProtocol) waitReady(ctx context.Context) error {
	line, err := p.readFrame(ctx, nativeProtocolReadyTimeout)
	if err != nil || !bytes.Equal(line, nativeReadyFrame) {
		return errors.New("native configuration helper did not become ready")
	}
	return nil
}

func (p *nativeConfigurationProtocol) acknowledge(ctx context.Context) error {
	if err := p.writeFrame(ctx, nativeAckFrame, nativeProtocolIOTimeout); err != nil {
		return errors.New("native configuration acknowledgement failed")
	}
	return nil
}

func (p *nativeConfigurationProtocol) readFrame(parent context.Context, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readNativeProtocolFrame(p.reader)
		done <- result{line: line, err: err}
	}()
	select {
	case result := <-done:
		return result.line, result.err
	case <-ctx.Done():
		// Closing usually releases command pipes, but inherited stdio and arbitrary
		// closers are not guaranteed to unblock. Never make protocol teardown depend
		// on either Close or the pending I/O completing.
		if closer, ok := p.input.(io.Closer); ok {
			go func() { _ = closer.Close() }()
		}
		grace := time.NewTimer(nativeProtocolCloseGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
		return nil, ctx.Err()
	}
}

func readNativeProtocolFrame(reader *bufio.Reader) ([]byte, error) {
	frame := make([]byte, 0, 256)
	for {
		part, err := reader.ReadSlice('\n')
		if len(frame)+len(part) > nativeConfigurationFrameLimit+1 {
			return nil, errors.New("native configuration frame too large")
		}
		frame = append(frame, part...)
		if err == nil {
			return frame, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func (p *nativeConfigurationProtocol) writeFrame(parent context.Context, frame []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		remaining := frame
		for len(remaining) != 0 {
			n, err := p.output.Write(remaining)
			if err != nil {
				done <- err
				return
			}
			if n <= 0 {
				done <- io.ErrShortWrite
				return
			}
			remaining = remaining[n:]
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if closer, ok := p.output.(io.Closer); ok {
			go func() { _ = closer.Close() }()
		}
		grace := time.NewTimer(nativeProtocolCloseGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
		return ctx.Err()
	}
}
