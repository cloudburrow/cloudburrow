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
	// MatchedColumn and MatchedValue name where the query was found, when it was
	// not in the name.
	//
	// Without them a search for an image tag returned a list of pod names with
	// no indication of why any of them matched, so the reader had to open each
	// one to find out. Naming the column is the difference between a result and
	// a guess.
	MatchedColumn string `json:"matchedColumn,omitempty"`
	MatchedValue  string `json:"matchedValue,omitempty"`
	// Kind distinguishes a resource hit from a log hit, so the results screen can
	// group them and link each to the right place.
	Kind string `json:"kind,omitempty"`
	// Link is where this hit goes, for a hit that is not a resource on a list
	// screen.
	Link string `json:"link,omitempty"`
}

// ProductHit is a product whose own name matched.
//
// Typing "tasks" used to return every resource in Cloud Tasks and no way to
// reach the screen itself, which is what someone typing a product name is
// looking for.
type ProductHit struct {
	Service string `json:"service"`
	Title   string `json:"title"`
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
	// Products are the screens whose own names matched.
	Products []ProductHit `json:"products,omitempty"`
	// Logs are matching log entries, capped.
	//
	// A search that covered resources and not logs claimed to search "the
	// instance" while missing everything the instance had said — and an error
	// message is the thing people most often paste into a search box.
	Logs []Entry `json:"logs,omitempty"`
	// LogsTruncated means more entries matched than are returned.
	LogsTruncated bool `json:"logsTruncated"`
	// Scope says what was and was not searched, so the screen can stop implying
	// completeness it does not have.
	Scope string `json:"scope,omitempty"`
}

const (
	// searchTimeout bounds the whole search. Some providers shell out to
	// kubectl, and a search is an interactive action: a slow one that
	// eventually completes is worse than a fast one that says it was cut
	// short.
	searchTimeout = 10 * time.Second
	// maxHits bounds the response. The cap is reported rather than hidden.
	maxHits = 200
	// maxLogHits bounds the log half of a search. Fewer than the resource cap,
	// because log lines are long and a hundred of them is already more than a
	// results page can usefully show.
	maxLogHits = 50
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
				// Every column, not only the name.
				//
				// A search that matched names alone could not find a pod by its
				// image, a service by its URL, a queue by its state or anything
				// at all by its namespace — which is most of what is on screen
				// and most of what anyone has to hand when they search.
				column, value, ok := matchIn(listing, item, needle)
				if !ok {
					continue
				}
				hit := SearchHit{
					Service: p.ID(), Title: p.Title(), Kind: "resource",
					Name: item.Name, Status: item.Status,
				}
				if len(listing.Columns) > 0 && item.Fields != nil {
					hit.Detail = item.Fields[listing.Columns[0]]
				}
				// Only when the match was not the name: repeating the name as
				// the reason would be noise on every ordinary hit.
				if column != "" {
					hit.MatchedColumn, hit.MatchedValue = column, value
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

	// The products whose own names matched. Someone typing "tasks" wants the
	// Cloud Tasks screen; returning only its contents makes the product itself
	// the one thing the search cannot find.
	for _, id := range s.order {
		p := s.providers[id]
		if strings.Contains(strings.ToLower(p.Title()), needle) ||
			strings.Contains(strings.ToLower(p.ID()), needle) {
			out.Products = append(out.Products, ProductHit{Service: p.ID(), Title: p.Title()})
		}
	}

	// And the logs. An error message is the thing people most often paste into a
	// search box, and a search that covered resources and not logs was missing
	// everything the instance had actually said.
	logs := s.logs.Entries(Filter{Project: project, Contains: query, Limit: maxLogHits + 1})
	if len(logs) > maxLogHits {
		logs = logs[:maxLogHits]
		out.LogsTruncated = true
	}
	out.Logs = logs

	// What this search did and did not cover, stated rather than implied. The
	// previous screen reported "searched N services" and left the reader to
	// conclude that was everything.
	out.Scope = "Resource names and every column on each list screen, plus " +
		"product names and the log entries this instance still holds. Not " +
		"searched: the contents of documents, rows and objects — those are " +
		"read through each product's own query surface."

	if len(out.Failed) == 0 {
		out.Failed = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// matchIn reports where a needle appears in one row.
//
// The name wins, and matches there are reported with no column: a hit whose
// reason is its own name needs no explanation. Otherwise the columns are checked
// in the order the screen shows them, so the reason given is the leftmost one a
// reader would have found by eye.
func matchIn(listing Listing, item Resource, needle string) (column, value string, ok bool) {
	if strings.Contains(strings.ToLower(item.Name), needle) {
		return "", "", true
	}
	for _, c := range listing.Columns {
		v := item.Fields[c]
		if v != "" && strings.Contains(strings.ToLower(v), needle) {
			return c, v, true
		}
	}
	if item.Status != "" && strings.Contains(strings.ToLower(item.Status), needle) {
		return "Status", item.Status, true
	}
	return "", "", false
}
