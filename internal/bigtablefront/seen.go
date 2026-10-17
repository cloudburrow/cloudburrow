package bigtablefront

import (
	"net/url"
	"sort"
	"strings"
	"sync"

	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
)

// seenInstances is every instance a call through the front has named, the
// instance admin's or not (#1550). The emulator serves tables under any
// instance ID, and cannot list instances, so a table created under an
// instance the front never made would otherwise survive the project's purge:
// the front deletes the tables of each instance it has seen.
type seenInstances struct {
	mu    sync.Mutex
	names map[string]bool
}

// seenInstanceOf is the instance a resource name is under,
// "projects/p/instances/i", or "".
func seenInstanceOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "instances" ||
		parts[1] == "" || parts[3] == "" || parts[1] == "-" || parts[3] == "-" {
		return ""
	}
	return strings.Join(parts[:4], "/")
}

// see records the instance name is under, if any; it reports a new one.
func (s *seenInstances) see(name string) bool {
	in := seenInstanceOf(name)
	if in == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names[in] {
		return false
	}
	if s.names == nil {
		s.names = map[string]bool{}
	}
	s.names[in] = true
	return true
}

// seeFrame records the instance a request names in its field 1 (its table,
// name or parent).
func (s *seenInstances) seeFrame(b []byte) bool { return s.see(calllog.FirstName(b)) }

// seeMetadata records the instances the routing header names
// (x-goog-request-params: table_name=, name=, parent=, ...).
func (s *seenInstances) seeMetadata(md metadata.MD) bool {
	added := false
	for _, v := range md.Get("x-goog-request-params") {
		q, err := url.ParseQuery(v)
		if err != nil {
			continue
		}
		for _, vs := range q {
			for _, n := range vs {
				if s.see(n) {
					added = true
				}
			}
		}
	}
	return added
}

// take removes and returns the seen instances of project, or of every
// project when project is "", sorted.
func (s *seenInstances) take(project string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.names {
		if project == "" || strings.HasPrefix(n, "projects/"+project+"/") {
			out = append(out, n)
			delete(s.names, n)
		}
	}
	sort.Strings(out)
	return out
}

// list is the seen instances, sorted.
func (s *seenInstances) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// set replaces the seen instances (a restore).
func (s *seenInstances) set(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = map[string]bool{}
	for _, n := range names {
		if in := seenInstanceOf(n); in != "" {
			s.names[in] = true
		}
	}
}

// sawIf writes the state when a call has named an instance not seen
// before, so the instance is kept before its first table is.
func (f *Front) sawIf(added bool) bool {
	if added {
		f.flush()
	}
	return added
}
