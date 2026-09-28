package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// policyFake is a provider with one resource, "r", holding an IAM policy the
// way the services do: SetPolicy refuses an etag that is not the current one.
type policyFake struct {
	fakeProvider
	policy *Policy
	sets   *int
}

func (f policyFake) Detail(context.Context, string, []string) (Detail, error) {
	return Detail{Sections: []Section{{ID: "things", Label: "Things"}}}, nil
}

func (f policyFake) PolicyOn(path []string) *PolicyTarget {
	if len(path) != 1 || path[0] != "r" {
		return nil
	}
	return &PolicyTarget{Link: "https://example.com/row", RoleHelp: "any role"}
}

func (f policyFake) GetPolicy(context.Context, string, []string) (Policy, error) {
	return clonePolicy(*f.policy), nil
}

func (f policyFake) SetPolicy(_ context.Context, _ string, _ []string, p Policy) error {
	*f.sets++
	if p.Etag != f.policy.Etag {
		return errors.New("the policy's etag does not match the current policy")
	}
	for _, b := range p.Bindings {
		if b.Role == "" {
			return errors.New("policy.bindings.role is required")
		}
	}
	p.Etag += "+"
	*f.policy = p
	return nil
}

func postPermissions(t *testing.T, url string, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/api/permissions/iam?project=p", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// The Permissions tab is added by the server to a resource whose provider
// says it has a policy, carries the not-enforced note whatever the provider
// says, and changes go through the provider's SetPolicy with the page's etag
// (#793).
func TestPermissionsTabAndChanges(t *testing.T) {
	t.Parallel()
	sets := 0
	f := policyFake{fakeProvider: fakeProvider{id: "iam", title: "IAM fake"},
		policy: &Policy{Etag: "e1", Bindings: []Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}, sets: &sets}
	srv := serve(t, f)

	code, body := get(t, srv, "/api/detail/iam?project=p&name=r", nil)
	var d Detail
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("detail = %d %v: %s", code, err, body)
	}
	if len(d.Sections) != 2 || d.Sections[1].Kind != KindPermissions || d.Sections[1].Permissions == nil {
		t.Fatalf("sections %+v", d.Sections)
	}
	v := d.Sections[1].Permissions
	if v.Note != PermissionsNote || v.Etag != "e1" || v.Link != "https://example.com/row" || v.PrincipalHelp != DefaultPrincipalHelp {
		t.Errorf("the tab is %+v", v)
	}
	for _, want := range []string{"stores IAM policies but does not enforce them", "ADR-0006", "Every caller can still do everything"} {
		if !strings.Contains(PermissionsNote, want) {
			t.Errorf("the note does not say %q", want)
		}
	}
	if code, body := get(t, srv, "/api/detail/iam?project=p&name=other", nil); code != http.StatusOK || strings.Contains(body, `"permissions"`) {
		t.Errorf("a resource with no policy has a Permissions tab: %s", body)
	}

	// Grant merges into the role's binding, without duplicates.
	if code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Etag": "e1",
		"Grant": map[string]any{"Members": []string{"user:a@example.com", "user:b@example.com"}, "Role": "roles/viewer"}}); code != http.StatusOK {
		t.Fatalf("grant = %d %s", code, body)
	}
	if b := f.policy.Bindings; len(b) != 1 || !slices.Equal(b[0].Members, []string{"user:a@example.com", "user:b@example.com"}) {
		t.Errorf("after the grant the policy is %+v", b)
	}
	// The page's etag is now stale: the provider's refusal is the answer.
	code, body = postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Etag": "e1",
		"Remove": map[string]any{"Member": "user:a@example.com", "Role": "roles/viewer"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "etag does not match") {
		t.Errorf("a stale removal = %d %s", code, body)
	}
	// So is a removal of a binding already gone under a stale etag.
	code, body = postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Etag": "e1",
		"Remove": map[string]any{"Member": "user:gone@example.com", "Role": "roles/viewer"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "etag does not match") {
		t.Errorf("a stale removal of a principal already gone = %d %s", code, body)
	}
	before := sets
	for what, req := range map[string]map[string]any{
		"no etag":     {"Path": []string{"r"}, "Grant": map[string]any{"Members": []string{"user:c@example.com"}, "Role": "roles/viewer"}},
		"no member":   {"Path": []string{"r"}, "Etag": "e1+", "Grant": map[string]any{"Members": []string{""}, "Role": "roles/viewer"}},
		"no change":   {"Path": []string{"r"}, "Etag": "e1+"},
		"not held":    {"Path": []string{"r"}, "Etag": "e1+", "Remove": map[string]any{"Member": "user:z@example.com", "Role": "roles/viewer"}},
		"no policy":   {"Path": []string{"other"}, "Etag": "e1+", "Remove": map[string]any{"Member": "user:a@example.com", "Role": "roles/viewer"}},
		"a bad field": {"Path": []string{"r"}, "Etag": "e1+", "Force": true},
	} {
		if code, body := postPermissions(t, srv.URL, req); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s; want 400", what, code, body)
		}
	}
	if sets != before {
		t.Errorf("a refused request reached SetPolicy %d times", sets-before)
	}
	// An empty role reaches the service, which refuses it with its message.
	if code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Etag": "e1+",
		"Grant": map[string]any{"Members": []string{"user:c@example.com"}, "Role": " "}}); code != http.StatusBadRequest || !strings.Contains(body, "role is required") {
		t.Errorf("an empty role = %d %s", code, body)
	}
	// Removing the last member drops the binding.
	for _, m := range []string{"user:a@example.com", "user:b@example.com"} {
		if code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Etag": f.policy.Etag,
			"Remove": map[string]any{"Member": m, "Role": "roles/viewer"}}); code != http.StatusOK {
			t.Fatalf("remove %s = %d %s", m, code, body)
		}
	}
	if len(f.policy.Bindings) != 0 {
		t.Errorf("after removing every member the policy is %+v", f.policy.Bindings)
	}
}

