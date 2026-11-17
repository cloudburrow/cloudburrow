package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TestEmbedObjectsReadsLocalStorageOnly (#1651): EmbedContent's file_data
// reader reads an object and its content type from this instance's Cloud
// Storage, tells a missing bucket from a missing object (both NOT_FOUND),
// bounds the size, and refuses outright, dialing nothing, when no storage
// endpoint is known.
func TestEmbedObjectsReadsLocalStorageOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/storage/v1/b/media/o/speech%2Fclip.wav":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFF0000WAVE"))
		case "/storage/v1/b/media":
			_, _ = w.Write([]byte(`{"name":"media"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	o := embedObjects{f: storageSourceFetcher{client: srv.Client(), addr: func() string { return addr }}}
	ctx := context.Background()

	data, ct, err := o.Read(ctx, "media", "speech/clip.wav", 1<<20)
	if err != nil || string(data) != "RIFF0000WAVE" || ct != "audio/wav" {
		t.Fatalf("Read = %q, %q, %v", data, ct, err)
	}
	if _, _, err := o.Read(ctx, "media", "speech/clip.wav", 4); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("an object over the limit: %v, want INVALID_ARGUMENT", err)
	}
	_, _, err = o.Read(ctx, "media", "none.wav", 1<<20)
	if e := apierror.From(err); e.Code != codes.NotFound || !strings.Contains(err.Error(), "gs://media/none.wav not found") {
		t.Errorf("a missing object: %v, want NOT_FOUND naming it", err)
	}
	_, _, err = o.Read(ctx, "nobucket", "a.wav", 1<<20)
	if e := apierror.From(err); e.Code != codes.NotFound || !strings.Contains(err.Error(), `bucket "nobucket" not found`) {
		t.Errorf("a missing bucket: %v, want NOT_FOUND naming the bucket", err)
	}

	// No endpoint: refused before any request.
	dialed := false
	none := embedObjects{f: storageSourceFetcher{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dialed = true
		return nil, http.ErrHandlerTimeout
	})}, addr: func() string { return "" }}}
	if _, _, err := none.Read(ctx, "media", "speech/clip.wav", 1<<20); apierror.From(err).Code != codes.FailedPrecondition || dialed {
		t.Errorf("no endpoint: %v (dialed %v), want FAILED_PRECONDITION without a request", err, dialed)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
