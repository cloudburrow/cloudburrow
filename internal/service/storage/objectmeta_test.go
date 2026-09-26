package storage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
)

// Metageneration is 1 after an upload and advances with each metadata
// change, while the generation and the bytes stay.
func TestStorageMetagenerationAdvancesOnPatch(t *testing.T) {
	_, bh, _ := sdkBucket(t, "metagen")
	o := bh.Object("m")
	a := write(t, o, []byte("body"), nil)
	if a.Metageneration != 1 {
		t.Fatalf("after upload: metageneration %d", a.Metageneration)
	}
	ctx := context.Background()
	for want := int64(2); want <= 3; want++ {
		u, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "application/json", Metadata: map[string]string{"n": strings.Repeat("x", int(want))}})
		if err != nil || u.Metageneration != want || u.Generation != a.Generation {
			t.Fatalf("patch %d: metageneration %d, generation %d (was %d), %v", want, u.Metageneration, u.Generation, a.Generation, err)
		}
	}
	got, err := o.Attrs(ctx)
	if err != nil || got.ContentType != "application/json" || got.Metadata["n"] != "xxx" {
		t.Errorf("after patches = %+v, %v", got, err)
	}
	if b := read(t, o, 0, -1); string(b) != "body" {
		t.Errorf("a metadata patch changed the bytes: %q", b)
	}
	// A null metadata key removes it; the others stay.
	if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{Metadata: map[string]string{"n": "", "keep": "yes"}}); err != nil {
		t.Fatal(err)
	}
	custom := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	u, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{CustomTime: custom})
	if err != nil || !u.CustomTime.Equal(custom) {
		t.Errorf("customTime = %v, %v", u.CustomTime, err)
	}
	if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{CustomTime: custom.Add(-time.Hour)}); httpCode(err) != 400 {
		t.Errorf("customTime moved earlier = %v, want 400", err)
	}
}

// A stale metageneration is 412, on patch and on delete.
func TestStorageMetagenerationMatchOnPatchAndDelete(t *testing.T) {
	_, bh, _ := sdkBucket(t, "metagen-match")
	o := bh.Object("m")
	write(t, o, []byte("x"), nil)
	ctx := context.Background()
	if _, err := o.If(gcs.Conditions{MetagenerationMatch: 1}).Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "text/csv"}); err != nil {
		t.Fatalf("a matching metageneration: %v", err)
	}
	if _, err := o.If(gcs.Conditions{MetagenerationMatch: 1}).Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "text/tab-separated-values"}); httpCode(err) != 412 {
		t.Errorf("a stale patch = %v, want 412", err)
	}
	if err := o.If(gcs.Conditions{MetagenerationMatch: 1}).Delete(ctx); httpCode(err) != 412 {
		t.Errorf("a stale delete = %v, want 412", err)
	}
	if err := o.If(gcs.Conditions{MetagenerationMatch: 2}).Delete(ctx); err != nil {
		t.Errorf("a matching delete = %v", err)
	}
}

