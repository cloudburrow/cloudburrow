package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The console's Instance page (#801): state save and load, reset and seed,
// through the admin API's own handlers, called in process with the page's
// Authorization header and nothing the console holds (#553). Save and load
// run the code `cloudburrow state save` and `state load` run, over a client
// whose transport is the admin API's handler rather than the control port.

// consoleInstance is the Instance page's source for one running instance.
type consoleInstance struct {
	client   *http.Client
	instance string
	// dir is where an exported archive is written, owner-only, before it is
	// streamed to the browser and removed.
	dir string
}

// adminBase is the address the in-process client's requests carry; the
// transport serves them with the admin handler and never dials it.
const adminBase = "http://cloudburrow-admin.invalid"

func newConsoleInstance(cfg config.Config, api *admin.API) consoleInstance {
	mux := http.NewServeMux()
	api.Routes(mux)
	return consoleInstance{
		client:   &http.Client{Transport: inProcessTransport{h: mux}},
		instance: cfg.Name,
		dir:      os.TempDir(),
	}
}

// request is one request to the admin API carrying only the page's
// Authorization header.
func (c consoleInstance) request(ctx context.Context, authorization, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := adminBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req, nil
}

// call sends one JSON request and returns the admin API's answer, or its
// refusal with its status and message.
func (c consoleInstance) call(ctx context.Context, authorization, method, path string, q url.Values, body []byte) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := c.request(ctx, authorization, method, path, q, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &console.InstanceRefusal{Status: resp.StatusCode, Message: refusalMessage(path, resp.StatusCode, b), Body: b}
	}
	return b, nil
}

// refusalMessage is the admin API's message; for a reset that failed part
// way, which carries no error field, the components that failed and why.
func refusalMessage(path string, status int, b []byte) string {
	var res resetResult
	if path == "/admin/reset" && json.Unmarshal(b, &res) == nil && len(res.Failed) > 0 {
		names := make([]string, 0, len(res.Failed))
		for name := range res.Failed {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, name+": "+res.Failed[name])
		}
		return "reset did not complete; their state may remain: " + strings.Join(parts, "; ")
	}
	return adminError(status, b)
}

func (c consoleInstance) Info(ctx context.Context, authorization string) (json.RawMessage, error) {
	return c.call(ctx, authorization, http.MethodGet, "/admin/instance", nil, nil)
}

// SaveState is `state save`'s export, into a temporary owner-only file.
func (c consoleInstance) SaveState(ctx context.Context, authorization string) (console.StateArchive, error) {
	req, err := c.request(ctx, authorization, http.MethodPost, "/admin/state/export", nil, nil)
	if err != nil {
		return console.StateArchive{}, err
	}
	path, _, err := exportStateArchive(c.client, req, c.dir)
	if err != nil {
		return console.StateArchive{}, stateRefusal(err)
	}
	return console.StateArchive{Path: path, Name: stateArchiveName(c.instance, time.Now())}, nil
}

// LoadState is `state load`'s import.
func (c consoleInstance) LoadState(ctx context.Context, authorization string, archive io.Reader) (json.RawMessage, error) {
	req, err := c.request(ctx, authorization, http.MethodPost, "/admin/state/import", nil, archive)
	if err != nil {
		return nil, err
	}
	res, err := importStateArchive(c.client, req)
	if err != nil {
		return nil, stateRefusal(err)
	}
	return json.Marshal(res)
}

func (c consoleInstance) Reset(ctx context.Context, authorization string, q url.Values) (json.RawMessage, error) {
	return c.call(ctx, authorization, http.MethodPost, "/admin/reset", q, nil)
}

func (c consoleInstance) Seed(ctx context.Context, authorization string, doc []byte, ifNotExists bool) (json.RawMessage, error) {
	var q url.Values
	if ifNotExists {
		q = url.Values{"ifNotExists": {"true"}}
	}
	return c.call(ctx, authorization, http.MethodPost, "/admin/seed", q, doc)
}

// stateRefusal carries an admin refusal from the state commands' code to
// the console with its status.
func stateRefusal(err error) error {
	var refused *stateRefused
	if errors.As(err, &refused) {
		return &console.InstanceRefusal{Status: refused.status, Message: refused.message}
	}
	return err
}

// stateArchiveName is the file name the console offers a saved archive
// under: the instance and the time, so two saves do not overwrite each other
// in a downloads folder.
func stateArchiveName(instance string, now time.Time) string {
	return fmt.Sprintf("cloudburrow-state-%s-%s.tar.gz", instance, now.UTC().Format("20060102T150405Z"))
}

var _ console.InstanceSource = consoleInstance{}

// inProcessTransport serves a request with h in this process, streaming the
// response body as h writes it, so a large archive is never held in memory.
type inProcessTransport struct{ h http.Handler }

func (t inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A server-side handler reads the body of every request; an outgoing
	// request without one has a nil Body.
	in := req.Clone(req.Context())
	if in.Body == nil {
		in.Body = http.NoBody
	}
	in.RequestURI = in.URL.RequestURI()
	pr, pw := io.Pipe()
	rw := &streamedResponse{header: http.Header{}, body: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			rw.WriteHeader(http.StatusOK)
			_ = pw.Close()
		}()
		t.h.ServeHTTP(rw, in)
	}()
	select {
	case <-rw.ready:
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", rw.status, http.StatusText(rw.status)), StatusCode: rw.status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: rw.header, Body: pr, ContentLength: -1, Request: req,
	}, nil
}

// streamedResponse is an http.ResponseWriter whose body is a pipe; ready is
// closed once the status is known.
type streamedResponse struct {
	header http.Header
	body   *io.PipeWriter
	once   sync.Once
	status int
	ready  chan struct{}
}

func (s *streamedResponse) Header() http.Header { return s.header }

func (s *streamedResponse) WriteHeader(status int) {
	s.once.Do(func() {
		s.status = status
		close(s.ready)
	})
}

func (s *streamedResponse) Write(b []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	return s.body.Write(b)
}
