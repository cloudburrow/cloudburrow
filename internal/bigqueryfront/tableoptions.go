package bigqueryfront

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// More table OPTIONS in DDL (#1390): max_staleness, default_rounding_mode,
// tags, require_partition_filter, enable_change_history, storage_uri and
// privacy_policy, in CREATE TABLE and ALTER TABLE SET OPTIONS
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#table_option_list).
//
// Those that are a field of the table resource are written to it, as the
// other options are (ddloptions.go, altertable.go): maxStaleness,
// defaultRoundingMode, resourceTags, requirePartitionFilter and
// biglakeConfiguration.storageUri. The emulator keeps each through
// tables.update (its Table is the API's, measured), and tables.get and
// INFORMATION_SCHEMA.TABLE_OPTIONS read them back.
//
// enable_change_history and privacy_policy are no field of the resource:
// the front keeps them (tableSettings, a file on its state directory) for
// TABLE_OPTIONS, the table's creation time with them, so a table made
// again of the name has none.
//
// What they do:
//   - require_partition_filter: a query that reads the table with no
//     filter on its partitioning column fails 400 as BigQuery's does
//     (partitionFilterRefusal); the check is that the column is named in a
//     WHERE of the statement that reads it, not that the filter can prune.
//   - max_staleness: a query reads the table as it is, never older data.
//   - default_rounding_mode: kept; the engine rounds a value written to a
//     parameterized NUMERIC or BIGNUMERIC column to its scale by the
//     column's roundingMode, else this, else ROUND_HALF_AWAY_FROM_ZERO
//     (engine patches googlesqlite 0031 and bigquery-emulator 0020, #1401;
//     a CREATE TABLE's columns, decimalddl.go).
//   - enable_change_history: CHANGES and APPENDS are not served (501).
//   - privacy_policy: enforced in the front's query rewrite: an
//     aggregation threshold and a join restriction as BigQuery does (#1426,
//     privacypolicy.go), and differential privacy with the noise and the
//     epsilon and delta budget (#1455, #1482, differentialprivacy.go). A
//     view's too.
//   - storage_uri: the table's data stays in the engine, never written to
//     Cloud Storage as Apache Iceberg files.

// moreOption is one of the options above, as a statement sets it.
type moreOption struct {
	name string
	// key is the table resource's field, value its value, or nil to clear
	// it; eval, when not "", is an expression the engine evaluates to the
	// value (max_staleness's interval, as a STRING).
	key   string
	value any
	eval  string
	// setting is enable_change_history or privacy_policy, kept by the
	// front: value is its SQL text, nil to clear it.
	setting bool
}

// moreOptionNames are the options readMoreOption reads.
var moreOptionNames = map[string]bool{"max_staleness": true, "default_rounding_mode": true, "tags": true,
	"require_partition_filter": true, "enable_change_history": true, "storage_uri": true, "privacy_policy": true,
	"file_format": true, "table_format": true} // #1427, iceberg.go

