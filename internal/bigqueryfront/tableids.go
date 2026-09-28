package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Which table IDs more than one dataset has (#1063).
//
// tabledata.list, an extract and the Storage Read API of a table whose ID
// another dataset has too are read by the table's whole name, since the
// emulator reads them by the bare ID (qualify.go, storageread.go, #1015).
// Telling whether a table is such a one (sharedID) asked the emulator
// datasets.list and then tables.get of the ID in every other dataset, for
// every request, every page of one read included, and the emulator answers
// them one at a time: measured in a run of the BigQuery compat suite
// (#1051), 0.2–0.5 s each late in the run.
//
// So the front keeps, per project, every table ID of every dataset
// (tableIDs), read with datasets.list and one tables.list a dataset, and
// answers from it until something may have made, deleted or renamed a
// table. What may is any request to the REST API but a read (GET, HEAD)
// and a query job or jobs.query of one lone SELECT with no destination
// table (tableIDsMayChange); the cache is dropped when such a request
// begins and again when it ends, and is not kept while one is in flight,
// so an answer read during one is never kept. It is dropped too when the
// emulator restarts (restart.go). The Storage Write API writes the rows of
// tables that exist and makes none.
//
// The front's own dataset is left out: resultsDataset (results.go,
// #1017), which holds a table per query job, named after the job. Its
// tables are the front's, named so that no client table has their IDs.
//
// Table IDs are compared without case: the emulator's engine may resolve
// names so, and taking a table to be shared when it is not only means it
// is read by its whole name, which is right either way.

// tableIDs is the cache above. The zero value is ready; a nil *tableIDs
// keeps nothing, and every lookup reads the emulator.
type tableIDs struct {
	mu sync.Mutex
	// gen changes whenever the cache is dropped.
	gen uint64
	// busy counts the requests in flight that may change the tables.
	busy int
	// byProject is project → lower-cased table ID → the datasets that
	// have a table of that ID.
	byProject map[string]map[string][]string
}

// change notes a request that may change the tables beginning, and
// returns the func that notes it ending.
func (x *tableIDs) change() func() {
	if x == nil {
		return func() {}
	}
	x.mu.Lock()
	x.busy++
	x.gen++
	x.byProject = nil
	x.mu.Unlock()
	return func() {
		x.mu.Lock()
		x.busy--
		x.gen++
		x.byProject = nil
		x.mu.Unlock()
	}
}

// reset drops the cache: the emulator restarted (#1016).
func (x *tableIDs) reset() {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.gen++
	x.byProject = nil
}

// lookup returns project's table IDs, from the cache or read by read and
// kept when nothing changed the tables meanwhile; ok is false when they
// could not be read.
func (x *tableIDs) lookup(project string, read func() (map[string][]string, bool)) (map[string][]string, bool) {
	if x == nil {
		return read()
	}
	x.mu.Lock()
	if ids, ok := x.byProject[project]; ok && x.busy == 0 {
		x.mu.Unlock()
		return ids, true
	}
	gen := x.gen
	x.mu.Unlock()
	ids, ok := read()
	if !ok {
		return nil, false
	}
	x.mu.Lock()
	if x.gen == gen && x.busy == 0 {
		if x.byProject == nil {
			x.byProject = map[string]map[string][]string{}
		}
		x.byProject[project] = ids
	}
	x.mu.Unlock()
	return ids, true
}

// frontDataset reports whether a dataset is the front's own, whose tables
// are left out of tableIDs.
func frontDataset(id string) bool {
	return id == resultsDataset
}

// sharedID reports whether a dataset other than dataset has a table (or
// view) of the ID table, or whether that cannot be told: then the
// emulator's reads by the bare ID may be of another table (#1015). It
// reads only the emulator's metadata, datasets.list and tables.list, and
// keeps what it read in f.ids (above).
func (f front) sharedID(r *http.Request, dataset, table string) bool {
	ids, ok := f.ids.lookup(projectOf(f.base), func() (map[string][]string, bool) { return f.readTableIDs(r) })
	if !ok {
		return true
	}
	for _, d := range ids[strings.ToLower(table)] {
		if d != dataset {
			return true
		}
	}
	return false
}

// readTableIDs reads every table ID of every dataset of f's project but
// the front's own: lower-cased table ID → datasets.
func (f front) readTableIDs(r *http.Request) (map[string][]string, bool) {
	var datasets []string
	ok := f.eachPage(r, "/datasets?all=true", func(body []byte) (string, bool) {
		var list struct {
			NextPageToken string `json:"nextPageToken"`
			Datasets      []struct {
				DatasetReference struct {
					DatasetID string `json:"datasetId"`
				} `json:"datasetReference"`
			} `json:"datasets"`
		}
		if json.Unmarshal(body, &list) != nil {
			return "", false
		}
		for _, d := range list.Datasets {
			if id := d.DatasetReference.DatasetID; id != "" && !frontDataset(id) {
				datasets = append(datasets, id)
			}
		}
		return list.NextPageToken, true
	})
	if !ok {
		return nil, false
	}
	ids := map[string][]string{}
	for _, ds := range datasets {
		ok := f.eachPage(r, "/datasets/"+url.PathEscape(ds)+"/tables?maxResults=1000", func(body []byte) (string, bool) {
			var list struct {
				NextPageToken string `json:"nextPageToken"`
				Tables        []struct {
					TableReference struct {
						TableID string `json:"tableId"`
					} `json:"tableReference"`
				} `json:"tables"`
			}
			if json.Unmarshal(body, &list) != nil {
				return "", false
			}
			for _, t := range list.Tables {
				if id := strings.ToLower(t.TableReference.TableID); id != "" {
					ids[id] = append(ids[id], ds)
				}
			}
			return list.NextPageToken, true
		})
		if !ok {
			return nil, false
		}
	}
	return ids, true
}

// eachPage reads path and each page after it, handing each body to page,
// which returns the next page's token; false when a page could not be read.
func (f front) eachPage(r *http.Request, path string, page func([]byte) (string, bool)) bool {
	token := ""
	for {
		p := path
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		status, body := f.get(r, p)
		if status != http.StatusOK && status != 0 {
			return false
		}
		next, ok := page(body)
		if !ok {
			return false
		}
		if next == "" || next == token {
			return true
		}
		token = next
	}
}

// tableIDsMayChange reports whether r may make, delete or rename a table
// (above): any request but a read, and but a query of one lone SELECT
// with no destination table.
func tableIDsMayChange(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	case http.MethodPost:
	default:
		return true
	}
	path := r.URL.EscapedPath()
	j := jobsRoute.FindStringSubmatch(path)
	if j == nil || strings.HasPrefix(path, "/upload/") || r.Header.Get("Content-Encoding") != "" {
		return true
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
		return true
	}
	body, err := readBody(r)
	if err != nil {
		return true
	}
	var req struct {
		// jobs.query
		Query *string `json:"query"`
		// jobs.insert
		Configuration struct {
			Query *struct {
				Query            string          `json:"query"`
				DestinationTable json.RawMessage `json:"destinationTable"`
			} `json:"query"`
		} `json:"configuration"`
	}
	if json.NewDecoder(bytes.NewReader(body)).Decode(&req) != nil {
		return true
	}
	switch {
	case j[3] == "queries" && req.Query != nil:
		return !isLoneQuery(*req.Query)
	case j[3] == "jobs" && req.Configuration.Query != nil:
		q := req.Configuration.Query
		return !isLoneQuery(q.Query) || len(q.DestinationTable) > 0 && string(q.DestinationTable) != "null"
	}
	return true
}
