package sharelink

import (
	"strings"
	"testing"
)

func TestHysteria2(t *testing.T) {
	for _, scheme := range []string{"hy2", "hysteria2"} {
		n, e := Parse(scheme + "://user:pass%40word@[2001:db8::1]:8443/?sni=edge.example&insecure=1&obfs=salamander&obfs-password=mask#My%20VPS")
		if e != nil {
			t.Fatal(e)
		}
		if n.Name != "My VPS" || n.Port != 8443 || n.Network != "quic" || n.Outbound["password"] != "user:pass@word" {
			t.Fatalf("bad parsed fields")
		}
		tls := n.Outbound["tls"].(map[string]any)
		if tls["server_name"] != "edge.example" || tls["insecure"] != true {
			t.Fatal(tls)
		}
	}
	n, e := Parse("hysteria2://secret@example.invalid")
	if e != nil || n.Port != 443 {
		t.Fatal(e)
	}
	if _, ok := n.Outbound["tls"].(map[string]any)["insecure"]; ok {
		t.Fatal("TLS verification disabled by default")
	}
}
func TestRejectUnsafeOrUnsupportedWithoutLeaking(t *testing.T) {
	for _, raw := range []string{"hy2://secret@example.invalid:0", "hy2://secret@example.invalid?pinSHA256=secret", "hy2://secret@example.invalid?insecure=2", "hy2://secret@example.invalid?obfs=salamander", "hy2://secret@example.invalid?insecure=0&insecure=1", "hy2://secret@example.invalid?ech=secret", "hy2://secret@bad:port"} {
		_, e := Parse(raw)
		if e == nil || strings.Contains(e.Error(), "secret") {
			t.Fatalf("unsafe error/result: %v", e)
		}
	}
}