// readMoreOption reads name = val, one of moreOptionNames, of the statement
// text sql. msg is why it cannot be read, a 400's.
func readMoreOption(name string, val []token, sql string) (o moreOption, msg string) {
	o.name = name
	isNull := len(val) == 1 && val[0].is("NULL")
	boolean := func() (any, bool) {
		switch {
		case isNull:
			return nil, true
		case len(val) == 1 && val[0].is("TRUE"):
			return true, true
		case len(val) == 1 && val[0].is("FALSE"):
			return false, true
		}
		return nil, false
	}
	str := func() (any, bool) {
		if isNull {
			return nil, true
		}
		if len(val) == 1 {
			if s, ok := stringLiteral(val[0]); ok {
				return s, true
			}
		}
		return nil, false
	}
	switch name {
	case "max_staleness":
		o.key = "maxStaleness"
		if !isNull {
			o.eval = "CAST((" + sql[val[0].pos:val[len(val)-1].end] + ") AS STRING)"
		}
	case "default_rounding_mode":
		o.key = "defaultRoundingMode"
		v, ok := str()
		if s, _ := v.(string); !ok || v != nil && s != "ROUND_HALF_AWAY_FROM_ZERO" && s != "ROUND_HALF_EVEN" {
			return o, "Invalid value for default_rounding_mode: expected 'ROUND_HALF_AWAY_FROM_ZERO' or 'ROUND_HALF_EVEN'"
		}
		o.value = v
	case "tags":
		o.key = "resourceTags"
		if isNull {
			break
		}
		tags, ok := labelsLiteral(val)
		if !ok {
			return o, "the option tags must be an array of (key, value) string literals"
		}
		o.value = tags
	case "require_partition_filter":
		o.key = "requirePartitionFilter"
		v, ok := boolean()
		if !ok {
			return o, "the option require_partition_filter must be a BOOL literal"
		}
		o.value = v
	case "storage_uri":
		o.key = "biglakeConfiguration"
		v, ok := str()
		if !ok {
			return o, "the option storage_uri must be a STRING literal"
		}
		if v != nil {
			o.value = map[string]any{"storageUri": v}
		}
	case "file_format", "table_format": // #1427, iceberg.go
		o.key = "biglakeConfiguration"
		v, ok := str()
		if !ok || v == nil {
			return o, "the option " + name + " must be a STRING literal"
		}
		k := map[string]string{"file_format": "fileFormat", "table_format": "tableFormat"}[name]
		o.value = map[string]any{k: strings.ToUpper(v.(string))}
	case "enable_change_history":
		o.setting = true
		v, ok := boolean()
		if !ok {
			return o, "the option enable_change_history must be a BOOL literal"
		}
		if v != nil {
			o.value = strings.ToLower(fmt.Sprint(v))
		}
	case "privacy_policy":
		o.setting = true
		v, ok := str()
		if !ok {
			return o, "the option privacy_policy must be a JSON STRING literal"
		}
		if s, _ := v.(string); v != nil && !json.Valid([]byte(s)) {
			return o, "Invalid privacy_policy: it is not valid JSON"
		}
		if v != nil {
			o.value = v
		}
	}
	return o, ""
}

// put sets o in meta, a table resource; value is eval's result.
func (o moreOption) put(meta map[string]json.RawMessage) {
	if o.setting {
		return
	}
	if o.value == nil {
		delete(meta, o.key)
		return
	}
	if part, ok := o.value.(map[string]any); ok && o.key == "biglakeConfiguration" { // #1427: merged
		var cur map[string]any
		_ = json.Unmarshal(meta[o.key], &cur)
		if cur == nil {
			cur = map[string]any{}
		}
		for k, v := range part {
			cur[k] = v
		}
		b, _ := json.Marshal(cur)
		meta[o.key] = b
		return
	}
	b, _ := json.Marshal(o.value)
	meta[o.key] = b
}

// tableSetting is what the front keeps of one table (above).
type tableSetting struct {
	Created       string  `json:"created,omitempty"`
	ChangeHistory *string `json:"changeHistory,omitempty"`
	PrivacyPolicy *string `json:"privacyPolicy,omitempty"`
	// MVChanged marks a materialized view whose base tables changed
	// since its last refresh, its backing table already brought up to
	// date by a read (materializedviews.go, #1415).
	MVChanged bool `json:"mvChanged,omitempty"`
}

// tableSettings are the settings of every table, by project, dataset and
// table (streamKey), and the tables whose reads are checked (watch:
// require_partition_filter, materialized views, privacy policies).
type tableSettings struct {
	mu    sync.Mutex
	m     map[string]tableSetting
	watch map[string]bool
	path  string
	logf  func(string, ...any)
}

type savedTableSettings struct {
	Settings map[string]tableSetting `json:"settings,omitempty"`
	Watch    []string                `json:"watch,omitempty"`
}

func tableSettingsStateFile(dir string) string { return filepath.Join(dir, "tablesettings.json") }

// keep restores the settings from path and from then on keeps them there.
func (s *tableSettings) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved savedTableSettings
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the table settings it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the table settings it kept", path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m, s.watch = saved.Settings, map[string]bool{}
	for _, k := range saved.Watch {
		s.watch[k] = true
	}
	s.path, s.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	s.saveLocked()
}

func (s *tableSettings) saveLocked() {
	if s.path == "" {
		return
	}
	saved := savedTableSettings{Settings: s.m}
	for k := range s.watch {
		saved.Watch = append(saved.Watch, k)
	}
	sort.Strings(saved.Watch)
	b, err := json.Marshal(saved)
	if err == nil {
		err = writeFileAtomic(s.path, b)
	}
	if err != nil {
		s.logf("bigquery front: keeping the table settings in %s: %v", s.path, err)
	}
}

