package spannerfront

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
)

func TestEmulatorQueryMode(t *testing.T) {
	in := &spannerpb.ExecuteSqlRequest{Sql: "SELECT 1", QueryMode: spannerpb.ExecuteSqlRequest_WITH_STATS}
	r, strip := emulatorMode(in)
	if !strip || r.GetQueryMode() != spannerpb.ExecuteSqlRequest_PROFILE || in.GetQueryMode() != spannerpb.ExecuteSqlRequest_WITH_STATS {
		t.Errorf("WITH_STATS = %v, %v; want PROFILE to the emulator, the plan dropped, the caller's request unchanged", r, strip)
	}
	r, strip = emulatorMode(&spannerpb.ExecuteSqlRequest{Sql: "SELECT 1"})
	if strip || r.GetQueryMode() != spannerpb.ExecuteSqlRequest_NORMAL {
		t.Errorf("NORMAL = %v, %v; want it passed as it is", r, strip)
	}
}

func TestClaimsRESTQuery(t *testing.T) {
	const path = "/v1/projects/p/instances/i/databases/d/sessions/s1:executeSql"
	for body, want := range map[string]bool{
		`{"sql": "SELECT 1"}`:                            false,
		`{"sql": "SELECT 1", "queryMode": "NORMAL"}`:     false,
		`{"sql": "SELECT 1", "queryMode": "PLAN"}`:       true,
		`{"sql": "SELECT 1", "queryMode": "PROFILE"}`:    true,
		`{"sql": "SELECT 1", "queryMode": "WITH_STATS"}`: true,
		`{"sql": "SELECT 1", "queryMode": 1}`:            true,
	} {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if got := claimsRESTQuery(r); got != want {
			t.Errorf("claimsRESTQuery(%s) = %v, want %v", body, got, want)
		}
		if b, _ := io.ReadAll(r.Body); string(b) != body {
			t.Errorf("the body after the claim = %q, want it put back", b)
		}
	}
	if claimsRESTQuery(httptest.NewRequest("POST", "/v1/projects/p/instances/i/databases/d/sessions/s1:read", strings.NewReader(`{"queryMode": "PLAN"}`))) {
		t.Error("a read is not a query call")
	}
}
