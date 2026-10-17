package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// TestBigQueryResetProjectDeletesItsDatasets: a project's reset (#1540)
// deletes each of the project's datasets with its contents, and a project
// the emulator does not serve (404) has nothing to delete.
func TestBigQueryResetProjectDeletesItsDatasets(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[strings.Index(r.URL.Path, "/projects/"):]
		switch {
		case r.Method == http.MethodGet && path == "/projects/served/datasets":
			if r.URL.Query().Get("all") != "true" {
				t.Errorf("list without all=true: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"datasets": []any{
				map[string]any{"id": "served:a", "datasetReference": map[string]string{"projectId": "served", "datasetId": "a"}},
				map[string]any{"id": "served:_hidden", "datasetReference": map[string]string{"projectId": "served", "datasetId": "_hidden"}},
			}})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"project not found"}}`))
		case r.Method == http.MethodDelete:
			if r.URL.Query().Get("deleteContents") != "true" || r.Header.Get(bigqueryfront.PurgeHeader) == "" {
				t.Errorf("delete without deleteContents: %s", r.URL.RawQuery)
			}
			mu.Lock()
			deleted = append(deleted, path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	if bigqueryPurgeHeader != bigqueryfront.PurgeHeader || bigqueryDroppedDataset != bigqueryfront.DroppedDatasetID {
		t.Fatal("the purge's header or the dropped dataset differs from the front's")
	}
	f := forwarderAt(t, srv.Listener.Addr().String())
	b := &bigqueryResetter{tunnel: func() *netfwd.Forwarder { return f }, projects: func() []string { return []string{"served", "other"} }}
	if err := b.ResetProject(context.Background(), "served"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "/projects/served/datasets/a,/projects/served/datasets/_hidden,/projects/served/datasets/"+bigqueryfront.DroppedDatasetID {
		t.Errorf("deleted = %v", deleted)
	}
	if err := b.ResetProject(context.Background(), "other"); err != nil {
		t.Errorf("a project the emulator does not serve: %v", err)
	}
}
