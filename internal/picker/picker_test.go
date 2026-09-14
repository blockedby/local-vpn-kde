package picker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBest(t *testing.T) {
	rs := []NodeResult{{OK: true, Mbps: 1, Name: "a"}, {OK: false, Mbps: 9}, {OK: true, Mbps: 2, Name: "b"}}
	if Best(rs).Name != "b" {
		t.Fatal(Best(rs))
	}
}

func TestNodeResultJSONFieldsAreDistinct(t *testing.T) {
	b, err := json.Marshal(NodeResult{Index: 1, Name: "n", Host: "h", Network: "ws", Security: "tls", Error: "e", Link: "vless://x", OK: false})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"name":"n"`, `"host":"h"`, `"network":"ws"`, `"security":"tls"`, `"error":"e"`, `"link":"vless://x"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}
