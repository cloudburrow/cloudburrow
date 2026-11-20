package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Analytics Hub linked datasets (#1960).
//
// Subscribing to a listing makes "a read-only BigQuery dataset that serves
// as a pointer or reference to a shared dataset". "Subscribers can query
// standard tables and views in real time", "but they can't add or update
// objects in the dataset"
// (https://docs.cloud.google.com/bigquery/docs/analytics-hub-introduction).
//
// The Analytics Hub server (internal/service/analyticshub, through
// cmd/cloudburrow's linker) makes the dataset here with HubLinkHeader,
// whose value is the link (hubLink, JSON): the shared dataset, the
// listing's selected tables and its restricted export configuration. The
// engine has no dataset that points at another, so the front keeps it a
// pointer:
//
//   - tables.list and tables.get answer the shared dataset's tables (of the
//     selected ones, when the listing selects some) under the linked
//     dataset's name, with their own type, schema and rows; tabledata.list
//     reads the shared table;
//   - a query names a linked table as BigQuery's does: each is a view, in
//     the engine, of the shared table (SELECT *), made or dropped as the
//     shared dataset's tables come and go before every query, tables.list
//     and tables.get that reads the linked dataset (syncHubLink). So a row
//     written to the shared table is read at once, and a table added to the
//     shared dataset is listed and queried at once;
//   - every write into it is refused 403 accessDenied (refuseLinkedWrite,
//     loglinks.go), as a Log Analytics linked dataset's is. Deleting it is
//     not a write: it is allowed, and the link forgotten;
//   - with restricted export (restrictedExportConfig.enabled), "Copy,
//     clone, export, and snapshot APIs are unavailable", and "CREATE TABLE
//     AS SELECT statements and writing to a destination table", "CREATE
//     VIEW AS SELECT statements and writing to a destination view" are
//     too: a copy or extract job whose source is a linked table, a query
//     job with a destinationTable, and a query with a statement that writes
//     (CREATE, INSERT, MERGE, UPDATE, EXPORT DATA) that reads the linked
//     dataset are refused 403 accessDenied before anything runs; and so is
//     direct table access, tabledata.list (restrict_direct_table_access,
//     "restrict direct table access (read api/tabledata.list) on linked
//     table"). Reading the shared tables through the Storage Read API, a
//     separate port the front does not see, is not refused;
//   - a revoked subscription's linked dataset "remains in the subscriber's
//     project", but "the subscriber can no longer query" it: every read of
//     it is refused 403 accessDenied.
//
// Google publishes none of these errors' wording; the front's is
// UNVERIFIED.
// Docs read 2026-11-20: https://docs.cloud.google.com/bigquery/docs/analytics-hub-introduction,
// https://docs.cloud.google.com/bigquery/docs/analytics-hub-manage-subscriptions

// HubLinkHeader marks the Analytics Hub server's own requests that make,
// change or delete a linked dataset. Its value is the link, JSON.
const HubLinkHeader = "X-Cloudburrow-Analytics-Hub-Link"

// HubLinkedLabel marks a linked dataset for whoever lists datasets.
const HubLinkedLabel = "cloudburrow_analytics_hub_link"

// hubLink is one linked dataset: HubLinkHeader's value.
type hubLink struct {
	// SourceProject and SourceDataset are the shared dataset.
	SourceProject string `json:"sourceProject"`
	SourceDataset string `json:"sourceDataset"`
	// Tables are the listing's selected tables, or nil for all.
	Tables []string `json:"tables,omitempty"`
	// Restricted is restricted export; QueryResult its
	// restrict_query_result.
	Restricted  bool `json:"restricted,omitempty"`
	QueryResult bool `json:"queryResult,omitempty"`
	Revoked     bool `json:"revoked,omitempty"`
}

func (l hubLink) selected(table string) bool {
	if len(l.Tables) == 0 {
		return true
	}
	for _, t := range l.Tables {
		if t == table {
			return true
		}
	}
	return false
}

// hubLinks are the linked datasets by project.dataset, kept in the state
// directory as the Log Analytics ones are.
type hubLinks struct {
	mu   sync.Mutex
	m    map[string]hubLink
	path string
	logf func(string, ...any)
}

func hubLinksStateFile(dir string) string { return filepath.Join(dir, "hublinks.json") }

func (h *hubLinks) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	saved := map[string]hubLink{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the Analytics Hub links it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the Analytics Hub links it kept", path)
		saved = map[string]hubLink{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m, h.path, h.logf = saved, path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	h.saveLocked()
}

func (h *hubLinks) saveLocked() {
	if h.path == "" {
		return
	}
	b, err := json.Marshal(h.m)
	if err == nil {
		err = writeFileAtomic(h.path, b)
	}
	if err != nil {
		h.logf("bigquery front: keeping the Analytics Hub links in %s: %v", h.path, err)
	}
}

func (h *hubLinks) set(project, dataset string, l *hubLink) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]hubLink{}
	}
	if l == nil {
		delete(h.m, project+"."+dataset)
	} else {
		h.m[project+"."+dataset] = *l
	}
	h.saveLocked()
}

