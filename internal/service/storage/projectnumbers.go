package storage

// Project numbers (#1544).
//
// A bucket's projectNumber, a project team's entity and the project's
// service agent name the project by number. The registry gives a project
// its number, and it is the derived one (resource.DerivedProjectNumber)
// unless the ID was used by a project since purged; this server cannot read
// the registry, so `cloudburrow up` sends it the numbers that differ, whole,
// at POST /_cloudburrow/project-numbers, at start and after every project
// is created or purged. They are kept in the metadata store, so a restart
// keeps them until the next push.

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// ProjectNumbersPath is where the host sends the project numbers that are
// not the derived ones: a POST of {"projectId": number, ...}, answered 204.
const ProjectNumbersPath = "/_cloudburrow/project-numbers"

// projectNumbersKey is the metadata record holding them. It is outside
// every reset prefix: a reset drops resources, not the projects' numbers.
const projectNumbersKey = "projectnumbers"

// projectNumbers is the in-memory copy of the set, which is all
// ProjectNumberOf reads. It is filled from the metadata store once, when
// the server is made (loadProjectNumbers), and replaced on each push.
//
// ProjectNumberOf never reads the metadata store: it is called from inside
// metadata transactions (an upload's default object ACL names the project
// team by number), and the store's lock is not reentrant. When it loaded
// the set lazily, the first lookup after a restart, if it came from such a
// transaction, took the store's read lock under its own write lock and
// held the whole store, readiness included, forever (#1630).
type projectNumbers struct {
	mu   sync.RWMutex
	byID map[string]int64
	// push serializes pushes, so the store and the copy end on the same
	// set. It is never held by a lookup, and mu is never held while the
	// store's lock is taken.
	push sync.Mutex
}

// loadProjectNumbers reads the last set the host sent from the metadata
// store, as after a restart.
func (s *Server) loadProjectNumbers() {
	m := map[string]int64{}
	_ = s.meta.View(func(tx Tx) error {
		if raw, ok := tx.Get(projectNumbersKey); ok {
			_ = json.Unmarshal(raw, &m)
		}
		return nil
	})
	s.numbers.mu.Lock()
	s.numbers.byID = m
	s.numbers.mu.Unlock()
}

// ProjectNumberOf is the number the host sent for a project, if any. It is
// what storageserver installs as resource.SetProjectNumberResolver, so
// every number this server reports is the registry's. It is safe inside a
// metadata transaction.
func (s *Server) ProjectNumberOf(id string) (int64, bool) {
	s.numbers.mu.RLock()
	defer s.numbers.mu.RUnlock()
	n, ok := s.numbers.byID[id]
	return n, ok
}

// serveProjectNumbers is POST /_cloudburrow/project-numbers: the whole set,
// replacing the last one.
func (s *Server) serveProjectNumbers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "POST %s sets the project numbers", ProjectNumbersPath))
		return
	}
	m := map[string]int64{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil {
		writeError(w, badRequest("project numbers: %v", err))
		return
	}
	for id, n := range m {
		if n == resource.DerivedProjectNumber(id) || n <= 0 {
			delete(m, id)
		}
	}
	raw, _ := json.Marshal(m)
	s.numbers.push.Lock()
	defer s.numbers.push.Unlock()
	if err := s.meta.Update(func(tx Tx) error { tx.Put(projectNumbersKey, raw); return nil }); err != nil {
		writeError(w, err)
		return
	}
	s.numbers.mu.Lock()
	s.numbers.byID = m
	s.numbers.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
