package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// What a deletion keeps of a table besides its rows (#1483, dropped.go).
//
// BigQuery's undelete restores a dataset "to the state it was in when it
// was deleted" (https://cloud.google.com/bigquery/docs/restore-deleted-datasets,
// read 2026-10-16), its tables and views among it. So a deletion the front
// keeps also keeps what the front holds of the table on its own, keyed by
// the table's creation (tableoptions.go, iam.go, rowpolicies.go): the
// settings it keeps (enable_change_history, privacy_policy), the table's
// IAM policy and its row access policies. An undelete puts them back on
// the table it makes again, under that table's creation. A materialized
// view keeps its resource and is made again from its query (as
// tables.insert makes one, materializedviews.go); a table snapshot keeps
// its rows (it is read-only, its past is its rows) and its
// snapshotDefinition.
//
// An external table (#1522) keeps its definition, its resource with its
// externalDataConfiguration: BigQuery keeps none of its data, which stays
// in its source ("External tables ... the data is not stored in BigQuery",
// https://cloud.google.com/bigquery/docs/external-tables, read
// 2026-10-17). The emulator keeps the definition itself (tables.get answers
// it, type EXTERNAL) and reads the source once, when the table is made, so
// an undelete makes it again through tables.insert with that resource,
// which reads the source as it is then. A copy of table@TIME of a deleted
// one fails as a copy of a live one does (copyjob.go).
//
// A restored table keeps its change history (#1483): the kept changes are
// carried into its new change table as a rebuild carries them
// (changehistorycarry.go), each row still there ended at the deletion, so
// time travel reads the table before it was deleted, and nothing between
// the deletion and the restore.

// keptExtras are the front's own records of a deleted table.
type keptExtras struct {
	Settings *tableSetting     `json:"settings,omitempty"`
	IAM      *iampolicy.Stored `json:"iam,omitempty"`
	Policies []rowPolicy       `json:"rowAccessPolicies,omitempty"`
}

// keepExtras reads the front's records of dataset.table, made at created.
func (f front) keepExtras(dataset, table, created string) *keptExtras {
	project := projectOf(f.base)
	var e keptExtras
	if f.tables != nil {
		if s := f.tables.get(streamKey(project, dataset, table), created); !s.zero() {
			e.Settings = &s
		}
	}
	if f.iam != nil {
		e.IAM = f.iam.stored(iamKey(project, dataset, "tables", table), created)
	}
	if f.policies != nil {
		if pols, c := f.policies.list(policyKey(project, dataset, table)); c == created && len(pols) > 0 {
			e.Policies = pols
		}
	}
	if e.Settings == nil && e.IAM == nil && len(e.Policies) == 0 {
		return nil
	}
	return &e
}

// restoreExtras puts e back on dataset.table, made again at created.
func (f front) restoreExtras(dataset, table, created string, e *keptExtras) {
	if e == nil {
		return
	}
	project := projectOf(f.base)
	if e.Settings != nil && f.tables != nil {
		f.tables.restore(streamKey(project, dataset, table), created, *e.Settings)
	}
	if e.IAM != nil && f.iam != nil {
		f.iam.restore(iamKey(project, dataset, "tables", table), created, *e.IAM)
	}
	if len(e.Policies) > 0 && f.policies != nil {
		f.policies.restore(policyKey(project, dataset, table), created, e.Policies)
	}
}

// restore puts back the settings of a table made again at created.
func (s *tableSettings) restore(key, created string, t tableSetting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]tableSetting{}
	}
	t.Created = created
	s.m[key] = t
	if t.PrivacyPolicy != nil {
		s.watchLocked(key)
	}
	s.saveLocked()
}

// stored is a copy of the policy of key's resource made at created, or nil.
func (p *iamPolicies) stored(key, created string) *iampolicy.Stored {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.currentLocked(key, created)
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// restore puts back the policy of a resource made again at created.
func (p *iamPolicies) restore(key, created string, s iampolicy.Stored) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.policies == nil {
		p.policies = map[string]*storedIAM{}
	}
	p.policies[key] = &storedIAM{Key: key, Created: created, Policy: s}
	p.saveLocked()
}

// restore puts back the policies of a table made again at created.
func (p *rowPolicies) restore(key, created string, pols []rowPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tables == nil {
		p.tables = map[string]*protectedTable{}
	}
	p.tables[key] = &protectedTable{Key: key, Created: created, Policies: append([]rowPolicy(nil), pols...)}
	p.saveLocked()
}

// keptHistory is the history kept of t, as carryHistory reads one: its
// changes, and an END at the deletion of each row still there then.
func keptHistory(t droppedTable, fields []field) string {
	var cols []string
	for _, fl := range fields {
		if pseudoColumn(fl.Name) {
			continue
		}
		cols = append(cols, quotePath([]string{fl.Name}))
	}
	list := strings.Join(cols, ", ")
	copyPath := quotePath([]string{t.Project, droppedDatasetID, t.Copy})
	forUpdate := "FALSE"
	if t.ForUpdate {
		forUpdate = "COALESCE(_cloudburrow_for_update, FALSE)"
	}
	deleted := t.Deleted * 1000
	return fmt.Sprintf("(SELECT %s, _cloudburrow_change AS _CHANGE_TYPE, TIMESTAMP_MICROS(_cloudburrow_micros) AS _CHANGE_TIMESTAMP, "+
		"%s AS _CHANGE_IS_FOR_UPDATE, _cloudburrow_micros AS cloudburrow_micros, _cloudburrow_rowid AS cloudburrow_row, "+
		"_cloudburrow_order AS cloudburrow_order FROM %s "+
		"UNION ALL SELECT %s, '%s', TIMESTAMP_MICROS(%d), FALSE, %d, _cloudburrow_rowid, "+
		"(SELECT MAX(_cloudburrow_order) + 1 FROM %s) FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY _cloudburrow_rowid "+
		"ORDER BY _cloudburrow_order DESC) AS _cloudburrow_last FROM %s WHERE _cloudburrow_change != 'START') "+
		"WHERE _cloudburrow_last = 1 AND _cloudburrow_change NOT IN ('DELETE', '%s'))",
		list, forUpdate, copyPath, list, changeEnd, deleted, deleted, copyPath, copyPath, changeEnd)
}

// restoreHistory carries the history kept of t into the change table of
// the table restoreTable just made again with its rows (above).
func (f front) restoreHistory(r *http.Request, t droppedTable, fields []field) string {
	if t.Final || !f.hasHistory(r, t.Dataset, t.Table) {
		return ""
	}
	var cols []carriedColumn
	for _, fl := range fields {
		if pseudoColumn(fl.Name) {
			continue
		}
		cols = append(cols, carriedColumn{name: fl.Name, expr: quoteName(fl.Name)})
	}
	return f.carryHistory(r, keptHistory(t, fields), t.Dataset, t.Table, cols, false)
}

// createdOf is the creationTime of a tables.get answer.
func createdOf(got []byte) string {
	var m struct {
		Created string `json:"creationTime"`
	}
	_ = json.Unmarshal(got, &m)
	return m.Created
}
