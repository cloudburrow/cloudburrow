package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// routinesAPI is a BigQuery REST API with one dataset d holding a function
// f and a table function tf; it records each DELETE.
func routinesAPI(t *testing.T, deleted *[]string) *httptest.Server {
	t.Helper()
	routines := map[string]string{
		"f": `{"routineReference":{"projectId":"p","datasetId":"d","routineId":"f"},"routineType":"SCALAR_FUNCTION","language":"SQL",
			"arguments":[{"name":"x","dataType":{"typeKind":"INT64"}},{"name":"a","dataType":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"STRING"}}}],
			"returnType":{"typeKind":"INT64"},"definitionBody":"x + 1","creationTime":"1700000000000","lastModifiedTime":"1700000000000"}`,
		"tf": `{"routineReference":{"projectId":"p","datasetId":"d","routineId":"tf"},"routineType":"TABLE_VALUED_FUNCTION","language":"SQL",
			"returnTableType":{"columns":[{"name":"y","type":{"typeKind":"INT64"}}]},"definitionBody":"SELECT 1 AS y"}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/bigquery/v2")
		switch {
		case r.Method == http.MethodGet && path == "/projects/p/datasets/d/routines":
			_, _ = io.WriteString(w, `{"routines":[{"routineReference":{"projectId":"p","datasetId":"d","routineId":"f"}},`+
				`{"routineReference":{"projectId":"p","datasetId":"d","routineId":"tf"}}]}`)
		case strings.HasPrefix(path, "/projects/p/datasets/d/routines/"):
			id := strings.TrimPrefix(path, "/projects/p/datasets/d/routines/")
			body, ok := routines[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Not found: Routine p:d.`+id+`","errors":[{"reason":"notFound","message":"Not found"}]}}`)
				return
			}
			if r.Method == http.MethodDelete {
				*deleted = append(*deleted, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
}

// TestBigQueryRoutinesSectionAndPage (#782): a dataset's Routines section
// lists each routine with its type, language and arguments, opening its
// page at [dataset, routines, routine] and offering Delete routine; a
// routine's page shows its body, arguments and return type; Delete routine
// sends routines.delete; a page of a routine that is gone says so.
func TestBigQueryRoutinesSectionAndPage(t *testing.T) {
	var deleted []string
	api := routinesAPI(t, &deleted)
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()

	c, err := p.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sec := p.routinesSection(ctx, c.Dataset("d"))
	if sec.ID != "routines" || sec.Listing.Unavailable != "" || len(sec.Listing.Items) != 2 {
		t.Fatalf("the Routines section = %+v", sec)
	}
	f, tf := sec.Listing.Items[0], sec.Listing.Items[1]
	if f.Name != "f" || f.Fields["Type"] != "Function" || f.Fields["Language"] != "SQL" || f.Fields["Arguments"] != "x INT64, a ARRAY<STRING>" ||
		strings.Join(f.Opens, "/") != "d/routines/f" || len(f.Actions) != 1 || f.Actions[0].ID != actDeleteRoutine {
		t.Errorf("the f row = %+v", f)
	}
	if tf.Fields["Type"] != "Table function" || tf.Fields["Arguments"] != "—" {
		t.Errorf("the tf row = %+v", tf)
	}

	d, err := p.Detail(ctx, "p", []string{"d", "routines", "f"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the routine's page: %v %+v", err, d)
	}
	if len(d.Trail) != 2 || strings.Join(d.Trail[0].Path, "/") != "d" || d.Trail[1].Label != "f" {
		t.Errorf("the routine page's trail = %+v, want the dataset, then the routine", d.Trail)
	}
	var body, returns string
	args := 0
	for _, s := range d.Sections {
		switch s.ID {
		case "body":
			body = s.Text
		case "arguments":
			args = len(s.Listing.Items)
		case "details":
			for _, g := range s.Groups {
				for _, pr := range g.Properties {
					if pr.Label == "Returns" {
						returns = pr.Value
					}
				}
			}
		}
	}
	if body != "x + 1" || args != 2 || returns != "INT64" {
		t.Errorf("the routine's page: body %q, %d arguments, returns %q", body, args, returns)
	}
	if d, _ := p.Detail(ctx, "p", []string{"d", "routines", "tf"}); !strings.Contains(propertiesText(d), "TABLE<y INT64>") {
		t.Errorf("the table function's page does not show its return table: %+v", d)
	}
	if acts := p.DetailActions(ctx, "p", []string{"d", "routines", "f"}); len(acts) != 1 || acts[0].ID != actDeleteRoutine || !acts[0].Destructive {
		t.Errorf("the routine page's actions = %+v", acts)
	}

	if err := p.ActAt(ctx, "p", []string{"d", "routines", "f"}, actDeleteRoutine, nil); err != nil {
		t.Fatalf("Delete routine: %v", err)
	}
	if strings.Join(deleted, ",") != "f" {
		t.Errorf("deleted = %v, want [f]", deleted)
	}
	if d, _ := p.Detail(ctx, "p", []string{"d", "routines", "gone"}); !strings.Contains(d.Unavailable, "cannot read the routine") {
		t.Errorf("a gone routine's page = %+v", d)
	}
	if err := p.ActAt(ctx, "p", []string{"d", "routines", "gone"}, actDeleteRoutine, nil); err == nil {
		t.Error("Delete routine of a routine that is gone succeeded")
	}
}

// propertiesText is every property value of a page's sections, joined.
func propertiesText(d console.Detail) string {
	var b strings.Builder
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				b.WriteString(pr.Value + "\n")
			}
		}
	}
	return b.String()
}
