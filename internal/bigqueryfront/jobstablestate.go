package bigqueryfront

// What the front wrote to each project's table of INFORMATION_SCHEMA.JOBS
// rows (jobstable.go), kept in a file on the front's state directory
// (#1632).
//
// The front made a project's table again, dropped first, the first time
// its rows were asked for after the front started, as it no longer knew
// what the table held: measured on one instance with 7,398 jobs
// (--services storage,bigquery, arm64, 2026-10-18), the first COUNT(*) of
// `region-us`.INFORMATION_SCHEMA.JOBS after a restart of the front's
// container took 20.3 s, the next 0.11 s, and a test whose context also
// covered the restart ran out of it. The engine keeps the table across a
// restart of the front's container (only its own restart empties it), so
// the front now keeps a digest of each row it wrote, and the cursor and
// the jobs not DONE, as a line per change: after a restart it has the
// table as it left it, and the first query lists every job (0.5 s at
// 7,400 jobs) and writes only the rows that changed.
//
// A restored table is checked before it is used: its COUNT(*) must be
// the rows the file has. Each DELETE and INSERT is recorded once the
// engine has answered it, and its rows as dirty before it is sent
// (jobstable.go, #1628), so a front ended between the two has the rows
// of the statement it sent dirty: the first sync deletes them, and the
// check then finds the table as the file has it, where the table was made
// again (up to 26 s at 10,000 jobs). The check fails when the engine
// restarted meanwhile, and has no table, which is then made again.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// jobsTableStateFile is the file the tables' rows are kept in, in dir.
func jobsTableStateFile(dir string) string { return filepath.Join(dir, "jobstable.json") }

// rowDigest is what the front keeps of a row it wrote: enough to tell a
// changed row from the one written.
func rowDigest(row string) string {
	sum := sha256.Sum256([]byte(row))
	return base64.RawURLEncoding.EncodeToString(sum[:15])
}

