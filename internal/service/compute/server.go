// Package compute serves the VPC networking part of the Compute Engine API
// v1 (compute.googleapis.com, #1936) in the CLI process, over JSON as
// Google serves it: networks (auto and custom mode), subnetworks with their
// secondary ranges, firewall rules, routes, global and regional addresses,
// the global, regional and zonal operations, and the regions and zones.
//
// Nothing here carries traffic: a network is a record other services
// check their own network fields against (Memorystore's reserved ranges
// and PSC networks, Kubernetes Engine's cluster networks, Cloud SQL's
// private network), as Google checks them. Every mutation completes at
// once, and answers with a DONE operation in Compute's own shape
// (compute#operation), which clients poll or wait on as they do Google's.
//
// VM instances, disks, snapshots, machine types and the public images
// (#1963) are served too: each instance runs on a VMBackend (a systemd
// container in the instance's kind cluster), and its mutations answer
// operations that really run. Every other path answers 501.
package compute

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// Options configure a Server.
type Options struct {
	// Addr is the address to listen on; empty serves only through
	// ServeHTTP (tests, the console).
	Addr string
	// State is the JSON file the resources are kept in; empty keeps them
	// in memory only.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
	// Extra serves a path under /compute/v1/ that this package does not:
	// the Kubernetes Engine API's node pool instance group managers
	// (#1913). It reports whether it answered.
	Extra func(w http.ResponseWriter, r *http.Request) bool
	// NetworkUsers names the resources of other services that use a
	// network (projects/P/global/networks/N), which a network's deletion is
	// refused while they exist, as Google refuses it.
	NetworkUsers func(network string) []string
	// VMs runs VM instances and keeps their disks (#1963); nil answers
	// instances, disks and snapshots with 501.
	VMs VMBackend
	// MinFreeBytes is DefaultMinFreeBytes when zero.
	MinFreeBytes uint64
}

// obj is one resource, as its JSON is kept: link fields hold relative
// names (projects/P/...), made absolute when it is returned.
type obj = map[string]any

// Server is the Compute Engine API's VPC networking.
type Server struct {
	opts Options

	mu  sync.Mutex
	res map[string]obj // by relative name: projects/P/global/networks/N
	ops map[string]obj // by relative name: projects/P/global/operations/O
	// defaultDeleted are the projects whose default network was deleted,
	// so it is not made again.
	defaultDeleted map[string]bool
	// seeded are the projects whose default network has been made.
	seeded map[string]bool
	// waiters are the running operations, closed when each is done.
	waiters map[string]chan struct{}
	// pods are the pod IPs the running VMs' guests last reported (#1968),
	// by instance.
	pods map[string]podSeen

	srv *http.Server
	ln  net.Listener
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, res: map[string]obj{}, ops: map[string]obj{},
		defaultDeleted: map[string]bool{}, seeded: map[string]bool{}, waiters: map[string]chan struct{}{}}
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

// Name is the lifecycle name.
func (s *Server) Name() string { return "compute" }

// Start listens on Options.Addr.
func (s *Server) Start(context.Context) error {
	if s.opts.Addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: hostguard.Wrap(s), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop stops listening.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, or "" before Start.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// state is what the State file holds.
type state struct {
	Resources      map[string]obj  `json:"resources"`
	Operations     map[string]obj  `json:"operations"`
	DefaultDeleted map[string]bool `json:"defaultDeleted,omitempty"`
	Seeded         map[string]bool `json:"seeded,omitempty"`
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("compute: %s: %w", s.opts.State, err)
	}
	if st.Resources != nil {
		s.res = st.Resources
	}
	if st.Operations != nil {
		s.ops = st.Operations
	}
	if st.DefaultDeleted != nil {
		s.defaultDeleted = st.DefaultDeleted
	}
	if st.Seeded != nil {
		s.seeded = st.Seeded
	}
	return nil
}

// saveLocked writes the state file; s.mu is held.
func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := json.Marshal(state{Resources: s.res, Operations: s.ops, DefaultDeleted: s.defaultDeleted, Seeded: s.seeded})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o700); err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

// ResetProject forgets one project's resources, or every project's for "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.res, s.ops = map[string]obj{}, map[string]obj{}
		s.defaultDeleted, s.seeded = map[string]bool{}, map[string]bool{}
		return s.saveLocked()
	}
	prefix := "projects/" + project + "/"
	for k := range s.res {
		if strings.HasPrefix(k, prefix) {
			delete(s.res, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
		}
	}
	delete(s.defaultDeleted, project)
	delete(s.seeded, project)
	return s.saveLocked()
}

