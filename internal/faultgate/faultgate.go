// Package faultgate applies /admin/faults rules inside the cluster (#2113):
// in the fronts of the Pub/Sub, Bigtable, Spanner, Firestore and Datastore
// pods and in the Cloud Storage server's, which the rules, held by the CLI,
// did not reach before.
//
// The host keeps the rules (internal/admin's Faults) and serves two paths
// on the Resource Manager port, which every server in the cluster already
// reaches through cloudburrow-host (--resourcemanager), as it does for
// disabled APIs (internal/apienable): RulesPath, which services, methods
// and projects have a rule, and DecidePath, which decides one call. A front
// keeps the rules for CacheFor and asks the host to decide only a call a
// rule may match, so the host draws the probability, takes from the
// count and records the fault in /admin/events exactly as for an
// in-process service; a rule added reaches the cluster within CacheFor.
//
// A faulted gRPC call is answered with the rule's status, after its
// latency; as for the in-process services, REST calls pass untouched. The
// storage server's faults are its own error bodies (Faulter).
package faultgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// RulesPath answers Rules: the services, methods and projects rules
	// name, without their counts.
	RulesPath = "/cloudburrow/v1/faults"
	// DecidePath takes a Call and answers a Decision.
	DecidePath = "/cloudburrow/v1/faults:decide"
	// CacheFor is how long a front keeps the rules.
	CacheFor = time.Second
)

// Rule is what a front needs of a rule to tell whether a call may match.
type Rule struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Project string `json:"project,omitempty"`
}

// Rules is RulesPath's answer.
type Rules struct {
	Rules []Rule `json:"rules"`
}

// Call is one call a front asks about.
type Call struct {
	Service  string `json:"service"`
	Method   string `json:"method"`
	Resource string `json:"resource,omitempty"`
}

// Decision is the host's answer: whether the call is faulted, the gRPC
// code (0 for a latency-only rule), the HTTP status of that code, the
// latency and the message.
type Decision struct {
	Fault      bool   `json:"fault"`
	Code       int    `json:"code,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	LatencyMs  int    `json:"latencyMs,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Matches is whether r may fault a call of service's method (its short
// name) on resource.
func (r Rule) Matches(c Call) bool {
	if r.Service != c.Service {
		return false
	}
	m := r.Method
	if m == "" {
		m = "*"
	}
	if ok, _ := path.Match(m, ShortMethod(c.Method)); !ok {
		return false
	}
	return r.Project == "" || c.Resource == "projects/"+r.Project || strings.HasPrefix(c.Resource, "projects/"+r.Project+"/")
}

// ShortMethod is a method's name after its service: Publish of
// /google.pubsub.v1.Publisher/Publish.
func ShortMethod(m string) string { return m[strings.LastIndex(m, "/")+1:] }

// Host is the host side: the rules and their decisions.
type Host interface {
	RemoteRules() Rules
	RemoteDecide(Call) Decision
}

// Handler serves RulesPath and DecidePath from h.
func Handler(h Host) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch {
		case r.URL.Path == RulesPath && r.Method == http.MethodGet:
			out = h.RemoteRules()
		case r.URL.Path == DecidePath && r.Method == http.MethodPost:
			var c Call
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&c); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			out = h.RemoteDecide(c)
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// IsPath is whether p is one of the paths Handler serves.
func IsPath(p string) bool { return p == RulesPath || p == DecidePath }

// Remote is a front's view of the host at Base ("http://host:port").
// When the host does not answer, no call is faulted.
type Remote struct {
	Base   string
	Client *http.Client

	mu      sync.Mutex
	rules   Rules
	fetched time.Time
}

var remoteClient = &http.Client{Timeout: 5 * time.Second}

var current atomic.Pointer[Remote]

// Use makes the gates ask r (nil: fault nothing).
func Use(r *Remote) { current.Store(r) }

func (c *Remote) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return remoteClient
}

func (c *Remote) snapshot(ctx context.Context) Rules {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched.IsZero() && time.Since(c.fetched) < CacheFor {
		return c.rules
	}
	var rs Rules
	if err := c.do(ctx, http.MethodGet, RulesPath, nil, &rs); err == nil {
		c.rules = rs
	}
	c.fetched = time.Now()
	return c.rules
}

