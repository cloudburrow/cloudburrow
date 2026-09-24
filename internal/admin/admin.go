// Package admin implements the local-only control API: seed, reset and event
// inspection.
//
// It is served on the control port and refused on service ports, because reset
// destroys data and must be unreachable from the container network or the LAN
// (docs/adr/0004).
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Event is an observable thing that happened, recorded for inspection.
//
// Events answer "did my publish actually arrive?" without attaching a debugger
// or reading pod logs, which is the common question when a local workflow does
// not behave.
type Event struct {
	Time    time.Time         `json:"time"`
	Service string            `json:"service"`
	Kind    string            `json:"kind"`
	Target  string            `json:"target"`
	Detail  map[string]string `json:"detail,omitempty"`
}

// Recorder keeps a bounded ring of recent events.
//
// Bounded on purpose: an unbounded log in a long-running dev environment is a
// memory leak, and old events are rarely what you are looking for.
type Recorder struct {
	mu     sync.Mutex
	events []Event
	limit  int
	now    func() time.Time
	// watchers receive each event as it is recorded, for the console's live
	// Request Log (#291).
	watchers map[chan Event]struct{}
}

// Subscribe returns a channel of events recorded from now on, and a function
// that ends the subscription. Delivery never blocks recording: a subscriber
// that falls more than buf events behind misses events, rather than slowing
// every API call down to its pace.
func (r *Recorder) Subscribe(buf int) (<-chan Event, func()) {
	ch := make(chan Event, buf)
	if r == nil {
		close(ch)
		return ch, func() {}
	}
	r.mu.Lock()
	if r.watchers == nil {
		r.watchers = map[chan Event]struct{}{}
	}
	r.watchers[ch] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.watchers, ch)
			r.mu.Unlock()
			close(ch)
		})
	}
}

// NewRecorder returns a recorder holding at most limit events.
func NewRecorder(limit int, now func() time.Time) *Recorder {
	if limit <= 0 {
		limit = 1000
	}
	if now == nil {
		now = time.Now
	}
	return &Recorder{limit: limit, now: now}
}

// Record appends an event, discarding the oldest when full.
func (r *Recorder) Record(service, kind, target string, detail map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := Event{Time: r.now().UTC(), Service: service, Kind: kind, Target: target, Detail: detail}
	r.events = append(r.events, e)
	if len(r.events) > r.limit {
		r.events = r.events[len(r.events)-r.limit:]
	}
	for ch := range r.watchers {
		select {
		case ch <- e:
		default:
		}
	}
}

// Filter narrows the events returned. A zero field matches everything.
type Filter struct {
	Service string
	Kind    string
	// Since keeps only events recorded strictly after it, so a caller polling
	// with the last timestamp it saw gets each event once.
	Since time.Time
}

// Events returns recorded events for one service, newest first.
func (r *Recorder) Events(service string, limit int) []Event {
	return r.EventsWhere(Filter{Service: service}, limit)
}

// EventsWhere returns recorded events matching f, newest first.
func (r *Recorder) EventsWhere(f Filter, limit int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Walk backwards rather than sorting by timestamp. Insertion order is
	// already chronological, and sorting ties by time returns same-instant
	// events oldest-first — which is exactly wrong for a newest-first view,
	// and happens constantly because events are often recorded in the same
	// nanosecond.
	var out []Event
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if f.Service != "" && e.Service != f.Service {
			continue
		}
		if f.Kind != "" && e.Kind != f.Kind {
			continue
		}
		if !f.Since.IsZero() && !e.Time.After(f.Since) {
			// Chronological, so everything older follows. Stopping here keeps
			// a poll proportional to what is new rather than to the ring.
			break
		}
		out = append(out, e)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Len reports how many events are held.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// Resetter destroys a component's state.
//
// Implementations must cancel scheduled work before deleting state: a live
// worker would otherwise recreate what was just removed (architecture §7).
type Resetter interface {
	Name() string
	Reset(ctx context.Context) error
}

// ProjectResetter is a Resetter that can confine a reset to one project.
//
// Optional, because not every backend can: fake-gcs-server lists every bucket
// on the server whatever project is asked for, so a "project" reset of Cloud
// Storage would either delete another project's buckets or do nothing. A
// component that cannot scope by project does not implement this, and a
// project-scoped reset naming it is refused rather than guessed at.
type ProjectResetter interface {
	Resetter
	ResetProject(ctx context.Context, project string) error
}

// Seeder creates resources from a seed document.
type Seeder interface {
	Name() string
	Seed(ctx context.Context, spec json.RawMessage) error
}

// Validator is a Seeder that can check its document without creating
// anything. Every component's document is validated before any is seeded, so
// one bad field cannot leave the others half-created.
type Validator interface {
	Validate(spec json.RawMessage) error
}

// API is the admin HTTP surface.
type API struct {
	recorder *Recorder
	resets   []Resetter
	seeds    map[string]Seeder

	mu      sync.Mutex
	startup *SeedPlan // the seed file `up` applied, if any

	snapshots          []Snapshotter
	notCaptured        []ManifestService
	producer, instance string
}

// NewAPI returns an admin API.
func NewAPI(rec *Recorder) *API {
	return &API{recorder: rec, seeds: map[string]Seeder{}}
}

// RegisterResetter adds a component to the reset set. Order matters: resets run
// in registration order.
func (a *API) RegisterResetter(r ...Resetter) { a.resets = append(a.resets, r...) }

// RegisterSeeder adds a seedable component.
func (a *API) RegisterSeeder(s Seeder) { a.seeds[s.Name()] = s }

// Routes registers the admin endpoints on a mux.
//
// Every route is mutating or revealing, which is why this mux belongs only on
// the loopback-only control port.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/reset", a.handleReset)
	mux.HandleFunc("POST /admin/seed", a.handleSeed)
	mux.HandleFunc("GET /admin/events", a.handleEvents)
	mux.HandleFunc("POST /admin/state/export", a.handleStateExport)
	mux.HandleFunc("POST /admin/state/import", a.handleStateImport)
}

