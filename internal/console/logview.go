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
	// Read is the newest limit entries of the resource names (log views, or
	// a log scope's projects and log views), oldest first, through
	// ListLogEntries.
	Read(ctx context.Context, names []string, limit int) ([]Entry, error)
	// Tail sends the resource names' entries as they are written, through
	// TailLogEntries, until ctx ends. It returns at once with the API's
	// error when the tail is refused.
	Tail(ctx context.Context, names []string, send func(Entry)) error
	// LogScopes is the project's log scopes, its _Default first, and the
	// name of its default log scope (#2020): the Observability API's
	// _Default scope's logScope.
	LogScopes(ctx context.Context, project string) (LogScopeSet, error)
}

// LogScopeSet is a project's log scopes and its default one.
type LogScopeSet struct {
	Default string         `json:"default"`
	Scopes  []LogScopeInfo `json:"scopes"`
}

// LogScopeInfo is one log scope: the projects and log views it searches.
type LogScopeInfo struct {
	Name          string   `json:"name"`
	ResourceNames []string `json:"resourceNames"`
	Description   string   `json:"description,omitempty"`
}

// handleLogScopes is GET /api/logs/scopes?project=: the project's log
// scopes and its default one, for the Explorer's Refine scope.
func (s *Server) handleLogScopes(w http.ResponseWriter, r *http.Request) {
	if s.logViews == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance"})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project is required: log scopes are a project's"})
		return
	}
	set, err := s.logViews.LogScopes(r.Context(), project)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if set.Scopes == nil {
		set.Scopes = []LogScopeInfo{}
	}
	writeJSON(w, http.StatusOK, set)
}

// scopeResources is what a log view or log scope read names: the view
// itself, or the log scope's projects and log views, as Google's Logs
// Explorer searches a log scope's resources.
func (s *Server) scopeResources(ctx context.Context, r *http.Request) (names []string, label string, err error) {
	q := r.URL.Query()
	if view := q.Get("view"); view != "" {
		return []string{view}, view, nil
	}
	name := q.Get("logScope")
	project, rest, ok := strings.Cut(strings.TrimPrefix(name, "projects/"), "/")
	if !strings.HasPrefix(name, "projects/") || !ok || !strings.HasPrefix(rest, "locations/global/logScopes/") {
		return nil, "", fmt.Errorf("log scope %q is not projects/PROJECT/locations/global/logScopes/SCOPE", name)
	}
	set, err := s.logViews.LogScopes(ctx, project)
	if err != nil {
		return nil, "", err
	}
	for _, sc := range set.Scopes {
		if sc.Name == name {
			return sc.ResourceNames, name, nil
		}
	}
	return nil, "", fmt.Errorf("log scope %s does not exist", name)
}

// scopedRead reports whether the request reads through Cloud Logging: a
// log view or a log scope.
func scopedRead(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("view") != "" || q.Get("logScope") != ""
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
func (s *Server) viewEntries(ctx context.Context, names []string, f Filter) ([]Entry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	all, err := s.logViews.Read(ctx, names, 1000)
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

// handleViewLogs is GET /api/logs?view= or ?logScope=.
func (s *Server) handleViewLogs(w http.ResponseWriter, r *http.Request) {
	if s.logViews == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance, so no log view or log scope can be read"})
		return
	}
	f := filterFrom(r)
	if f.QueryError != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": f.QueryError})
		return
	}
	names, label, err := s.scopeResources(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	entries, err := s.viewEntries(r.Context(), names, f)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	out := map[string]any{
		"entries": entries, "held": len(entries), "unattributed": 0,
		"histogram": histogram(entries), "logDelete": s.logDeleter != nil,
	}
	if r.URL.Query().Get("view") != "" {
		out["view"] = label
	} else {
		out["logScope"], out["resourceNames"] = label, names
	}
	writeJSON(w, http.StatusOK, out)
}

// streamView is /api/stream?view= or ?logScope=: the backlog, then its
// tail.
func (s *Server) streamView(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	names, _, err := s.scopeResources(r.Context(), r)
	if err != nil {
		fmt.Fprintf(w, "event: failure\ndata: %s\n\n", mustJSON(map[string]string{"error": userMessage(err)}))
		flusher.Flush()
		return
	}
	filter := filterFrom(r)
	since := filter.Since
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			since = n
		}
	}
	filter.Since = 0
	backlog, err := s.viewEntries(r.Context(), names, filter)
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
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	live := make(chan Entry, 256)
	failed := make(chan error, 1)
	go func() {
		if err := s.logViews.Tail(ctx, names, func(e Entry) {
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