func (c *Remote) do(ctx context.Context, method, p string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("faultgate: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// Decide is the host's decision on c, asked only when a rule may match.
func (c *Remote) Decide(ctx context.Context, call Call) Decision {
	if c == nil {
		return Decision{}
	}
	may := false
	for _, r := range c.snapshot(ctx).Rules {
		if r.Matches(call) {
			may = true
			break
		}
	}
	if !may {
		return Decision{}
	}
	var d Decision
	if err := c.do(ctx, http.MethodPost, DecidePath, call, &d); err != nil {
		return Decision{}
	}
	return d
}

// Gate applies the rules for service to the gRPC calls h serves.
func Gate(service string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rem := current.Load()
		if rem == nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") || r.Method != http.MethodPost {
			h.ServeHTTP(w, r)
			return
		}
		d := rem.Decide(r.Context(), Call{Service: service, Method: r.URL.Path, Resource: RoutedResource(r.Header)})
		if !d.Fault {
			h.ServeHTTP(w, r)
			return
		}
		if d.LatencyMs > 0 {
			t := time.NewTimer(time.Duration(d.LatencyMs) * time.Millisecond)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				writeStatus(w, 4, "context deadline exceeded") // DEADLINE_EXCEEDED
				return
			}
		}
		if d.Code == 0 {
			h.ServeHTTP(w, r)
			return
		}
		writeStatus(w, d.Code, d.Message)
	})
}

// writeStatus answers a gRPC call with code and message alone (a
// trailers-only response).
func writeStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", strconv.Itoa(code))
	w.Header().Set("Grpc-Message", encodeGRPCMessage(msg))
	w.WriteHeader(http.StatusOK)
}

// encodeGRPCMessage percent-encodes msg as the gRPC wire spec says.
func encodeGRPCMessage(msg string) string {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c >= ' ' && c <= '~' && c != '%' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// RoutedResource is the resource a call's x-goog-request-params names:
// the first value that is a resource name under projects/, or
// projects/{project_id} for Datastore's project_id.
func RoutedResource(h http.Header) string {
	for _, v := range h.Values("X-Goog-Request-Params") {
		q, err := url.ParseQuery(v)
		if err != nil {
			continue
		}
		for _, vs := range q {
			for _, s := range vs {
				if strings.HasPrefix(s, "projects/") {
					return s
				}
			}
		}
		// Datastore routes by project_id (and database_id).
		if p := q.Get("project_id"); p != "" {
			return "projects/" + p
		}
	}
	return ""
}

// Faulter is the storage server's internal/service/storage Faulter over
// the host's rules for service.
type Faulter struct{ Service string }

// Fault implements storage.Faulter.
func (f Faulter) Fault(method, resource string) (int, time.Duration, bool) {
	d := current.Load().Decide(context.Background(), Call{Service: f.Service, Method: method, Resource: resource})
	if !d.Fault {
		return 0, 0, false
	}
	status := 0
	if d.Code != 0 {
		status = d.HTTPStatus
	}
	return status, time.Duration(d.LatencyMs) * time.Millisecond, true
}

// StreamInterceptor applies the rules for service to a gRPC server's
// calls, every one of which is a stream on a front that forwards unknown
// services (the Pub/Sub and Bigtable fronts).
func StreamInterceptor(service string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		rem := current.Load()
		if rem == nil {
			return handler(srv, ss)
		}
		h := http.Header{}
		if md, ok := metadata.FromIncomingContext(ss.Context()); ok {
			for _, v := range md.Get("x-goog-request-params") {
				h.Add("X-Goog-Request-Params", v)
			}
		}
		d := rem.Decide(ss.Context(), Call{Service: service, Method: info.FullMethod, Resource: RoutedResource(h)})
		if !d.Fault {
			return handler(srv, ss)
		}
		if d.LatencyMs > 0 {
			t := time.NewTimer(time.Duration(d.LatencyMs) * time.Millisecond)
			select {
			case <-t.C:
			case <-ss.Context().Done():
				t.Stop()
				return status.FromContextError(ss.Context().Err()).Err()
			}
		}
		if d.Code == 0 {
			return handler(srv, ss)
		}
		return status.Error(codes.Code(d.Code), d.Message)
	}
}