type resetResponse struct {
	Reset  []string          `json:"reset"`
	Failed map[string]string `json:"failed,omitempty"`
	// Reseeded names the components re-applied from the startup seed.
	Reseeded []string `json:"reseeded,omitempty"`
}

// handleReset destroys state across registered components.
//
// Every component is attempted even when one fails, and failures are reported
// per component: a partial reset that claimed success would leave a developer
// debugging state they believe was cleared.
//
// ?service= (repeatable, or comma-separated) narrows it to named components and
// ?project= to one project. Both are validated before anything is deleted: an
// unknown name, or a project scope a component cannot honour, is a 400 with
// nothing touched, because half-applying a malformed reset is worse than not
// applying it.
func (a *API) handleReset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var wanted []string
	for _, v := range q["service"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				wanted = append(wanted, name)
			}
		}
	}
	project := strings.TrimSpace(q.Get("project"))
	reseed := q.Get("reseed") == "true"
	a.mu.Lock()
	startup := a.startup
	a.mu.Unlock()
	// Refused before anything is reset: a reset that then could not reseed
	// would leave the instance emptier than the caller asked for.
	if reseed && startup == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reseed=true, but up was not given a seed file; nothing was reset"})
		return
	}
	if reseed && project != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reseed=true cannot be combined with project=: a seed file is not scoped to one project; nothing was reset"})
		return
	}

	selected := a.resets
	if len(wanted) > 0 {
		byName := map[string]bool{}
		for _, c := range a.resets {
			byName[c.Name()] = true
		}
		want := map[string]bool{}
		var unknown []string
		for _, name := range wanted {
			if !byName[name] {
				unknown = append(unknown, name)
			}
			want[name] = true
		}
		// Registration order, not the order named: it is the order the
		// resetters depend on, and naming a service twice resets it once.
		selected = nil
		for _, c := range a.resets {
			if want[c.Name()] {
				selected = append(selected, c)
			}
		}
		if len(unknown) > 0 {
			known := make([]string, 0, len(a.resets))
			for _, c := range a.resets {
				known = append(known, c.Name())
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "unknown service(s); nothing was reset", "unknown": unknown, "known": known})
			return
		}
	}
	if project != "" {
		var cannot []string
		for _, c := range selected {
			if _, ok := c.(ProjectResetter); !ok {
				cannot = append(cannot, c.Name())
			}
		}
		if len(cannot) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "these services cannot be reset by project; nothing was reset. " +
					"Reset them without project=, or leave them out with service=",
				"cannot_scope_by_project": cannot})
			return
		}
	}

	resp := resetResponse{Failed: map[string]string{}}
	for _, c := range selected {
		var err error
		if project != "" {
			err = c.(ProjectResetter).ResetProject(r.Context(), project)
		} else {
			err = c.Reset(r.Context())
		}
		if err != nil {
			resp.Failed[c.Name()] = err.Error()
			continue
		}
		resp.Reset = append(resp.Reset, c.Name())
	}
	status := http.StatusOK
	if len(resp.Failed) > 0 {
		status = http.StatusInternalServerError
	} else {
		resp.Failed = nil
	}
	// Reseeded only for the services that were reset, and only when every
	// reset succeeded, so the result is exactly the seed: the component of a
	// service that was not reset would conflict with what is still there.
	if reseed && status == http.StatusOK {
		wasReset := map[string]bool{}
		for _, n := range resp.Reset {
			wasReset[n] = true
		}
		seeded, err := a.ApplySeed(r.Context(), startup, func(name string) bool { return wasReset[name] })
		resp.Reseeded = seeded
		if err != nil {
			resp.Failed = map[string]string{"reseed": err.Error()}
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, resp)
}

