package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
)

// TestBigQueryEditDataset (#1205): the dataset page offers Edit dataset,
// drawn from the dataset, and saving it sends a datasets.patch of what
// changed, through the front, which the dataset then reads back.
func TestBigQueryEditDataset(t *testing.T) {
	var mu sync.Mutex
	content := `{"kind":"bigquery#dataset","id":"served-project:d","location":"US",` +
		`"datasetReference":{"projectId":"served-project","datasetId":"d"},"description":"old","labels":{"a":"1","b":"2"}}`
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/datasets/d") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
			return
		}
		if r.Method == http.MethodPut { // patch 0010: the update is saved
			b, _ := io.ReadAll(r.Body)
			content = string(b)
		}
		_, _ = io.WriteString(w, content)
	})
	srv := httptest.NewServer(bigqueryfront.Wrap(emulator))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}

	var edit []string
	for _, a := range p.DetailActions(ctx, "served-project", []string{"d"}) {
		if a.ID == actEditDataset {
			for _, f := range a.Fields {
				edit = append(edit, f.Name+"="+f.Default)
			}
		}
	}
	if len(edit) == 0 || edit[1] != "description=old" || !strings.HasPrefix(edit[2], "labels=") {
		t.Fatalf("Edit dataset's fields = %v", edit)
	}

	err := p.ActAt(ctx, "served-project", []string{"d"}, actEditDataset, map[string]string{
		"friendlyName": "Friendly", "description": "new", "labels": `{"a":"9","c":"3"}`,
		"defaultTableExpiration": "2", "defaultPartitionExpiration": "", "defaultCollation": "und:ci",
		"maxTimeTravelHours": "72", "access": `[{"role":"READER","specialGroup":"projectReaders"}]`,
	})
	if err != nil {
		t.Fatalf("Edit dataset: %v", err)
	}
	md, err := p.datasetMetadata(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if md.Name != "Friendly" || md.Description != "new" || md.Labels["a"] != "9" || md.Labels["c"] != "3" || len(md.Labels) != 2 ||
		md.DefaultTableExpiration != 2*time.Hour || md.DefaultCollation != "und:ci" || md.MaxTimeTravel != 72*time.Hour ||
		len(md.Access) != 1 || md.Access[0].EntityType != bigquery.SpecialGroupEntity {
		t.Errorf("after Edit dataset: %+v", md)
	}

	if err := p.ActAt(ctx, "served-project", []string{"d"}, actEditDataset, map[string]string{"description": "new"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an unchanged form = %v, want nothing to change", err)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d"}, actEditDataset, map[string]string{"maxTimeTravelHours": "36"}); err == nil ||
		!strings.Contains(err.Error(), "maxTimeTravelHours") {
		t.Errorf("a 36-hour window = %v, want the API's refusal", err)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d"}, actEditDataset, map[string]string{"access": `[{"role":"READER"}]`}); err == nil ||
		!strings.Contains(err.Error(), "access entry 1") {
		t.Errorf("an entry with no entity = %v", err)
	}
}
