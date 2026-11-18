package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBigQueryConnectionsSummary (#1755): the datasets list names the
// project's connections, their types and service accounts.
func TestBigQueryConnectionsSummary(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us/connections" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"connections":[{"name":"projects/1/locations/us/connections/ai","cloudResource":{"serviceAccountId":"bqcx-1-ab@gcp-sa-bigquery-condel.iam.gserviceaccount.com"}}]}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	g := p.connectionsSummary(context.Background(), "p")
	if len(g.Properties) != 1 || g.Properties[0].Label != "ai" || !strings.Contains(g.Properties[0].Value, "cloudResource, service account bqcx-1-ab@") {
		t.Errorf("summary %+v", g)
	}
	if g := p.connectionsSummary(context.Background(), "other"); len(g.Properties) != 1 || !strings.HasPrefix(g.Properties[0].Value, "unavailable") {
		t.Errorf("a failed list %+v", g)
	}
}

// TestBigQueryConnectionActions (#1760): the datasets page creates, edits
// and deletes a connection, each one Connection API call, and shows the
// API's own error.
func TestBigQueryConnectionActions(t *testing.T) {
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":404,"message":"Not found: Connection missing"}}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ids := map[string]bool{}
	for _, a := range connectionActions() {
		ids[a.ID] = true
	}
	if !ids[actCreateConnection] || !ids[actEditConnection] || !ids[actDeleteConnection] {
		t.Fatalf("actions %v", ids)
	}
	ctx := context.Background()
	if err := p.connectionAct(ctx, actCreateConnection, map[string]string{"connection": "ai", "friendlyName": "AI"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actEditConnection, map[string]string{"connection": "ai", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{"connection": "ai"}); err != nil {
		t.Fatal(err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{"connection": "missing"}); err == nil || !strings.Contains(err.Error(), "Not found: Connection missing") {
		t.Errorf("a missing connection: %v", err)
	}
	if err := p.connectionAct(ctx, actDeleteConnection, map[string]string{}); err == nil {
		t.Error("no ID was accepted")
	}
	want := []string{
		`POST /v1/projects/p/locations/us/connections?connectionId=ai {"cloudResource":{},"friendlyName":"AI"}`,
		`PATCH /v1/projects/p/locations/us/connections/ai?updateMask=friendlyName%2Cdescription {"description":"d","friendlyName":""}`,
		`DELETE /v1/projects/p/locations/us/connections/ai? `,
		`DELETE /v1/projects/p/locations/us/connections/missing? `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(got, "\n"))
	}
}
