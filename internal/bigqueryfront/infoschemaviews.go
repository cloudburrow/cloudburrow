package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// INFORMATION_SCHEMA.VIEWS of a dataset (#1274).
//
// Measured against the pinned image through the front (2026-10-15): the
// engine serves a dataset's INFORMATION_SCHEMA.SCHEMATA, TABLES, COLUMNS
// and TABLE_OPTIONS, and fails ds.INFORMATION_SCHEMA.VIEWS 400 "Table not
// found". BigQuery has one row per view of the dataset
// (https://cloud.google.com/bigquery/docs/information-schema-views):
// table_catalog, table_schema, table_name, view_definition, check_option
// (always NULL) and use_standard_sql ("YES", or "NO" for a legacy SQL
// view). The front answers a query that reads it by writing each
// reference as a subquery of those rows, read from tables.list and each
// view's tables.get (with the client's text of its query, as the front's
// tables.get answers it), and running the query so; the job keeps the client's
// text. A dataset that does not exist fails 404, as BigQuery fails it
// (wording UNVERIFIED).
//
// Not built: the region-qualified form (`region-us`.INFORMATION_SCHEMA.VIEWS),
// and the other views #1274 lists.

// viewsRef is a reference to a dataset's INFORMATION_SCHEMA view in a
// query's text.
type viewsRef struct {
	pos, end int
	dataset  string
	// view is the view's name, upper case (VIEWS, TABLES, ...).
	view string
	// alias is whether a name or AS follows it, so the subquery needs
	// none of its own.
	alias bool
}

// informationSchemaViews returns sql's references to
// [project.]dataset.INFORMATION_SCHEMA.<view> of the views the front
// answers (infoschema.go, #1286) or refuses 501 (unbuiltInfoSchema). A
// script's references to the views the engine serves itself (TABLES,
// COLUMNS, TABLE_OPTIONS) are left to it, which reads its catalog as each
// statement runs.
func informationSchemaViews(sql string) []viewsRef {
	t, ok := lex(sql)
	if !ok {
		return nil
	}
	script := len(splitStatements(trimSemicolons(t))) > 1
	var refs []viewsRef
	for i := 0; i < len(t); i++ {
		if t[i].kind != tokWord && t[i].kind != tokQuoted || i > 0 && t[i-1].punct(".") {
			continue
		}
		parts, next := path(t, i)
		n := len(parts)
		if n < 3 || n > 4 || !strings.EqualFold(parts[n-2], "INFORMATION_SCHEMA") || strings.HasPrefix(strings.ToLower(parts[n-3]), "region-") {
			i = next - 1
			continue
		}
		view := strings.ToUpper(parts[n-1])
		if _, ok := infoSchemaBuilders[view]; !ok && !unbuiltInfoSchema[view] || script && engineInfoSchema[view] {
			i = next - 1
			continue
		}
		ref := viewsRef{pos: t[i].pos, end: t[next-1].end, dataset: parts[n-3], view: view}
		if next < len(t) && (t[next].is("AS") || t[next].kind == tokQuoted ||
			t[next].kind == tokWord && !reservedWords[strings.ToUpper(t[next].text)]) {
			ref.alias = true
		}
		refs = append(refs, ref)
		i = next - 1
	}
	return refs
}

// trimSemicolons drops a script's trailing semicolons.
func trimSemicolons(t []token) []token {
	for len(t) > 0 && t[len(t)-1].punct(";") {
		t = t[:len(t)-1]
	}
	return t
}

// viewsColumns is the subquery's row type.
const viewsColumns = "STRUCT<table_catalog STRING, table_schema STRING, table_name STRING, view_definition STRING, " +
	"check_option STRING, use_standard_sql STRING>"

// viewsRows returns the subquery of dataset's views, or the status and
// message of why it cannot.
func (f front) viewsRows(r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
	project := projectOf(f.base)
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
		return "", http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset)
	}
	var rows []string
	token := ""
	for {
		p := "/datasets/" + url.PathEscape(dataset) + "/tables?maxResults=1000"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		st, got := f.get(r, p)
		var list struct {
			Tables []struct {
				TableReference struct {
					TableID string `json:"tableId"`
				} `json:"tableReference"`
				Type string `json:"type"`
			} `json:"tables"`
			NextPageToken string `json:"nextPageToken"`
		}
		if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
			return "", http.StatusInternalServerError, "cloudburrow: could not list the dataset's tables: " + errorMessage(got, st)
		}
		for _, tb := range list.Tables {
			if tb.Type != "VIEW" {
				continue
			}
			st, got := f.get(r, tablePath(dataset, tb.TableReference.TableID))
			var md struct {
				View struct {
					Query        string `json:"query"`
					UseLegacySQL bool   `json:"useLegacySql"`
				} `json:"view"`
			}
			if st == http.StatusNotFound {
				continue // deleted meanwhile
			}
			if st != http.StatusOK || json.Unmarshal(got, &md) != nil {
				return "", http.StatusInternalServerError, "cloudburrow: could not read a view: " + errorMessage(got, st)
			}
			if f.views != nil { // the client's text, as tables.get answers it (views.go, #1014)
				if t, ok := f.views.get(viewKey(project, dataset, tb.TableReference.TableID)); ok && t.engine == md.View.Query {
					md.View.Query = t.client
				}
			}
			std := "YES"
			if md.View.UseLegacySQL {
				std = "NO"
			}
			rows = append(rows, "("+strings.Join([]string{sqlString(project), sqlString(dataset),
				sqlString(tb.TableReference.TableID), sqlString(md.View.Query), "NULL", sqlString(std)}, ", ")+")")
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			break
		}
		token = list.NextPageToken
	}
	sort.Strings(rows)
	return "(SELECT * FROM UNNEST(ARRAY<" + viewsColumns + ">[" + strings.Join(rows, ", ") + "]))", 0, ""
}

// withInformationSchemaViews answers q, whose text reads refs, by writing
// each as the subquery of its dataset's views (above).
func (f front) withInformationSchemaViews(w http.ResponseWriter, r *http.Request, q queryOptions, refs []viewsRef, insert bool) {
	var b strings.Builder
	last := 0
	subs := map[string]string{}
	meta := &datasetMeta{}
	for _, ref := range refs {
		if unbuiltInfoSchema[ref.view] {
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: INFORMATION_SCHEMA."+ref.view+
				" is not built (docs/compatibility.md), so nothing was run.")
			return
		}
		key := ref.dataset + "\x00" + ref.view
		sub, ok := subs[key]
		if !ok {
			var status int
			var msg string
			sub, status, msg = infoSchemaBuilders[ref.view](f, r, ref.dataset, meta)
			if status != 0 {
				reason := "notFound"
				if status != http.StatusNotFound {
					reason = "internalError"
				}
				writeError(w, status, reason, msg)
				return
			}
			subs[key] = sub
		}
		b.WriteString(q.Query[last:ref.pos])
		b.WriteString(sub)
		if !ref.alias {
			b.WriteString(" AS " + ref.view)
		}
		last = ref.end
	}
	b.WriteString(q.Query[last:])
	sql := b.String()
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
}