// jobsTableLine is a line of the file: one change of a project's table,
// or, with Reset, of every table (the emulator restarted).
type jobsTableLine struct {
	Reset   bool   `json:"reset,omitempty"`
	Project string `json:"p,omitempty"`
	// Made: the table was made, empty; Forget: it is not made.
	Made   bool `json:"made,omitempty"`
	Forget bool `json:"forget,omitempty"`
	// Del are the rows deleted, Add the rows written (job ID to digest),
	// Dirty the rows of a statement about to be sent (jobstable.go).
	Del   []string          `json:"del,omitempty"`
	Add   map[string]string `json:"add,omitempty"`
	Dirty []string          `json:"dirty,omitempty"`
	// End: a sync ended, with Open the rows of jobs not DONE and Cursor
	// the last job listed.
	End    bool     `json:"end,omitempty"`
	Open   []string `json:"open,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
}

// jobsTableCompactBytes is the least size past which the file is written
// again, a line per kind of what it keeps, once it is twice the size it
// had when last written so.
const jobsTableCompactBytes = 1 << 20

// keep restores the tables from path, when it holds them, and from then on
// keeps them there. It must be called before the front serves. A file
// that cannot be read is logged and replaced.
func (j *jobsTable) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("bigquery front: read %s: %v; starting without the INFORMATION_SCHEMA.JOBS rows it kept", path, err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	rows := map[string]map[string]string{}
	open := map[string]map[string]bool{}
	cursor := map[string]string{}
	dirty := map[string]map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var l jobsTableLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // a line cut short; the count check finds the table unlike the file
		}
		switch {
		case l.Reset:
			rows, open, cursor = map[string]map[string]string{}, map[string]map[string]bool{}, map[string]string{}
			dirty = map[string]map[string]bool{}
		case l.Project == "":
		case l.Forget:
			delete(rows, l.Project)
			delete(open, l.Project)
			delete(cursor, l.Project)
			delete(dirty, l.Project)
		case l.Made:
			rows[l.Project] = map[string]string{}
			delete(open, l.Project)
			delete(cursor, l.Project)
			delete(dirty, l.Project)
		default:
			have := rows[l.Project]
			if have == nil {
				continue
			}
			d := dirty[l.Project]
			if d == nil {
				d = map[string]bool{}
				dirty[l.Project] = d
			}
			for _, id := range l.Dirty {
				delete(have, id)
				d[id] = true
			}
			for _, id := range l.Del {
				delete(have, id)
				delete(d, id)
			}
			for id, digest := range l.Add {
				have[id] = digest
				delete(d, id)
			}
			if l.End {
				o := map[string]bool{}
				for _, id := range l.Open {
					o[id] = true
				}
				open[l.Project] = o
				cursor[l.Project] = l.Cursor
			}
		}
	}
	n := 0
	for p, have := range rows {
		if j.rows == nil {
			j.rows, j.open, j.cursor = map[string]map[string]string{}, map[string]map[string]bool{}, map[string]string{}
		}
		j.rows[p], j.open[p], j.cursor[p] = have, open[p], cursor[p]
		if len(dirty[p]) > 0 {
			if j.dirty == nil {
				j.dirty = map[string]map[string]bool{}
			}
			j.dirty[p] = dirty[p]
		}
		if j.verify == nil {
			j.verify = map[string]bool{}
		}
		j.verify[p] = true
		j.setMade(p, true)
		// Changes the front was told of before it ended may not be in
		// the table: the first query lists every job.
		j.bg.Lock()
		j.overflowLocked(p)
		j.bg.Unlock()
		n += len(have)
	}
	if len(rows) > 0 {
		logf("bigquery front: restored the INFORMATION_SCHEMA.JOBS rows of %d projects (%d rows) from %s", len(rows), n, path)
	}
	j.path, j.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	j.compactLocked()
}

// compactLocked writes the file again, with what is kept now; j.mu is
// held.
func (j *jobsTable) compactLocked() {
	if j == nil || j.path == "" {
		return
	}
	var out bytes.Buffer
	line := func(l jobsTableLine) {
		b, _ := json.Marshal(l)
		out.Write(append(b, '\n'))
	}
	projects := make([]string, 0, len(j.rows))
	for p := range j.rows {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		line(jobsTableLine{Project: p, Made: true})
		add := map[string]string{}
		for id, d := range j.rows[p] {
			add[id] = d
			if len(add) == jobsTableBatch {
				line(jobsTableLine{Project: p, Add: add})
				add = map[string]string{}
			}
		}
		line(jobsTableLine{Project: p, Add: add, End: true, Open: sortedIDs(j.open[p]), Cursor: j.cursor[p]})
		if d := sortedIDs(j.dirty[p]); len(d) > 0 {
			line(jobsTableLine{Project: p, Dirty: d})
		}
	}
	if err := writeFileAtomic(j.path, out.Bytes()); err != nil {
		j.logf("bigquery front: keeping the INFORMATION_SCHEMA.JOBS rows in %s: %v", j.path, err)
	}
	j.size, j.compacted = out.Len(), out.Len()
}

// recordLocked appends l to the file, when the tables are kept, and
// writes the file again (compactLocked) once it has grown past twice its
// size when last written so; j.mu is held.
func (j *jobsTable) recordLocked(l jobsTableLine) {
	if j == nil || j.path == "" {
		return
	}
	if j.size >= 2*max(j.compacted, jobsTableCompactBytes) {
		j.compactLocked()
		return
	}
	b, _ := json.Marshal(l)
	b = append(b, '\n')
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		j.logf("bigquery front: keeping the INFORMATION_SCHEMA.JOBS rows in %s: %v", j.path, err)
	}
	j.size += len(b)
}

// verifiedLocked reports whether the project's table, restored from the
// file, has the rows the file has (above); a table not restored, or
// checked before, is taken as it is. j.mu is held.
func (j *jobsTable) verifiedLocked(f front, r *http.Request, project string) bool {
	if !j.verify[project] {
		return true
	}
	delete(j.verify, project)
	body, _ := json.Marshal(map[string]any{"query": "SELECT COUNT(*) FROM " + jobsTablePath(), "useLegacySql": false})
	st, got := f.send(r, http.MethodPost, "/queries", body)
	var answer struct {
		Rows []struct {
			F []struct {
				V string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if st != http.StatusOK || json.Unmarshal(got, &answer) != nil || len(answer.Rows) != 1 || len(answer.Rows[0].F) != 1 {
		return false
	}
	n, err := strconv.Atoi(answer.Rows[0].F[0].V)
	return err == nil && n == len(j.rows[project])
}

// sortedIDs returns the IDs of set, sorted.
func sortedIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