// NotMatch preconditions that fail on a read are 304, over JSON and XML;
// If-None-Match on the etag likewise.
func TestStorageNotMatchPreconditionsReturn304(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=o", "text/plain", "hello", nil)
	_, body := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/o?prettyPrint=false", "")
	gen := between(body, `"generation":"`, `"`)
	etag := between(body, `"etag":"`, `"`)
	for _, c := range []struct {
		name, path string
		hdr        map[string]string
		want       int
	}{
		{"ifGenerationNotMatch", "/storage/v1/b/raw/o/o?ifGenerationNotMatch=" + gen, nil, 304},
		{"ifMetagenerationNotMatch", "/storage/v1/b/raw/o/o?ifMetagenerationNotMatch=1", nil, 304},
		{"ifGenerationNotMatch on media", "/storage/v1/b/raw/o/o?alt=media&ifGenerationNotMatch=" + gen, nil, 304},
		{"If-None-Match", "/storage/v1/b/raw/o/o", map[string]string{"If-None-Match": `"` + etag + `"`}, 304},
		{"If-Match stale", "/storage/v1/b/raw/o/o", map[string]string{"If-Match": `"nope"`}, 412},
		{"ifGenerationMatch stale", "/storage/v1/b/raw/o/o?ifGenerationMatch=1", nil, 412},
		{"XML If-Modified-Since future", "/raw/o", map[string]string{"If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}, 304},
		{"XML If-Unmodified-Since past", "/raw/o", map[string]string{"If-Unmodified-Since": time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)}, 412},
		{"XML x-goog-if-generation-match stale", "/raw/o", map[string]string{"x-goog-if-generation-match": "1"}, 412},
		{"XML x-goog-if-generation-match", "/raw/o", map[string]string{"x-goog-if-generation-match": gen}, 200},
	} {
		req, _ := http.NewRequest("GET", h.URL+c.path, nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func between(s, a, b string) string {
	_, rest, ok := strings.Cut(s, a)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, b)
	return v
}

// The Go client's XML reads send generation and metageneration conditions as
// x-goog-if-* headers.
func TestStorageXMLReadPreconditions(t *testing.T) {
	_, bh, _ := sdkBucket(t, "xml-pre")
	o := bh.Object("o")
	a := write(t, o, []byte("v1"), nil)
	ctx := context.Background()
	r, err := o.Generation(a.Generation).If(gcs.Conditions{GenerationMatch: a.Generation}).NewReader(ctx)
	if err != nil {
		t.Fatalf("a matching generation condition: %v", err)
	}
	r.Close()
	if _, err := o.If(gcs.Conditions{GenerationMatch: a.Generation + 1}).NewReader(ctx); httpCode(err) != 412 && !strings.Contains(errString(err), "412") {
		t.Errorf("a stale generation condition = %v, want 412", err)
	}
	if _, err := o.If(gcs.Conditions{MetagenerationMatch: 2}).NewReader(ctx); err == nil {
		t.Error("a stale metageneration condition read the object")
	}
	if _, err := o.Generation(a.Generation + 7).NewReader(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("a generation that does not exist = %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// delete honours generation= and all four conditions, and leaves the object
// when one fails.
func TestStorageDeleteHonoursPreconditions(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=d", "text/plain", "x", nil)
	_, body := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/d?prettyPrint=false", "")
	gen := between(body, `"generation":"`, `"`)
	for _, q := range []string{"ifGenerationMatch=1", "ifGenerationNotMatch=" + gen, "ifMetagenerationMatch=9", "ifMetagenerationNotMatch=1"} {
		if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/raw/o/d?"+q, ""); code != 412 {
			t.Errorf("delete ?%s = %d, want 412", q, code)
		}
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/raw/o/d?generation=1", ""); code != 404 {
		t.Errorf("delete of another generation = %d, want 404", code)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/d", ""); code != 200 {
		t.Fatalf("a refused delete removed the object (%d)", code)
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/raw/o/d?generation="+gen+"&ifGenerationMatch="+gen+"&ifMetagenerationMatch=1", ""); code != 204 {
		t.Errorf("a delete whose conditions all hold = %d", code)
	}
	if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/raw/o/absent", `{"contentType":"x"}`); code != 404 {
		t.Errorf("patch of a missing object = %d %s", code, body)
	}
}

// Fields a patch cannot change are refused by name.
func TestStorageObjectPatchRefusesUnkeptFields(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=f", "text/plain", "x", nil)
	for field, body := range map[string]string{
		"storageClass": `{"storageClass":"COLDLINE"}`,
		"kmsKeyName":   `{"kmsKeyName":"k"}`,
		"bogus":        `{"bogus":1}`,
	} {
		if code, resp := raw(t, "PATCH", h.URL+"/storage/v1/b/raw/o/f", body); code != 400 || !strings.Contains(resp, field) {
			t.Errorf("patch %s = %d %s", field, code, resp)
		}
	}
	// Update replaces: an omitted field goes back to its default.
	raw(t, "PATCH", h.URL+"/storage/v1/b/raw/o/f", `{"cacheControl":"no-store","metadata":{"a":"1"}}`)
	code, resp := raw(t, "PUT", h.URL+"/storage/v1/b/raw/o/f?prettyPrint=false", `{"contentType":"text/csv"}`)
	if code != 200 || strings.Contains(resp, "cacheControl") || strings.Contains(resp, `"metadata"`) || !strings.Contains(resp, `"metageneration":"3"`) {
		t.Errorf("update = %d %s", code, resp)
	}
}

// An update that restates the object's own storage class is accepted, as
// Terraform's full-resource update needs (#515); a different class is still
// refused, since only a rewrite changes it.
func TestStorageObjectUpdateAcceptsItsOwnStorageClass(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=c", "text/plain", "x", nil)
	if code, resp := raw(t, "PUT", h.URL+"/storage/v1/b/raw/o/c", `{"storageClass":"STANDARD","temporaryHold":true}`); code != 200 || !strings.Contains(resp, `"temporaryHold": true`) {
		t.Errorf("update restating STANDARD = %d %s", code, resp)
	}
	if code, resp := raw(t, "PATCH", h.URL+"/storage/v1/b/raw/o/c", `{"storageClass":"standard","temporaryHold":false}`); code != 200 {
		t.Errorf("patch restating the class in lower case = %d %s", code, resp)
	}
	if code, resp := raw(t, "PUT", h.URL+"/storage/v1/b/raw/o/c", `{"storageClass":"COLDLINE"}`); code != 400 || !strings.Contains(resp, "storageClass") {
		t.Errorf("update to COLDLINE = %d %s, want 400 naming storageClass", code, resp)
	}
}
