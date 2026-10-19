package profiles

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDecodeRequestIsBoundedAndConsumesWholeFrame(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"list", `{"action":"list"}`, true},
		{"whitespace", " {\"action\":\"list\"}\n\t", true},
		{"direct server", `{"action":"add","name":"VPS","source":"hy2://private-marker@example.invalid"}`, true},
		{"trailing frame", `{"action":"list"}{"action":"remove","id":"profile_one"}`, false},
		{"trailing junk", `{"action":"list"}private-marker`, false},
		{"unknown fields", `{"action":"list","private-marker":true}`, false},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"missing action", `{}`, false},
		{"extra fields", `{"action":"list","source":"private-marker"}`, false},
		{"at limit", `{"action":"list"}` + strings.Repeat(" ", MaxRequestBytes-len(`{"action":"list"}`)), true},
		{"over limit", `{"action":"list"}` + strings.Repeat(" ", MaxRequestBytes), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRequest(strings.NewReader(tc.input))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-marker") {
				t.Fatal("input leaked")
			}
		})
	}
	_, err := DecodeRequest(io.MultiReader(strings.NewReader(`{"action":"list"}`), failingReader{}))
	if err == nil || strings.Contains(err.Error(), "private-marker") {
		t.Fatal("read failure was lost or exposed")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private-marker") }

func TestDecodeAddRequestBindsTheBridgeAction(t *testing.T) {
	for _, input := range []string{
		`{"name":"VPS","source":"hy2://fixture@example.invalid"}`,
		`{"name":"Sub","source":"https://example.invalid/private-token"}`,
	} {
		r, err := DecodeAddRequest(strings.NewReader(input))
		if err != nil || r.Action != "add" {
			t.Fatal(err, r.Action)
		}
	}
	for _, input := range []string{
		`{"action":"list","name":"VPS","source":"hy2://fixture@example.invalid"}`,
		`{"name":"VPS","source":"hy2://fixture@example.invalid","config":"/tmp/private-marker"}`,
		`{"name":"VPS","source":"hy2://fixture@example.invalid"} {}`,
		`{"id":"profile_one","name":"VPS","source":"hy2://fixture@example.invalid"}`,
	} {
		if _, err := DecodeAddRequest(strings.NewReader(input)); err == nil {
			t.Fatal("unsafe bridge input accepted")
		}
	}
}
