package console

// Reading the Logs Explorer through a Cloud Logging log view (#1410).
//
// The Explorer's other scopes read the console's own recorder, which holds
// every entry the Logging API was written. A log view is Cloud Logging's own
// read: ListLogEntries and TailLogEntries on {bucket}/views/{view}, which
// apply the view's filter and the bucket's retention, so what the Explorer
// shows through a view is what an SDK reads through it. The bucket and view
// are chosen from a parent's buckets: a project's, a folder's, an
// organization's or a billing account's.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LogBucketViews is one log bucket and its views, for the Explorer's view
// picker.
type LogBucketViews struct {
	// Name is the bucket's resource name,
	// {parent}/locations/{location}/buckets/{bucket}.
	Name          string        `json:"name"`
	RetentionDays int32         `json:"retentionDays"`
	Views         []LogViewInfo `json:"views"`
}

// LogViewInfo is one of a bucket's views.
type LogViewInfo struct {
	// Name is the view's resource name, {bucket}/views/{view}.
	Name   string `json:"name"`
	Filter string `json:"filter,omitempty"`
}

// LogViewReader is Cloud Logging's read through a view, called in process on
// the servers the gRPC and JSON transports serve, with the instance's fault
// rules applied first.
type LogViewReader interface {
	// Buckets lists parent's buckets and their views.
	Buckets(ctx context.Context, parent string) ([]LogBucketViews, error)
	// Read is the view's newest limit entries, oldest first, through
	// ListLogEntries.
	Read(ctx context.Context, view string, limit int) ([]Entry, error)
	// Tail sends the view's entries as they are written, through
	// TailLogEntries, until ctx ends. It returns at once with the API's
	// error when the tail is refused.
	Tail(ctx context.Context, view string, send func(Entry)) error
}

// SetLogViewReader attaches Cloud Logging's view reads. Nil, the default,
// offers no view scope.
func (s *Server) SetLogViewReader(r LogViewReader) { s.logViews = r }

// handleLogBuckets is GET /api/logs/buckets?parent=: the parent's buckets
// and views.
func (s *Server) handleLogBuckets(w http.ResponseWriter, r *http.Request) {
	if s.logViews == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance"})
		return
	}
	parent := strings.TrimSpace(r.URL.Query().Get("parent"))
	if parent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parent is required: projects/P, folders/F, organizations/O or billingAccounts/B"})
		return
	}
	bs, err := s.logViews.Buckets(r.Context(), parent)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if bs == nil {
		bs = []LogBucketViews{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": bs})
}

// viewEntries reads a view and applies the Explorer's own filters to it; the
// view names the scope, so the project and parent filters do not apply.
func (s *Server) viewEntries(ctx context.Context, view string, f Filter) ([]Entry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	all, err := s.logViews.Read(ctx, view, 1000)
	if err != nil {
		return nil, err
	}
	f.Project, f.Parent, f.Limit = "", "", 0
	out := []Entry{}
	for i, e := range all {
		e.ID = uint64(i + 1)
		if f.matches(e) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// handleViewLogs is GET /api/logs?view=.
func (s *Server) handleViewLogs(w http.ResponseWriter, r *http.Request, view string) {
	if s.logViews == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance, so no log view can be read"})
		return
	}
	entries, err := s.viewEntries(r.Context(), view, filterFrom(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "held": len(entries), "unattributed": 0,
		"histogram": histogram(entries), "logDelete": s.logDeleter != nil, "view": view,
	})
}

// streamView is /api/stream?view=: the view's backlog, then its tail.
func (s *Server) streamView(w http.ResponseWriter, r *http.Request, flusher http.Flusher, view string) {
	filter := filterFrom(r)
	since := filter.Since
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			since = n
		}
	}
	filter.Since = 0
	backlog, err := s.viewEntries(r.Context(), view, filter)
	if err != nil {
		fmt.Fprintf(w, "event: failure\ndata: %s\n\n", mustJSON(map[string]string{"error": userMessage(err)}))
		flusher.Flush()
		return
	}
	var next uint64
	for _, e := range backlog {
		if e.ID > since {
			writeEvent(w, e)
		}
		next = e.ID
	}
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	live := make(chan Entry, 256)
	failed := make(chan error, 1)
	go func() {
		if err := s.logViews.Tail(ctx, view, func(e Entry) {
			select {
			case live <- e:
			case <-ctx.Done():
			}
		}); err != nil && ctx.Err() == nil {
			failed <- err
		}
	}()
	match := filter
	match.Project, match.Parent, match.Limit = "", "", 0
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-failed:
			fmt.Fprintf(w, "event: failure\ndata: %s\n\n", mustJSON(map[string]string{"error": userMessage(err)}))
			flusher.Flush()
			return
		case <-ticker.C:
			fmt.Fprint(w, "event: keepalive\ndata: {}\n\n")
			flusher.Flush()
		case e := <-live:
			next++
			e.ID = next
			if !match.matches(e) {
				continue
			}
			writeEvent(w, e)
			flusher.Flush()
		}
	}
}
