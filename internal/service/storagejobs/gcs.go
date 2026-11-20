package storagejobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// GCS is the instance's Cloud Storage, reached through its JSON API.
type GCS struct {
	// Base is the JSON API's base URL (http://host:port), read at each
	// call; "" when Cloud Storage has not started.
	Base   func() string
	Client *http.Client
}

// Object is an object's resource as the JSON API answers it.
type Object struct {
	Bucket                  string            `json:"bucket"`
	Name                    string            `json:"name"`
	Generation              string            `json:"generation"`
	Metageneration          string            `json:"metageneration"`
	Size                    string            `json:"size"`
	ContentType             string            `json:"contentType,omitempty"`
	ContentEncoding         string            `json:"contentEncoding,omitempty"`
	ContentLanguage         string            `json:"contentLanguage,omitempty"`
	ContentDisposition      string            `json:"contentDisposition,omitempty"`
	CacheControl            string            `json:"cacheControl,omitempty"`
	TimeCreated             string            `json:"timeCreated"`
	Updated                 string            `json:"updated"`
	TimeDeleted             string            `json:"timeDeleted,omitempty"`
	TimeStorageClassUpdated string            `json:"timeStorageClassUpdated,omitempty"`
	RetentionExpirationTime string            `json:"retentionExpirationTime,omitempty"`
	StorageClass            string            `json:"storageClass,omitempty"`
	Etag                    string            `json:"etag,omitempty"`
	Crc32c                  string            `json:"crc32c,omitempty"`
	MD5Hash                 string            `json:"md5Hash,omitempty"`
	KMSKeyName              string            `json:"kmsKeyName,omitempty"`
	Metadata                map[string]string `json:"metadata,omitempty"`
}

// SizeBytes is the object's size.
func (o Object) SizeBytes() int64 { n, _ := strconv.ParseInt(o.Size, 10, 64); return n }

// Gen is the object's generation.
func (o Object) Gen() int64 { n, _ := strconv.ParseInt(o.Generation, 10, 64); return n }

// UpdatedTime is when the object was last modified.
func (o Object) UpdatedTime() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, o.Updated)
	return t
}

// Bucket is a bucket's resource, as far as the companions read it.
type Bucket struct {
	Name          string `json:"name"`
	Location      string `json:"location"`
	ProjectNumber string `json:"projectNumber"`
	Versioning    *struct {
		Enabled bool `json:"enabled"`
	} `json:"versioning,omitempty"`
}

// gcsError is an error the JSON API answered.
type gcsError struct {
	Code    int
	Message string
}

func (e *gcsError) Error() string { return e.Message }

// IsNotFound reports whether err is the JSON API's 404.
func IsNotFound(err error) bool {
	ge, ok := err.(*gcsError)
	return ok && ge.Code == http.StatusNotFound
}

// statusOf maps a JSON API error to its google.rpc.Code number.
func statusOf(err error) int32 {
	ge, ok := err.(*gcsError)
	if !ok {
		return 13 // INTERNAL
	}
	switch ge.Code {
	case http.StatusNotFound:
		return 5
	case http.StatusForbidden:
		return 7
	case http.StatusUnauthorized:
		return 16
	case http.StatusConflict:
		return 6
	case http.StatusPreconditionFailed:
		return 9
	case http.StatusBadRequest:
		return 3
	case http.StatusTooManyRequests:
		return 8
	}
	return 13
}

func (g *GCS) base() (string, error) {
	if g == nil || g.Base == nil {
		return "", precondition("Cloud Storage is not served on this instance")
	}
	b := g.Base()
	if b == "" {
		return "", precondition("Cloud Storage has not started on this instance")
	}
	return b, nil
}

func (g *GCS) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

// do sends a JSON API request and decodes its answer into out (when not
// nil).
func (g *GCS) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	base, err := g.base()
	if err != nil {
		return err
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return answer(resp, out)
}

func answer(resp *http.Response, out any) error {
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(b, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = resp.Status
		}
		return &gcsError{Code: resp.StatusCode, Message: msg}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func objPath(bucket, name string) string {
	return "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(name)
}

func genQuery(gen int64) url.Values {
	q := url.Values{}
	if gen > 0 {
		q.Set("generation", strconv.FormatInt(gen, 10))
	}
	return q
}

// Bucket reads a bucket.
func (g *GCS) Bucket(ctx context.Context, bucket string) (Bucket, error) {
	var b Bucket
	err := g.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket), nil, nil, &b)
	return b, err
}

