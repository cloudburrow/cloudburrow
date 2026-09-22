package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Severity mirrors the levels a Logs Explorer filters by.
type Severity string

const (
	SeverityDefault Severity = "DEFAULT"
	SeverityInfo    Severity = "INFO"
	SeverityWarning Severity = "WARNING"
	SeverityError   Severity = "ERROR"
)

// severityRank orders severities so a filter can mean "this and above".
var severityRank = map[Severity]int{
	SeverityDefault: 0, SeverityInfo: 1, SeverityWarning: 2, SeverityError: 3,
}

// AtLeast reports whether s is at least as severe as min.
func (s Severity) AtLeast(min Severity) bool {
	return severityRank[s] >= severityRank[min]
}

// Entry is one log line or activity record.
type Entry struct {
	// ID is monotonic within a process, so a reconnecting client can resume
	// from where it stopped rather than replaying everything.
	ID        uint64    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Severity  Severity  `json:"severity"`
	// Source names where the line came from — "run/my-service", "tasks",
	// "pubsub". A line with no attributable source is worse than no line:
	// it cannot be correlated with anything.
	Source string `json:"source"`
	// Project scopes the entry, so one project's activity never appears
	// under another.
	Project string `json:"project,omitempty"`
	// Resource is the specific thing this concerns.
	Resource string `json:"resource,omitempty"`
	// OperationID links an entry to the operation that caused it, which is
	// what makes "click a failed operation to see its logs" possible.
	OperationID string `json:"operationId,omitempty"`
	Message     string `json:"message"`
}

// OperationState is the lifecycle of a tracked operation.
type OperationState string

const (
	OperationPending   OperationState = "PENDING"
	OperationSucceeded OperationState = "SUCCEEDED"
	OperationFailed    OperationState = "FAILED"
	OperationCancelled OperationState = "CANCELLED"
)

// Operation is one tracked mutation.
type Operation struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Resource string         `json:"resource"`
	Project  string         `json:"project,omitempty"`
	State    OperationState `json:"state"`
	Started  time.Time      `json:"started"`
	Ended    time.Time      `json:"ended,omitempty"`
	// Error carries the failure's cause, so a failed operation explains
	// itself rather than only being red.
	Error string `json:"error,omitempty"`
}

