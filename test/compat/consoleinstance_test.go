//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleInstanceDo is one request to the console's Instance endpoints (#801),
// with the admin token when token is true, as the Instance page sends it.
func consoleInstanceDo(t *testing.T, addr, method, path, contentType string, body []byte, token bool) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token {
		req.Header.Set("Authorization", "Bearer "+adminToken(t))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// TestConsoleStateSaveResetLoadRestores (#801): what
// TestStateSaveResetLoadRestoresEverything checks through the CLI, through
// the console's Instance endpoints instead. A bucket with an object, a secret
// with a version and a queue, made with the official clients, are saved from
// the console (an archive whose manifest names storage, secretmanager and
// tasks as captured), reset from the console for this project only, which
// leaves another project's queue alone, and loaded back from the console;
// the official clients then read all three, the object's bytes and the
// secret's value as they were. A seed document uploaded twice with If not
// exists creates its queue once, and a third time without it is refused with
// the admin API's conflict. Without the admin token each endpoint answers 401
// and changes nothing (#553).
func TestConsoleStateSaveResetLoadRestores(t *testing.T) {
	h, bystander := New(t), New(t)
	addr := consoleAddr(t, h)
	h.Endpoint(EnvStorage)
	st := storageClient(t, h)
	sc := secretsClient(t, h)
	tc := tasksClient(t, h)
	if adminToken(t) == "" {
		t.Skipf("%s is not set and no instance directory is known; the console's Instance endpoints need the token", EnvAdminToken)
	}
	ctx := h.Context()

	bucket := st.Bucket(h.Project() + "-console-state")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bucket) })
	w := bucket.Object("kept.txt").NewWriter(ctx)
	w.ContentType = "text/plain"
	if _, err := w.Write([]byte("kept by the console")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write object: %v", err)
	}
	sec, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "console-state-secret",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name})
	})
	if _, err := sc.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("console value")}}); err != nil {
		t.Fatal(err)
	}
	q := queue(t, h, tc, "console-state-queue")
	kept := queue(t, bystander, tc, "console-state-bystander")

	// Without the token: refused, and nothing is touched.
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/instance"},
		{http.MethodPost, "/api/instance/save"},
		{http.MethodPost, "/api/instance/reset?service=tasks&project=" + h.Project()},
	} {
		if code, body := consoleInstanceDo(t, addr, r.method, r.path, "", nil, false); code != http.StatusUnauthorized {
			t.Fatalf("%s %s without the admin token = %d %.300s; want 401", r.method, r.path, code, body)
		}
	}
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err != nil {
		t.Fatalf("the queue after a refused reset: %v", err)
	}

	code, archive := consoleInstanceDo(t, addr, http.MethodPost, "/api/instance/save", "", nil, true)
	if code != http.StatusOK {
		t.Fatalf("save from the console = %d %.300s", code, archive)
	}
	captured := map[string]bool{}
	for _, s := range firstEntryManifest(t, archive).Services {
		captured[s.Name] = s.Captured
	}
	for _, s := range []string{"storage", "secretmanager", "tasks"} {
		if !captured[s] {
			t.Errorf("the console's archive does not capture %s: %v", s, captured)
		}
	}

	scope := "/api/instance/reset?service=storage&service=secretmanager&service=tasks&project=" + h.Project()
	if code, body := consoleInstanceDo(t, addr, http.MethodPost, scope, "", nil, true); code != http.StatusOK {
		t.Fatalf("reset from the console = %d %s", code, body)
	}
	if _, err := bucket.Attrs(ctx); err == nil {
		t.Fatal("the bucket survived the console's reset; the test would prove nothing")
	}
	if _, err := sc.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: sec.Name}); status.Code(err) != codes.NotFound {
		t.Fatalf("the secret after the console's reset: %v, want NOT_FOUND", err)
	}
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); status.Code(err) != codes.NotFound {
		t.Fatalf("the queue after the console's reset: %v, want NOT_FOUND", err)
	}
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: kept}); err != nil {
		t.Errorf("the reset of project %s removed %s: %v", h.Project(), kept, err)
	}

	code, body := consoleInstanceDo(t, addr, http.MethodPost, "/api/instance/load", "application/gzip", archive, true)
	var loaded struct{ Loaded []string }
	if err := json.Unmarshal(body, &loaded); err != nil || code != http.StatusOK {
		t.Fatalf("load from the console = %d %s", code, body)
	}
	for _, s := range []string{"storage", "secretmanager", "tasks"} {
		if !strings.Contains(","+strings.Join(loaded.Loaded, ",")+",", ","+s+",") {
			t.Errorf("the console's load did not load %s: %s", s, body)
		}
	}
	r, err := bucket.Object("kept.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("the object is not back: %v", err)
	}
	back, _ := io.ReadAll(r)
	_ = r.Close()
	if string(back) != "kept by the console" {
		t.Errorf("the object came back as %q", back)
	}
	v, err := sc.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: sec.Name + "/versions/latest"})
	if err != nil || string(v.GetPayload().GetData()) != "console value" {
		t.Errorf("the secret's value after the load = %q (%v)", v.GetPayload().GetData(), err)
	}
	if _, err := tc.GetQueue(ctx, &taskspb.GetQueueRequest{Name: q}); err != nil {
		t.Errorf("the queue is not back: %v", err)
	}

	seeded := location(h) + "/queues/console-seeded"
	t.Cleanup(func() { _ = tc.DeleteQueue(context.Background(), &taskspb.DeleteQueueRequest{Name: seeded}) })
	doc := []byte(`{"components":{"tasks":{"queues":["` + seeded + `"]}}}`)
	for i := 1; i <= 2; i++ {
		if code, body := consoleInstanceDo(t, addr, http.MethodPost, "/api/instance/seed?ifNotExists=true", "application/json", doc, true); code != http.StatusOK {
			t.Fatalf("seed %d with If not exists from the console = %d %s", i, code, body)
		}
	}
	n := 0
	it := tc.ListQueues(ctx, &taskspb.ListQueuesRequest{Parent: location(h)})
	for qu, err := it.Next(); err == nil; qu, err = it.Next() {
		if qu.GetName() == seeded {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ListQueues lists %s %d times after two seeds with If not exists, want once", seeded, n)
	}
	if code, body := consoleInstanceDo(t, addr, http.MethodPost, "/api/instance/seed", "application/json", doc, true); code != http.StatusConflict ||
		!strings.Contains(string(body), "already exists") {
		t.Errorf("a seed without If not exists of an existing queue = %d %s; want the admin API's 409", code, body)
	}
}

// stateManifestEntry is the part of an archive's manifest the test reads.
type stateManifestEntry struct {
	Services []struct {
		Name     string `json:"name"`
		Captured bool   `json:"captured"`
	} `json:"services"`
}

// firstEntryManifest reads manifest.json, which must be the archive's first
// entry.
func firstEntryManifest(t *testing.T, archive []byte) stateManifestEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the console's archive is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		t.Fatalf("the console's archive begins with %v (%v), want manifest.json", hdr, err)
	}
	var m stateManifestEntry
	if err := json.NewDecoder(tr).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}
