package iampolicy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRewriteMember(t *testing.T) {
	s := &Stored{Bindings: []Binding{
		{Role: "roles/a", Members: []string{"serviceAccount:x", "user:u"}},
		{Role: "roles/b", Members: []string{"serviceAccount:x"}},
		{Role: "roles/c", Members: []string{"serviceAccount:x", "deleted:x"}},
	}, Etag: []byte("old")}
	if !s.RewriteMember("serviceAccount:x", "deleted:x") {
		t.Fatal("nothing changed")
	}
	want := []Binding{
		{Role: "roles/a", Members: []string{"deleted:x", "user:u"}},
		{Role: "roles/b", Members: []string{"deleted:x"}},
		{Role: "roles/c", Members: []string{"deleted:x"}},
	}
	if !reflect.DeepEqual(s.Bindings, want) || string(s.Etag) == "old" {
		t.Errorf("got %v etag %q", s.Bindings, s.Etag)
	}
	if !s.RewriteMember("deleted:x", "") || len(s.Bindings) != 1 || s.Bindings[0].Role != "roles/a" {
		t.Errorf("drop: %v", s.Bindings)
	}
	if s.RewriteMember("nobody", "x") {
		t.Error("an absent member changed the policy")
	}
	var nilStored *Stored
	if nilStored.RewriteMember("a", "b") {
		t.Error("a nil policy changed")
	}
}

func TestRewriteMemberInRecord(t *testing.T) {
	raw := []byte(`{"name":"r","other":{"k":1},"iamPolicy":{"bindings":[{"role":"roles/a","members":["serviceAccount:x"]}]}}`)
	out, changed, err := RewriteMemberInRecord(raw, "iamPolicy", "serviceAccount:x", "deleted:x")
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	var rec struct {
		Name      string         `json:"name"`
		Other     map[string]int `json:"other"`
		IAMPolicy Stored         `json:"iamPolicy"`
	}
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "r" || rec.Other["k"] != 1 || rec.IAMPolicy.Bindings[0].Members[0] != "deleted:x" {
		t.Errorf("record %s", out)
	}
	if _, changed, _ := RewriteMemberInRecord([]byte(`{"name":"r"}`), "iamPolicy", "a", "b"); changed {
		t.Error("a record without a policy changed")
	}
}
