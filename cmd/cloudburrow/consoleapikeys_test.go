package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The Credentials page's API keys (#1915): Create credentials > API key
// answers the new key string; the list shows it with its restrictions'
// summary; Save changes the name and restrictions; Show key answers the
// string; Rotate key makes a new key with the same restrictions that names
// the old one, and Delete the previous key deletes it; Delete and Restore
// move a key between the list and Restore deleted credentials.
func TestConsoleAPIKeys(t *testing.T) {
	cfg := config.Default()
	st := store.NewMemory()
	p := serviceUsageProvider{api: newServiceUsageServer(cfg, nil, st), apiKeys: newAPIKeysServer(cfg, nil, st)}
	ctx := context.Background()
	call := func(name string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		out, err := p.StudioCall(ctx, "keys-proj", name, raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(out)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	keys := func(deleted bool) []any {
		t.Helper()
		l, _ := call("keys", map[string]any{"deleted": deleted})["keys"].([]any)
		return l
	}
	created := call("key-create", nil)
	id, _ := created["id"].(string)
	if ks, _ := created["keyString"].(string); !strings.HasPrefix(ks, "AIza") || id == "" {
		t.Fatalf("key-create = %v", created)
	}
	if l := keys(false); len(l) != 1 || l[0].(map[string]any)["summary"] != "None" || l[0].(map[string]any)["keyString"] != nil {
		t.Errorf("keys = %v", l)
	}
	call("key-update", map[string]any{"id": id, "displayName": "Web key", "restrictions": map[string]any{
		"browserKeyRestrictions": map[string]any{"allowedReferrers": []string{"*.example.com/*"}},
		"apiTargets":             []any{map[string]any{"service": "storage.googleapis.com"}}}})
	if k := call("key", map[string]any{"id": id}); k["displayName"] != "Web key" || k["summary"] != "Websites, 1 API" {
		t.Errorf("after Save: %v", k)
	}
	if _, err := p.StudioCall(ctx, "keys-proj", "key-update", json.RawMessage(`{"id":"`+id+`","restrictions":{"browserKeyRestrictions":{"allowedReferrers":["a*b.com"]}}}`)); err == nil {
		t.Error("a website with * in the middle was saved")
	}
	if s := call("key-string", map[string]any{"id": id}); s["keyString"] != created["keyString"] {
		t.Errorf("Show key = %v", s)
	}
	rotated := call("key-rotate", map[string]any{"id": id, "displayName": "Web key 2"})
	nid, _ := rotated["id"].(string)
	if rotated["keyString"] == created["keyString"] || rotated["summary"] != "Websites, 1 API" || rotated["previousKey"] != id {
		t.Errorf("Rotate key = %v", rotated)
	}
	if k := call("key", map[string]any{"id": nid}); k["previous"] == nil {
		t.Errorf("the rotated key's page has no previous key: %v", k)
	}
	call("key-delete-previous", map[string]any{"id": nid})
	if k := call("key", map[string]any{"id": nid}); k["previous"] != nil || k["previousKey"] != nil {
		t.Errorf("after Delete the previous key: %v", k)
	}
	if l := keys(true); len(l) != 1 || l[0].(map[string]any)["id"] != id {
		t.Errorf("deleted keys = %v", l)
	}
	call("key-undelete", map[string]any{"id": id})
	call("key-delete", map[string]any{"id": nid})
	if l := keys(false); len(l) != 1 || l[0].(map[string]any)["id"] != id {
		t.Errorf("after Restore and Delete, keys = %v", l)
	}
}
