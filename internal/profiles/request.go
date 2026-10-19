package profiles

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode"

	"github.com/blockedby/local-vpn-kde/internal/sharelink"
)

type Request struct {
	Action string `json:"action"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}

// DecodeRequest is the shared boundary for CLI and privileged profile requests.
// Read the entire bounded frame so a valid prefix cannot hide trailing data.
func DecodeRequest(input io.Reader) (Request, error) {
	return decodeRequest(input, "")
}

// DecodeAddRequest accepts the bridge's name/source payload. The operation is
// fixed by the bridge action rather than by untrusted payload fields.
func DecodeAddRequest(input io.Reader) (Request, error) {
	return decodeRequest(input, "add")
}

func decodeRequest(input io.Reader, action string) (Request, error) {
	var r Request
	data, err := io.ReadAll(io.LimitReader(input, MaxRequestBytes+1))
	if err != nil || len(data) > MaxRequestBytes {
		return r, errors.New("invalid profile request")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF {
		return Request{}, errors.New("invalid profile request")
	}
	if action != "" {
		if r.Action != "" {
			return Request{}, errors.New("invalid profile request")
		}
		r.Action = action
	}
	if r.Validate() != nil {
		return Request{}, errors.New("invalid profile request")
	}
	return r, nil
}

func SourceKind(s string) (string, error) {
	if len(s) == 0 || len(s) > 16384 || strings.ContainsAny(s, "\r\n\x00") {
		return "", errors.New("invalid profile source")
	}
	if sharelink.Supported(s) {
		if _, e := sharelink.Parse(s); e != nil {
			return "", e
		}
		return "server", nil
	}
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("expected HTTPS subscription or supported server link")
	}
	return "subscription", nil
}
func (r Request) Validate() error {
	switch r.Action {
	case "list":
		if r.ID == "" && r.Name == "" && r.Source == "" {
			return nil
		}
	case "open", "remove":
		if IDPattern.MatchString(r.ID) && r.Name == "" && r.Source == "" {
			return nil
		}
	case "add":
		if r.ID != "" || len([]rune(r.Name)) < 1 || len([]rune(r.Name)) > 60 || strings.Contains(r.Name, "://") {
			break
		}
		for _, c := range r.Name {
			if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
				return errors.New("invalid profile name")
			}
		}
		_, e := SourceKind(r.Source)
		return e
	}
	return errors.New("invalid profile request")
}