// credentialPattern matches things that must never reach a log view.
//
// Redaction happens on the way in, not on the way out: an entry that was
// stored with a token in it has already been written somewhere a later
// change might expose.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ya29\.[A-Za-z0-9_\-]+)`),
	regexp.MustCompile(`(?i)\b(cbl_[A-Za-z0-9_\-]+)`),
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(bearer\s+)?\S+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|password|secret|token|credential)\s*[:=]\s*)\S+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]+`),
}

// Redact removes credentials from a message.
func Redact(message string) string {
	out := message
	for _, re := range credentialPatterns {
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			// The prefix is kept so the line still says what kind of thing
			// was there; a line that loses its shape loses its meaning.
			if i := strings.IndexAny(match, ":="); i >= 0 && !strings.HasPrefix(match, "ya29.") &&
				!strings.HasPrefix(match, "cbl_") && !strings.HasPrefix(match, "eyJ") {
				return match[:i+1] + " [REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return out
}

// MaxMessageBytes bounds one entry.
//
// An application that logs a whole request body would otherwise fill the
// buffer with one payload and push out everything that explains it. The
// truncation is marked so nobody reads a cut line as the whole line.
const MaxMessageBytes = 2048

// Recorder holds a bounded, in-memory log.
//
// In memory and bounded on purpose: a development console that grew without
// limit would eventually be the reason the machine ran out of memory, and
// persisting logs would make CloudBurrow responsible for data it never
// promised to keep.
type Recorder struct {
	mu        sync.Mutex
	entries   []Entry
	ops       map[string]*Operation
	opOrder   []string
	limit     int
	nextID    uint64
	now       func() time.Time
	watchers  map[int]chan Entry
	nextWatch int
}

// DefaultLogLimit is how many entries are kept.
const DefaultLogLimit = 2000

// NewRecorder returns a recorder keeping at most limit entries.
func NewRecorder(limit int, now func() time.Time) *Recorder {
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		limit: limit, now: now,
		ops:      map[string]*Operation{},
		watchers: map[int]chan Entry{},
	}
}

// Log records one entry and returns it.
func (r *Recorder) Log(e Entry) Entry {
	if r == nil {
		return Entry{}
	}
	r.mu.Lock()

	if e.Timestamp.IsZero() {
		e.Timestamp = r.now().UTC()
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}
	e.Message = Redact(e.Message)
	if len(e.Message) > MaxMessageBytes {
		e.Message = e.Message[:MaxMessageBytes] + "… [truncated]"
	}
	r.nextID++
	e.ID = r.nextID

	r.entries = append(r.entries, e)
	if len(r.entries) > r.limit {
		r.entries = r.entries[len(r.entries)-r.limit:]
	}

	watchers := make([]chan Entry, 0, len(r.watchers))
	for _, ch := range r.watchers {
		watchers = append(watchers, ch)
	}
	r.mu.Unlock()

	for _, ch := range watchers {
		// Never block on a slow reader: a stalled browser tab must not stop
		// the process from logging.
		select {
		case ch <- e:
		default:
		}
	}
	return e
}

// Entries returns matching entries, oldest first.
func (r *Recorder) Entries(f Filter) []Entry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		if f.matches(e) {
			out = append(out, e)
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out
}

// Filter narrows a log query.
type Filter struct {
	Project     string
	Source      string
	Resource    string
	OperationID string
	MinSeverity Severity
	Since       uint64
	Contains    string
	Limit       int
}

func (f Filter) matches(e Entry) bool {
	if f.Since > 0 && e.ID <= f.Since {
		return false
	}
	// Project scoping is strict: an entry with no project never appears
	// under a specific one, because attributing it would be a guess.
	if f.Project != "" && e.Project != f.Project {
		return false
	}
	if f.Source != "" && !strings.HasPrefix(e.Source, f.Source) {
		return false
	}
	if f.Resource != "" && e.Resource != f.Resource {
		return false
	}
	if f.OperationID != "" && e.OperationID != f.OperationID {
		return false
	}
	if f.MinSeverity != "" && !e.Severity.AtLeast(f.MinSeverity) {
		return false
	}
	if f.Contains != "" && !strings.Contains(strings.ToLower(e.Message), strings.ToLower(f.Contains)) {
		return false
	}
	return true
}

// StartOperation records a pending operation and returns its ID.
func (r *Recorder) StartOperation(kind, resource, project string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	id := fmt.Sprintf("op-%d", r.nextID)
	r.ops[id] = &Operation{
		ID: id, Kind: kind, Resource: resource, Project: project,
		State: OperationPending, Started: r.now().UTC(),
	}
	r.opOrder = append(r.opOrder, id)
	if len(r.opOrder) > r.limit {
		delete(r.ops, r.opOrder[0])
		r.opOrder = r.opOrder[1:]
	}
	return id
}

// FinishOperation records a terminal state.
//
// The state comes from what the backend reported, never from the console's
// own optimism: an operation is not successful because the request returned.
func (r *Recorder) FinishOperation(id string, state OperationState, cause string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	op, ok := r.ops[id]
	if ok {
		op.State = state
		op.Ended = r.now().UTC()
		op.Error = Redact(cause)
	}
	r.mu.Unlock()
}

// NameOperation records what an operation turned out to be about.
//
// A create does not know the resource's name until the backend has made it —
// the console's own form may not even carry it, since a provider is free to
// derive one. Without this the Activity screen and the notifications panel
// both said "create Pub/Sub", naming the product rather than the thing.
func (r *Recorder) NameOperation(id, resource string) {
	if r == nil || id == "" || resource == "" {
		return
	}
	r.mu.Lock()
	if op, ok := r.ops[id]; ok {
		op.Resource = resource
	}
	r.mu.Unlock()
}

// Operations returns tracked operations, newest first.
func (r *Recorder) Operations(project string) []Operation {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Operation, 0, len(r.opOrder))
	for _, id := range r.opOrder {
		op := r.ops[id]
		if op == nil {
			continue
		}
		if project != "" && op.Project != project {
			continue
		}
		out = append(out, *op)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Watch registers a stream of new entries and returns a stop function.
func (r *Recorder) Watch(buffer int) (<-chan Entry, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Entry, buffer)

	r.mu.Lock()
	id := r.nextWatch
	r.nextWatch++
	r.watchers[id] = ch
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		delete(r.watchers, id)
		r.mu.Unlock()
		close(ch)
	}
}

// --- HTTP ------------------------------------------------------------

func filterFrom(r *http.Request) Filter {
	q := r.URL.Query()
	f := Filter{
		Project:     q.Get("project"),
		Source:      q.Get("source"),
		Resource:    q.Get("resource"),
		OperationID: q.Get("operation"),
		Contains:    q.Get("contains"),
	}
	if s := q.Get("severity"); s != "" {
		f.MinSeverity = Severity(strings.ToUpper(s))
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		f.Limit = n
	}
	if n, err := strconv.ParseUint(q.Get("since"), 10, 64); err == nil {
		f.Since = n
	}
	return f
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	entries := s.logs.Entries(filterFrom(r))
	if entries == nil {
		entries = []Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	ops := s.logs.Operations(r.URL.Query().Get("project"))
	if ops == nil {
		ops = []Operation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

// handleStream serves Server-Sent Events (#88).
//
// SSE rather than a socket: the console only ever pushes, browsers reconnect
// on their own, and Last-Event-ID gives resume for free — a socket would add
// a protocol to maintain for a stream that never needs to carry anything
// upstream.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this server", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Without this a proxy may buffer the stream into uselessness.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	filter := filterFrom(r)
	// A reconnecting browser sends the last id it saw, so the gap is
	// replayed rather than lost. Replaying everything instead would make a
	// reconnect look like a flood of new activity.
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			filter.Since = n
		}
	}

	stream, stop := s.logs.Watch(256)
	defer stop()

	// The backlog is sent before live entries, and the watch is registered
	// first, so nothing logged between the two is dropped.
	backlog := s.logs.Entries(filter)
	var lastSent uint64
	for _, e := range backlog {
		writeEvent(w, e)
		lastSent = e.ID
	}
	flusher.Flush()

	// A heartbeat keeps an idle connection from being closed by anything in
	// between, and tells the client the stream is alive rather than stalled.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A named event rather than a comment. A comment keeps the
			// connection open but is invisible to EventSource, so the client
			// could not tell a healthy idle stream from one that had stalled
			// with the socket still open — the two look identical, and one of
			// them means the log view is lying.
			fmt.Fprint(w, "event: keepalive\ndata: {}\n\n")
			flusher.Flush()
		case e, ok := <-stream:
			if !ok {
				return
			}
			if e.ID <= lastSent || !filter.matches(e) {
				continue
			}
			writeEvent(w, e)
			lastSent = e.ID
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e Entry) {
	fmt.Fprintf(w, "id: %d\n", e.ID)
	fmt.Fprint(w, "event: log\n")
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(e))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// An entry that cannot be encoded is dropped rather than breaking
		// the stream for every other entry.
		return `{"message":"[entry could not be encoded]"}`
	}
	return string(b)
}