type seedRequest struct {
	// Components maps a component name to its seed document.
	Components map[string]json.RawMessage `json:"components"`
}

func (a *API) handleSeed(w http.ResponseWriter, r *http.Request) {
	doc, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed seed document: " + err.Error()})
		return
	}
	plan, err := a.PlanSeed(doc, false)
	if err != nil {
		writeSeedError(w, err)
		return
	}
	seeded, err := a.ApplySeed(r.Context(), plan, nil)
	if err != nil {
		writeSeedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seeded": seeded})
}

// SeedPlan is a seed document every component has validated.
type SeedPlan struct {
	names []string // in the fixed order they are applied
	docs  map[string]json.RawMessage
}

// Components names the plan's components, in the order they are applied.
func (p *SeedPlan) Components() []string { return append([]string(nil), p.names...) }

// SeedError is a seeding failure with the status it maps to.
type SeedError struct {
	Status    int
	Component string
	Message   string
	Seeded    []string
}

func (e *SeedError) Error() string { return e.Message }

func writeSeedError(w http.ResponseWriter, err error) {
	var se *SeedError
	if !errors.As(err, &se) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	body := map[string]any{"error": se.Message}
	if se.Component != "" {
		body["component"] = se.Component
	}
	if se.Seeded != nil {
		body["seeded"] = se.Seeded
	}
	writeJSON(w, se.Status, body)
}

// PlanSeed validates a seed document without creating anything: every
// component name, then every component's document, so an unknown component
// or a bad field cannot leave a half-seeded environment behind.
//
// skipExisting sets ifNotExists on every component, which is how a startup
// seed is applied: a persistent instance restarted with the same seed file
// already has what it declares, and that is not an error.
func (a *API) PlanSeed(doc []byte, skipExisting bool) (*SeedPlan, error) {
	var req seedRequest
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, &SeedError{Status: http.StatusBadRequest, Message: "malformed seed document: " + err.Error()}
	}
	if len(req.Components) == 0 {
		return nil, &SeedError{Status: http.StatusBadRequest, Message: "no components named"}
	}
	plan := &SeedPlan{docs: map[string]json.RawMessage{}}
	for name, spec := range req.Components {
		if _, ok := a.seeds[name]; !ok {
			return nil, &SeedError{Status: http.StatusBadRequest, Message: fmt.Sprintf("unknown component %q", name)}
		}
		if skipExisting {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(spec, &fields); err != nil {
				return nil, &SeedError{Status: http.StatusBadRequest, Component: name,
					Message: fmt.Sprintf("seed %s: %v; nothing was seeded", name, err)}
			}
			fields["ifNotExists"] = json.RawMessage("true")
			spec, _ = json.Marshal(fields)
		}
		plan.names = append(plan.names, name)
		plan.docs[name] = spec
	}
	// A fixed order, so a failure part-way is reproducible.
	sort.Strings(plan.names)
	for _, name := range plan.names {
		v, ok := a.seeds[name].(Validator)
		if !ok {
			continue
		}
		if err := v.Validate(plan.docs[name]); err != nil {
			return nil, &SeedError{Status: http.StatusBadRequest, Component: name,
				Message: fmt.Sprintf("seed %s: %v; nothing was seeded", name, err)}
		}
	}
	return plan, nil
}

// ApplySeed seeds a validated plan, in its order. only, when set, limits it
// to the components it accepts.
func (a *API) ApplySeed(ctx context.Context, plan *SeedPlan, only func(string) bool) ([]string, error) {
	seeded := []string{}
	for _, name := range plan.names {
		if only != nil && !only(name) {
			continue
		}
		if err := a.seeds[name].Seed(ctx, plan.docs[name]); err != nil {
			return seeded, &SeedError{Status: seedStatus(err), Component: name,
				Message: fmt.Sprintf("seed %s: %v", name, err), Seeded: seeded}
		}
		seeded = append(seeded, name)
	}
	return seeded, nil
}

// SetStartupSeed records the seed `up` applied, for reset?reseed=true.
func (a *API) SetStartupSeed(plan *SeedPlan) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startup = plan
}

// seedStatus maps a seeding failure to its HTTP status. A resource that
// already exists is the caller's conflict, not a server fault, and a script
// re-running a seed needs to tell the two apart.
func seedStatus(err error) int {
	c := status.Code(err)
	if c == codes.Unknown {
		return http.StatusInternalServerError
	}
	return (&apierror.Error{Code: c}).HTTPStatus()
}

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &limit); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be an integer"})
			return
		}
	}
	q := r.URL.Query()
	f := Filter{Service: q.Get("service"), Kind: q.Get("kind")}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "since must be an RFC 3339 timestamp, such as the time of the last event seen"})
			return
		}
		f.Since = t
	}
	events := a.recorder.EventsWhere(f, limit)
	if events == nil {
		events = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
