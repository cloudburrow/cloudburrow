// Package marketplacetest serves Docker Hub's recorded answers for
// Marketplace's tests (#2149): the unit tests and the console's browser
// tests read the same fixture, so neither reaches Docker Hub.
package marketplacetest

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/marketplace"
)

//go:embed dockerhub.json
var fixtureJSON []byte

// The fixture is Docker Hub's answers for nginx, recorded on 2026-11-22
// (dockerhub.json): a v4 search, a filtered search, the
// categories, library/nginx's repository and first tags, and nginx:alpine's
// index, two platform manifests and their configs. The pull token is not
// recorded; the fixture's token endpoint answers a fixed one.
type hubFixture struct {
	SearchNginx      json.RawMessage            `json:"search_nginx"`
	SearchOfficialDB json.RawMessage            `json:"search_official_db"`
	Categories       json.RawMessage            `json:"categories"`
	RepoNginx        json.RawMessage            `json:"repo_nginx"`
	TagsNginx        json.RawMessage            `json:"tags_nginx"`
	IndexAlpine      json.RawMessage            `json:"index_alpine"`
	Manifests        map[string]json.RawMessage `json:"manifests"`
	Configs          map[string]json.RawMessage `json:"configs"`
}

// Recorded is what the fixture was asked.
type Recorded struct {
	mu   sync.Mutex
	URLs []string
	Auth []string
}

// DockerHub is a Docker Hub source over the fixture.
func DockerHub(t testing.TB) (*marketplace.DockerHub, *Recorded) {
	t.Helper()
	var fx hubFixture
	if err := json.Unmarshal(fixtureJSON, &fx); err != nil {
		t.Fatal(err)
	}
	rec := &Recorded{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	mux.HandleFunc("/api/search/v4", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("from") == "1000":
			w.WriteHeader(http.StatusForbidden)
			write(w, []byte(`{"message":"pagination too large for anonymous requests; sign in to get more results and page further"}`))
		case q.Get("badges") != "":
			write(w, fx.SearchOfficialDB)
		default:
			write(w, fx.SearchNginx)
		}
	})
	mux.HandleFunc("/v2/categories", func(w http.ResponseWriter, _ *http.Request) { write(w, fx.Categories) })
	mux.HandleFunc("/v2/repositories/library/nginx/", func(w http.ResponseWriter, _ *http.Request) { write(w, fx.RepoNginx) })
	mux.HandleFunc("/v2/repositories/library/nginx/tags", func(w http.ResponseWriter, _ *http.Request) { write(w, fx.TagsNginx) })
	mux.HandleFunc("/v2/repositories/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		write(w, []byte(`{"message":"object not found","errinfo":{}}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:library/nginx:pull" {
			t.Errorf("token scope %q", r.URL.Query().Get("scope"))
		}
		write(w, []byte(`{"token":"fixture-token"}`))
	})
	mux.HandleFunc("/v2/library/nginx/manifests/", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.Auth = append(rec.Auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		ref := strings.TrimPrefix(r.URL.Path, "/v2/library/nginx/manifests/")
		if m, ok := fx.Manifests[ref]; ok {
			write(w, m)
			return
		}
		if !strings.HasPrefix(ref, "sha256:") {
			// Every tag is nginx:alpine's index, the one recorded.
			write(w, fx.IndexAlpine)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		write(w, []byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
	})
	mux.HandleFunc("/v2/library/nginx/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if c, ok := fx.Configs[strings.TrimPrefix(r.URL.Path, "/v2/library/nginx/blobs/")]; ok {
			write(w, c)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.URLs = append(rec.URLs, r.URL.String())
		rec.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &marketplace.DockerHub{HubURL: srv.URL, AuthURL: srv.URL, RegistryURL: srv.URL, Client: srv.Client(), Arch: "arm64"}, rec
}