func TestGrantAccessAndRemovePrincipal(t *testing.T) {
	p := Policy{Etag: "e", Bindings: []Binding{
		{Role: "roles/viewer", Members: []string{"user:a@example.com"}, Condition: "c: true"},
		{Role: "roles/viewer", Members: []string{"user:b@example.com"}},
	}}
	g, err := GrantAccess(p, " roles/viewer ", []string{"user:c@example.com", " user:c@example.com "})
	if err != nil {
		t.Fatal(err)
	}
	// The unconditioned binding takes the grant; the conditioned one is left.
	if !slices.Equal(g.Bindings[0].Members, []string{"user:a@example.com"}) ||
		!slices.Equal(g.Bindings[1].Members, []string{"user:b@example.com", "user:c@example.com"}) {
		t.Errorf("grant = %+v", g.Bindings)
	}
	if !slices.Equal(p.Bindings[1].Members, []string{"user:b@example.com"}) {
		t.Error("GrantAccess changed the policy it was given")
	}
	n, _ := GrantAccess(p, "roles/owner", []string{"user:d@example.com"})
	if len(n.Bindings) != 3 || n.Bindings[2].Role != "roles/owner" {
		t.Errorf("a grant of a new role = %+v", n.Bindings)
	}
	if _, err := GrantAccess(p, "roles/owner", []string{" ", ""}); err == nil {
		t.Error("a grant to no principal was accepted")
	}
	r, err := RemovePrincipal(p, "roles/viewer", "user:b@example.com")
	if err != nil || len(r.Bindings) != 1 || r.Bindings[0].Condition == "" {
		t.Errorf("remove = %+v, %v", r.Bindings, err)
	}
	if _, err := RemovePrincipal(p, "roles/viewer", "user:a@example.com"); err == nil {
		t.Error("a principal held only under a condition was removed as though unconditioned")
	}
}

// The page draws the tab the server describes: the note first, whatever
// else fails; Grant access through a form whose Cancel only closes; Remove
// principal behind a confirmation; every change with the page's etag (#793).
func TestPermissionsTabIsDrawn(t *testing.T) {
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		`case "permissions":`,
		`section.kind === "permissions" ? permissionsNote(section.permissions) : null,`,
		`setChildren(into, permissionsNote(view),`,
		`text: "Grant access"`,
		`text: "Remove principal"`,
		`"POST", { Path: segments, Etag: view.etag, ...body });`,
		`confirmWord: row.member,`,
		`onclick: () => close() });`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
}
