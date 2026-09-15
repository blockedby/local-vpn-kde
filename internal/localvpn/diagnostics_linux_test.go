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
