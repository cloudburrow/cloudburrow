package bigqueryfront

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
)

// The engine's jobs that are not DONE, which a query of
// INFORMATION_SCHEMA.JOBS WHERE state = 'PENDING' or 'RUNNING' lists alone
// (#1633).
//
// Until a project's table of JOBS rows is made (jobstable.go), a filtered
// JOBS query lists jobs and cuts them in the front (jobsRows); one that
// names its jobs lists those alone (newestJobs, joblisting.go), but one
// that keeps the jobs of a state listed every job, so it cost what the
// project's history did. So the front keeps, of each project, the jobs the
// engine listed not DONE, and the job its list of them ended with (the
// cursor): every job of the engine not DONE is one of them or one the
// engine made after the cursor. A query that keeps only jobs not DONE
// lists, with bigquery-emulator patch 0064, those jobs and the ones made
// since the cursor, and the filter cuts them as it cuts a list of every
// job, so the rows are the same.
//
// What it keeps is brought up to date by each list of the engine's jobs
// (engineListing): a list of every job (a client's jobs.list, a JOBS
// query's, a sync of the JOBS table's) makes it anew; this query's own
// list moves the cursor to the newest job it listed; any other list (cut
// by creation time, or of some jobs) takes out those it lists DONE and
// adds those it lists not DONE. A job is never listed not DONE once it is
// DONE: the engine's jobs end DONE, and the layers above the engine (the
// failures the front gives, jobFailures) only make a job DONE, so what is
// kept is the engine's jobs not DONE, or more.
//
// It is kept in the front's state directory (jobsOpenStateFile), so a
// restart of the front's container alone keeps it; the engine's restart
// empties the engine and forgets it (restart.go), as a purge of the
// project does (purge.go), and the next query lists every job and makes
// it anew. When the engine restarted while the front was down, the cursor
// is no job of the engine: patch 0064 then lists every job, and what is
// kept is made anew from that. Each query's list names the cursor, and
// checks the engine's job of that ID was made when the cursor's was; one
// made since (a job ID a client gave again after the engine restarted) is
// not the cursor, and the query lists every job.
//
// A job inserted, or read DONE by jobs.get, needs no record of its own:
// one made after the cursor is listed for that, and one read DONE is
// dropped by the next list that has it.

// jobsOpenStateFile is the file the jobs not DONE are kept in, in dir.
func jobsOpenStateFile(dir string) string { return filepath.Join(dir, "jobsopen.json") }

// openJobs are the engine's jobs not DONE, of each project (above).
type openJobs struct {
	mu sync.Mutex
	p  map[string]*openProject
	// path is the file they are kept in (keep), or "".
	path string
	logf func(string, ...any)
}

// openProject is what is kept of a project.
type openProject struct {
	// Cursor is the last job of the engine's list the jobs were read
	// from, "" for a list of none; Created its creationTime as the
	// engine listed it.
	Cursor  string `json:"cursor,omitempty"`
	Created string `json:"created,omitempty"`
	// Open are the jobs listed not DONE.
	Open map[string]bool `json:"-"`
	// OpenIDs is Open, sorted, as the file keeps it.
	OpenIDs []string `json:"open,omitempty"`
}

// openListMax is the most jobs a query lists by ID (with the cursor); a
// project with more not DONE lists every job.
const openListMax = jobIDsMax

