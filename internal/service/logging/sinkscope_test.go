package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// ListSinks' documented filter (#1577): in_scope("ANCESTOR") lists the
// ancestors' intercepting sinks, in_scope("ALL") the parent's own sinks
// too, the empty filter and in_scope("DEFAULT") the parent's own; anything else is
// INVALID_ARGUMENT.
func TestListSinksInScope(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	a.UseHierarchy(fakeHierarchy{})
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { a.Register(g) }))
	t.Cleanup(srv.Close)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return resp.StatusCode, out
	}
	mk := func(parent, body string) {
		t.Helper()
		if code, out := call(http.MethodPost, "/v2/"+parent+"/sinks", body); code != http.StatusOK {
			t.Fatalf("create in %s = %d %v", parent, code, out)
		}
	}
	mk("projects/child-proj", `{"name":"own","destination":"logging.googleapis.com/projects/p"}`)
	mk("folders/10", `{"name":"grab","destination":"logging.googleapis.com/projects/p","includeChildren":true,"interceptChildren":true}`)
	mk("folders/10", `{"name":"plain","destination":"logging.googleapis.com/projects/p","includeChildren":true}`)
	mk("organizations/1", `{"name":"top","destination":"logging.googleapis.com/projects/p","includeChildren":true,"interceptChildren":true}`)

	list := func(filter string, size int) ([]string, bool) {
		t.Helper()
		var names []string
		intercepting := true
		token := ""
		for {
			q := url.Values{"filter": {filter}}
			if size > 0 {
				q.Set("pageSize", "1")
			}
			if token != "" {
				q.Set("pageToken", token)
			}
			code, out := call(http.MethodGet, "/v2/projects/child-proj/sinks?"+q.Encode(), "")
			if code != http.StatusOK {
				t.Fatalf("list %q = %d %v", filter, code, out)
			}
			ss, _ := out["sinks"].([]any)
			for _, s := range ss {
				m := s.(map[string]any)
				names = append(names, m["name"].(string))
				if m["name"] != "own" && m["name"] != "_Default" && m["name"] != "_Required" && m["interceptChildren"] != true {
					intercepting = false
				}
			}
			token, _ = out["nextPageToken"].(string)
			if token == "" {
				return names, intercepting
			}
		}
	}
	for _, tc := range []struct {
		filter string
		want   []string
	}{
		{"", []string{"own", "_Default", "_Required"}},
		{`in_scope("DEFAULT")`, []string{"own", "_Default", "_Required"}},
		{`in_scope("ANCESTOR")`, []string{"grab", "top"}},
		{`in_scope("ALL")`, []string{"own", "grab", "top"}},
	} {
		for _, size := range []int{0, 1} {
			got, ic := list(tc.filter, size)
			if !slices.Equal(got, tc.want) || !ic {
				t.Errorf("filter %q (page size %d) listed %v (intercepting %v), want %v", tc.filter, size, got, ic, tc.want)
			}
		}
	}
	if code, out := call(http.MethodGet, "/v2/projects/child-proj/sinks?filter="+url.QueryEscape(`name="own"`), ""); code != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %d %v, want 400", code, out)
	}
}
