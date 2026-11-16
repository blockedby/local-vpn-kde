package localvpn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func namesRequest(t *testing.T, b *Bridge, action string, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"action": action, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := b.request(context.Background(), raw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func namesFixture(t *testing.T) (*Bridge, string, string) {
	t.Helper()
	b := bridgeFixture(t, `{}`)
	id := "srv_" + strings.Repeat("a", 27)
	other := "srv_" + strings.Repeat("b", 27)
	script := fmt.Sprintf(`#!/bin/sh
case "$2" in
ping) printf '%%s\n' '{"schema":"vibe-vpn.server-browser.v2","status":"ok","server":{"server_id":"%s","display_name":"Original Place"}}' ;;
check-batch|speed-batch) printf '%%s\n' '{"schema":"vibe-vpn.server-browser.v2","status":"ok","servers":[{"server_id":"%s","display_name":"Original Place"}]}' ;;
*) printf '%%s\n' '{"schema":"vibe-vpn.server-browser.v2","status":"ok","servers":[{"server_id":"%s","display_name":"Original Place"},{"server_id":"%s","display_name":"Another Server","original_name":"Unwanted Original"}]}' ;;
esac
`, id, id, id, other)
	if err := os.WriteFile(b.options.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return b, id, other
}

func nameRow(t *testing.T, reply map[string]any, single bool) map[string]any {
	t.Helper()
	if reply["reason"] != "ok" {
		t.Fatalf("unexpected reply: %#v", reply)
	}
	catalog := reply["catalog"].(map[string]any)
	if single {
		return catalog["server"].(map[string]any)
	}
	return catalog["servers"].([]any)[0].(map[string]any)
}

func TestServerNamesLifecycleAndCatalogShapes(t *testing.T) {
	b, id, _ := namesFixture(t)
	rename := func(name string) map[string]any {
		return namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": name})
	}
	row := nameRow(t, rename("  My !Place  "), false)
	if row["display_name"] != "My Place" || row["original_name"] != "Original Place" {
		t.Fatalf("rename: %#v", row)
	}
	st, err := os.Stat(filepath.Join(b.options.Base, serverNamesFile))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
	data, err := os.ReadFile(filepath.Join(b.options.Base, serverNamesFile))
	if err != nil {
		t.Fatal(err)
	}
	var stored serverNamesData
	if json.Unmarshal(data, &stored) != nil || stored.Schema != 1 || len(stored.Names) != 1 || stored.Names[id] != "My Place" {
		t.Fatalf("store: %s", data)
	}
	if got := nameRow(t, rename("Other Name"), false)["display_name"]; got != "Other Name" {
		t.Fatal(got)
	}
	newBridge, err := NewBridge(b.options)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		action string
		value  any
		single bool
	}{
		{"servers/list", "", false}, {"servers/ping", id, true},
		{"servers/check-batch", fmt.Sprintf(`{"ids":[%q],"url":"https://example.com"}`, id), false},
		{"servers/speed-batch", fmt.Sprintf(`{"ids":[%q],"url":"https://example.com"}`, id), false},
	} {
		row := nameRow(t, namesRequest(t, newBridge, tc.action, tc.value), tc.single)
		if row["display_name"] != "Other Name" || row["original_name"] != "Original Place" {
			t.Fatalf("%s: %#v", tc.action, row)
		}
	}
	list := namesRequest(t, newBridge, "servers/list", "")["catalog"].(map[string]any)["servers"].([]any)
	if _, ok := list[1].(map[string]any)["original_name"]; ok {
		t.Fatal("unoverridden row exposed original_name")
	}
	row = nameRow(t, namesRequest(t, newBridge, "servers/rename", map[string]any{"id": id, "name": ""}), false)
	if row["display_name"] != "Original Place" {
		t.Fatal(row)
	}
	if _, ok := row["original_name"]; ok {
		t.Fatal(row)
	}
}

func TestServerNamesInvalidAndNative(t *testing.T) {
	b, id, _ := namesFixture(t)
	for _, name := range []string{" ", "!!!", "bad\nname", "https://example.com", "single"} {
		if got := namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": name})["reason"]; got != "invalid-name" {
			t.Fatalf("%q: %v", name, got)
		}
	}
	for _, value := range []any{nil, "bad", map[string]any{"id": "bad", "name": "Good Name"}, map[string]any{"id": id}, map[string]any{"id": id, "name": nil}} {
		if got := namesRequest(t, b, "servers/rename", value)["reason"]; got != "invalid-request" {
			t.Fatalf("%#v: %v", value, got)
		}
	}
	name := strings.Repeat("Long ", 30)
	row := nameRow(t, namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": name}), false)
	if len([]rune(row["display_name"].(string))) > 60 {
		t.Fatal(row)
	}
	b.options.Runtime = "native"
	if got := namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": "Native Name"})["reason"]; got != "ok" {
		t.Fatal(got)
	}
}

func TestServerNamesCapAndCorruptStore(t *testing.T) {
	b, id, _ := namesFixture(t)
	for i := 0; i < maxServerNames; i++ {
		b.serverNames[fmt.Sprintf("srv_%027d", i)] = "Stored Name"
	}
	data, err := json.Marshal(serverNamesData{Schema: 1, Names: b.serverNames})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.options.Base, serverNamesFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": "New Name"})["reason"]; got != "too-many-names" {
		t.Fatal(got)
	}
	existing := fmt.Sprintf("srv_%027d", 0)
	if got := namesRequest(t, b, "servers/rename", map[string]any{"id": existing, "name": "Replacement Name"})["reason"]; got != "ok" {
		t.Fatal(got)
	}
	for _, contents := range []string{`{broken`, strings.Repeat("x", maxServerNamesBytes+1), `{"schema":1,"names":{"bad":"Private Value"}}`} {
		if err := os.WriteFile(filepath.Join(b.options.Base, serverNamesFile), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		fresh, err := NewBridge(b.options)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.serverNames) != 0 {
			t.Fatal("invalid file not ignored")
		}
		if got := namesRequest(t, fresh, "servers/rename", map[string]any{"id": id, "name": "Valid Name"})["reason"]; got != "ok" {
			t.Fatal(got)
		}
	}
}

func TestServerNamesAcrossBridgeInstances(t *testing.T) {
	b, id, other := namesFixture(t)
	second, err := NewBridge(b.options)
	if err != nil {
		t.Fatal(err)
	}
	namesRequest(t, b, "servers/rename", map[string]any{"id": id, "name": "First Name"})
	namesRequest(t, second, "servers/rename", map[string]any{"id": other, "name": "Second Name"})
	for _, bridge := range []*Bridge{b, second} {
		rows := namesRequest(t, bridge, "servers/list", "")["catalog"].(map[string]any)["servers"].([]any)
		if rows[0].(map[string]any)["display_name"] != "First Name" || rows[1].(map[string]any)["display_name"] != "Second Name" {
			t.Fatalf("lost or stale names: %#v", rows)
		}
	}
}
