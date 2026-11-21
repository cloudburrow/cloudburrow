package iamcond

import "testing"

func TestCompilePrincipal(t *testing.T) {
	sa := Attributes{PrincipalType: "iam.googleapis.com/ServiceAccount", PrincipalSubject: "a@p.iam.gserviceaccount.com"}
	user := Attributes{PrincipalType: "iam.googleapis.com/WorkspaceIdentity", PrincipalSubject: "special-admin@example.com"}
	for _, c := range []struct {
		expr     string
		sa, user bool
	}{
		{"principal.type == 'iam.googleapis.com/ServiceAccount'", true, false},
		{"principal.subject != 'special-admin@example.com'", true, false},
		{"principal.subject.endsWith('@example.com')", false, true},
		{"principal.type in ['iam.googleapis.com/ServiceAccount', 'iam.googleapis.com/WorkspaceIdentity']", true, true},
	} {
		x, err := CompilePrincipal(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := x.Eval(sa); got != c.sa {
			t.Errorf("%s on a service account = %v", c.expr, got)
		}
		if got := x.Eval(user); got != c.user {
			t.Errorf("%s on a user = %v", c.expr, got)
		}
	}
	for _, bad := range []string{"resource.name == 'x'", "request.time < timestamp('2030-01-01T00:00:00Z')"} {
		if _, err := CompilePrincipal(bad); err == nil {
			t.Errorf("CompilePrincipal(%q) compiled", bad)
		}
	}
	if _, err := Compile("principal.type == 'x'"); err == nil {
		t.Error("an allow condition read principal.type")
	}
}
