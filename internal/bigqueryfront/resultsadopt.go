package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Result tables made before the front started (#1660), and their sizes
// (#1662).
//
// The front keeps the list of the tables it made in resultsDataset in
// memory (resultsexpiry.go). A front that restarts while the emulator goes
// on would not know the tables made before it, and so would never delete
// them: measured, about 220 of 318 after one run of the BigQuery compat
// suite, whose restart tests end the front three times. So when it starts,
// the front lists the tables already in each project's resultsDataset
// once, with their creationTime, and adopts the query results among them:
// they are then deleted by age, count and size as the tables it made are.
// It adopts only a job's table: not jobsTableName, nor a table whose ID
// begins with scratchPrefix (a scratch, session or materialized view
// table, which the front that made it deletes or keeps on purpose).
//
// The front also bounds what the tables hold, not only how many there are
// (#1662): their rows and bytes, which the emulator keeps in its SQLite
// file. It reads each table's size once, with tables.get, in the
// background (resultsSizeBatch every resultsTick): numRows, which the
// engine counts, and numBytes, which the engine answers 0 (logicalbytes.go),
// so the bytes are numRows times the width of a row by its schema
// (rowWidth), BigQuery's documented data type sizes, with a STRING, BYTES
// or JSON value counted as resultsVarBytes.

// resultsVarBytes is the width rowWidth gives a value of a variable size.
const resultsVarBytes = 32

// resultsSizeBatch is how many tables' sizes are read at each tick.
const resultsSizeBatch = 50

// adopt lists the tables in every project's resultsDataset and adopts its
// jobs' tables (above).
func (q *queryResults) adopt(ctx context.Context) error {
	var projects struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := q.getJSON(ctx, "/projects", &projects); err != nil {
		return err
	}
	var found []resultTable
	for _, p := range projects.Projects {
		if p.ID == "" {
			continue
		}
		base := "/projects/" + url.PathEscape(p.ID)
		var list struct {
			Tables []struct {
				TableReference struct {
					TableID string `json:"tableId"`
				} `json:"tableReference"`
				CreationTime looseInt `json:"creationTime"`
			} `json:"tables"`
		}
		err := q.getJSON(ctx, base+"/datasets/"+resultsDataset+"/tables", &list)
		if err == errNotThere {
			continue
		}
		if err != nil {
			return err
		}
		q.setMade(p.ID, true)
		for _, t := range list.Tables {
			id := t.TableReference.TableID
			if id == "" || id == jobsTableName || strings.HasPrefix(id, scratchPrefix) {
				continue
			}
			made := time.Time{}
			if t.CreationTime > 0 {
				made = time.UnixMilli(int64(t.CreationTime))
			}
			found = append(found, resultTable{project: p.ID, job: id, base: base, made: made})
		}
	}
	q.expiry.adopt(found)
	return nil
}

// errNotThere is getJSON's error for a 404.
var errNotThere = fmt.Errorf("not found")

// getJSON GETs path of the emulator into v.
func (q *queryResults) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	rec := newRecorder()
	q.next.ServeHTTP(rec, req)
	switch rec.status {
	case 0, http.StatusOK:
	case http.StatusNotFound:
		return errNotThere
	default:
		return fmt.Errorf("GET %s: %d", path, rec.status)
	}
	return json.Unmarshal(rec.body.Bytes(), v)
}

// sizeTables reads the size of at most resultsSizeBatch tables not sized
// yet (above), and stops at the first that fails.
func (q *queryResults) sizeTables(ctx context.Context) {
	for _, t := range q.expiry.unsized(resultsSizeBatch) {
		var tb struct {
			NumRows  looseInt `json:"numRows"`
			NumBytes looseInt `json:"numBytes"`
			Schema   struct {
				Fields []field `json:"fields"`
			} `json:"schema"`
		}
		err := q.getJSON(ctx, t.base+"/datasets/"+resultsDataset+"/tables/"+url.PathEscape(t.job), &tb)
		if err != nil && err != errNotThere {
			return
		}
		rows, bytes := int64(tb.NumRows), int64(tb.NumBytes)
		if bytes <= 0 {
			bytes = rows * rowWidth(tb.Schema.Fields)
		}
		q.expiry.sized(t, rows, bytes)
	}
}

// looseInt is an int64 the API writes as a string or a number, or not at
// all (0).
type looseInt int64

func (n *looseInt) UnmarshalJSON(b []byte) error {
	v, _ := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	*n = looseInt(v)
	return nil
}

// rowWidth is the bytes of a row of fields (above): a repeated field
// counted as one value.
func rowWidth(fields []field) int64 {
	var n int64
	for _, f := range fields {
		switch strings.ToUpper(f.Type) {
		case "INTEGER", "INT64", "FLOAT", "FLOAT64", "DATE", "DATETIME", "TIME", "TIMESTAMP":
			n += 8
		case "NUMERIC", "INTERVAL", "RANGE":
			n += 16
		case "BIGNUMERIC":
			n += 32
		case "BOOLEAN", "BOOL":
			n++
		case "RECORD", "STRUCT":
			n += rowWidth(f.Fields)
		default: // STRING, BYTES, JSON, GEOGRAPHY
			n += resultsVarBytes
		}
	}
	return n
}

// adopt adds tables found in resultsDataset (above), older than any the
// front made, oldest first; a table it knows already is left as it is,
// and one with no creationTime counts as made now.
func (e *resultsExpiry) adopt(found []resultTable) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	known := map[string]bool{}
	for _, t := range e.tables {
		known[expiryKey(t.project, t.job)] = true
	}
	now := e.now()
	var add []resultTable
	for _, t := range found {
		if known[expiryKey(t.project, t.job)] {
			continue
		}
		known[expiryKey(t.project, t.job)] = true
		if t.made.IsZero() || t.made.After(now) {
			t.made = now
		}
		add = append(add, t)
	}
	sort.SliceStable(add, func(i, j int) bool { return add[i].made.Before(add[j].made) })
	e.tables = append(add, e.tables...)
}

// unsized returns at most n of the jobs' tables not sized yet, oldest
// first.
func (e *resultsExpiry) unsized(n int) []resultTable {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []resultTable
	for _, t := range e.tables {
		if len(out) == n {
			break
		}
		if !t.scratch && !t.sized {
			out = append(out, t)
		}
	}
	return out
}

// sized notes t's rows and bytes.
func (e *resultsExpiry) sized(t resultTable, rows, bytes int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.tables {
		if u := &e.tables[i]; u.project == t.project && u.job == t.job && !u.scratch {
			u.rows, u.bytes, u.sized = max(rows, 0), max(bytes, 0), true
			return
		}
	}
}
