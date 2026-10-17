package firestoreadmin

import (
	"sort"
	"strings"
)

// The databases calls name (#1550). The emulator serves documents in any
// database ID it is sent, and cannot list databases, so documents written
// to a database the Admin API never made would otherwise survive the
// project's purge. The call front shows the API every call's resource
// (SawResource), and the API keeps each database named, to clear at the
// project's purge.

// databaseOf is the database a resource name is under,
// "projects/p/databases/d", or "".
func databaseOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "databases" ||
		parts[1] == "" || parts[3] == "" || parts[1] == "-" || parts[3] == "-" {
		return ""
	}
	return strings.Join(parts[:4], "/")
}

// SawResource records the database resource names, the resource a call
// through the front named (the google-cloud-resource-prefix header or a
// REST path), and writes the state when it is new.
func (a *Admin) SawResource(resource string) {
	db := databaseOf(resource)
	if db == "" {
		return
	}
	a.mu.Lock()
	added := !a.seen[db]
	a.seen[db] = true
	a.mu.Unlock()
	if added {
		a.persist()
	}
}

// seenList is the seen databases of project, or of every project for "",
// sorted. a.mu is held.
func (a *Admin) seenList(project string) []string {
	var out []string
	for n := range a.seen {
		if project == "" || strings.HasPrefix(n, "projects/"+project+"/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
