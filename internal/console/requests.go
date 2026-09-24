package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The Request Log (#291): the API calls CloudBurrow served, from the same
// recorder /admin/events reads, filterable and live.
//
// Only services CloudBurrow serves itself can appear: traffic to Storage,
// Pub/Sub and the opt-in emulators goes over a raw port-forward to an
// upstream process and is never seen. The page names those services rather
// than showing an empty list that would read as "no calls were made".

// RequestEvent is one served call, with only the fields the page shows. It
// is built field by field, so nothing a recorder might hold beyond these,
// such as a payload, can reach a console response.
type RequestEvent struct {
	Time       time.Time `json:"time"`
	Service    string    `json:"service"`
	Method     string    `json:"method"`
	Resource   string    `json:"resource,omitempty"`
	Project    string    `json:"project,omitempty"`
	Code       string    `json:"code"`
	DurationMS int64     `json:"duration_ms"`
	Transport  string    `json:"transport"`
}

// RequestFilter narrows the log. A zero field matches everything.
type RequestFilter struct {
	Service, Code, Project string
}

func (f RequestFilter) match(e RequestEvent) bool {
	return (f.Service == "" || e.Service == f.Service) &&
		(f.Code == "" || e.Code == f.Code) &&
		(f.Project == "" || e.Project == f.Project)
}

// RequestSource is where requests come from: the admin recorder, adapted.
type RequestSource interface {
	// Requests returns recorded requests, newest first.
	Requests() []RequestEvent
	// WatchRequests delivers requests as they are recorded.
	WatchRequests(buf int) (<-chan RequestEvent, func())
	// Unobserved names the enabled services whose requests cannot be seen.
	Unobserved() []string
}

// SetRequests attaches the Request Log's source.
func (s *Server) SetRequests(src RequestSource) { s.requests = src }

// NotObservable labels a service whose traffic CloudBurrow never sees.
const NotObservable = "requests not observable (direct port-forward to upstream emulator)"

func requestFilterFrom(r *http.Request) RequestFilter {
	q := r.URL.Query()
	return RequestFilter{Service: q.Get("service"), Code: q.Get("code"), Project: q.Get("project")}
}

type unobserved struct {
	Service string `json:"service"`
	Label   string `json:"label"`
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Requests   []RequestEvent `json:"requests"`
		Unobserved []unobserved   `json:"unobserved"`
	}{Requests: []RequestEvent{}, Unobserved: []unobserved{}}
	if s.requests != nil {
		f := requestFilterFrom(r)
		limit := 500
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < limit {
			limit = v
		}
		for _, e := range s.requests.Requests() {
			if f.match(e) {
				body.Requests = append(body.Requests, e)
				if len(body.Requests) == limit {
					break
				}
			}
		}
		for _, svc := range s.requests.Unobserved() {
			body.Unobserved = append(body.Unobserved, unobserved{svc, NotObservable})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// streamRequests is /api/stream?stream=requests: each matching request as a
// `request` event, as it is recorded.
func (s *Server) streamRequests(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	if s.requests == nil {
		return
	}
	f := requestFilterFrom(r)
	ch, stop := s.requests.WatchRequests(256)
	defer stop()
	// Flushed at once, so the client sees the stream open before the first
	// request. Without it the headers waited for one, and an idle instance's
	// page sat at "connecting" indefinitely.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !f.match(e) {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: request\ndata: %s\n\n", b)
			flusher.Flush()
		}
	}
}
