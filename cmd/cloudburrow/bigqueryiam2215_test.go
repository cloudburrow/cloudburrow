package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestBigQueryIAMIssuedOwnAccount (#2215 round 7): a token minted for the
// instance's own account, which the front's payload never gives, is not
// pushed to the front (so a metadata-server token request never waits on
// it); one minted for another account is pushed before it is answered.
func TestBigQueryIAMIssuedOwnAccount(t *testing.T) {
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	reg := resourcemanager.New(store.NewMemory())
	creds := &metadata.Credentials{Email: "instance@local-project.iam.gserviceaccount.com"}
	b := &bigqueryIAM{creds: creds, registry: func() *resourcemanager.Registry { return reg },
		addr: func() string { return strings.TrimPrefix(srv.URL, "http://") }, client: srv.Client()}
	ctx := context.Background()
	for _, email := range []string{creds.Email, strings.ToUpper(creds.Email), ""} {
		b.issued(ctx, metadata.IssuedToken{Email: email, Expires: time.Now().Add(time.Hour)})
	}
	if n := puts.Load(); n != 0 {
		t.Errorf("the instance's own tokens pushed the payload %d times; want 0", n)
	}
	b.issued(ctx, metadata.IssuedToken{Email: "worker@local-project.iam.gserviceaccount.com", Expires: time.Now().Add(time.Hour)})
	if n := puts.Load(); n != 1 {
		t.Errorf("another account's token pushed the payload %d times; want 1", n)
	}
}
