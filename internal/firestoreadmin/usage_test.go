package firestoreadmin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestClientAuthSendsServerLibrariesAsTheOwner(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	firebase := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://securetoken.google.com/p","sub":"alice"}`)) + "."
	other := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{"iss":"https://accounts.google.com","sub":"x"}`)) + ".c2ln"
	doc := "/v1/projects/p/databases/(default)/documents/users/alice"
	for _, c := range []struct {
		name, path, auth, client, want string
	}{
		{"no credentials", doc, "", "gl-go/1.26 gccl/1.0", ownerToken},
		{"an OAuth token", doc, "Bearer ya29.local", "", ownerToken},
		{"a Google ID token", doc, "Bearer " + other, "", ownerToken},
		{"a Firebase ID token", doc, "Bearer " + firebase, "", "Bearer " + firebase},
		{"an unauthenticated Firebase SDK", doc, "", "gl-js/ fire/12.0.0", ""},
		{"the web SDK's WebChannel", "/google.firestore.v1.Firestore/Listen/channel", "", "", ""},
		{"an admin call", "/v1/projects/p/databases", "", "", ""},
	} {
		var got string
		h := ClientAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if c.client != "" {
			r.Header.Set("X-Goog-Api-Client", c.client)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("%s: the emulator sees Authorization %q, want %q", c.name, got, c.want)
		}
	}
}

func TestQueryShapeReplacesValues(t *testing.T) {
	q := &firestorepb.StructuredQuery{
		From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: "posts"}},
		Where: &firestorepb.StructuredQuery_Filter{FilterType: &firestorepb.StructuredQuery_Filter_CompositeFilter{
			CompositeFilter: &firestorepb.StructuredQuery_CompositeFilter{Op: firestorepb.StructuredQuery_CompositeFilter_AND,
				Filters: []*firestorepb.StructuredQuery_Filter{
					{FilterType: &firestorepb.StructuredQuery_Filter_FieldFilter{FieldFilter: &firestorepb.StructuredQuery_FieldFilter{
						Field: &firestorepb.StructuredQuery_FieldReference{FieldPath: "n"}, Op: firestorepb.StructuredQuery_FieldFilter_GREATER_THAN}}},
					{FilterType: &firestorepb.StructuredQuery_Filter_FieldFilter{FieldFilter: &firestorepb.StructuredQuery_FieldFilter{
						Field: &firestorepb.StructuredQuery_FieldReference{FieldPath: "tag"}, Op: firestorepb.StructuredQuery_FieldFilter_EQUAL}}},
				}}}},
		OrderBy: []*firestorepb.StructuredQuery_Order{{Field: &firestorepb.StructuredQuery_FieldReference{FieldPath: "n"},
			Direction: firestorepb.StructuredQuery_DESCENDING}},
		Limit: wrapperspb.Int32(5),
	}
	got := queryShape("projects/p/databases/(default)/documents/users/alice", q)
	if want := "/users/*/posts | where n > ? and tag == ? | order by n descending | limit ?"; got != want {
		t.Errorf("queryShape = %q, want %q", got, want)
	}
}

func TestUsageCountsRESTReadsWritesAndQueries(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":runQuery"):
			_, _ = w.Write([]byte(`[{"document":{"name":"projects/p/databases/(default)/documents/items/a"}},` +
				`{"document":{"name":"projects/p/databases/(default)/documents/items/b"}},{"readTime":"2026-11-20T00:00:00Z"}]`))
		case strings.HasSuffix(r.URL.Path, ":commit"):
			_, _ = w.Write([]byte(`{"writeResults":[{},{}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	a := New("")
	u := a.Usage()
	h := u.Wrap(upstream)
	base := "/v1/projects/p/databases/(default)/documents"
	do := func(method, path, body string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	do(http.MethodGet, base+"/items/a", "")
	do(http.MethodGet, base+"/items/missing", "")
	do(http.MethodPost, base+":runQuery", `{"structuredQuery":{"from":[{"collectionId":"items"}],"where":{"fieldFilter":{"field":{"fieldPath":"n"},"op":"GREATER_THAN","value":{"integerValue":"0"}}}}}`)
	// The emulator is not reachable, so neither document is known to exist:
	// the update is a create, and the delete a no-op.
	do(http.MethodPost, base+":commit", `{"writes":[{"update":{"name":"projects/p/databases/(default)/documents/items/c"}},{"delete":"projects/p/databases/(default)/documents/items/d"}]}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, UsagePath+"?since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil))
	var got struct{ Minutes []UsageMinute }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	reads, writes := map[string]int64{}, map[string]int64{}
	var deletes int64
	var queries []*QueryStat
	for _, m := range got.Minutes {
		d := m.Databases["projects/p/databases/(default)"]
		if d == nil {
			continue
		}
		for k, n := range d.Reads {
			reads[k] += n
		}
		for k, n := range d.Writes {
			writes[k] += n
		}
		deletes += d.Deletes
		for _, q := range d.Queries {
			queries = append(queries, q)
		}
	}
	if reads["LOOKUP"] != 1 || reads["QUERY"] != 2 || writes["CREATE"] != 1 || deletes != 0 {
		t.Errorf("counts: reads %v, writes %v, deletes %d; want 1 lookup, 2 query reads, 1 create, no delete", reads, writes, deletes)
	}
	if len(queries) != 1 || queries[0].Shape != "/items | where n > ?" || queries[0].Executions != 1 || queries[0].Results != 2 {
		t.Errorf("queries = %+v; want one execution of /items | where n > ? with 2 results", queries)
	}
}
