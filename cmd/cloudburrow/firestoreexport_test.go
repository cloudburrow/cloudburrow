package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// TestStopExportsTheFirestoreDocumentsFirst (#1615): `stop` asks the
// Firestore front to export before it stops the cluster, says when it
// did, and warns, naming what may be lost, when the export failed.
func TestStopExportsTheFirestoreDocumentsFirst(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != firestoreadmin.ExportPath {
			t.Errorf("%s %s; want POST %s", r.Method, r.URL.Path, firestoreadmin.ExportPath)
		}
		asked++
		switch asked {
		case 1:
			_, _ = w.Write([]byte(`{"exported":true,"seconds":1.4}`))
		case 2:
			_, _ = w.Write([]byte(`{"exported":false}`))
		default:
			http.Error(w, `{"error":"the emulator does not hold the documents yet"}`, http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for i, want := range []string{"exported the Firestore documents in ", "", "warning: the Firestore documents were not exported"} {
		var out strings.Builder
		exportFirestoreDocuments(addr, 5*time.Second, &out)
		if got := out.String(); (want == "") != (got == "") || !strings.HasPrefix(got, want) {
			t.Errorf("call %d: output %q; want it to start %q", i+1, got, want)
		}
	}
	var out strings.Builder
	exportFirestoreDocuments("", time.Second, &out)
	if out.Len() != 0 || asked != 3 {
		t.Errorf("without a Firestore endpoint: output %q, %d calls; want neither", out.String(), asked)
	}
}