// listing returns the jobs.list a query that keeps jobs not DONE sends
// (above): after the cursor, and of the jobs not DONE and the cursor;
// ok is false when nothing is kept of the project, and every job is
// listed.
func (o *openJobs) listing(project string) (*engineList, bool) {
	if o == nil {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.p[project]
	if p == nil || len(p.Open) >= openListMax {
		return nil, false
	}
	ids := make([]string, 0, len(p.Open)+1)
	for id := range p.Open {
		ids = append(ids, id)
	}
	if p.Cursor != "" && !p.Open[p.Cursor] {
		ids = append(ids, p.Cursor)
	}
	sort.Strings(ids)
	return &engineList{some: true, after: p.Cursor, ids: ids, open: true}, true
}

// listed brings what is kept of project up to date with items, the
// engine's answer to a jobs.list (engineListing): l is what it asked for
// (nil for every job), cut whether it was cut by creation time. It
// reports false when l was a query's own list (listing) and the engine
// had another job of the cursor's ID: the list may lack jobs not DONE,
// and every job is to be listed.
func (o *openJobs) listed(project string, l *engineList, items []any, cut bool) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.p[project]
	full := !cut && (l == nil || !l.some || l.after == "")
	if !full && !cut && l.open && p != nil && l.after == p.Cursor {
		created, listed := "", false
		for _, item := range items {
			job, _ := item.(map[string]any)
			if _, id := jobRef(job); id == p.Cursor {
				created, listed = jobCreated(job), true
			}
		}
		switch {
		case !listed:
			// The engine has no job of the cursor's ID: patch 0064 listed
			// every job.
			full = true
		case created != p.Created:
			delete(o.p, project)
			o.saveLocked()
			return false
		default:
			before := len(p.Open)
			changed := o.seenLocked(p, items)
			had := map[string]bool{}
			for _, item := range items {
				job, _ := item.(map[string]any)
				_, id := jobRef(job)
				had[id] = true
			}
			for _, id := range l.ids {
				if !had[id] && p.Open[id] {
					delete(p.Open, id) // gone: jobs.delete
					changed = true
				}
			}
			// The last job listed is the newest the engine made after
			// the cursor, unless it is one named (one made before it, or
			// after it and named too: the cursor then stays, which lists
			// more next time).
			if n := len(items); n > 0 {
				job, _ := items[n-1].(map[string]any)
				if _, id := jobRef(job); id != "" && !slices.Contains(l.ids, id) {
					p.Cursor, p.Created = id, jobCreated(job)
					changed = true
				}
			}
			if changed || before != len(p.Open) {
				o.saveLocked()
			}
			return true
		}
	}
	if full {
		np := &openProject{Open: map[string]bool{}}
		for _, item := range items {
			job, _ := item.(map[string]any)
			_, id := jobRef(job)
			if id == "" {
				continue
			}
			if jobStateOf(job) != "DONE" {
				np.Open[id] = true
			}
			np.Cursor, np.Created = id, jobCreated(job)
		}
		if o.p == nil {
			o.p = map[string]*openProject{}
		}
		if !p.same(np) {
			o.p[project] = np
			o.saveLocked()
		}
		return true
	}
	if p != nil && o.seenLocked(p, items) {
		o.saveLocked()
	}
	return true
}

// seenLocked takes out of p the jobs items lists DONE, and adds those it
// lists not DONE, reporting whether p changed; o.mu is held.
func (o *openJobs) seenLocked(p *openProject, items []any) bool {
	changed := false
	for _, item := range items {
		job, _ := item.(map[string]any)
		_, id := jobRef(job)
		if id == "" {
			continue
		}
		open := jobStateOf(job) != "DONE"
		if open != p.Open[id] {
			changed = true
			if open {
				p.Open[id] = true
			} else {
				delete(p.Open, id)
			}
		}
	}
	return changed
}

// same reports whether p and q keep the same.
func (p *openProject) same(q *openProject) bool {
	if p == nil || q == nil || p.Cursor != q.Cursor || p.Created != q.Created || len(p.Open) != len(q.Open) {
		return false
	}
	for id := range p.Open {
		if !q.Open[id] {
			return false
		}
	}
	return true
}

// jobCreated is a listed job's statistics.creationTime, as the engine
// wrote it.
func jobCreated(job map[string]any) string {
	stats, _ := job["statistics"].(map[string]any)
	switch v := stats["creationTime"].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// forget forgets what is kept of project: the next query lists every job.
func (o *openJobs) forget(project string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.p[project]; ok {
		delete(o.p, project)
		o.saveLocked()
	}
}

// reset forgets every project's: the emulator restarted.
func (o *openJobs) reset() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.p = nil
	o.saveLocked()
}

// openJobsState is the file's content.
type openJobsState struct {
	Projects map[string]*openProject `json:"projects"`
}

// keep restores what path holds, and from then on keeps it there. It must
// be called before the front serves. A file that cannot be read is logged
// and replaced.
func (o *openJobs) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.path, o.logf = path, logf
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("bigquery front: read %s: %v; starting without the jobs not DONE it kept", path, err)
		}
		return
	}
	var s openJobsState
	if err := json.Unmarshal(b, &s); err != nil {
		logf("bigquery front: read %s: %v; starting without the jobs not DONE it kept", path, err)
		return
	}
	o.p = map[string]*openProject{}
	for project, p := range s.Projects {
		if p == nil {
			continue
		}
		p.Open = map[string]bool{}
		for _, id := range p.OpenIDs {
			p.Open[id] = true
		}
		p.OpenIDs = nil
		o.p[project] = p
	}
}

// saveLocked writes what is kept to the file, when it is kept; o.mu is
// held.
func (o *openJobs) saveLocked() {
	if o.path == "" {
		return
	}
	s := openJobsState{Projects: map[string]*openProject{}}
	for project, p := range o.p {
		s.Projects[project] = &openProject{Cursor: p.Cursor, Created: p.Created, OpenIDs: sortedIDs(p.Open)}
	}
	b, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		o.logf("bigquery front: %v", err)
		return
	}
	if err := writeFileAtomic(o.path, b); err != nil {
		o.logf("bigquery front: keeping the jobs not DONE in %s: %v", o.path, err)
	}
}