func (h *hubLinks) get(project, dataset string) (hubLink, bool) {
	if h == nil || dataset == "" {
		return hubLink{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.m[project+"."+dataset]
	return l, ok
}

func (h *hubLinks) any() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m) > 0
}

func (h *hubLinks) reset() {
	if h != nil {
		h.mu.Lock()
		h.m = nil
		h.saveLocked()
		h.mu.Unlock()
	}
}

func (f front) hubLink(dataset string) (hubLink, bool) {
	return f.hubs.get(projectOf(f.base), dataset)
}

// serveHubWriter serves the Analytics Hub server's own requests (r carries
// HubLinkHeader): a datasets.insert that makes a linked dataset, a
// datasets.patch that changes its link, and a datasets.delete; and
// reports whether r was one.
func (f front) serveHubWriter(w http.ResponseWriter, r *http.Request, dataset string, insert func(http.ResponseWriter, *http.Request)) bool {
	raw := r.Header.Get(HubLinkHeader)
	if raw == "" || f.hubs == nil {
		return false
	}
	project := projectOf(f.base)
	var link hubLink
	if json.Unmarshal([]byte(raw), &link) != nil && r.Method != http.MethodDelete {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: "+HubLinkHeader+" is not a link")
		return true
	}
	switch {
	case r.Method == http.MethodPost && dataset == "":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var ds struct {
			DatasetReference struct {
				DatasetID string `json:"datasetId"`
			} `json:"datasetReference"`
		}
		_ = json.Unmarshal(body, &ds)
		sw := &statusWriter{ResponseWriter: w} // resumable.go
		insert(sw, r)
		if sw.status == http.StatusOK && ds.DatasetReference.DatasetID != "" {
			f.hubs.set(project, ds.DatasetReference.DatasetID, &link)
			f.syncHubLink(r, ds.DatasetReference.DatasetID)
		}
		return true
	case r.Method == http.MethodPatch && dataset != "" && !strings.Contains(r.URL.Path, "/tables"):
		if _, ok := f.hubLink(dataset); !ok {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Dataset %s:%s", project, dataset))
			return true
		}
		f.hubs.set(project, dataset, &link)
		if !link.Revoked {
			f.syncHubLink(r, dataset)
		}
		f.getDataset(w, r, dataset)
		return true
	case r.Method == http.MethodDelete && dataset != "" && !strings.Contains(r.URL.Path, "/tables"):
		f.deleteHubLinked(w, r, dataset)
		return true
	}
	return false
}

// deleteHubLinked deletes a linked dataset with its views, and forgets
// the link.
func (f front) deleteHubLinked(w http.ResponseWriter, r *http.Request, dataset string) {
	q := r.URL.Query()
	q.Set("deleteContents", "true")
	r.URL.RawQuery = q.Encode()
	f.next.ServeHTTP(w, r)
	f.hubs.set(projectOf(f.base), dataset, nil)
}

// hubSourceFront is f in the shared dataset's project.
func (f front) hubSourceFront(l hubLink) front { return f.inProject(l.SourceProject) }

