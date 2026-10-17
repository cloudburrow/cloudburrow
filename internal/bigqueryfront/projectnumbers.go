package bigqueryfront

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Project numbers (#1551). The service agent projects.getServiceAccount
// names, bq-PROJECT_NUMBER@bigquery-encryption.iam.gserviceaccount.com,
// names the project by number. The registry gives a project its number:
// the derived one (resource.DerivedProjectNumber) unless the ID was used by
// a project since purged. The front, in its pod, cannot read the registry,
// so `cloudburrow up` sends it the numbers that differ, whole, at POST
// ProjectNumbersPath, as it sends the storage server them
// (internal/service/storage), at start and after every project is created
// or purged. They are kept in the state directory, so a restart of the
// front keeps them until the next push.

// ProjectNumbersPath is where the host sends the project numbers that are
// not the derived ones: a POST of {"projectId": number, ...}, answered 204.
// It is the storage server's path.
const ProjectNumbersPath = "/_cloudburrow/project-numbers"

func projectNumbersStateFile(dir string) string { return filepath.Join(dir, "projectnumbers.json") }

// projectNumbers is the numbers the host sent.
type projectNumbers struct {
	mu   sync.RWMutex
	byID map[string]int64
	path string
	logf func(string, ...any)
}

// keep restores the numbers from path, and from then on keeps them there.
func (p *projectNumbers) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := map[string]int64{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the project numbers it kept", path, err)
	case json.Unmarshal(b, &m) != nil:
		logf("bigquery front: %s is unreadable; starting without the project numbers it kept", path)
		m = map[string]int64{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID, p.path, p.logf = m, path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
}

// of is project's number: the one the host sent, else the derived one.
func (p *projectNumbers) of(project string) int64 {
	if p != nil {
		p.mu.RLock()
		n, ok := p.byID[project]
		p.mu.RUnlock()
		if ok {
			return n
		}
	}
	return resource.ProjectNumber(project)
}

// serve answers POST ProjectNumbersPath: the whole set, replacing the last.
func (p *projectNumbers) serve(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != ProjectNumbersPath {
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "methodNotAllowed", "POST "+ProjectNumbersPath+" sets the project numbers")
		return true
	}
	m := map[string]int64{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "project numbers: "+err.Error())
		return true
	}
	for id, n := range m {
		if n <= 0 || n == resource.DerivedProjectNumber(id) {
			delete(m, id)
		}
	}
	p.mu.Lock()
	p.byID = m
	path, logf := p.path, p.logf
	p.mu.Unlock()
	if path != "" {
		b, _ := json.Marshal(m)
		if err := writeFileAtomic(path, b); err != nil {
			logf("bigquery front: keeping the project numbers in %s: %v", path, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}
