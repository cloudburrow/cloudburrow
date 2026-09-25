package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// sdk serves a fresh server and returns the official Go client pointed at it.
func sdk(t *testing.T) (*gcs.Client, *httptest.Server) {
	t.Helper()
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	c, err := gcs.NewClient(context.Background(), option.WithEndpoint(h.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, h
}

func httpCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func TestStorageBucketLifecycleInProcess(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	bh := c.Bucket("life-cycle")
	if err := bh.Create(ctx, "demo-project", &gcs.BucketAttrs{Location: "us-central1", Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "life-cycle" || a.Location != "US-CENTRAL1" || a.LocationType != "region" || a.StorageClass != "STANDARD" ||
		a.MetaGeneration != 1 || a.Labels["env"] != "dev" || a.SoftDeletePolicy == nil || a.SoftDeletePolicy.RetentionDuration.Hours() != 168 ||
		a.ProjectNumber == 0 || a.Created.IsZero() {
		t.Errorf("attrs = %+v", a)
	}
	if err := bh.Create(ctx, "demo-project", nil); httpCode(err) != 409 {
		t.Errorf("a duplicate create = %v, want 409", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Bucket(fmt.Sprintf("listed-%d", i)).Create(ctx, "demo-project", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Bucket("elsewhere").Create(ctx, "other-project", nil); err != nil {
		t.Fatal(err)
	}
	it := c.Buckets(ctx, "demo-project")
	it.Prefix = "listed-"
	var names []string
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, b.Name)
	}
	if strings.Join(names, ",") != "listed-0,listed-1,listed-2" {
		t.Errorf("list with prefix = %v", names)
	}
	// Pages of one.
	it = c.Buckets(ctx, "demo-project")
	pager := iterator.NewPager(it, 1, "")
	pages := 0
	for {
		var page []*gcs.BucketAttrs
		tok, err := pager.NextPage(&page)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if tok == "" || pages > 10 {
			break
		}
	}
	if pages != 4 {
		t.Errorf("pages of one over 4 buckets = %d", pages)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := bh.Attrs(ctx); !errors.Is(err, gcs.ErrBucketNotExist) {
		t.Errorf("Attrs after delete = %v", err)
	}
	if err := c.Bucket("Bad_Name").Create(ctx, "demo-project", nil); httpCode(err) != 400 {
		t.Errorf("an invalid name = %v, want 400", err)
	}
}

// A patch leaves the fields it omits unchanged (the documented behaviour;
// fake-gcs-server resets them).
func TestStorageBucketPatchKeepsOmittedFields(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	bh := c.Bucket("patched")
	if err := bh.Create(ctx, "demo-project", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{DefaultEventBasedHold: true}); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{VersioningEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !a.DefaultEventBasedHold || !a.VersioningEnabled || a.MetaGeneration != 3 {
		t.Errorf("after patching versioning only: hold %v, versioning %v, metageneration %d", a.DefaultEventBasedHold, a.VersioningEnabled, a.MetaGeneration)
	}
	var up gcs.BucketAttrsToUpdate
	up.SetLabel("a", "1")
	up.SetLabel("b", "2")
	if _, err := bh.Update(ctx, up); err != nil {
		t.Fatal(err)
	}
	var del gcs.BucketAttrsToUpdate
	del.DeleteLabel("a")
	a, err = bh.Update(ctx, del)
	if err != nil || len(a.Labels) != 1 || a.Labels["b"] != "2" {
		t.Errorf("after deleting label a = %v, %v", a.Labels, err)
	}
}

func raw(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// In a patch, a null member removes one label and leaves the others.
func TestStorageBucketPatchNullClears(t *testing.T) {
	_, h := sdk(t)
	b := h.URL + "/storage/v1/b"
	if code, body := raw(t, "POST", b+"?project=p1", `{"name":"nulls","labels":{"a":"1","b":"2"},"versioning":{"enabled":true}}`); code != 200 {
		t.Fatal(body)
	}
	code, body := raw(t, "PATCH", b+"/nulls?prettyPrint=false", `{"labels":{"a":null}}`)
	if code != 200 || !strings.Contains(body, `"labels":{"b":"2"}`) || !strings.Contains(body, `"versioning":{"enabled":true}`) {
		t.Errorf("patch labels.a=null = %d %s", code, body)
	}
	code, body = raw(t, "PATCH", b+"/nulls?prettyPrint=false", `{"labels":null}`)
	if code != 200 || strings.Contains(body, `"labels"`) {
		t.Errorf("patch labels=null = %d %s", code, body)
	}
	// Update (PUT) replaces: an omitted field goes back to its default.
	code, body = raw(t, "PUT", b+"/nulls?prettyPrint=false", `{"name":"nulls","storageClass":"nearline"}`)
	if code != 200 || strings.Contains(body, "versioning") || !strings.Contains(body, `"storageClass":"NEARLINE"`) {
		t.Errorf("update = %d %s", code, body)
	}
}

// A stale ifMetagenerationMatch is 412; a matching NotMatch on a read is 304.
func TestStorageBucketMetagenerationPreconditions(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	bh := c.Bucket("preconditions")
	if err := bh.Create(ctx, "demo-project", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bh.If(gcs.BucketConditions{MetagenerationMatch: 1}).Update(ctx, gcs.BucketAttrsToUpdate{VersioningEnabled: true}); err != nil {
		t.Fatalf("a matching metageneration = %v", err)
	}
	if _, err := bh.If(gcs.BucketConditions{MetagenerationMatch: 1}).Update(ctx, gcs.BucketAttrsToUpdate{VersioningEnabled: false}); httpCode(err) != 412 {
		t.Errorf("a stale MetagenerationMatch = %v, want 412", err)
	}
	if err := bh.If(gcs.BucketConditions{MetagenerationNotMatch: 2}).Delete(ctx); httpCode(err) != 412 {
		t.Errorf("a failed NotMatch on delete = %v, want 412", err)
	}
	_, h := sdk(t)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"cached"}`)
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/cached?ifMetagenerationNotMatch=1", ""); code != 304 {
		t.Errorf("a GET whose NotMatch fails = %d, want 304", code)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/cached?ifMetagenerationMatch=x", ""); code != 400 {
		t.Errorf("a non-numeric precondition = %d %s", code, body)
	}
}

// A field CloudBurrow does not keep yet is refused by name, never dropped
// with 200 (#374).
func TestStorageBucketUnkeptFieldRefused(t *testing.T) {
	_, h := sdk(t)
	b := h.URL + "/storage/v1/b"
	for field, body := range map[string]string{
		"website":         `{"name":"web","website":{"mainPageSuffix":"index.html"}}`,
		"cors":            `{"name":"web","cors":[{"origin":["*"]}]}`,
		"lifecycle":       `{"name":"web","lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":30}}]}}`,
		"retentionPolicy": `{"name":"web","retentionPolicy":{"retentionPeriod":"60"}}`,
		"bogusField":      `{"name":"web","bogusField":1}`,
	} {
		code, resp := raw(t, "POST", b+"?project=p", body)
		if code != 400 || !strings.Contains(resp, field) {
			t.Errorf("%s = %d %s; want 400 naming it", field, code, resp)
		}
	}
	raw(t, "POST", b+"?project=p", `{"name":"web"}`)
	if code, resp := raw(t, "PATCH", b+"/web", `{"website":{"mainPageSuffix":"index.html"}}`); code != 400 || !strings.Contains(resp, "website") {
		t.Errorf("patch website = %d %s", code, resp)
	}
	// A null or empty unkept field says nothing: Terraform's NullFields send
	// nulls, and the Go client sends an empty lifecycle with every create.
	if code, resp := raw(t, "PATCH", b+"/web", `{"website":null,"cors":null,"lifecycle":{"rule":[]}}`); code != 200 {
		t.Errorf("patch with nulls for unkept fields = %d %s", code, resp)
	}
	if code, resp := raw(t, "POST", b+"?project=p&predefinedAcl=publicRead", `{"name":"acl"}`); code != 501 || !strings.Contains(resp, "predefinedAcl") {
		t.Errorf("predefinedAcl = %d %s; want 501 naming it", code, resp)
	}
}

// A non-empty bucket delete is 409.
//
// unverified: storage.buckets.delete 409: a non-empty bucket (no page states the JSON API's status; the XML API documents 409 BucketNotEmpty)
func TestStorageNonEmptyBucketDeleteInProcess(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	defer h.Close()
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"full"}`)
	if err := s.meta.Update(func(tx Tx) error { tx.Put(objectPrefix+"full/x", []byte("{}")); return nil }); err != nil {
		t.Fatal(err)
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/full", ""); code != 409 || !strings.Contains(body, "not empty") {
		t.Errorf("delete of a non-empty bucket = %d %s", code, body)
	}
}
