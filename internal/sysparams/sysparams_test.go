package sysparams

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func TestParse(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"name", "name"},
		{"name,state", "name,state"},
		{" name , state ", "name,state"},
		{"items(id,name)", "items(id,name)"},
		{"items/id,items/name", "items(id,name)"},
		{"a/b/c", "a(b(c))"},
		{"a.b.c", "a(b(c))"},
		{"a(b(c,d),e/f),g", "a(b(c,d),e(f)),g"},
		{"items/*", "items(*)"},
		{"*", "*"},
		{"a/b,a", "a"}, // the whole field wins
		{"a,a/b", "a"},
		{"kind,items(title,characteristics/length)", "items(characteristics(length),title),kind"},
	} {
		m, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got := m.String(); got != c.want {
			t.Errorf("Parse(%q) = %q; want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{",", "a,", "a,,b", "a/", "(a)", "a(b", "a(b))", "a)", "a()", "a*", "a b"} {
		_, err := Parse(in)
		e := apierror.From(err)
		if err == nil || e.Code != codes.InvalidArgument || e.Message != "Invalid field selection "+in {
			t.Errorf("Parse(%q) = %v; want INVALID_ARGUMENT Invalid field selection %s", in, err, in)
		}
	}
}

func prune(t *testing.T, spec, in string) string {
	t.Helper()
	m, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m.Prune(v))
	return string(b)
}

func TestPrune(t *testing.T) {
	doc := `{"kind":"k","nextPageToken":"t","items":[{"id":"1","name":"a","meta":{"x":1,"y":2}},{"id":"2","name":"b"}],` +
		`"response":{"@type":"type.googleapis.com/X","name":"n","other":true}}`
	for _, c := range []struct{ spec, want string }{
		{"", doc},
		{"kind", `{"kind":"k"}`},
		{"items(id)", `{"items":[{"id":"1"},{"id":"2"}]}`},
		{"items(id,meta/y),nextPageToken", `{"items":[{"id":"1","meta":{"y":2}},{"id":"2"}],"nextPageToken":"t"}`},
		{"items/meta", `{"items":[{"meta":{"x":1,"y":2}},{}]}`},
		{"response(name)", `{"response":{"@type":"type.googleapis.com/X","name":"n"}}`},
		{"absent", `{}`},
		{"kind/sub", `{"kind":"k"}`},
		{"items/*", `{"items":[{"id":"1","meta":{"x":1,"y":2},"name":"a"},{"id":"2","name":"b"}]}`},
		{"*", `{"items":[{"id":"1","meta":{"x":1,"y":2},"name":"a"},{"id":"2","name":"b"}],"kind":"k","nextPageToken":"t","response":{"@type":"type.googleapis.com/X","name":"n","other":true}}`},
	} {
		got := prune(t, c.spec, doc)
		if c.spec == "" {
			var a, b any
			_ = json.Unmarshal([]byte(got), &a)
			_ = json.Unmarshal([]byte(doc), &b)
			ab, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			if string(ab) != string(bb) {
				t.Errorf("empty mask changed the document: %s", got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("prune %q = %s; want %s", c.spec, got, c.want)
		}
	}
}

func TestValidateAndPruneProto(t *testing.T) {
	md := (&schedulerpb.ListJobsResponse{}).ProtoReflect().Descriptor()
	for _, ok := range []string{"jobs", "jobs(name,state)", "jobs/httpTarget/uri", "jobs/http_target/uri", "next_page_token", "*", "jobs/*"} {
		m, _ := Parse(ok)
		if err := m.Validate(md); err != nil {
			t.Errorf("Validate(%q): %v", ok, err)
		}
	}
	for spec, want := range map[string]string{"bogus": "bogus", "jobs(bogus)": "jobs/bogus", "nextPageToken/x": "nextPageToken/x"} {
		m, _ := Parse(spec)
		if err := m.Validate(md); err == nil || apierror.From(err).Message != "Invalid field selection "+want {
			t.Errorf("Validate(%q) = %v; want Invalid field selection %s", spec, err, want)
		}
	}
	// An Any accepts any selection.
	m, _ := Parse("response(anything/at/all),done")
	if err := m.Validate((&longrunningpb.Operation{}).ProtoReflect().Descriptor()); err != nil {
		t.Errorf("Validate under Any: %v", err)
	}

	resp := &schedulerpb.ListJobsResponse{NextPageToken: "t", Jobs: []*schedulerpb.Job{
		{Name: "a", Description: "d", State: schedulerpb.Job_ENABLED, Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "u", Body: []byte("b")}}},
	}}
	m, _ = Parse("jobs(name,httpTarget/uri)")
	m.PruneProto(resp.ProtoReflect())
	want := &schedulerpb.ListJobsResponse{Jobs: []*schedulerpb.Job{{Name: "a", Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "u"}}}}}
	if !proto.Equal(resp, want) {
		t.Errorf("PruneProto = %v; want %v", resp, want)
	}

	// An Any is unpacked, pruned and packed again.
	inner, _ := anypb.New(&schedulerpb.Job{Name: "n", Description: "d"})
	op := &longrunningpb.Operation{Name: "o", Done: true, Result: &longrunningpb.Operation_Response{Response: inner}}
	m, _ = Parse("name,response(name)")
	m.PruneProto(op.ProtoReflect())
	var job schedulerpb.Job
	if err := op.GetResponse().UnmarshalTo(&job); err != nil || op.GetDone() || job.GetName() != "n" || job.GetDescription() != "" {
		t.Errorf("PruneProto under Any = %v (%v), job %v", op, err, &job)
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?$fields=a&prettyPrint=false&$.xgafv=1&callback=cb&alt=json;enum-encoding=int&quotaUser=q&$outputDefaults=true", nil)
	p, err := FromRequest(r)
	if err != nil || p.Fields.String() != "a" || p.Pretty || !p.PrettySet || p.XGAFV != 1 || p.Callback != "cb" ||
		p.Alt != "json" || !p.EnumInt || !p.OutputDefaults {
		t.Errorf("FromRequest = %+v, %v", p, err)
	}
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Goog-FieldMask", "a.b,c")
	if p, err := FromRequest(r); err != nil || p.Fields.String() != "a(b),c" || !p.Pretty || p.XGAFV != 2 {
		t.Errorf("FromRequest header = %+v, %v", p, err)
	}
	for _, q := range []string{"fields=a&$fields=b", "prettyPrint=maybe", "$.xgafv=0", "callback=alert(1)", "alt=json;x=y"} {
		if _, err := FromRequest(httptest.NewRequest("GET", "/x?"+q, nil)); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("FromRequest(%s) = %v; want INVALID_ARGUMENT", q, err)
		}
	}
}

