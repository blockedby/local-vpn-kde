package vless

import "testing"

func TestParseWS(t *testing.T) {
	n, err := Parse("vless://uuid@h:80?security=none&type=ws&path=%2Fws&host=ex#Name")
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "Name" || n.Host != "h" || n.Port != 80 || n.Network != "ws" {
		t.Fatalf("%+v", n)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	if ss["network"] != "ws" {
		t.Fatal(ss)
	}
}
func TestParseReality(t *testing.T) {
	n, err := Parse("vless://u@h:443?type=tcp&security=reality&sni=github.com&fp=chrome&pbk=pk&sid=aa&spx=%2F&flow=xtls-rprx-vision#r")
	if err != nil {
		t.Fatal(err)
	}
	if n.Security != "reality" {
		t.Fatal(n)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	rs := ss["realitySettings"].(map[string]any)
	if rs["publicKey"] != "pk" || rs["spiderX"] != "/" {
		t.Fatal(rs)
	}
}

func TestParseRejectsMissingHostAndBadPort(t *testing.T) {
	for _, link := range []string{"vless://u@:443?security=tls", "vless://u@h:99999?security=tls", "vless://@h:443?security=tls"} {
		if _, err := Parse(link); err == nil {
			t.Fatalf("expected error for %s", link)
		}
	}
}

func TestParseTLSALPNAndDefaultName(t *testing.T) {
	n, err := Parse("vless://u@example.com?security=tls&alpn=h2,http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Port != 443 || n.Name != "example.com:443" {
		t.Fatalf("%+v", n)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	tls := ss["tlsSettings"].(map[string]any)
	if got := tls["alpn"].([]string); len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatal(tls)
	}
}

func TestSingBoxOutboundRealityOmitsXraySchema(t *testing.T) {
	out, err := SingBoxOutbound("vless://user-id@example.com:443?type=tcp&security=reality&sni=github.com&fp=chrome&pbk=public-key&sid=abcd&flow=xtls-rprx-vision#r")
	if err != nil {
		t.Fatal(err)
	}
	if out["type"] != "vless" || out["server"] != "example.com" || out["server_port"] != 443 || out["uuid"] != "user-id" {
		t.Fatalf("unexpected sing-box outbound: %#v", out)
	}
	if _, ok := out["protocol"]; ok {
		t.Fatalf("sing-box outbound contains xray protocol key: %#v", out)
	}
	if _, ok := out["settings"]; ok {
		t.Fatalf("sing-box outbound contains xray settings key: %#v", out)
	}
	if _, ok := out["streamSettings"]; ok {
		t.Fatalf("sing-box outbound contains xray streamSettings key: %#v", out)
	}
	tls := out["tls"].(map[string]any)
	if tls["enabled"] != true || tls["server_name"] != "github.com" {
		t.Fatalf("unexpected tls: %#v", tls)
	}
	reality := tls["reality"].(map[string]any)
	if reality["enabled"] != true || reality["public_key"] != "public-key" || reality["short_id"] != "abcd" {
		t.Fatalf("unexpected reality: %#v", reality)
	}
}

func TestSingBoxOutboundTLSWSAndGRPC(t *testing.T) {
	ws, err := SingBoxOutbound("vless://u@h:443?security=tls&type=ws&path=%2Fws&host=edge.example&sni=origin.example&fp=firefox&alpn=h2,http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	tls := ws["tls"].(map[string]any)
	if tls["server_name"] != "origin.example" {
		t.Fatalf("unexpected tls: %#v", tls)
	}
	tr := ws["transport"].(map[string]any)
	if tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Fatalf("unexpected ws transport: %#v", tr)
	}
	if tr["headers"].(map[string]any)["Host"] != "edge.example" {
		t.Fatalf("unexpected ws headers: %#v", tr)
	}
	grpc, err := SingBoxOutbound("vless://u@h:443?security=tls&type=grpc&serviceName=svc")
	if err != nil {
		t.Fatal(err)
	}
	gtr := grpc["transport"].(map[string]any)
	if gtr["type"] != "grpc" || gtr["service_name"] != "svc" {
		t.Fatalf("unexpected grpc transport: %#v", gtr)
	}
}