// apiError is an error in Compute's JSON shape.
type apiError struct {
	code   int
	reason string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// Status is the error's HTTP status and reason.
func (e *apiError) Status() (int, string) { return e.code, e.reason }

func errf(code int, reason, format string, args ...any) error {
	return &apiError{code: code, reason: reason, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errf(http.StatusBadRequest, "invalid", format, args...)
}

func notFound(name string) error {
	return errf(http.StatusNotFound, "notFound", "The resource '%s' was not found", name)
}

func unimplemented(format string, args ...any) error {
	return errf(http.StatusNotImplemented, "notImplemented", "Not available locally: "+format, args...)
}

// inUse is Google's refusal to delete a resource another uses.
func inUse(kind, name, user string) error {
	return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
		"The %s resource '%s' is already being used by '%s'", kind, name, user)
}

// ErrorStatus is err's HTTP status, for callers in process (the console).
func ErrorStatus(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{code: http.StatusInternalServerError, reason: "backendError", msg: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg,
		"errors": []map[string]string{{"domain": "global", "reason": ae.reason, "message": ae.msg}}}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// newID is a resource's numeric ID, as a decimal string as Compute's JSON
// carries uint64s.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatUint(binary.BigEndian.Uint64(b[:])>>1, 10)
}

func newFingerprint() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func (s *Server) stamp() string {
	return s.opts.Now().Format("2006-01-02T15:04:05.000-07:00")
}

// linkKeys are the fields that hold another resource's name, made absolute
// on the way out.
var linkKeys = map[string]bool{"selfLink": true, "network": true, "subnetwork": true, "region": true, "zone": true,
	"targetLink": true, "nextHopGateway": true, "nextHopNetwork": true, "nextHopInstance": true, "nextHopVpnTunnel": true,
	"subnetworks": true, "users": true, "zones": true, "machineType": true, "source": true, "sourceImage": true,
	"sourceSnapshot": true, "sourceDisk": true, "type": true}

// renderNested makes the link fields of objects inside lists and objects
// absolute (an instance's disks and network interfaces).
func renderNested(v any, base string) any {
	switch t := v.(type) {
	case map[string]any:
		return render(t, base)
	case []any:
		l := make([]any, len(t))
		for i, e := range t {
			l[i] = renderNested(e, base)
		}
		return l
	}
	return v
}

// render is o with its link fields made absolute under base.
func render(o obj, base string) obj {
	out := make(obj, len(o))
	for k, v := range o {
		if strings.HasPrefix(k, "_") {
			continue // kept, never returned
		}
		if linkKeys[k] {
			switch t := v.(type) {
			case string:
				if k == "sourceImage" && imageProjectLink(t) {
					// A public image lives in Google's image project, and
					// Google names it under www.googleapis.com whichever
					// endpoint was called: Terraform's provider recognises a
					// boot disk's image only in that form (#1965).
					out[k] = abs(t, googleBase)
					continue
				}
				out[k] = abs(t, base)
				continue
			case []any:
				l := make([]any, len(t))
				for i, e := range t {
					if es, ok := e.(string); ok {
						l[i] = abs(es, base)
					} else {
						l[i] = e
					}
				}
				out[k] = l
				continue
			}
		}
		if k != "items" {
			v = renderNested(v, base)
		}
		out[k] = v
	}
	if sl, ok := out["selfLink"].(string); ok {
		if id, ok := o["id"].(string); ok {
			if i := strings.LastIndex(sl, "/"); i >= 0 {
				out["selfLinkWithId"] = sl[:i+1] + id
			}
		}
	}
	return out
}

// googleBase is the base of Google's own Compute Engine links.
const googleBase = "https://www.googleapis.com/compute/v1/"

// imageProjectLink reports whether rel names an image of a public image
// project.
func imageProjectLink(rel string) bool {
	for _, p := range PublicImages {
		if strings.HasPrefix(rel, "projects/"+p.Project+"/global/images/") {
			return true
		}
	}
	return false
}

func abs(rel, base string) string {
	if strings.HasPrefix(rel, "projects/") {
		return base + rel
	}
	return rel
}

// Rel is the relative name ("projects/P/...") a link names: a full URL of
// any host, a projects/... name, or one relative to project (global/...,
// regions/..., zones/...). Anything else is returned as it is.
func Rel(link, project string) string {
	if i := strings.Index(link, "/projects/"); i >= 0 && strings.Contains(link, "://") {
		return link[i+1:]
	}
	switch {
	case strings.HasPrefix(link, "projects/"):
		return link
	case strings.HasPrefix(link, "global/"), strings.HasPrefix(link, "regions/"), strings.HasPrefix(link, "zones/"):
		return "projects/" + project + "/" + link
	}
	return link
}

// base is the absolute prefix of r's own selfLinks.
func base(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/compute/v1/"
}

// list pages through the objects under prefix (a collection's relative
// name and a slash), filtered.
func (s *Server) listLocked(m map[string]obj, prefix string, q map[string][]string) ([]obj, string, error) {
	f, err := parseFilter(first(q, "filter"))
	if err != nil {
		return nil, "", err
	}
	var keys []string
	for k := range m {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k[len(prefix):], "/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var all []obj
	for _, k := range keys {
		if f.match(m[k]) {
			all = append(all, m[k])
		}
	}
	return page(all, q)
}

func page(all []obj, q map[string][]string) ([]obj, string, error) {
	start := 0
	if t := first(q, "pageToken"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > len(all) {
			return nil, "", invalid("Invalid value for field 'pageToken': '%s'. Supplied page token is not valid.", t)
		}
		start = n
	}
	size := 500
	if v := first(q, "maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 500 {
			return nil, "", invalid("Invalid value for field 'maxResults': '%s'. Must be less than or equal to 500", v)
		}
		if n > 0 {
			size = n
		}
	}
	end := min(start+size, len(all))
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return all[start:end], next, nil
}

func first(q map[string][]string, k string) string {
	if v := q[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
