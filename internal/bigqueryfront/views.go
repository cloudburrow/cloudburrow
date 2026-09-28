package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// A view's query as the client wrote it (#1014).
//
// The emulator keeps a view made by CREATE VIEW with the SQL its engine
// rewrote the query to, and tables.get answers that as view.query:
// measured against the pinned image, `CREATE VIEW p1.v AS SELECT id FROM
// p1.t WHERE id > 0` read back as "SELECT `id#1` AS `id` FROM (SELECT
// `id#1` FROM (SELECT `id` AS `id#1`,`region` AS `region#2` FROM
// `w1008-local_p1_t`) WHERE googlesqlite_greater(`id#1`,0))", which names
// the emulator's own table and function and is not GoogleSQL anyone can
// read or run. BigQuery returns the query as written
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#ViewDefinition).
// A view made by tables.insert reads back as written; tables.list gives
// no view's query, in the emulator as in BigQuery.
//
// So after a query that makes a view succeeds, the front reads the view
// the emulator made and keeps the client's text of its query with the
// emulator's (keepViewTexts), and tables.get of the view answers with the
// client's text while the emulator's is still the one it kept
// (getTable): a view replaced or dropped since, by any means, reads back
// as the emulator has it. A CREATE VIEW ... IF NOT EXISTS of a view that
// existed changes nothing, and nothing is kept for it. The texts are kept
// by the front while it runs, the most recent maxViewTexts of them.
type viewText struct {
	engine, client string
}

type viewTexts struct {
	mu    sync.Mutex
	texts map[string]viewText
	order []string
}

const maxViewTexts = 1000

func viewKey(project, dataset, table string) string {
	return project + "\x00" + dataset + "\x00" + table
}

func (v *viewTexts) add(key string, t viewText) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.texts == nil {
		v.texts = map[string]viewText{}
	}
	if _, ok := v.texts[key]; !ok {
		v.order = append(v.order, key)
	}
	v.texts[key] = t
	for len(v.order) > maxViewTexts {
		delete(v.texts, v.order[0])
		v.order = v.order[1:]
	}
}

func (v *viewTexts) get(key string) (viewText, bool) {
	if v == nil {
		return viewText{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.texts[key]
	return t, ok
}

// madeViews reports whether a query's statements make a view.
func madeViews(v ddlVerdict) bool {
	for _, c := range v.creates {
		if c.view && !c.temp {
			return true
		}
	}
	return false
}

// keepViewTexts serves a query that makes views (serveQuery), then keeps
// the client's text of each view's query (above).
func (f front) keepViewTexts(w http.ResponseWriter, r *http.Request, q queryOptions, v ddlVerdict, insert bool) {
	type made struct {
		dataset, table, query string
		existed               bool
	}
	last := map[string]int{}
	var views []made
	for _, c := range v.creates {
		if !c.view || c.temp || strings.TrimSpace(c.query) == "" {
			continue
		}
		ds, table, ok := tableOf(q, c.path)
		if !ok {
			continue
		}
		m := made{dataset: ds, table: table, query: strings.TrimSpace(c.query)}
		if c.ifNotExists {
			status, _ := f.get(r, tablePath(ds, table))
			m.existed = status == http.StatusOK
		}
		last[ds+"\x00"+table] = len(views)
		views = append(views, m)
	}
	rec := newRecorder()
	f.serveQuery(rec, r, q, insert)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	if _, failed := queryFailure(rec, job); !failed {
		project := projectOf(f.base)
		for i, m := range views {
			if last[m.dataset+"\x00"+m.table] != i || m.existed {
				continue
			}
			status, got := f.get(r, tablePath(m.dataset, m.table))
			var meta struct {
				Type string `json:"type"`
				View *struct {
					Query string `json:"query"`
				} `json:"view"`
			}
			if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || !strings.EqualFold(meta.Type, "VIEW") ||
				meta.View == nil || meta.View.Query == "" || meta.View.Query == m.query {
				continue
			}
			f.views.add(viewKey(project, m.dataset, m.table), viewText{engine: meta.View.Query, client: m.query})
		}
	}
	rec.copyTo(w)
}

// getTable answers tables.get, with the client's text of a view's query
// when the front kept it (above).
func (f front) getTable(w http.ResponseWriter, r *http.Request, dataset, table string) {
	t, ok := f.views.get(viewKey(projectOf(f.base), dataset, table))
	if !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var meta map[string]any
	if rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), &meta) == nil {
		if view, _ := meta["view"].(map[string]any); view != nil && view["query"] == t.engine {
			view["query"] = t.client
			writeRecorded(w, rec, meta)
			return
		}
	}
	rec.copyTo(w)
}
