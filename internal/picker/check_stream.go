package picker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
)

// CheckProgress contains only measurement evidence, never endpoints or configs.
// It is provisional until the final catalog publication succeeds.
type CheckProgress struct {
	Event           string  `json:"event"`
	ServerID        string  `json:"server_id"`
	Stage           string  `json:"stage"`
	PingStatus      string  `json:"ping_status"`
	LatencyMS       int     `json:"latency_ms"`
	Availability    string  `json:"availability"`
	Status          string  `json:"status,omitempty"`
	DownloadMbps    float64 `json:"download_mbps,omitempty"`
	DownloadSeconds float64 `json:"download_seconds,omitempty"`
	DownloadedBytes int64   `json:"downloaded_bytes,omitempty"`
}

var checkID = regexp.MustCompile(`^srv_[A-Za-z0-9_-]{27}$`)

// CheckStream removes and validates progress frames before passing the final
// response to the existing bounded capture. Use once per serialized operation.
type CheckStream struct {
	Final    io.Writer
	Progress func(CheckProgress) error
	ids      map[string]int
	pending  []byte
	total    int
	failed   bool
	final    bool
}

func NewCheckStream(ids []string, final io.Writer, progress func(CheckProgress) error) *CheckStream {
	s := &CheckStream{Final: final, Progress: progress, ids: map[string]int{}}
	for _, id := range ids {
		s.ids[id] = 0
	}
	return s
}
func (s *CheckStream) Write(data []byte) (int, error) {
	n := len(data)
	s.total += n
	if s.failed || s.total > 3<<20 {
		s.failed = true
		return 0, errors.New("check stream limit")
	}
	s.pending = append(s.pending, data...)
	for {
		at := bytes.IndexByte(s.pending, '\n')
		if at < 0 {
			break
		}
		if at > 1<<20 {
			s.failed = true
			return 0, errors.New("check frame limit")
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
		return 0, errors.New("check frame limit")
	}
	return n, nil
}
func (s *CheckStream) line(line []byte) error {
	var header struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(line, &header) != nil || s.final {
		return errors.New("invalid check frame")
	}
	if header.Event == "" {
		s.final = true
		_, err := s.Final.Write(append(append([]byte(nil), line...), '\n'))
		return err
	}
	var p CheckProgress
	if header.Event != "server-check" || json.Unmarshal(line, &p) != nil {
		return errors.New("invalid check event")
	}
	previous, allowed := s.ids[p.ServerID]
	stage := 0
	switch p.Stage {
	case "start":
		stage = 1
	case "ping":
		stage = 2
	case "speed":
		stage = 3
	case "complete":
		stage = 4
	}
	if !allowed || !checkID.MatchString(p.ServerID) || stage <= previous || p.LatencyMS < 0 || p.LatencyMS > 3600000 ||
		(p.PingStatus != "ready" && p.PingStatus != "failed" && !(stage == 1 && p.PingStatus == "untested")) ||
		(p.Availability != "ready" && p.Availability != "failed" && p.Availability != "untested") ||
		(stage <= 2 && p.Availability != "untested") ||
		(stage == 3 && (p.PingStatus != "ready" || p.Availability == "untested")) ||
		(p.Status != "" && p.Status != "ready" && p.Status != "failed" && p.Status != "untested") ||
		(stage < 4 && (p.Status != "" || p.DownloadMbps != 0 || p.DownloadSeconds != 0 || p.DownloadedBytes != 0)) ||
		math.IsNaN(p.DownloadMbps) || math.IsInf(p.DownloadMbps, 0) || p.DownloadMbps < 0 || p.DownloadMbps > 1e9 ||
		math.IsNaN(p.DownloadSeconds) || math.IsInf(p.DownloadSeconds, 0) || p.DownloadSeconds < 0 || p.DownloadSeconds > 3600000 ||
		p.DownloadedBytes < 0 || p.DownloadedBytes > 1<<53 ||
		(stage == 1 && (p.PingStatus != "untested" || p.LatencyMS != 0)) ||
		(p.PingStatus == "failed" && (p.LatencyMS != 0 || p.Availability != "untested")) {
		return errors.New("invalid check evidence")
	}
	s.ids[p.ServerID] = stage
	if s.Progress != nil {
		return s.Progress(p)
	}
	return nil
}
func (s *CheckStream) Finish() error {
	if len(s.pending) > 0 && !s.failed {
		s.failed = s.line(s.pending) != nil
		s.pending = nil
	}
	if s.failed || !s.final {
		return errors.New("incomplete check response")
	}
	return nil
}