// List lists a bucket's live objects under prefix (every version with
// versions).
func (g *GCS) List(ctx context.Context, bucket, prefix string, versions bool) ([]Object, error) {
	var out []Object
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if versions {
		q.Set("versions", "true")
	}
	q.Set("maxResults", "1000")
	for {
		var page struct {
			Items         []Object `json:"items"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := g.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket)+"/o", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// Get reads an object's metadata (its live version for generation 0).
func (g *GCS) Get(ctx context.Context, bucket, name string, gen int64) (Object, error) {
	var o Object
	err := g.do(ctx, http.MethodGet, objPath(bucket, name), genQuery(gen), nil, &o)
	return o, err
}

// Read opens an object's data.
func (g *GCS) Read(ctx context.Context, bucket, name string, gen int64) (io.ReadCloser, error) {
	base, err := g.base()
	if err != nil {
		return nil, err
	}
	q := genQuery(gen)
	q.Set("alt", "media")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+objPath(bucket, name)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, answer(resp, nil)
	}
	return resp.Body, nil
}

// Write uploads an object with its metadata (a multipart upload).
func (g *GCS) Write(ctx context.Context, bucket string, meta map[string]any, data io.Reader) (Object, error) {
	base, err := g.base()
	if err != nil {
		return Object{}, err
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return Object{}, err
	}
	ct, _ := meta["contentType"].(string)
	if ct == "" {
		ct = "application/octet-stream"
	}
	const boundary = "cloudburrow-storagejobs-boundary"
	pr, pw := io.Pipe()
	go func() {
		_, _ = fmt.Fprintf(pw, "--%s\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n%s\r\n--%s\r\nContent-Type: %s\r\n\r\n",
			boundary, mb, boundary, ct)
		if _, err := io.Copy(pw, data); err != nil {
			pw.CloseWithError(err)
			return
		}
		_, _ = fmt.Fprintf(pw, "\r\n--%s--\r\n", boundary)
		pw.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/upload/storage/v1/b/"+url.PathEscape(bucket)+"/o?uploadType=multipart", pr)
	if err != nil {
		return Object{}, err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	resp, err := g.client().Do(req)
	if err != nil {
		return Object{}, err
	}
	defer resp.Body.Close()
	var o Object
	err = answer(resp, &o)
	return o, err
}

// Rewrite copies an object to dst (with the metadata in body overriding
// its own) until the rewrite is done.
func (g *GCS) Rewrite(ctx context.Context, srcBucket, srcName string, srcGen int64, dstBucket, dstName string, q url.Values, body map[string]any) (Object, error) {
	if q == nil {
		q = url.Values{}
	}
	if srcGen > 0 {
		q.Set("sourceGeneration", strconv.FormatInt(srcGen, 10))
	}
	path := objPath(srcBucket, srcName) + "/rewriteTo" + objPath(dstBucket, dstName)[len("/storage/v1"):]
	if body == nil {
		body = map[string]any{}
	}
	for {
		var r struct {
			Done          bool   `json:"done"`
			RewriteToken  string `json:"rewriteToken"`
			Resource      Object `json:"resource"`
			ObjectSize    string `json:"objectSize"`
			TotalRewrites string `json:"totalBytesRewritten"`
		}
		if err := g.do(ctx, http.MethodPost, path, q, body, &r); err != nil {
			return Object{}, err
		}
		if r.Done {
			return r.Resource, nil
		}
		q.Set("rewriteToken", r.RewriteToken)
	}
}

// Patch patches an object's metadata.
func (g *GCS) Patch(ctx context.Context, bucket, name string, gen int64, body map[string]any) (Object, error) {
	var o Object
	err := g.do(ctx, http.MethodPatch, objPath(bucket, name), genQuery(gen), body, &o)
	return o, err
}

// Delete deletes an object's live version, or one generation.
func (g *GCS) Delete(ctx context.Context, bucket, name string, gen int64) error {
	return g.do(ctx, http.MethodDelete, objPath(bucket, name), genQuery(gen), nil, nil)
}

// IntelligenceEdition is the effective Storage Intelligence edition of a
// project ("" when it cannot be read).
func (g *GCS) IntelligenceEdition(ctx context.Context, project string) (string, error) {
	var c struct {
		Effective struct {
			EffectiveEdition string `json:"effectiveEdition"`
		} `json:"effectiveIntelligenceConfig"`
		EditionConfig string `json:"editionConfig"`
	}
	if err := g.do(ctx, http.MethodGet, "/v2/projects/"+url.PathEscape(project)+"/locations/global/intelligenceConfig", nil, nil, &c); err != nil {
		return "", err
	}
	return c.Effective.EffectiveEdition, nil
}
