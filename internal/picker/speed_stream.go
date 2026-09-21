package picker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
)

// SpeedProgress is measured cumulative throughput, never a simulated animation.
type SpeedProgress struct {
	Event           string  `json:"event"`
	ServerID        string  `json:"server_id"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	ElapsedSeconds  float64 `json:"elapsed_seconds"`
	DownloadMbps    float64 `json:"download_mbps"`
}

// SpeedStream removes and validates progress frames before passing the final
// response to the existing bounded capture. Use once per serialized operation.
type SpeedStream struct {
	Final    io.Writer
	Progress func(SpeedProgress) error
	id       string
	previous SpeedProgress
	frames   int
	pending  []byte
	total    int
	failed   bool
	final    bool
}

func NewSpeedStream(id string, final io.Writer, progress func(SpeedProgress) error) *SpeedStream {
	return &SpeedStream{Final: final, Progress: progress, id: id}
}
func (s *SpeedStream) Write(data []byte) (int, error) {
	n := len(data)
	s.total += n
	if s.failed || s.total > 3<<20 {
		s.failed = true
		return 0, errors.New("speed stream limit")
	}
	s.pending = append(s.pending, data...)
	for {
		at := bytes.IndexByte(s.pending, '\n')
		if at < 0 {
			break
		}
		if at > 1<<20 {
			s.failed = true
			return 0, errors.New("speed frame limit")
		}
		line := s.pending[:at]
		if err := s.line(line); err != nil {
			s.failed = true
			return 0, err
		}
		s.pending = s.pending[at+1:]
	}
	if len(s.pending) > 1<<20 {
		s.failed = true
		return 0, errors.New("speed frame limit")
	}
	return n, nil
}
func (s *SpeedStream) line(line []byte) error {
	var header struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(line, &header) != nil || s.final {
		return errors.New("invalid speed frame")
	}
	if header.Event == "" {
		s.final = true
		_, err := s.Final.Write(append(append([]byte(nil), line...), '\n'))
		return err
	}
	var p SpeedProgress
	if header.Event != "speed-progress" || json.Unmarshal(line, &p) != nil {
		return errors.New("invalid speed event")
	}
	s.frames++
	if s.frames > 4096 || !checkID.MatchString(p.ServerID) || p.ServerID != s.id ||
		p.DownloadedBytes < 0 || p.DownloadedBytes > 1<<53 || p.DownloadedBytes < s.previous.DownloadedBytes ||
		math.IsNaN(p.ElapsedSeconds) || math.IsInf(p.ElapsedSeconds, 0) || p.ElapsedSeconds <= s.previous.ElapsedSeconds || p.ElapsedSeconds > 3600 ||
		math.IsNaN(p.DownloadMbps) || math.IsInf(p.DownloadMbps, 0) || p.DownloadMbps < 0 || p.DownloadMbps > 1e9 {
		return errors.New("invalid speed evidence")
	}
	expected := float64(p.DownloadedBytes) * 8 / p.ElapsedSeconds / 1e6
	if math.Abs(expected-p.DownloadMbps) > math.Max(1e-6, expected*1e-6) {
		return errors.New("inconsistent speed evidence")
	}
	s.previous = p
	if s.Progress != nil {
		return s.Progress(p)
	}
	return nil
}
func (s *SpeedStream) Finish() error {
	if len(s.pending) > 0 && !s.failed {
		s.failed = s.line(s.pending) != nil
		s.pending = nil
	}
	if s.failed || !s.final {
		return errors.New("incomplete speed response")
	}
	return nil
}
