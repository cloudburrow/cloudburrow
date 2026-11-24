package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// tokenInstance is an InstanceSource whose admin API accepts one token.
type tokenInstance struct{ *loadCounter }

func (tokenInstance) Info(_ context.Context, authorization string) (json.RawMessage, error) {
	if authorization != "Bearer good" {
		return nil, &InstanceRefusal{Status: http.StatusUnauthorized, Message: "the admin token is missing or wrong"}
	}
	return json.RawMessage(`{}`), nil
}

func (t tokenInstance) Reset(ctx context.Context, a string, q url.Values) (json.RawMessage, error) {
	return t.loadCounter.Reset(ctx, a, q)
}

type fakeCompute struct {
	reconnects int
	fail       error
}

func (f *fakeCompute) Compute() any {
	return map[string]any{"location": "ssh", "target": "ssh://dev@box", "connected": true, "reconnects": f.reconnects}
}

func (f *fakeCompute) Reconnect(context.Context) error {
	if f.fail != nil {
		return f.fail
	}
	f.reconnects++
	return nil
}

// TestComputePlacement (#2209): GET /api/instance/compute says where the
// compute runs, this machine's Docker without a source; Reconnect is
// refused without the admin token, refused on local compute, and with the
// token drops the connection and answers the new state.
func TestComputePlacement(t *testing.T) {
	do := func(srv *httptest.Server, method, path, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return res.StatusCode, out
	}

	local := New("127.0.0.1:0", nil)
	local.SetInstance(tokenInstance{&loadCounter{}}, "")
	ls := httptest.NewServer(local.Handler())
	t.Cleanup(ls.Close)
	if code, v := do(ls, http.MethodGet, "/api/instance/compute", ""); code != http.StatusOK || v["location"] != "local" {
		t.Fatalf("local compute = %d %v", code, v)
	}
	if code, v := do(ls, http.MethodPost, "/api/instance/compute/reconnect", "good"); code != http.StatusPreconditionFailed ||
		!strings.Contains(v["error"].(string), "this machine's Docker") {
		t.Fatalf("reconnect on local compute = %d %v", code, v)
	}

	fc := &fakeCompute{}
	remote := New("127.0.0.1:0", nil)
	remote.SetInstance(tokenInstance{&loadCounter{}}, "")
	remote.SetCompute(fc)
	rs := httptest.NewServer(remote.Handler())
	t.Cleanup(rs.Close)
	if code, v := do(rs, http.MethodGet, "/api/instance/compute", ""); code != http.StatusOK || v["location"] != "ssh" || v["target"] != "ssh://dev@box" {
		t.Fatalf("remote compute = %d %v", code, v)
	}
	for _, token := range []string{"", "bad"} {
		if code, _ := do(rs, http.MethodPost, "/api/instance/compute/reconnect", token); code != http.StatusUnauthorized || fc.reconnects != 0 {
			t.Fatalf("reconnect with token %q = %d, %d reconnects; want 401 and none", token, code, fc.reconnects)
		}
	}
	if code, v := do(rs, http.MethodPost, "/api/instance/compute/reconnect", "good"); code != http.StatusOK || v["reconnects"] != float64(1) {
		t.Fatalf("reconnect = %d %v", code, v)
	}
	fc.fail = errors.New("connection refused")
	if code, v := do(rs, http.MethodPost, "/api/instance/compute/reconnect", "good"); code != http.StatusServiceUnavailable ||
		!strings.Contains(v["error"].(string), "connection refused") {
		t.Fatalf("a failed reconnect = %d %v", code, v)
	}
}