func TestWrap(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"k","items":[{"id":"1","name":"a"}]}`)
		case "/err":
			apierror.WriteJSON(w, apierror.NotFound("gone"))
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, `{"kind":"k"}`)
		case "/handled":
			MarkHandled(r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"k","x":1}`)
		case "/stream":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"k",`)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, `"x":1}`)
		}
	})
	srv := httptest.NewServer(Wrap(inner, Options{}))
	defer srv.Close()
	get := func(path string) (int, string, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	for _, c := range []struct {
		path string
		code int
		want string
	}{
		{"/ok", 200, `{"kind":"k","items":[{"id":"1","name":"a"}]}`},
		{"/ok?alt=json", 200, `{"kind":"k","items":[{"id":"1","name":"a"}]}`},
		{"/ok?fields=items(id)&prettyPrint=false", 200, `{"items":[{"id":"1"}]}`},
		{"/ok?fields=kind", 200, `{"kind":"k"}`},
		{"/ok?fields=kind&prettyPrint=true", 200, "{\n  \"kind\": \"k\"\n}\n"},
		{"/ok?fields=kind&callback=f&prettyPrint=false", 200, "// API callback\nf({\"kind\":\"k\"});\n"},
		{"/bin?fields=nothing", 200, `{"kind":"k"}`},
		{"/handled?fields=kind", 200, `{"kind":"k","x":1}`},
		{"/stream?fields=kind", 200, `{"kind":"k","x":1}`},
		{"/ok?fields=a(", 400, ""},
	} {
		code, _, body := get(c.path)
		if code != c.code || (c.want != "" && body != c.want) {
			t.Errorf("GET %s = %d %q; want %d %q", c.path, code, body, c.code, c.want)
		}
		if c.code == 400 && !strings.Contains(body, "Invalid field selection a(") {
			t.Errorf("GET %s: %s", c.path, body)
		}
	}
	code, _, body := get("/err?$.xgafv=1&prettyPrint=false")
	if code != 404 || !strings.Contains(body, `"errors":[{"domain":"global","message":"gone","reason":"notFound"}]`) {
		t.Errorf("v1 error = %d %s", code, body)
	}
}

func TestInterceptors(t *testing.T) {
	ic := UnaryServerInterceptor()
	held := &schedulerpb.Job{Name: "n", Description: "d"}
	handler := func(context.Context, any) (any, error) { return held, nil }
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(FieldMaskMetadata, "name"))
	resp, err := ic(ctx, nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil || resp.(*schedulerpb.Job).GetDescription() != "" || resp.(*schedulerpb.Job).GetName() != "n" {
		t.Errorf("unary = %v, %v", resp, err)
	}
	if held.GetDescription() != "d" {
		t.Error("the interceptor pruned the handler's own message")
	}
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(FieldMaskMetadata, "bogus"))
	if _, err := ic(ctx, nil, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unary bad mask = %v", err)
	}
	if resp, err := ic(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); err != nil || resp != held {
		t.Errorf("unary without a mask = %v, %v", resp, err)
	}
}
