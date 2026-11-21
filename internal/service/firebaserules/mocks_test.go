package firebaserules

import (
	"strings"
	"testing"

	rules "google.golang.org/api/firebaserules/v1"
)

func TestMockedSourceRewritesServiceCalls(t *testing.T) {
	src := `service cloud.firestore {
  match /databases/{database}/documents {
    // get(x) in a comment stays
    match /a/{id} {
      allow read: if get(/databases/$(database)/documents/u/$(request.auth.uid)).data.role == 'get(' &&
        request.resource.data.get('k', 0) == 0 && exists (/databases/$(database)/documents/x/y);
    }
  }
}`
	mocks := []*rules.FunctionMock{
		{Function: "get", Args: []*rules.Arg{{ExactValue: "/databases/(default)/documents/u/alice"}}, Result: &rules.Result{Value: map[string]any{"data": map[string]any{"role": "admin"}}}},
		{Function: "get", Args: []*rules.Arg{{AnyValue: &rules.Empty{}}}, Result: &rules.Result{Undefined: &rules.Empty{}}},
	}
	if err := checkMocks(0, mocks); err != nil {
		t.Fatal(err)
	}
	out, err := mockedSource(src, mocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if cloudburrowMock_get(/databases/",
		"cloudburrowMock_exists (/databases/",
		"data.get('k', 0)",
		"// get(x) in a comment stays",
		"== 'get('",
		"function cloudburrowMock_get(p) {\n      return ((p is path && p == path('/databases/(default)/documents/u/alice')) || (p is string && p == '/databases/(default)/documents/u/alice')) ? ({'data': {'role': 'admin'}}) : (" + mockError + ");",
		"function cloudburrowMock_exists(p) {\n      return " + mockError + ";",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rewritten source lacks %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "function cloudburrowMock_"), strings.Index(out, "match /databases/{database}/documents {"); i < j {
		t.Errorf("the mocks are defined outside the documents match:\n%s", out)
	}
	if s, err := mockedSource("service cloud.firestore { match /a/{b} { allow read: if true; } }", nil); err != nil || strings.Contains(s, mockPrefix) {
		t.Errorf("a source with no service calls was rewritten: %q %v", s, err)
	}
}

func TestCheckMocks(t *testing.T) {
	anyArg := []*rules.Arg{{AnyValue: &rules.Empty{}}}
	for _, c := range []struct {
		name  string
		mocks []*rules.FunctionMock
	}{
		{"not a service function", []*rules.FunctionMock{{Function: "isAdmin", Args: anyArg, Result: &rules.Result{Value: true}}}},
		{"two args", []*rules.FunctionMock{{Function: "get", Args: append(anyArg, anyArg...), Result: &rules.Result{Value: true}}}},
		{"no result", []*rules.FunctionMock{{Function: "get", Args: anyArg}}},
		{"repeated matchers", []*rules.FunctionMock{{Function: "exists", Args: anyArg, Result: &rules.Result{Value: true}}, {Function: "exists", Args: anyArg, Result: &rules.Result{Value: false}}}},
	} {
		if err := checkMocks(0, c.mocks); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}