// listAll reads every page of a tables.list from the engine and returns
// the tables' JSON objects.
func (f front) listAll(r *http.Request, dataset string) ([]map[string]any, int) {
	var out []map[string]any
	token := ""
	for range 1000 {
		path := "/datasets/" + url.PathEscape(dataset) + "/tables?maxResults=1000"
		if token != "" {
			path += "&pageToken=" + url.QueryEscape(token)
		}
		status, b := f.get(r, path)
		if status != http.StatusOK {
			return nil, status
		}
		var page struct {
			Tables        []map[string]any `json:"tables"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if json.Unmarshal(b, &page) != nil {
			return nil, http.StatusInternalServerError
		}
		out = append(out, page.Tables...)
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return out, http.StatusOK
}

func tableIDOf(t map[string]any) string {
	ref, _ := t["tableReference"].(map[string]any)
	id, _ := ref["tableId"].(string)
	return id
}

// syncHubLink makes the linked dataset's views match the shared dataset's
// tables (of the selected ones): a view, SELECT *, for each, and none for
// a table that is gone. A shared dataset that cannot be read leaves the
// views as they are.
func (f front) syncHubLink(r *http.Request, dataset string) {
	l, ok := f.hubLink(dataset)
	if !ok || l.Revoked {
		return
	}
	src, status := f.hubSourceFront(l).listAll(r, l.SourceDataset)
	if status != http.StatusOK {
		return
	}
	have, status := f.listAll(r, dataset)
	if status != http.StatusOK {
		return
	}
	want := map[string]bool{}
	for _, t := range src {
		if id := tableIDOf(t); id != "" && l.selected(id) {
			want[id] = true
		}
	}
	got := map[string]bool{}
	for _, t := range have {
		id := tableIDOf(t)
		got[id] = true
		if !want[id] {
			f.send(r, http.MethodDelete, tablePath(dataset, id), nil)
		}
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		if !got[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	project := projectOf(f.base)
	for _, id := range ids {
		body, _ := json.Marshal(map[string]any{
			"tableReference": map[string]string{"projectId": project, "datasetId": dataset, "tableId": id},
			"view": map[string]any{"query": "SELECT * FROM `" + l.SourceProject + "." + l.SourceDataset + "." + id + "`",
				"useLegacySql": false},
		})
		f.send(r, http.MethodPost, "/datasets/"+url.PathEscape(dataset)+"/tables", body)
	}
}

// hubLinkRefused answers 403 accessDenied with why.
func hubAccessDenied(w http.ResponseWriter, why string) {
	writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: "+why)
}

func hubRevokedMessage(project, dataset string) string {
	return fmt.Sprintf("Dataset %s:%s is a linked dataset whose Analytics Hub subscription was revoked; it can no longer be queried. "+
		"Delete it, or subscribe to the listing again.", project, dataset)
}

func hubEgressMessage(project, dataset, what string) string {
	return fmt.Sprintf("Dataset %s:%s is a linked dataset of an Analytics Hub listing with restricted export (data egress restriction): "+
		"%s would copy its data out, which the listing's publisher does not allow.", project, dataset, what)
}

// hubLinkedWriteMessage is linkedWriteMessage for a linked dataset of a
// listing.
func hubLinkedWriteMessage(project, dataset, what string) string {
	return fmt.Sprintf("Access Denied: Dataset %s:%s is an Analytics Hub linked dataset, which is read-only: %s cannot write to it. "+
		"Its tables are the shared dataset's; the listing's publisher changes them there.", project, dataset, what)
}

// serveHubRead serves a client's read of a linked dataset's tables:
// tables.list, tables.get and tabledata.list, from the shared dataset; and
// reports whether it did. table is "" for tables.list.
func (f front) serveHubRead(w http.ResponseWriter, r *http.Request, dataset, table, action string) bool {
	if r.Method != http.MethodGet || dataset == "" {
		return false
	}
	l, ok := f.hubLink(dataset)
	if !ok {
		return false
	}
	isList := table == "" && strings.HasSuffix(r.URL.EscapedPath(), "/tables")
	if table == "" && !isList {
		return false // datasets.get is the linked dataset's own
	}
	project := projectOf(f.base)
	if l.Revoked {
		hubAccessDenied(w, hubRevokedMessage(project, dataset))
		return true
	}
	src := f.hubSourceFront(l)
	rewrite := func(t map[string]any) {
		id := tableIDOf(t)
		t["tableReference"] = map[string]any{"projectId": project, "datasetId": dataset, "tableId": id}
		t["id"] = project + ":" + dataset + "." + id
		delete(t, "selfLink")
	}
	switch {
	case isList:
		f.syncHubLink(r, dataset)
		all, status := src.listAll(r, l.SourceDataset)
		if status != http.StatusOK {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Dataset %s:%s", project, dataset))
			return true
		}
		// By table ID, as BigQuery lists a dataset's tables.
		sort.Slice(all, func(i, j int) bool { return tableIDOf(all[i]) < tableIDOf(all[j]) })
		out := make([]any, 0, len(all))
		for _, t := range all {
			if !l.selected(tableIDOf(t)) {
				continue
			}
			rewrite(t)
			out = append(out, t)
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#tableList", "tables": out, "totalItems": len(out)})
	case action == "":
		if !l.selected(table) {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", project, dataset, table))
			return true
		}
		f.syncHubLink(r, dataset)
		status, b := src.get(r, tablePath(l.SourceDataset, table))
		var t map[string]any
		if status != http.StatusOK || json.Unmarshal(b, &t) != nil {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", project, dataset, table))
			return true
		}
		rewrite(t)
		writeJSON(w, http.StatusOK, t)
	case action == "data":
		if l.Restricted {
			hubAccessDenied(w, hubEgressMessage(project, dataset, "tabledata.list (direct table access, restrict_direct_table_access)"))
			return true
		}
		if !l.selected(table) {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", project, dataset, table))
			return true
		}
		// The shared table's rows, read as the shared table: the request
		// is sent as tabledata.list of it.
		r2 := r.Clone(r.Context())
		p := src.base + tablePath(l.SourceDataset, table) + "/data"
		r2.URL.Path, r2.URL.RawPath = "", ""
		if u, err := url.PathUnescape(p); err == nil {
			r2.URL.Path, r2.URL.RawPath = u, p
		}
		src.listTableData(w, r2, l.SourceDataset, table)
	default:
		return false
	}
	return true
}

// hubRefs are the linked datasets a query names: by a two- or three-part
// path, or as its default dataset.
func (f front) hubRefs(q queryOptions) []string {
	if !f.hubs.any() {
		return nil
	}
	project := projectOf(f.base)
	seen := map[string]bool{}
	var out []string
	add := func(p, ds string) {
		if p != project {
			return // a linked dataset is named in its own project's jobs
		}
		if _, ok := f.hubs.get(p, ds); ok && !seen[ds] {
			seen[ds] = true
			out = append(out, ds)
		}
	}
	if def := defaultDatasetOf(q); def != "" {
		add(project, def)
	}
	toks, ok := lex(q.Query)
	if !ok {
		return out
	}
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tokWord && toks[i].kind != tokQuoted {
			continue
		}
		if i > 0 && toks[i-1].punct(".") {
			continue
		}
		parts, next := path(toks, i)
		switch {
		case len(parts) >= 3:
			add(parts[0], parts[1])
		case len(parts) == 2:
			add(project, parts[0])
		}
		if next > i {
			i = next - 1
		}
	}
	return out
}

// queryWritesOut reports whether a statement of q writes a table, a view
// or files: what restricted export refuses of a query that reads a linked
// dataset.
func queryWritesOut(q string) bool {
	toks, ok := lex(q)
	if !ok {
		return true
	}
	for _, s := range splitStatements(toks) {
		body, _, _ := stripControlFlow(s)
		if len(body) == 0 {
			continue
		}
		switch strings.ToUpper(body[0].text) {
		case "CREATE", "INSERT", "MERGE", "UPDATE", "EXPORT", "REPLACE":
			return true
		}
	}
	return false
}

// refuseHubQuery refuses, before anything runs, a query that reads a
// revoked linked dataset, or (with restricted export) writes what it reads
// of one out; it brings each linked dataset it reads up to date first. dest
// is whether the query job has a destinationTable.
func (f front) refuseHubQuery(w http.ResponseWriter, r *http.Request, q queryOptions, insert, dest bool) bool {
	refs := f.hubRefs(q)
	project := projectOf(f.base)
	for _, ds := range refs {
		l, _ := f.hubLink(ds)
		var why string
		switch {
		case l.Revoked:
			why = hubRevokedMessage(project, ds)
		case l.Restricted && dest:
			why = hubEgressMessage(project, ds, "writing the query's results to a destination table")
		case l.Restricted && queryWritesOut(q.Query):
			why = hubEgressMessage(project, ds, "a statement that writes a table, a view or files from it (CREATE TABLE AS SELECT, CREATE VIEW, INSERT, MERGE, UPDATE or EXPORT DATA)")
		}
		if why != "" {
			f.failBeforeRun(w, r, q, insert, ds, http.StatusForbidden, rowError{Reason: "accessDenied", Message: "Access Denied: " + why})
			return true
		}
	}
	for _, ds := range refs {
		f.syncHubLink(r, ds)
	}
	return false
}

// refuseHubCopyOut refuses a copy or extract job (what) whose source is a
// table of a linked dataset with restricted export, or of a revoked one.
func (f front) refuseHubCopyOut(w http.ResponseWriter, r *http.Request, srcs []tableRef, what string) bool {
	if !f.hubs.any() {
		return false
	}
	project := projectOf(f.base)
	for _, s := range srcs {
		if refProject(s, project) != project {
			continue
		}
		l, ok := f.hubLink(s.DatasetID)
		switch {
		case !ok:
		case l.Revoked:
			hubAccessDenied(w, hubRevokedMessage(project, s.DatasetID))
			return true
		case l.Restricted:
			hubAccessDenied(w, hubEgressMessage(project, s.DatasetID, what))
			return true
		}
	}
	return false
}

// hubDatasetFields adds a linked dataset's linkedDatasetSource and type to
// a datasets.get answer.
func (f front) hubDatasetFields(dataset string, content map[string]json.RawMessage) {
	l, ok := f.hubLink(dataset)
	if !ok {
		return
	}
	src, _ := json.Marshal(map[string]any{"sourceDataset": map[string]string{"projectId": l.SourceProject, "datasetId": l.SourceDataset}})
	content["linkedDatasetSource"] = src
	content["type"] = json.RawMessage(`"LINKED"`)
}