func (s *tableSettings) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m, s.watch = nil, nil
	s.saveLocked()
}

// set keeps o, a setting of the table made at created.
func (s *tableSettings) set(key, created string, o moreOption) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]tableSetting{}
	}
	t := s.m[key]
	if t.Created != created {
		t = tableSetting{Created: created}
	}
	var v *string
	if str, ok := o.value.(string); ok {
		v = &str
	}
	switch o.name {
	case "enable_change_history":
		t.ChangeHistory = v
	case "privacy_policy":
		t.PrivacyPolicy = v
		if v != nil {
			s.watchLocked(key)
		}
	}
	s.m[key] = t
	s.saveLocked()
}

// setMVChanged sets MVChanged of the table made at created.
func (s *tableSettings) setMVChanged(key, created string, changed bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]tableSetting{}
	}
	t := s.m[key]
	if t.Created != created {
		t = tableSetting{Created: created}
	}
	if t.MVChanged == changed {
		return
	}
	t.MVChanged = changed
	s.m[key] = t
	s.saveLocked()
}

// get is the settings of the table made at created.
func (s *tableSettings) get(key, created string) tableSetting {
	if s == nil {
		return tableSetting{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.m[key]; ok && t.Created == created {
		return t
	}
	return tableSetting{}
}

// watchTable has the front check the reads of the table (above).
func (s *tableSettings) watchTable(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchLocked(key)
	s.saveLocked()
}

func (s *tableSettings) watchLocked(key string) {
	if s.watch == nil {
		s.watch = map[string]bool{}
	}
	s.watch[key] = true
}

func (s *tableSettings) watched(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watch[key]
}

func (s *tableSettings) anyWatched() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watch) > 0
}

// settingsKey is the key an INFORMATION_SCHEMA read puts a table's
// settings under in its resource (annotate).
const settingsKey = "_cloudburrow_settings"

// annotate puts the settings of t, project.dataset's table, in t.
func (s *tableSettings) annotate(project, dataset string, t infoTable) {
	if st := s.get(streamKey(project, dataset, str(t, "tableId")), str(t, "creationTime")); st != (tableSetting{}) {
		t[settingsKey] = st
	}
}

// moreTableOptions are the options above as TABLE_OPTIONS gives them.
func moreTableOptions(t infoTable, add func(name, typ, value string)) {
	if v := str(t, "defaultRoundingMode"); v != "" {
		add("default_rounding_mode", "STRING", strconv.Quote(v))
	}
	st, _ := t[settingsKey].(tableSetting)
	if st.ChangeHistory != nil {
		add("enable_change_history", "BOOL", *st.ChangeHistory)
	}
	if mv, _ := t["materializedView"].(map[string]any); mv != nil { // materializedviews.go
		add("enable_refresh", "BOOL", strconv.FormatBool(mv["enableRefresh"] == true))
		if ms, err := strconv.ParseInt(str(mv, "refreshIntervalMs"), 10, 64); err == nil {
			add("refresh_interval_minutes", "FLOAT64", strconv.FormatFloat(float64(ms)/60000, 'g', -1, 64))
		}
		if mv["allowNonIncrementalDefinition"] == true {
			add("allow_non_incremental_definition", "BOOL", "true")
		}
	}
	if v := str(t, "maxStaleness"); v != "" {
		add("max_staleness", "INTERVAL", `INTERVAL "`+v+`" YEAR TO SECOND`)
	}
	if st.PrivacyPolicy != nil {
		add("privacy_policy", "JSON", strconv.Quote(*st.PrivacyPolicy))
	}
	if b, _ := t["biglakeConfiguration"].(map[string]any); str(b, "storageUri") != "" {
		add("storage_uri", "STRING", strconv.Quote(str(b, "storageUri")))
	}
	if tags, _ := t["resourceTags"].(map[string]any); len(tags) > 0 {
		keys := make([]string, 0, len(tags))
		for k := range tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			v, _ := tags[k].(string)
			parts[i] = "STRUCT(" + strconv.Quote(k) + ", " + strconv.Quote(v) + ")"
		}
		add("tags", "ARRAY<STRUCT<STRING, STRING>>", "["+strings.Join(parts, ", ")+"]")
	}
}

// evalString evaluates expr, a STRING expression, as q's query would.
func (f front) evalString(r *http.Request, q queryOptions, expr string) (*string, string) {
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT " + expr, UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return nil, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	if status != http.StatusOK {
		return nil, errorMessage(got, status)
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return nil, "could not evaluate " + expr
	}
	return res.Rows[0].F[0].V, ""
}

