package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// TestCloudBuildDefaultLogsBucket (#1430): Cloud Build writes a log to its
// default logs bucket through CloudBuildLogsPath, which makes the bucket;
// the official client reads the log, and every write through the API is
// 403, as the bucket is Google's; buckets.list leaves it out, and a user
// cannot create the name.
func TestCloudBuildDefaultLogsBucket(t *testing.T) {
	c, h := sdk(t)
	ctx := context.Background()
	const bucket = "123456789.cloudbuild-logs.googleusercontent.com"
	put := func(name, body string) *http.Response {
		t.Helper()
		u := h.URL + CloudBuildLogsPath + "?" + url.Values{"project": {"proj-1"}, "bucket": {bucket}, "name": {name}}.Encode()
		resp, err := http.Post(u, "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := put("log-1.txt", "first"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the first log = %d", resp.StatusCode)
	}
	if resp := put("log-1.txt", "starting build\nDONE\n"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the log rewritten = %d", resp.StatusCode)
	}
	r, err := c.Bucket(bucket).Object("log-1.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "starting build\nDONE\n" {
		t.Errorf("the log = %q", got)
	}
	if _, err := c.Bucket(bucket).Attrs(ctx); err != nil {
		t.Errorf("buckets.get = %v", err)
	}
	it := c.Bucket(bucket).Objects(ctx, nil)
	if o, err := it.Next(); err != nil || o.Name != "log-1.txt" {
		t.Errorf("objects.list = %v, %v", o, err)
	}

	// Writes through the API are refused.
	w := c.Bucket(bucket).Object("mine.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); httpCode(err) != http.StatusForbidden {
		t.Errorf("an upload = %v; want 403", err)
	}
	if err := c.Bucket(bucket).Object("log-1.txt").Delete(ctx); httpCode(err) != http.StatusForbidden {
		t.Errorf("objects.delete = %v; want 403", err)
	}
	if err := c.Bucket(bucket).Delete(ctx); httpCode(err) != http.StatusForbidden {
		t.Errorf("buckets.delete = %v; want 403", err)
	}
	if _, err := c.Bucket(bucket).Update(ctx, gcs.BucketAttrsToUpdate{StorageClass: "NEARLINE"}); httpCode(err) != http.StatusForbidden {
		t.Errorf("buckets.patch = %v; want 403", err)
	}
	// A copy out of it is a read; a copy into it is refused.
	if err := c.Bucket("mine-1430").Create(ctx, "proj-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bucket("mine-1430").Object("copy.txt").CopierFrom(c.Bucket(bucket).Object("log-1.txt")).Run(ctx); err != nil {
		t.Errorf("a copy out of the logs bucket = %v", err)
	}
	if _, err := c.Bucket(bucket).Object("in.txt").CopierFrom(c.Bucket("mine-1430").Object("copy.txt")).Run(ctx); httpCode(err) != http.StatusForbidden {
		t.Errorf("a copy into the logs bucket = %v; want 403", err)
	}
	// The XML API too.
	req, _ := http.NewRequest(http.MethodPut, h.URL+"/"+bucket+"/x.txt", bytes.NewReader([]byte("x")))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("an XML PUT = %v, %v; want 403", resp, err)
	} else {
		resp.Body.Close()
	}

	// The project's bucket list leaves it out, and no user can create one.
	bi := c.Buckets(ctx, "proj-1")
	for {
		b, err := bi.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Name == bucket {
			t.Errorf("buckets.list shows the default logs bucket")
		}
	}
	if err := c.Bucket("987.cloudbuild-logs.googleusercontent.com").Create(ctx, "proj-1", nil); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a user creating a logs bucket name = %v; want 400", err)
	}
	// The internal path takes only a logs bucket name.
	u := h.URL + CloudBuildLogsPath + "?" + url.Values{"project": {"proj-1"}, "bucket": {"mine-1430"}, "name": {"x"}}.Encode()
	if resp, err := http.Post(u, "text/plain", strings.NewReader("x")); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a log to another bucket = %v, %v; want 400", resp, err)
	} else {
		resp.Body.Close()
	}
}

func TestJSONTargetBucket(t *testing.T) {
	for rest, want := range map[string]string{
		"b/src/o/a%2Fb/copyTo/b/dst/o/c": "dst",
		"b/src/o/a/rewriteTo/b/dst/o/c":  "dst",
		"b/one/o":                        "one",
		"b/one/o/b":                      "one",
		"b":                              "",
		"b/one/o/x/compose":              "one",
		"projects/p/serviceAccount":      "",
		"b/one/notificationConfigs/1":    "one",
	} {
		if got := jsonTargetBucket(rest); got != want {
			t.Errorf("jsonTargetBucket(%q) = %q, want %q", rest, got, want)
		}
	}
}
