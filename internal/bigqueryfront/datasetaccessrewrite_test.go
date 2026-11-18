package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// A deleted account's access entries become iamMember entries naming the
// deleted member, come back as userByEmail on undelete and go on purge;
// other entries, roles and conditions are kept (#1751).
func TestRewriteAccessList(t *testing.T) {
	const (
		live    = "serviceAccount:sa@p.iam.gserviceaccount.com"
		deleted = "deleted:serviceAccount:sa@p.iam.gserviceaccount.com?uid=123"
	)
	in := `[{"role":"READER","userByEmail":"SA@p.iam.gserviceaccount.com"},` +
		`{"role":"WRITER","iamMember":"serviceAccount:sa@p.iam.gserviceaccount.com","condition":{"title":"t","expression":"true"}},` +
		`{"role":"READER","userByEmail":"keep@example.com"},{"role":"OWNER","specialGroup":"projectOwners"}]`
	got, changed, err := rewriteAccessList(json.RawMessage(in), live, deleted)
	if err != nil || !changed {
		t.Fatalf("delete: changed %v, err %v", changed, err)
	}
	want := `[{"iamMember":"` + deleted + `","role":"READER"},` +
		`{"condition":{"title":"t","expression":"true"},"iamMember":"` + deleted + `","role":"WRITER"},` +
		`{"role":"READER","userByEmail":"keep@example.com"},{"role":"OWNER","specialGroup":"projectOwners"}]`
	if string(got) != want {
		t.Errorf("after delete:\n got %s\nwant %s", got, want)
	}
	back, changed, err := rewriteAccessList(got, deleted, live)
	if err != nil || !changed {
		t.Fatalf("undelete: changed %v, err %v", changed, err)
	}
	wantBack := `[{"role":"READER","userByEmail":"sa@p.iam.gserviceaccount.com"},` +
		`{"condition":{"title":"t","expression":"true"},"role":"WRITER","userByEmail":"sa@p.iam.gserviceaccount.com"},` +
		`{"role":"READER","userByEmail":"keep@example.com"},{"role":"OWNER","specialGroup":"projectOwners"}]`
	if string(back) != wantBack {
		t.Errorf("after undelete:\n got %s\nwant %s", back, wantBack)
	}
	purged, changed, err := rewriteAccessList(got, deleted, "")
	if err != nil || !changed {
		t.Fatalf("purge: changed %v, err %v", changed, err)
	}
	if want := `[{"role":"READER","userByEmail":"keep@example.com"},{"role":"OWNER","specialGroup":"projectOwners"}]`; string(purged) != want {
		t.Errorf("after purge:\n got %s\nwant %s", purged, want)
	}
	if _, changed, _ := rewriteAccessList(json.RawMessage(`[{"role":"READER","userByEmail":"keep@example.com"}]`), live, deleted); changed {
		t.Error("a list without the account changed")
	}
	// The same grant made twice (granted again while deleted) is one entry
	// after undelete.
	twice := `[{"role":"READER","iamMember":"` + deleted + `"},{"role":"READER","userByEmail":"sa@p.iam.gserviceaccount.com"}]`
	if one, _, _ := rewriteAccessList(json.RawMessage(twice), deleted, live); string(one) != `[{"role":"READER","userByEmail":"sa@p.iam.gserviceaccount.com"}]` {
		t.Errorf("a repeated grant: %s", one)
	}
}
