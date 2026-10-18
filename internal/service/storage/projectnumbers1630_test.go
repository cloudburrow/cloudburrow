package storage

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// restartedServer is a server started on meta, as the storage server is on
// its volume after `stop` and `up`, serving and resolving project numbers
// as storageserver wires it.
func restartedServer(t *testing.T, meta MetaStore) string {
	t.Helper()
	s, err := NewServer(Options{Hosts: []string{"storage.localhost"}, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	// Close waits for every handler; one deadlocked would hang the run
	// past the failure it reports.
	t.Cleanup(func() {
		if !t.Failed() {
			h.Close()
		}
	})
	resource.SetProjectNumberResolver(s.ProjectNumberOf)
	t.Cleanup(func() { resource.SetProjectNumberResolver(nil) })
	return h.URL
}

// seedUpload is the startup seed's request (cmd/cloudburrow, storageSeeder):
// a multipart upload, conditional on the object not existing.
func seedUpload(ctx context.Context, base, bucket, name string) (int, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	fmt.Fprintf(part, `{"name":%q,"contentType":"text/plain"}`, name)
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain"}})
	_, _ = part.Write([]byte("seeded by up --seed-file"))
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/upload/storage/v1/b/"+bucket+"/o?uploadType=multipart&ifGenerationMatch=0", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestRestartedServerUploadsBeforeTheNumbersArePushed (#1630): after a
// restart, the first project number lookup may come from inside a metadata
// transaction, before the host has pushed the numbers: an upload's default
// object ACL names the project team by number. That lookup loaded the set
// from the store, taking its read lock under the upload's write lock, and
// the store, and with it every request and the readiness probe, hung for
// good: main's CI run 39101134210, where the warm `up`'s seed timed out.
// The upload must answer, and the server go on serving.
func TestRestartedServerUploadsBeforeTheNumbersArePushed(t *testing.T) {
	meta := NewMemMetaStore()
	first := restartedServer(t, meta)
	if code, body := raw(t, "POST", first+"/storage/v1/b?project=ci-local", `{"name":"cloudburrow-seeded-at-startup"}`); code != 200 {
		t.Fatalf("create the bucket: %d %s", code, body)
	}

	base := restartedServer(t, meta)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := seedUpload(ctx, base, "cloudburrow-seeded-at-startup", "hello.txt")
	if err != nil || code != 200 {
		t.Fatalf("the restarted server's first upload: %d %v; want 200 (a hang is the #1630 deadlock)", code, err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/storage/v1/b?project=_", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the readiness request after the upload: %v", err)
	}
	_ = resp.Body.Close()
}

// TestProjectNumberPushesBesideUploads (#1630): a push takes the store's
// lock, and an upload's transaction looks a number up; neither may wait on
// the other while holding what the other needs.
func TestProjectNumberPushesBesideUploads(t *testing.T) {
	meta := NewMemMetaStore()
	base := restartedServer(t, meta)
	if code, body := raw(t, "POST", base+"/storage/v1/b?project=ci-local", `{"name":"busy"}`); code != 200 {
		t.Fatalf("create the bucket: %d %s", code, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if code, err := seedUpload(ctx, base, "busy", fmt.Sprintf("o-%d", i)); err != nil || code != 200 {
				errs <- fmt.Errorf("upload %d: %d %v", i, code, err)
			}
		}()
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+ProjectNumbersPath,
				strings.NewReader(fmt.Sprintf(`{"ci-local":%d}`, 100000000000+i)))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- fmt.Errorf("push %d: %v", i, err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- fmt.Errorf("push %d: %s", i, resp.Status)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
