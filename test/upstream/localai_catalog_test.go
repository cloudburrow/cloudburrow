//go:build upstream

package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/identity-wael/cloudburrow/internal/localai"
)

// The catalogue makes three checkable claims per entry: the repository exists,
// its gating is what we recorded, and the named artifact is a file in it. All
// three were once asserted from a convention rather than a listing, and the
// community entry's filename was wrong as a result — it carried the google/
// repositories' -int4 suffix, and the resolve URL returned 404. A catalogue
// that names a file which does not exist is worse than an empty one, because
// acquisition fails at download time rather than at lookup.
//
// This is tagged upstream because it queries a live third-party API: it
// belongs with the other probes, not in `make check`.
type hfModel struct {
	Siblings []struct {
		Rfilename string `json:"rfilename"`
	} `json:"siblings"`
	// Gated is false for open repositories and the string "auto" or "manual"
	// for gated ones, so it cannot be decoded as a bool.
	Gated json.RawMessage `json:"gated"`
	Error string          `json:"error"`
}

func fetchHFModel(t *testing.T, repo string) hfModel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://huggingface.co/api/models/%s?full=true", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Hugging Face is unreachable, so the claim cannot be checked: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: metadata = %d, want 200 — the catalogued repository does not resolve",
			repo, resp.StatusCode)
	}
	var m hfModel
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("%s: decode: %v", repo, err)
	}
	if m.Error != "" {
		t.Fatalf("%s: %s", repo, m.Error)
	}
	return m
}

// TestCatalogArtifactsExistUpstream is the check that was missing.
func TestCatalogArtifactsExistUpstream(t *testing.T) {
	for _, model := range localai.Models() {
		t.Run(model.ID, func(t *testing.T) {
			m := fetchHFModel(t, model.Repo)

			found := false
			names := make([]string, 0, len(m.Siblings))
			for _, s := range m.Siblings {
				names = append(names, s.Rfilename)
				if s.Rfilename == model.Artifact {
					found = true
				}
			}
			if !found {
				t.Errorf("%s names artifact %q, which %s does not contain.\nIt holds: %v",
					model.ID, model.Artifact, model.Repo, names)
			}

			// Gating decides whether acquisition may run unattended, so a
			// drift here is a behaviour change, not a documentation nit.
			gatedUpstream := string(m.Gated) != "false"
			gatedLocally := model.Access == localai.AccessGated
			if gatedUpstream != gatedLocally {
				t.Errorf("%s: catalogued as %s, upstream gated=%s",
					model.ID, model.Access, m.Gated)
			}
		})
	}
}

// TestOpenCatalogArtifactsAreDownloadableUpstream goes one step further for the
// entries we claim need no credentials: metadata being public does not mean
// the bytes are. A HEAD on the resolve URL is what acquisition actually does.
func TestOpenCatalogArtifactsAreDownloadableUpstream(t *testing.T) {
	for _, model := range localai.Models() {
		if model.Access != localai.AccessOpen {
			continue
		}
		t.Run(model.ID, func(t *testing.T) {
			url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s",
				model.Repo, model.Artifact)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Skipf("Hugging Face is unreachable: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: HEAD %s = %d, want 200 — an entry marked open is not fetchable",
					model.ID, url, resp.StatusCode)
			}
		})
	}
}
