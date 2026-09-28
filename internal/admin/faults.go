package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Fault injection (#306): rules that make CloudBurrow-served calls fail or
// slow down, so a client's retry and deadline handling can be exercised
// against the SDK it actually uses.
//
// Only the services CloudBurrow serves in this process can be interposed,
// because the fault is applied in their gRPC interceptor (or, for Cloud
// Storage's builtin server, its HTTP handler). Building a service's
// Interceptor is what makes its rules accepted, so the set of services a rule
// may name is exactly the set that applies rules, with no list beside it to
// fall out of step (#600). Pub/Sub and the opt-in emulators are reached
// through a raw port-forward to an upstream process that CloudBurrow never
// sees a request of, and a service that is not enabled has no server at all,
// so a rule for either is refused with the reason rather than accepted and
// never applied.

// FaultRule is one rule, as the API takes and lists it.
type FaultRule struct {
	ID string `json:"id"`
	// Service is one this instance interposes: tasks, secretmanager, run,
	// kms, scheduler, logging or resourcemanager when they are enabled.
	Service string `json:"service"`
	// Method is a glob over the method name, "AccessSecretVersion" or
	// "Get*"; empty or "*" matches every method.
	Method string `json:"method,omitempty"`
	// Project, when set, limits the rule to calls whose resource is under
	// projects/{project}.
	Project string `json:"project,omitempty"`
	// Probability is the chance a matching call is faulted; 0 means 1.
	Probability float64 `json:"probability,omitempty"`
	// Code is the gRPC code to return, by name: UNAVAILABLE, INTERNAL...
	Code string `json:"code,omitempty"`
	// HTTPStatus is an alternative to Code, mapped to its gRPC code the way
	// Google maps them.
	HTTPStatus int `json:"httpStatus,omitempty"`
	// LatencyMs delays a matching call. A rule with latency and no code or
	// status only delays; with either it delays and then fails.
	LatencyMs int `json:"latencyMs,omitempty"`
	// Count is how many faults the rule injects before it stops; 0 means no
	// limit. Remaining is how many are left.
	Count     int `json:"count,omitempty"`
	Remaining int `json:"remaining,omitempty"`
	// Seed makes a probabilistic rule reproducible: the same seed gives the
	// same sequence of faulted and passed calls.
	Seed *int64 `json:"seed,omitempty"`
	// Injected counts the faults this rule has injected.
	Injected int `json:"injected"`

	code codes.Code
	rng  *rand.Rand
}

// Faults holds the active rules.
type Faults struct {
	mu     sync.Mutex
	rules  []*FaultRule
	nextID int
	rec    *Recorder
	global *rand.Rand
	// interposed are the services whose rules are applied: every service an
	// Interceptor was built for, and any named to Interpose.
	interposed map[string]bool
	// enabled are the services the instance runs, named by Enabled, so the
	// list can say why each one that is not interposed is refused.
	enabled map[string]bool
}

// Enabled names services the instance runs. It changes nothing about which
// rules are accepted; it only lets GET /admin/faults list, for each enabled
// service that is not interposed, the reason a rule for it is refused, which
// is the message POST gives (#800).
func (f *Faults) Enabled(services ...string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled == nil {
		f.enabled = map[string]bool{}
	}
	for _, s := range services {
		f.enabled[s] = true
	}
}

// Refusal is one enabled service a rule may not name, and why.
type Refusal struct {
	Service string `json:"service"`
	Reason  string `json:"reason"`
}

// FaultServices is what a rule may name: the services whose rules are
// applied, the enabled services whose rules are refused with the reason POST
// gives, and the codes and HTTP statuses a rule may fail a call with.
type FaultServices struct {
	Interposed   []string  `json:"interposed"`
	Refused      []Refusal `json:"refused"`
	Codes        []string  `json:"codes"`
	HTTPStatuses []int     `json:"httpStatuses"`
}

// Services reports what a rule may name on this instance.
func (f *Faults) Services() FaultServices {
	out := FaultServices{Interposed: []string{}, Refused: []Refusal{}, Codes: []string{}, HTTPStatuses: []int{}}
	if f == nil {
		return out
	}
	f.mu.Lock()
	interposed := map[string]bool{}
	for k, v := range f.interposed {
		interposed[k] = v
	}
	var enabled []string
	for s := range f.enabled {
		enabled = append(enabled, s)
	}
	f.mu.Unlock()
	for s, ok := range interposed {
		if ok {
			out.Interposed = append(out.Interposed, s)
		}
	}
	sort.Strings(out.Interposed)
	sort.Strings(enabled)
	for _, s := range enabled {
		if interposed[s] {
			continue
		}
		// The reason is validate's own, so the list and a refused POST say
		// the same thing.
		if err := (&FaultRule{Service: s}).validate(interposed); err != nil {
			out.Refused = append(out.Refused, Refusal{Service: s, Reason: err.Error()})
		}
	}
	for c := codes.Canceled; c <= codes.Unauthenticated; c++ {
		out.Codes = append(out.Codes, codeName(c))
	}
	for s := 400; s <= 599; s++ {
		if _, ok := httpToCode(s); ok {
			out.HTTPStatuses = append(out.HTTPStatuses, s)
		}
	}
	return out
}

// Interpose accepts rules for a service whose requests this process applies
// them to. Interceptor calls it, so a gRPC service needs nothing more; a
// service served over HTTP that applies rules through DecideHTTP, such as
// Cloud Storage on the builtin server (#513), calls it directly. Until it is
// called, a rule for the service is refused, as for any service CloudBurrow
// never sees a request of.
func (f *Faults) Interpose(service string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.interposed == nil {
		f.interposed = map[string]bool{}
	}
	f.interposed[service] = true
}

// interposedList is the interposed services, sorted and joined for a message.
func interposedList(interposed map[string]bool) string {
	names := make([]string, 0, len(interposed))
	for s, ok := range interposed {
		if ok {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return "none on this instance"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// codeToHTTP maps a gRPC code to the HTTP status Google's APIs use for it.
func codeToHTTP(c codes.Code) int {
	for s := 400; s <= 504; s++ {
		if got, ok := httpToCode(s); ok && got == c {
			return s
		}
	}
	return http.StatusInternalServerError
}

// DecideHTTP applies the rules to an HTTP-served call and records the fault
// as the gRPC interceptor does: the status to answer with (0 when the rule
// only delays) and the delay, or ok false for no fault.
func (f *Faults) DecideHTTP(service, method, resource string) (status int, delay time.Duration, ok bool) {
	if f == nil {
		return 0, 0, false
	}
	rule := f.decide(service, method, resource)
	if rule == nil {
		return 0, 0, false
	}
	detail := map[string]string{"rule": rule.ID}
	if rule.LatencyMs > 0 {
		delay = time.Duration(rule.LatencyMs) * time.Millisecond
		detail["latency_ms"] = strconv.Itoa(rule.LatencyMs)
	}
	switch {
	case rule.HTTPStatus != 0:
		status = rule.HTTPStatus
	case rule.code != codes.OK:
		status = codeToHTTP(rule.code)
	}
	if status != 0 {
		detail["code"] = strconv.Itoa(status)
	}
	f.rec.Record(service, "fault", method, detail)
	return status, delay, true
}

// NewFaults returns an empty rule set that records each injected fault on rec.
func NewFaults(rec *Recorder) *Faults {
	return &Faults{rec: rec, global: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// httpToCode maps an HTTP status to the gRPC code Google's APIs use for it.
func httpToCode(s int) (codes.Code, bool) {
	switch s {
	case 400:
		return codes.InvalidArgument, true
	case 401:
		return codes.Unauthenticated, true
	case 403:
		return codes.PermissionDenied, true
	case 404:
		return codes.NotFound, true
	case 409:
		return codes.Aborted, true
	case 429:
		return codes.ResourceExhausted, true
	case 499:
		return codes.Canceled, true
	case 500:
		return codes.Internal, true
	case 501:
		return codes.Unimplemented, true
	case 503:
		return codes.Unavailable, true
	case 504:
		return codes.DeadlineExceeded, true
	}
	return 0, false
}

func codeByName(name string) (codes.Code, bool) {
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if strings.EqualFold(strings.ReplaceAll(c.String(), "_", ""), strings.ReplaceAll(name, "_", "")) {
			return c, true
		}
	}
	return 0, false
}

// validate completes a rule, or says why it cannot be applied. interposed
// are the services this process applies rules to; see Interpose.
func (r *FaultRule) validate(interposed map[string]bool) error {
	if !interposed[r.Service] {
		list := interposedList(interposed)
		if r.Service == "" {
			return fmt.Errorf("service is required: one of %s", list)
		}
		if r.Service == "storage" {
			// `up` runs the storage server in the cluster (#514), where a rule
			// held by this process cannot reach it.
			return fmt.Errorf("service \"storage\" cannot be interposed: its server runs in the cluster, where "+
				"CloudBurrow's fault rules do not reach. Faults apply to %s", list)
		}
		return fmt.Errorf("service %q cannot be interposed on this instance: faults apply only to the services "+
			"CloudBurrow serves in its own process and has enabled (%s). A service that is not enabled has no "+
			"server to fault, and Pub/Sub and the opt-in emulators are reached through a port-forward to an "+
			"upstream process whose requests CloudBurrow never sees", r.Service, list)
	}
	if r.Method == "" {
		r.Method = "*"
	}
	if _, err := path.Match(r.Method, "x"); err != nil {
		return fmt.Errorf("method %q is not a valid glob: %v", r.Method, err)
	}
	if r.Probability == 0 {
		r.Probability = 1
	}
	if r.Probability < 0 || r.Probability > 1 {
		return fmt.Errorf("probability must be between 0 and 1")
	}
	if r.LatencyMs < 0 || r.LatencyMs > 600000 {
		return fmt.Errorf("latencyMs must be between 0 and 600000")
	}
	if r.Count < 0 {
		return fmt.Errorf("count must not be negative")
	}
	switch {
	case r.Code != "" && r.HTTPStatus != 0:
		return fmt.Errorf("give code or httpStatus, not both")
	case r.Code != "":
		c, ok := codeByName(r.Code)
		if !ok || c == codes.OK {
			return fmt.Errorf("code %q is not a gRPC error code", r.Code)
		}
		r.code = c
	case r.HTTPStatus != 0:
		c, ok := httpToCode(r.HTTPStatus)
		if !ok {
			return fmt.Errorf("httpStatus %d has no gRPC equivalent; use code", r.HTTPStatus)
		}
		r.code = c
	case r.LatencyMs > 0:
		r.code = codes.OK // latency only
	default:
		r.code = codes.Unavailable
	}
	if r.code != codes.OK {
		r.Code = strings.ToUpper(codeName(r.code))
	}
	r.Remaining = r.Count
	if r.Seed != nil {
		r.rng = rand.New(rand.NewSource(*r.Seed))
	}
	return nil
}

// codeName is a code's canonical name, NOT_FOUND rather than NotFound.
func codeName(c codes.Code) string {
	var b strings.Builder
	for i, ch := range c.String() {
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(ch)
	}
	return strings.ToUpper(b.String())
}

// Clear removes the rules for the named services, or every rule.
func (f *Faults) Clear(services ...string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(services) == 0 {
		f.rules = nil
		return
	}
	drop := map[string]bool{}
	for _, s := range services {
		drop[s] = true
	}
	kept := f.rules[:0]
	for _, r := range f.rules {
		if !drop[r.Service] {
			kept = append(kept, r)
		}
	}
	f.rules = kept
}

// decide returns the rule a call triggers, if any, consuming one of its
// count. The draw for a seeded rule is taken on every matching call, faulted
// or not, so its sequence depends only on its seed and the order of calls.
func (f *Faults) decide(service, method, resource string) *FaultRule {
	short := method[strings.LastIndex(method, "/")+1:]
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rules {
		if r.Service != service || (r.Count > 0 && r.Remaining == 0) {
			continue
		}
		if ok, _ := path.Match(r.Method, short); !ok {
			continue
		}
		if r.Project != "" && !strings.HasPrefix(resource, "projects/"+r.Project+"/") && resource != "projects/"+r.Project {
			continue
		}
		rng := r.rng
		if rng == nil {
			rng = f.global
		}
		if r.Probability < 1 && rng.Float64() >= r.Probability {
			continue
		}
		if r.Count > 0 {
			r.Remaining--
		}
		r.Injected++
		copied := *r
		return &copied
	}
	return nil
}

// resourceOf reads the request's name or parent, as the call observer does.
func resourceOf(req any) string {
	if r, ok := req.(interface{ GetName() string }); ok && r.GetName() != "" {
		return r.GetName()
	}
	if r, ok := req.(interface{ GetParent() string }); ok {
		return r.GetParent()
	}
	return ""
}

// Interceptor applies the rules for one service. It belongs inside the call
// observer, so the recorded code is the injected one. Building it is what
// makes the service's rules accepted (see Interpose), so a service is
// fault-injectable exactly when its server applies the rules.
func (f *Faults) Interceptor(service string) grpc.UnaryServerInterceptor {
	f.Interpose(service)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := f.Apply(ctx, service, info.FullMethod, resourceOf(req)); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Apply applies the rules to one call of a service's method on resource, as
// the service's gRPC interceptor does: it waits out any latency, records the
// fault, and returns the status error the call fails with, or nil when no
// rule fails it. The interceptor is built on it, and so is the console's read
// of the store an in-process service serves (#594), so a rule on ListQueues
// fails the Cloud Tasks screen with the same message an SDK receives.
func (f *Faults) Apply(ctx context.Context, service, method, resource string) error {
	if f == nil {
		return nil
	}
	rule := f.decide(service, method, resource)
	if rule == nil {
		return nil
	}
	detail := map[string]string{"rule": rule.ID}
	if rule.LatencyMs > 0 {
		detail["latency_ms"] = strconv.Itoa(rule.LatencyMs)
		select {
		case <-time.After(time.Duration(rule.LatencyMs) * time.Millisecond):
		case <-ctx.Done():
			detail["code"] = "DEADLINE_EXCEEDED"
			f.rec.Record(service, "fault", method, detail)
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	if rule.code == codes.OK {
		f.rec.Record(service, "fault", method, detail)
		return nil
	}
	detail["code"] = rule.Code
	f.rec.Record(service, "fault", method, detail)
	return status.Errorf(rule.code, "injected fault (rule %s): %s", rule.ID, rule.Code)
}

// routes are /admin/faults.
func (f *Faults) routes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /admin/faults", guard(f.handleAdd))
	mux.HandleFunc("GET /admin/faults", guard(f.handleList))
	mux.HandleFunc("DELETE /admin/faults", guard(f.handleDelete))
}

func (f *Faults) handleAdd(w http.ResponseWriter, r *http.Request) {
	var rule FaultRule
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed rule: " + err.Error()})
		return
	}
	f.mu.Lock()
	interposed := map[string]bool{}
	for k, v := range f.interposed {
		interposed[k] = v
	}
	f.mu.Unlock()
	if err := rule.validate(interposed); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f.mu.Lock()
	f.nextID++
	rule.ID = "fault-" + strconv.Itoa(f.nextID)
	f.rules = append(f.rules, &rule)
	out := rule
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (f *Faults) handleList(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	out := make([]FaultRule, 0, len(f.rules))
	for _, r := range f.rules {
		out = append(out, *r)
	}
	f.mu.Unlock()
	svc := f.Services()
	writeJSON(w, http.StatusOK, map[string]any{"faults": out, "interposed": svc.Interposed, "refused": svc.Refused,
		"codes": svc.Codes, "httpStatuses": svc.HTTPStatuses})
}

// handleDelete removes one rule with ?id=, or every rule.
func (f *Faults) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		n := len(f.rules)
		f.rules = nil
		writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
		return
	}
	for i, rule := range f.rules {
		if rule.ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			writeJSON(w, http.StatusOK, map[string]int{"deleted": 1})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no fault rule " + id})
}
