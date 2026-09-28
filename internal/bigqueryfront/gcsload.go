package bigqueryfront

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A CSV load from Cloud Storage (sourceUris) is read by the front (#944).
//
// The emulator reads such a load's objects itself, from the Cloud Storage
// its STORAGE_EMULATOR_HOST names (the instance's, #919), and loads them
// as it loads an upload, so it takes the first row of each file as a
// header (csvLoad) and ignores the load's CSV options (csvDialect). The
// front cannot change data it does not see, so it reads the objects
// itself, from the same Cloud Storage (the --storage address it is started
// with, never storage.googleapis.com), and sends them to the emulator as
// the data of a multipart upload of the same job, without its sourceUris:
// the path a load from the client takes, whose data the front passes
// through csvStream. BigQuery skips skipLeadingRows rows of each file; so
// does the front, file by file, and the files are loaded as one job, in
// the order of their URIs and, for a wildcard, of their names.
//
// A URI may name one object, or many with one * wildcard in the object
// name, which matches any run of characters: "You can use only one
// wildcard for objects (filenames) within your bucket"
// (https://cloud.google.com/bigquery/docs/batch-loading-data#load-wildcards),
// as the emulator reads it too (its source, importFromGCS). An object that
// does not exist, or a wildcard that matches none, is 404 notFound, and
// nothing is loaded.

// storageReader reads objects from the instance's Cloud Storage JSON API.
type storageReader struct {
	base   string
	client *http.Client
}

func newStorageReader(endpoint string) *storageReader {
	if endpoint == "" {
		return nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	return &storageReader{base: strings.TrimRight(endpoint, "/"), client: &http.Client{Timeout: 10 * time.Minute}}
}

// gsObject is one object a load reads.
type gsObject struct {
	bucket, name string
}

func (o gsObject) uri() string { return "gs://" + o.bucket + "/" + o.name }

// resolve returns the objects a load's URIs name, in order, or why the
// load cannot read them: a loadDataError.
func (s *storageReader) resolve(ctx context.Context, uris []string) ([]gsObject, error) {
	var out []gsObject
	for _, uri := range uris {
		rest, ok := strings.CutPrefix(uri, "gs://")
		bucket, name, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || bucket == "" || name == "" {
			return nil, &loadDataError{code: http.StatusBadRequest, reason: "invalid",
				msg: fmt.Sprintf("Invalid source URI %q: a load reads gs://bucket/object.", uri)}
		}
		switch strings.Count(name, "*") {
		case 0:
			status, err := s.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(name), nil)
			if err != nil {
				return nil, s.unreachable(uri, err)
			}
			if status != http.StatusOK {
				return nil, notFoundURI(uri, status)
			}
			out = append(out, gsObject{bucket, name})
		case 1:
			prefix, suffix, _ := strings.Cut(name, "*")
			n := len(out)
			token := ""
			for {
				q := url.Values{"prefix": {prefix}}
				if token != "" {
					q.Set("pageToken", token)
				}
				var list struct {
					Items []struct {
						Name string `json:"name"`
					} `json:"items"`
					NextPageToken string `json:"nextPageToken"`
				}
				status, err := s.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), &list)
				if err != nil {
					return nil, s.unreachable(uri, err)
				}
				if status != http.StatusOK {
					return nil, notFoundURI(uri, status)
				}
				for _, it := range list.Items {
					if len(it.Name) >= len(prefix)+len(suffix) && strings.HasSuffix(it.Name, suffix) && !strings.HasSuffix(it.Name, "/") {
						out = append(out, gsObject{bucket, it.Name})
					}
				}
				if token = list.NextPageToken; token == "" {
					break
				}
			}
			if len(out) == n {
				return nil, notFoundURI(uri, http.StatusNotFound)
			}
		default:
			return nil, &loadDataError{code: http.StatusBadRequest, reason: "invalid",
				msg: fmt.Sprintf("Invalid source URI %q: only one * wildcard is supported in a Cloud Storage URI.", uri)}
		}
	}
	return out, nil
}

// do reads path's JSON into v, when v is not nil.
func (s *storageReader) do(ctx context.Context, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && v != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v); err != nil {
			return 0, err
		}
	}
	return resp.StatusCode, nil
}

// open returns o's data.
func (s *storageReader) open(ctx context.Context, o gsObject) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.base+"/storage/v1/b/"+url.PathEscape(o.bucket)+"/o/"+url.PathEscape(o.name)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, s.unreachable(o.uri(), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, notFoundURI(o.uri(), resp.StatusCode)
	}
	return resp.Body, nil
}

func notFoundURI(uri string, status int) error {
	return &loadDataError{code: http.StatusNotFound, reason: "notFound",
		msg: fmt.Sprintf("Not found: URI %s (Cloud Storage answered HTTP %d). Nothing was loaded.", uri, status)}
}

func (s *storageReader) unreachable(uri string, err error) error {
	return &loadDataError{code: http.StatusNotFound, reason: "notFound", msg: fmt.Sprintf("Not found: URI %s: the "+
		"instance's Cloud Storage at %s did not answer (%v); is Cloud Storage enabled? Nothing was loaded.", uri, s.base, err)}
}

// gcsUpload returns a multipart upload of job, a jobs.insert body, without
// its sourceUris, with data as its data: the request r would have been
// had the client sent the data itself.
func (f front) gcsUpload(r *http.Request, job []byte, data io.ReadCloser) (*http.Request, error) {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(job))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	if conf, ok := body["configuration"].(map[string]any); ok {
		if load, ok := conf["load"].(map[string]any); ok {
			delete(load, "sourceUris")
		}
	}
	meta, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rb := make([]byte, 12)
	_, _ = rand.Read(rb)
	boundary := "cloudburrow-" + hex.EncodeToString(rb)
	head := []byte("--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n")
	head = append(head, meta...)
	head = append(head, []byte("\r\n--"+boundary+"\r\nContent-Type: application/octet-stream\r\n\r\n")...)
	tail := []byte("\r\n--" + boundary + "--\r\n")
	// The emulator serves uploads only under /upload/bigquery/v2 (its
	// uploadAPIEndpoint), whatever prefix the client's REST path had.
	p := "/upload/bigquery/v2" + strings.TrimPrefix(f.base, "/bigquery/v2") + "/jobs"
	u := *r.URL
	u.Path, u.RawPath = p, ""
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path, u.RawPath = unescaped, p
	}
	u.RawQuery = url.Values{"uploadType": {"multipart"}}.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(),
		io.MultiReader(bytes.NewReader(head), data, bytes.NewReader(tail)))
	if err != nil {
		return nil, err
	}
	// Closing the body, as the transport does when it is done with the
	// request, ends the data's stream.
	req.Body = struct {
		io.Reader
		io.Closer
	}{req.Body, data}
	req.Host = r.Host
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	req.ContentLength = -1
	return req, nil
}
