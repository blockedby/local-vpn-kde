package picker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckStreamForwardsEachFragmentedEventBeforeFinal(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	var final bytes.Buffer
	var events []CheckProgress
	s := NewCheckStream([]string{id}, &final, func(p CheckProgress) error { events = append(events, p); return nil })
	for _, stage := range []string{"start", "ping", "complete"} {
		p := CheckProgress{Event: "server-check", ServerID: id, Stage: stage, PingStatus: "ready", LatencyMS: 12, Availability: "untested"}
		if stage == "start" {
			p.PingStatus = "untested"
			p.LatencyMS = 0
		}
		raw, _ := json.Marshal(p)
		// Extra private fields must never be forwarded by either transport boundary.
		raw = append(raw[:len(raw)-1], []byte(`,"secret":"private-marker"}`)...)
		for _, b := range append(raw, '\n') {
			if _, err := s.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if final.Len() != 0 {
			t.Fatal("event entered final response")
		}
	}
	if len(events) != 3 {
		t.Fatal("events waited for final")
	}
	encoded, _ := json.Marshal(events)
	if bytes.Contains(encoded, []byte("private-marker")) {
		t.Fatal("private field leaked")
	}
	_, err := s.Write([]byte("{\"status\":\"ok\"}\n"))
	if err != nil || s.Finish() != nil || !strings.Contains(final.String(), "ok") {
		t.Fatal("final lost")
	}
}

func TestCheckStreamRejectsForeignDuplicateOutOfOrderAndMalformedEvents(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	valid := CheckProgress{Event: "server-check", ServerID: id, Stage: "ping", PingStatus: "ready", LatencyMS: 1, Availability: "untested"}
	for _, kind := range []string{"foreign", "duplicate", "backwards", "negative", "unknown-stage", "unknown-event", "ping-site", "failed-latency", "oversized", "truncated", "after-final"} {
		t.Run(kind, func(t *testing.T) {
			var final bytes.Buffer
			s := NewCheckStream([]string{id}, &final, nil)
			p := valid
			switch kind {
			case "foreign":
				p.ServerID = "srv_" + strings.Repeat("b", 27)
			case "duplicate":
				raw, _ := json.Marshal(p)
				s.Write(append(raw, '\n'))
			case "backwards":
				p.Stage = "complete"
				raw, _ := json.Marshal(p)
				s.Write(append(raw, '\n'))
				p.Stage = "ping"
			case "negative":
				p.LatencyMS = -1
			case "unknown-stage":
				p.Stage = "next"
			case "unknown-event":
				p.Event = "secret"
			case "ping-site":
				p.Availability = "ready"
			case "failed-latency":
				p.PingStatus = "failed"
			case "after-final":
				s.Write([]byte("{}\n"))
			}
			raw, _ := json.Marshal(p)
			if kind == "oversized" {
				raw = []byte(strings.Repeat("x", 1<<20+1))
			} else if kind == "truncated" {
				raw = raw[:len(raw)-2]
			} else {
				raw = append(raw, '\n')
			}
			_, err := s.Write(raw)
			if err == nil && s.Finish() == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}

func TestSpeedCheckStreamProgressAndMetricBounds(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	var final bytes.Buffer
	var events []CheckProgress
	stream := NewCheckStream([]string{id}, &final, func(p CheckProgress) error { events = append(events, p); return nil })
	for _, p := range []CheckProgress{
		{Event: "server-check", ServerID: id, Stage: "ping", PingStatus: "ready", LatencyMS: 12, Availability: "untested"},
		{Event: "server-check", ServerID: id, Stage: "speed", PingStatus: "ready", LatencyMS: 12, Availability: "failed"},
		{Event: "server-check", ServerID: id, Stage: "complete", PingStatus: "ready", LatencyMS: 12, Availability: "failed", Status: "ready", DownloadMbps: 8, DownloadSeconds: 3, DownloadedBytes: 3000000},
	} {
		data, _ := json.Marshal(p)
		if _, err := stream.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.Write([]byte("{}\n")); err != nil || stream.Finish() != nil {
		t.Fatal("valid speed stream rejected", err)
	}
	if len(events) != 3 || events[2].DownloadedBytes != 3000000 || events[2].Availability != "failed" {
		t.Fatal("mixed speed/site evidence lost", events)
	}
	for _, payload := range []string{
		`"stage":"speed","ping_status":"ready","latency_ms":1,"availability":"ready","download_mbps":1`,
		`"stage":"speed","ping_status":"ready","latency_ms":1,"availability":"untested"`,
		`"stage":"complete","ping_status":"ready","latency_ms":1,"availability":"ready","download_seconds":-1`,
		`"stage":"complete","ping_status":"ready","latency_ms":1,"availability":"ready","download_mbps":1e20`,
		`"stage":"complete","ping_status":"ready","latency_ms":1,"availability":"ready","downloaded_bytes":9007199254740993`,
	} {
		s := NewCheckStream([]string{id}, &bytes.Buffer{}, nil)
		if _, err := s.Write([]byte(`{"event":"server-check","server_id":"` + id + `",` + payload + "}\n")); err == nil {
			t.Fatal("invalid speed evidence accepted", payload)
		}
	}
}
