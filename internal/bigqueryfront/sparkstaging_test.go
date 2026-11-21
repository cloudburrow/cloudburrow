package bigqueryfront

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// TestSparkKey (#2104): an INVOKER procedure's CMEK is
// @@spark_proc_properties.kms_key_name, a DEFINER one's its connection's
// kmsKeyName; a key is refused for a multi-region connection, a global
// or multi-region key, and a key outside the connection's region.
func TestSparkKey(t *testing.T) {
	key := "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"
	cn := &bqConnection{Location: "us-central1", Fields: map[string]json.RawMessage{"kmsKeyName": json.RawMessage(`"` + key + `"`)}}
	props := map[string]string{"kms_key_name": "projects/p/locations/us-central1/keyRings/r/cryptoKeys/other"}
	if got := sparkKey(true, props, cn); got != props["kms_key_name"] {
		t.Errorf("INVOKER key %q", got)
	}
	if got := sparkKey(false, props, cn); got != key {
		t.Errorf("DEFINER key %q", got)
	}
	if got := sparkKey(true, nil, cn); got != "" {
		t.Errorf("INVOKER without the variable: %q", got)
	}
	for _, c := range []struct{ key, loc, want string }{
		{key, "us-central1", ""},
		{key, "US-CENTRAL1", ""},
		{"nope", "us-central1", "Invalid kms_key_name"},
		{"projects/p/locations/us/keyRings/r/cryptoKeys/k", "us", "single-region"},
		{"projects/p/locations/global/keyRings/r/cryptoKeys/k", "us-central1", "multi-region CMEK keys"},
		{"projects/p/locations/eu/keyRings/r/cryptoKeys/k", "europe-west1", "multi-region CMEK keys"},
		{"projects/p/locations/us-east1/keyRings/r/cryptoKeys/k", "us-central1", "its connection's region"},
	} {
		got := checkSparkKey(c.key, c.loc)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("checkSparkKey(%q, %q) = %q; want %q", c.key, c.loc, got, c.want)
		}
	}
}

// TestStagedFiles (#2104): a job's inline code and files are staged
// under spark-procedures/JOB/staging/, written with the key as their
// kmsKeyName.
func TestStagedFiles(t *testing.T) {
	job := sparkproc.Job{SparkJobID: "bqsp-1", Code: "print(1)", Main: &sparkproc.File{URI: "gs://b/d/main2.py", Data: []byte("m")},
		PyFiles: []sparkproc.File{{URI: "gs://b/lib.py", Data: []byte("l")}}, Jars: []sparkproc.File{{URI: "gs://b/x.jar", Data: []byte("j")}}}
	st := sparkStage{bucket: "stage", key: "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"}
	var names []string
	for _, o := range stagedFiles(st, job) {
		names = append(names, o.name)
	}
	want := "spark-procedures/bqsp-1/staging/main.py spark-procedures/bqsp-1/staging/main2.py " +
		"spark-procedures/bqsp-1/staging/lib.py spark-procedures/bqsp-1/staging/x.jar"
	if strings.Join(names, " ") != want {
		t.Errorf("staged %v", names)
	}
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Query().Get("name")] = r.URL.Query().Get("kmsKeyName") + "|" + string(b)
		mu.Unlock()
		if r.URL.Query().Get("name") == "fail" {
			http.Error(w, `{"error":{"message":"the key is disabled"}}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	s := &scriptRun{f: front{storageHost: strings.TrimPrefix(srv.URL, "http://")}}
	if err := s.stageSparkFiles(context.Background(), st, job); err != nil {
		t.Fatal(err)
	}
	if err := s.stageDriverOutput(context.Background(), st, job, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if g := got["spark-procedures/bqsp-1/staging/main.py"]; g != st.key+"|print(1)" {
		t.Errorf("main.py written %q", g)
	}
	if g := got["spark-procedures/bqsp-1/driveroutput.000000000"]; g != st.key+"|hello\n" {
		t.Errorf("driver output written %q", g)
	}
	if err := s.f.uploadKeyed(context.Background(), "gs://stage/fail", "text/plain", nil, st.key); err == nil ||
		!strings.Contains(err.Error(), "the key is disabled") {
		t.Errorf("a refused write: %v", err)
	}
	// No staging bucket: nothing is written.
	got = map[string]string{}
	if err := s.stageSparkFiles(context.Background(), sparkStage{}, job); err != nil || len(got) != 0 {
		t.Errorf("no bucket: %v, %v", err, got)
	}
}
