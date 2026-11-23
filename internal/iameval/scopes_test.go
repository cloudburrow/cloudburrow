package iameval

import (
	"context"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// A token minted with scopes (#2179) calls only what they cover, and is
// refused otherwise with ACCESS_TOKEN_SCOPE_INSUFFICIENT before its
// permissions are checked.
func TestAccessScopes2179(t *testing.T) {
	sc := func(s ...string) []string {
		for i := range s {
			s[i] = "https://www.googleapis.com/auth/" + s[i]
		}
		return s
	}
	for _, c := range []struct {
		service, perm string
		scopes        []string
		want          bool
	}{
		{"logging.googleapis.com", "logging.logEntries.list", nil, true},
		{"logging.googleapis.com", "logging.logEntries.list", sc("cloud-platform"), true},
		{"logging.googleapis.com", "logging.logEntries.create", sc("logging.write"), true},
		{"logging.googleapis.com", "logging.logEntries.list", sc("logging.write"), false},
		{"logging.googleapis.com", "logging.logEntries.list", sc("logging.read"), true},
		{"storage.googleapis.com", "storage.objects.get", sc("devstorage.read_only"), true},
		{"storage.googleapis.com", "storage.objects.create", sc("devstorage.read_only"), false},
		{"storage.googleapis.com", "storage.objects.create", sc("devstorage.read_write"), true},
		{"secretmanager.googleapis.com", "secretmanager.versions.access", sc("devstorage.read_only"), false},
		{"secretmanager.googleapis.com", "secretmanager.secrets.list", sc("cloud-platform.read-only"), true},
		{"secretmanager.googleapis.com", "secretmanager.secrets.create", sc("cloud-platform.read-only"), false},
		{"pubsub.googleapis.com", "pubsub.topics.publish", sc("userinfo.email", "pubsub"), true},
	} {
		if got := ScopesAllow(c.service, c.perm, c.scopes); got != c.want {
			t.Errorf("%s %s with %v: %v, want %v", c.service, c.perm, c.scopes, got, c.want)
		}
	}
	e := &Enforcer{Eval: &Evaluator{}, On: func() bool { return true },
		CallerOf: func(string) Caller { return Caller{Email: "vm@p.iam.gserviceaccount.com", Scopes: sc("logging.write")} }}
	err := e.Authorize(context.Background(), &Service{Name: "logging.googleapis.com"}, "tok", "logging.logEntries.list", "projects/p")
	st := status.Convert(err)
	if st.Code() != codes.PermissionDenied || st.Message() != "Request had insufficient authentication scopes." {
		t.Fatalf("Authorize: %v", err)
	}
	var reason string
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			reason = ei.GetReason()
		}
	}
	if reason != "ACCESS_TOKEN_SCOPE_INSUFFICIENT" {
		t.Errorf("reason %q", reason)
	}
	d := e.CheckRemote(iamcheck.Request{Bearer: "tok", Service: "logging.googleapis.com", Permissions: []string{"logging.logEntries.list"}, Resource: "projects/p"})
	if !d.ScopeInsufficient {
		t.Errorf("CheckRemote: %+v", d)
	}
}
