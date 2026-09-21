package picker

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSpeedStreamForwardsFragmentedMeasuredSamplesBeforeFinal(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	var final bytes.Buffer
	var samples []SpeedProgress
	stream := NewSpeedStream(id, &final, func(p SpeedProgress) error { samples = append(samples, p); return nil })
	frame := `{"event":"speed-progress","server_id":"` + id + `","downloaded_bytes":1000,"elapsed_seconds":0.1,"download_mbps":0.08,"secret":"private-marker"}` + "\n"
	for _, b := range []byte(frame) {
		if _, err := stream.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if len(samples) != 1 || final.Len() != 0 {
		t.Fatal("intermediate sample buffered")
	}
	safe, _ := json.Marshal(samples[0])
	if bytes.Contains(safe, []byte("private-marker")) {
		t.Fatal("private field leaked")
	}
	if _, err := stream.Write([]byte("{\"status\":\"ok\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := stream.Finish(); err != nil || final.String() != "{\"status\":\"ok\"}\n" {
		t.Fatal("lost final", err)
	}
	if _, err := stream.Write([]byte(frame)); err == nil {
		t.Fatal("progress after final accepted")
	}
}

func TestSpeedStreamRejectsInvalidOrNonmonotonicEvidence(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	valid := `{"event":"speed-progress","server_id":"` + id + `","downloaded_bytes":1000,"elapsed_seconds":0.1,"download_mbps":0.08}` + "\n"
	cases := []string{
		strings.Replace(valid, id, "srv_"+strings.Repeat("b", 27), 1),
		strings.Replace(valid, "1000", "-1", 1),
		strings.Replace(valid, "0.1", "0", 1),
		strings.Replace(valid, "0.08", "-1", 1),
		strings.Replace(valid, "0.08", "0.09", 1),
		strings.Replace(valid, "0.08", "1e999", 1),
		strings.Replace(valid, "speed-progress", "server-check", 1),
		strings.Replace(valid, "0.1", "3601", 1),
	}
	for _, data := range cases {
		var final bytes.Buffer
		s := NewSpeedStream(id, &final, nil)
		if _, err := s.Write([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	for _, data := range []string{valid, strings.Replace(valid, "1000", "500", 1)} {
		var final bytes.Buffer
		s := NewSpeedStream(id, &final, nil)
		s.Write([]byte(valid))
		if _, err := s.Write([]byte(data)); err == nil {
			t.Fatal("duplicate/regressing evidence accepted")
		}
	}
}

func TestSpeedStreamBoundsAndCallbackFailure(t *testing.T) {
	id := "srv_" + strings.Repeat("a", 27)
	var final bytes.Buffer
	s := NewSpeedStream(id, &final, nil)
	if _, err := s.Write([]byte(strings.Repeat("x", (1<<20)+1))); err == nil {
		t.Fatal("unbounded frame accepted")
	}
	marker := errors.New("closed writer")
	s = NewSpeedStream(id, &final, func(SpeedProgress) error { return marker })
	data, _ := json.Marshal(SpeedProgress{"speed-progress", id, 1000, 0.1, 0.08})
	if _, err := s.Write(append(data, '\n')); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	if err := s.Finish(); err == nil {
		t.Fatal("callback failure accepted as complete")
	}
	s = NewSpeedStream(id, &final, nil)
	s.Write(append(data, '\n'))
	if err := s.Finish(); err == nil {
		t.Fatal("missing final accepted")
	}
}
