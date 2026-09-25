package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"path"
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
// Only the services CloudBurrow serves itself can be interposed — Cloud
// Tasks, Secret Manager and the Cloud Run adapter — because the fault is
// applied in their gRPC interceptor. Cloud Storage, Pub/Sub and the opt-in
// emulators are reached through a raw port-forward to an upstream process
// that CloudBurrow never sees a request of, so a rule for one is refused with
// the reason rather than accepted and never applied.

// FaultRule is one rule, as the API takes and lists it.
type FaultRule struct {
	ID string `json:"id"`
	// Service is tasks, secretmanager or run.
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

// interposable are the services whose requests CloudBurrow serves itself.
var interposable = map[string]bool{"tasks": true, "secretmanager": true, "run": true, "kms": true}

// Faults holds the active rules.
type Faults struct {
	mu     sync.Mutex
	rules  []*FaultRule
	nextID int
	rec    *Recorder
	global *rand.Rand
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

// validate completes a rule, or says why it cannot be applied.
func (r *FaultRule) validate() error {
	if !interposable[r.Service] {
		if r.Service == "" {
			return fmt.Errorf("service is required: one of tasks, secretmanager, run, kms")
		}
		return fmt.Errorf("service %q cannot be interposed: CloudBurrow reaches it through a port-forward to "+
			"the upstream emulator and never sees its requests. Faults apply to tasks, secretmanager, run and kms", r.Service)
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
// observer, so the recorded code is the injected one.
func (f *Faults) Interceptor(service string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if f == nil {
			return handler(ctx, req)
		}
		rule := f.decide(service, info.FullMethod, resourceOf(req))
		if rule == nil {
			return handler(ctx, req)
		}
		detail := map[string]string{"rule": rule.ID}
		if rule.LatencyMs > 0 {
			detail["latency_ms"] = strconv.Itoa(rule.LatencyMs)
			select {
			case <-time.After(time.Duration(rule.LatencyMs) * time.Millisecond):
			case <-ctx.Done():
				detail["code"] = "DEADLINE_EXCEEDED"
				f.rec.Record(service, "fault", info.FullMethod, detail)
				return nil, status.FromContextError(ctx.Err()).Err()
			}
		}
		if rule.code == codes.OK {
			f.rec.Record(service, "fault", info.FullMethod, detail)
			return handler(ctx, req)
		}
		detail["code"] = rule.Code
		f.rec.Record(service, "fault", info.FullMethod, detail)
		return nil, status.Errorf(rule.code, "injected fault (rule %s): %s", rule.ID, rule.Code)
	}
}

// routes are /admin/faults.
func (f *Faults) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/faults", f.handleAdd)
	mux.HandleFunc("GET /admin/faults", f.handleList)
	mux.HandleFunc("DELETE /admin/faults", f.handleDelete)
}

func (f *Faults) handleAdd(w http.ResponseWriter, r *http.Request) {
	var rule FaultRule
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed rule: " + err.Error()})
		return
	}
	if err := rule.validate(); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{"faults": out})
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
