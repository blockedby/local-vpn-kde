package localvpn

import (
	"os"
	"reflect"
	"testing"
)

func TestAttemptPhasesConsumeOnlyNewCompleteLines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []string
	w := &attemptOutput{file: f, progress: func(phase string) { got = append(got, phase) }}
	for _, chunk := range []string{"vpnkit_phase=preparing\nvpnkit_phase=prepared\n", "ordinary output\n", "vpnkit_pha", "se=compose-up", "-done\n", "more output\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"preparing", "prepared", "compose-up-done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestAttemptClassifiesBackendFailuresAcrossChunks(t *testing.T) {
	for _, entry := range diagnosticReasons {
		t.Run(entry.reason+"/"+entry.message, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "attempt")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			w := &attemptOutput{file: f}
			// Process pipes can split an error anywhere. Feed one byte at a time.
			for _, b := range []byte("ERROR: " + entry.message + "\n") {
				if _, err := w.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if w.reason != entry.reason {
				t.Fatalf("got %q, want %q", w.reason, entry.reason)
			}
			// A rollback/wrapper failure must not hide the original reason.
			_, _ = w.Write([]byte("same-host local VPN smoke failed\n"))
			if w.reason != entry.reason {
				t.Fatal("primary failure overwritten")
			}
		})
	}
}

func TestAttemptUnknownOutputDoesNotBecomeReason(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &attemptOutput{file: f}
	_, _ = w.Write([]byte("private endpoint or credential: never a public reason\n"))
	if w.reason != "" {
		t.Fatalf("unknown output became public reason: %q", w.reason)
	}
}
