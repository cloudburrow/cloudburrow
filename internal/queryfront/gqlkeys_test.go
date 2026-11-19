package queryfront

import (
	"testing"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGQLKeyLiteralsNameTheDatabasesEmulatorProject(t *testing.T) {
	ep := "'" + EmulatorProject("demo", "db-a") + "'"
	for _, c := range [][2]string{
		{"SELECT * FROM Task WHERE __key__ = KEY(PROJECT('demo'), 'Task', 'x')",
			"SELECT * FROM Task WHERE __key__ = KEY(PROJECT(" + ep + "), 'Task', 'x')"},
		{`select * from Task where r = key(project("demo"), namespace('ns'), 'Task', 1) and s = KEY('Task', 2)`,
			`select * from Task where r = key(project(` + ep + `), namespace('ns'), 'Task', 1) and s = KEY('Task', 2)`},
		{"SELECT * FROM Task WHERE __key__ = KEY(NAMESPACE('n'), PROJECT('demo'), DATABASE('db-a'), 'Task', 'x')",
			"SELECT * FROM Task WHERE __key__ = KEY(NAMESPACE('n'), PROJECT(" + ep + "),  'Task', 'x')"},
		// Another project's key, another database's, and text that is
		// not a key literal pass unchanged.
		{"SELECT * FROM Task WHERE r = KEY(PROJECT('other'), DATABASE('db-b'), 'Task', 'x')",
			"SELECT * FROM Task WHERE r = KEY(PROJECT('other'), DATABASE('db-b'), 'Task', 'x')"},
		{"SELECT * FROM Task WHERE note = 'KEY(PROJECT(''demo''), ''Task'', 1)' AND `KEY` = 1",
			"SELECT * FROM Task WHERE note = 'KEY(PROJECT(''demo''), ''Task'', 1)' AND `KEY` = 1"},
		{"SELECT * FROM Task WHERE r = KEY(PROJECT('de\\'mo'), 'Task', 1)",
			"SELECT * FROM Task WHERE r = KEY(PROJECT('de\\'mo'), 'Task', 1)"},
	} {
		if got := rewriteGQLKeys(c[0], "demo", "db-a"); got != c[1] {
			t.Errorf("rewriteGQLKeys(%q)\n = %q\nwant %q", c[0], got, c[1])
		}
	}
	req := &pb.RunQueryRequest{ProjectId: "demo", DatabaseId: "db-a", QueryType: &pb.RunQueryRequest_GqlQuery{GqlQuery: &pb.GqlQuery{
		QueryString: "SELECT * FROM Task WHERE __key__ = KEY(PROJECT('demo'), 'Task', 'x')"}}}
	out, ok := mapRequest(req)
	if !ok || out.GetGqlQuery().GetQueryString() != "SELECT * FROM Task WHERE __key__ = KEY(PROJECT("+ep+"), 'Task', 'x')" {
		t.Errorf("the mapped GQL = %q", out.GetGqlQuery().GetQueryString())
	}
	if req.GetGqlQuery().GetQueryString() != "SELECT * FROM Task WHERE __key__ = KEY(PROJECT('demo'), 'Task', 'x')" {
		t.Error("the caller's request was changed")
	}
}

func TestAnErrorNamesTheProjectNotTheEmulatorProject(t *testing.T) {
	ep := EmulatorProject("demo", "db-a")
	err := errorFromEmulator(status.Error(codes.InvalidArgument, "key project "+ep+" mismatch"), ep)
	if st := status.Convert(err); st.Code() != codes.InvalidArgument || st.Message() != "key project demo mismatch" {
		t.Errorf("error = %v", err)
	}
}
