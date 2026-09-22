package console

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// SearchHit is one matching resource.
type SearchHit struct {
	// Service is the provider's ID, which the client maps to a screen.
	Service string `json:"service"`
	Title   string `json:"title"`
	Name    string `json:"name"`
	Status  string `json:"status,omitempty"`
	// Detail is the first field the screen shows, so a hit carries enough to
	// tell two similarly named resources apart.
	Detail string `json:"detail,omitempty"`
}

// SearchResults is what the search screen renders.
type SearchResults struct {
	Query string      `json:"query"`
	Hits  []SearchHit `json:"hits"`
	// Searched counts the services that answered.
	Searched int `json:"searched"`
	// Failed names the services that did not, and why.
	//
	// A search that quietly skipped an unreachable service would report "no
	// results" for a resource that exists, which is the worst answer a search
	// can give.
	Failed map[string]string `json:"failed,omitempty"`
	// Truncated means the cap was reached and there may be more.
	Truncated bool `json:"truncated"`
}

const (
	// searchTimeout bounds the whole search. Some providers shell out to
	// kubectl, and a search is an interactive action: a slow one that
	// eventually completes is worse than a fast one that says it was cut
	// short.
	searchTimeout = 10 * time.Second
	// maxHits bounds the response. The cap is reported rather than hidden.
	maxHits = 200
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	project := r.URL.Query().Get("project")

	out := SearchResults{Query: query, Failed: map[string]string{}}
	if query == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		hits []SearchHit
	)
	needle := strings.ToLower(query)

	// Providers are searched concurrently: done in sequence, a single slow
	// backend would set the pace for the whole search.
	for _, id := range s.order {
		p := s.providers[id]
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()

			listing, err := p.List(ctx, project)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Failed[p.Title()] = err.Error()
				return
			}
			if listing.Unavailable != "" {
				out.Failed[p.Title()] = listing.Unavailable
				return
			}
			// A prompt is not a failure — the screen simply needs a project.
			// It is counted as searched so the total is honest.
			out.Searched++

			for _, item := range listing.Items {
				if !strings.Contains(strings.ToLower(item.Name), needle) {
					continue
				}
				hit := SearchHit{
					Service: p.ID(), Title: p.Title(),
					Name: item.Name, Status: item.Status,
				}
				if len(listing.Columns) > 0 && item.Fields != nil {
					hit.Detail = item.Fields[listing.Columns[0]]
				}
				hits = append(hits, hit)
			}
		}(p)
	}
	wg.Wait()

	// Ordered so the same query gives the same answer twice: concurrent
	// providers finish in whatever order they finish.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Title != hits[j].Title {
			return hits[i].Title < hits[j].Title
		}
		return hits[i].Name < hits[j].Name
	})
	if len(hits) > maxHits {
		hits = hits[:maxHits]
		out.Truncated = true
	}
	out.Hits = hits
	if len(out.Failed) == 0 {
		out.Failed = nil
	}
	writeJSON(w, http.StatusOK, out)
}