// checkReads answers q when a table it reads refuses it (a missing
// partition filter, a privacy policy, privacypolicy.go), having refreshed each materialized
// view it reads that is due (materializedviews.go); false when it is left
// to the rest of runQuery. Only the tables the front watches are read.
func (f front) checkReads(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	clause := hasPrivacyClause(q.Query) // #1426, privacypolicy.go
	if !f.tables.anyWatched() && !clause {
		return false
	}
	project := projectOf(f.base)
	reads, stmts, ok := tableReads(q.Query, project, defaultDatasetOf(q))
	if !ok {
		return false
	}
	checked := r.Context().Value(privacyCheckedKey{}) != nil
	seen := map[string]bool{}
	var prot []privacyRead
	for _, tr := range reads {
		key := streamKey(project, tr.dataset, tr.table)
		if !f.tables.watched(key) {
			if !checked && f.tables.anyWatched() {
				if meta, ok := f.metaOf(r, tr.dataset, tr.table); ok {
					if why := f.viewReadsPrivate(r, project, tr.dataset, tr.table, meta, 0); why != "" {
						writeError(w, http.StatusNotImplemented, "notImplemented", why)
						return true
					}
				}
			}
			continue
		}
		st, got := f.get(r, tablePath(tr.dataset, tr.table))
		var meta map[string]any
		if st != http.StatusOK || json.Unmarshal(got, &meta) != nil {
			continue
		}
		name := project + "." + tr.dataset + "." + tr.table
		if text := f.privacyOf(project, tr.dataset, tr.table, meta); text != "" && !checked { // #1426
			pol, why := parsePrivacyPolicy(text)
			if why != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", privacyRefusal("a query", name)+
					" The policy could not be read: "+why+".")
				return true
			}
			if why := f.viewReadsPrivate(r, project, tr.dataset, tr.table, meta, 0); why != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", why)
				return true
			}
			prot = append(prot, privacyRead{tr: tr, name: tr.dataset + "." + tr.table, pol: pol, columns: schemaColumns(meta)})
			continue
		}
		if !checked {
			if why := f.viewReadsPrivate(r, project, tr.dataset, tr.table, meta, 0); why != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", why)
				return true
			}
		}
		if tr.decorator != "" {
			continue
		}
		if !seen[key] && str(meta, "type") == "MATERIALIZED_VIEW" {
			f.syncMV(r, tr.dataset, tr.table, meta, true)
		}
		seen[key] = true
		if msg := partitionFilterRefusal(tr, meta, name); msg != "" {
			f.failBeforeRun(w, r, q, insert, tr.dataset, http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: msg})
			return true
		}
	}
	if (len(prot) > 0 || clause) && f.enforcePrivacy(w, r, q, insert, stmts, prot) {
		return true
	}
	return false
}

// partitionFilterRefusal is BigQuery's 400 of tr, a read of a table with
// require_partition_filter, whose resource is meta, when its statement
// names none of the partitioning columns in a WHERE clause, or "".
func partitionFilterRefusal(tr tableRead, meta map[string]any, name string) string {
	tp, _ := meta["timePartitioning"].(map[string]any)
	rp, _ := meta["rangePartitioning"].(map[string]any)
	if meta["requirePartitionFilter"] != true && (tp == nil || tp["requirePartitionFilter"] != true) {
		return ""
	}
	var columns []string
	switch {
	case tp != nil && str(tp, "field") != "":
		columns = []string{str(tp, "field")}
	case tp != nil:
		columns = []string{"_PARTITION_LOAD_TIME", "_PARTITIONDATE", "_PARTITIONTIME"}
	case rp != nil && str(rp, "field") != "":
		columns = []string{str(rp, "field")}
	default:
		return ""
	}
	where := false
	for _, t := range tr.stmt {
		if t.is("WHERE") {
			where = true
			continue
		}
		if !where || t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		name := strings.Trim(t.text, "`")
		for _, c := range columns {
			if strings.EqualFold(name, c) {
				return ""
			}
		}
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = "'" + c + "'"
	}
	return fmt.Sprintf("Cannot query over table '%s' without a filter over column(s) %s that can be used for "+
		"partition elimination", name, strings.Join(quoted, ", "))
}
