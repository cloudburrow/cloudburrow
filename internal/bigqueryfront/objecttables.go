package bigqueryfront

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BigQuery object tables (#1783), read-only external tables over the
// instance's own Cloud Storage, as Google documents them (read
// 2026-11-18): https://docs.cloud.google.com/bigquery/docs/object-table-introduction
// and https://docs.cloud.google.com/bigquery/docs/object-tables.
//
//   - CREATE [OR REPLACE] EXTERNAL TABLE [IF NOT EXISTS] t WITH CONNECTION
//     {project.region.connection | DEFAULT} OPTIONS (object_metadata =
//     'SIMPLE', uris = [...], max_staleness = INTERVAL ...,
//     metadata_cache_mode = 'AUTOMATIC' | 'MANUAL'), and tables.insert of
//     an externalDataConfiguration with objectMetadata SIMPLE, sourceUris,
//     connectionId, metadataCacheMode and the table's maxStaleness, make
//     one. Each URI is gs://bucket/[folder/]object with at most one *
//     wildcard. A column list or schema is refused: the schema is fixed.
//   - The fixed schema: uri, generation, content_type, size, md5_hash
//     (base64), updated, metadata (REPEATED name, value), and ref, the
//     object's ObjectRef, STRUCT<uri STRING, version STRING, authorizer
//     STRING, details JSON>: version is the generation, authorizer the
//     table's connection, details {"gcs_metadata": {content_type,
//     md5_hash (hex), size, updated (microseconds)}} as the ObjectRef page
//     shows (https://docs.cloud.google.com/bigquery/docs/work-with-objectref).
//     The data pseudocolumn is not in the schema and "You can't include
//     the data pseudocolumn in queries": a query naming it fails as an
//     unknown name does. objectData reads an object's bytes, at the
//     generation its row names, for the functions that take data
//     (ML.DECODE_IMAGE, on the BQML branches, #1775).
//   - The engine keeps the table as a table of that schema, whose rows the
//     front writes from Cloud Storage's listing (the table's metadata
//     cache). With max_staleness 0 (the default, caching off) every query
//     naming the table lists the objects again. With a staleness, a query
//     reads the cache while it is no older than the staleness, else lists
//     the objects again ("If the cache is older than this interval,
//     queries fall back to Cloud Storage"); AUTOMATIC refreshes it too
//     once it is 30 minutes old, the low end of the documented "system
//     defined interval, usually somewhere between 30 and 60 minutes";
//     MANUAL only at CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE('t'). Times
//     are the instance's clock.
//   - The connection must exist in the dataset's location and be a Cloud
//     resource connection; the caller needs bigquery.connections.use on it
//     (its policy or the project's, connectionauth.go). Each read of the
//     objects needs the connection's service account to hold
//     storage.objects.list (Storage Object Viewer, as the docs ask) on the
//     project or on the bucket's IAM policy; without it the query fails
//     403 and nothing is read.
//   - Object tables are read-only: DML and tabledata.insertAll into one
//     are 400.
//   - tables.get answers type EXTERNAL with the externalDataConfiguration
//     (sourceUris, objectMetadata, connectionId, metadataCacheMode) and
//     maxStaleness.
//
// UNVERIFIED (docs re-read 2026-11-18, the pages above, which quote none
// of them): the texts of every refusal here; that a CREATE succeeds while
// the service account lacks storage access (the query fails); the
// authorizer's form (the connection as the statement names it).

const objectTablesFile = "objecttables.json"

// objectTable is one object table the front keeps.
type objectTable struct {
	Base       string   `json:"base"`
	Dataset    string   `json:"dataset"`
	Table      string   `json:"table"`
	Created    string   `json:"created"`
	Connection string   `json:"connection"` // project.location.id
	Number     int64    `json:"number"`     // the connection's project number
	URIs       []string `json:"uris"`
	// StalenessMs is max_staleness in milliseconds, 0 for no cache.
	StalenessMs int64  `json:"stalenessMs,omitempty"`
	CacheMode   string `json:"cacheMode,omitempty"`
	// Cached is when the cache (the engine's rows) was last read from
	// Cloud Storage, in Unix milliseconds.
	Cached int64 `json:"cached,omitempty"`
}

// objectTables are the object tables, by streamKey.
type objectTables struct {
	mu      sync.Mutex
	refresh sync.Mutex // one refresh at a time
	m       map[string]objectTable
	path    string
	logf    func(string, ...any)
	storage *storageReader
	// agents are the connections' service agents' keys, public the
	// address clients reach Cloud Storage at, and signNow the signing
	// clock (nil: the system's), for SIGNED_URL (signedurls.go, #1786).
	agents  agentKeys
	public  string
	signNow func() time.Time
}

func (o *objectTables) keep(dir string, logf func(string, ...any)) {
	o.path, o.logf = filepath.Join(dir, objectTablesFile), logf
	o.agents.keep(dir, logf)
	b, err := os.ReadFile(o.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && logf != nil {
		logf("bigquery front: object tables: %v", err)
	}
	if err == nil && json.Unmarshal(b, &o.m) != nil && logf != nil {
		logf("bigquery front: %s is unreadable; starting without the object tables it kept", o.path)
	}
}

func (o *objectTables) saveLocked() {
	if o.path == "" {
		return
	}
	b, _ := json.Marshal(o.m)
	if err := writeFileAtomic(o.path, b); err != nil && o.logf != nil {
		o.logf("bigquery front: object tables: %v", err)
	}
}

func (o *objectTables) any() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.m) > 0
}

func (o *objectTables) put(key string, t objectTable) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[string]objectTable{}
	}
	o.m[key] = t
	o.saveLocked()
}

func (o *objectTables) forget(key string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.m[key]; ok {
		delete(o.m, key)
		o.saveLocked()
	}
}

// lookup is the object table dataset.table of f, if the engine's table
// is still the one it made (a table dropped and made again is not).
func (f front) objectTable(r *http.Request, dataset, table string) (objectTable, bool) {
	if !f.objects.any() {
		return objectTable{}, false
	}
	key := streamKey(projectOf(f.base), dataset, table)
	f.objects.mu.Lock()
	ot, ok := f.objects.m[key]
	f.objects.mu.Unlock()
	if !ok {
		return ot, false
	}
	status, got := f.get(r, tablePath(dataset, table))
	var meta struct {
		CreationTime string `json:"creationTime"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || meta.CreationTime != ot.Created {
		if status == http.StatusOK || status == http.StatusNotFound {
			f.objects.forget(key)
		}
		return ot, false
	}
	return ot, true
}

// objectTableSchema is the fixed schema of an object table.
var objectTableSchema = []map[string]any{
	{"name": "uri", "type": "STRING", "mode": "NULLABLE"},
	{"name": "generation", "type": "INTEGER", "mode": "NULLABLE"},
	{"name": "content_type", "type": "STRING", "mode": "NULLABLE"},
	{"name": "size", "type": "INTEGER", "mode": "NULLABLE"},
	{"name": "md5_hash", "type": "STRING", "mode": "NULLABLE"},
	{"name": "updated", "type": "TIMESTAMP", "mode": "NULLABLE"},
	{"name": "metadata", "type": "RECORD", "mode": "REPEATED", "fields": []map[string]any{
		{"name": "name", "type": "STRING", "mode": "NULLABLE"},
		{"name": "value", "type": "STRING", "mode": "NULLABLE"}}},
	{"name": "ref", "type": "RECORD", "mode": "NULLABLE", "fields": []map[string]any{
		{"name": "uri", "type": "STRING", "mode": "NULLABLE"},
		{"name": "version", "type": "STRING", "mode": "NULLABLE"},
		{"name": "authorizer", "type": "STRING", "mode": "NULLABLE"},
		{"name": "details", "type": "JSON", "mode": "NULLABLE"}}},
}

// objectSpec is an object table's definition, from DDL or tables.insert.
type objectSpec struct {
	path         []string
	replace      bool
	ifNotExists  bool
	connection   string // as written, a path or DEFAULT
	metadata     string
	uris         []string
	hasStaleness bool
	stalenessMs  int64
	cacheMode    string
	description  *string
	friendlyName *string
	labels       map[string]string
	// invalid is a 400's message, unsupported a 501's.
	invalid, unsupported string
}

// parseObjectTableDDL reads sql as one CREATE EXTERNAL TABLE whose
// OPTIONS name object_metadata; ok is false for any other statement.
func parseObjectTableDDL(sql string) (s objectSpec, ok bool) {
	if !strings.Contains(strings.ToLower(sql), "object_metadata") {
		return s, false
	}
	t, lexed := lex(sql)
	if !lexed || len(t) < 4 || !t[0].is("CREATE") {
		return s, false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return s, false
		}
	}
	t = t[:end]
	i := 1
	if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
		s.replace = true
		i += 2
	}
	if i+1 >= end || !t[i].is("EXTERNAL") || !t[i+1].is("TABLE") {
		return s, false
	}
	i += 2
	if i+2 < end && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		s.ifNotExists = true
		i += 3
	}
	s.path, i = path(t, i)
	if len(s.path) == 0 {
		return s, false
	}
	opt := -1
	for j := i; j < end; j++ {
		switch {
		case t[j].punct("("):
			if s.invalid == "" {
				s.invalid = "An object table has a fixed schema: CREATE EXTERNAL TABLE with object_metadata takes no column list."
			}
			j = skipTo(t, j+1, ")")
		case t[j].is("WITH") && j+2 < end && t[j+1].is("CONNECTION"):
			if t[j+2].is("DEFAULT") {
				s.connection = "DEFAULT"
				j += 2
				continue
			}
			p, k := path(t, j+2)
			s.connection = strings.Join(p, ".")
			j = k - 1
		case t[j].is("WITH") && j+3 < end && t[j+1].is("PARTITION") && t[j+2].is("COLUMNS"):
			if s.invalid == "" {
				s.invalid = objectHiveRefusal
			}
		case t[j].is("OPTIONS") && j+1 < end && t[j+1].punct("("):
			opt = j
			j = end
		}
	}
	if opt < 0 {
		return s, false
	}
	close := skipTo(t, opt+2, ")")
	if close != end-1 && s.unsupported == "" {
		s.unsupported = "a clause after OPTIONS"
	}
	if !s.readOptions(sql, t[opt+2:min(close, end)]) {
		return s, false
	}
	return s, true
}

// objectHiveRefusal is the 400 of hive partitioning on an object table
// (WITH PARTITION COLUMNS, hive_partition_uri_prefix,
// require_hive_partition_filter, hivePartitioningOptions): "Not all
// storage formats support hive partitioning; requesting hive partitioning
// on an unsupported format will lead to an error. Currently supported
// formats are: JSON, CSV, ORC, Avro and Parquet"
// (https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/tables,
// HivePartitioningOptions.mode, read 2026-11-18), and an object table has
// no format ("If ObjectMetadata is set, sourceFormat should be omitted").
// The text is not measured.
const objectHiveRefusal = "Hive partitioning is not supported for object tables: it is supported for the formats JSON, CSV, ORC, " +
	"Avro and Parquet, and an object table (object_metadata) has no format."

// readOptions reads an object table's OPTIONS: false when object_metadata
// is not among them (another kind of external table, the engine's).
func (s *objectSpec) readOptions(sql string, t []token) bool {
	seen := false
	for i := 0; i < len(t); {
		if t[i].kind != tokWord || i+1 >= len(t) || !t[i+1].punct("=") {
			if s.unsupported == "" {
				s.unsupported = "OPTIONS that CloudBurrow does not read"
			}
			return seen || s.metadata != ""
		}
		name := strings.ToLower(t[i].text)
		i += 2
		end := skipTo(t, i, ",")
		if end > len(t) {
			end = len(t)
		}
		val := t[i:end]
		i = end + 1
		bad := func(what string) {
			if s.invalid == "" {
				s.invalid = fmt.Sprintf("Invalid value for the option %s: %s.", name, what)
			}
		}
		str := func() (string, bool) {
			if len(val) != 1 {
				return "", false
			}
			return stringLiteral(val[0])
		}
		switch name {
		case "object_metadata":
			seen = true
			v, ok := str()
			if !ok {
				bad("a string literal is expected")
				continue
			}
			s.metadata = v
		case "uris":
			if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
				bad("an ARRAY<STRING> literal is expected")
				continue
			}
			for _, tok := range val[1 : len(val)-1] {
				if tok.punct(",") {
					continue
				}
				v, ok := stringLiteral(tok)
				if !ok {
					bad("an ARRAY<STRING> literal is expected")
					break
				}
				s.uris = append(s.uris, v)
			}
		case "max_staleness":
			ms, ok := intervalLiteralMs(val)
			if !ok {
				if s.unsupported == "" {
					s.unsupported = "max_staleness written other than as 0 or INTERVAL n MINUTE|HOUR|DAY or INTERVAL 'Y-M D H:M:S' YEAR TO SECOND"
				}
				continue
			}
			s.hasStaleness, s.stalenessMs = true, ms
		case "metadata_cache_mode":
			v, ok := str()
			if !ok {
				bad("a string literal is expected")
				continue
			}
			s.cacheMode = v
		case "hive_partition_uri_prefix", "require_hive_partition_filter":
			if s.invalid == "" {
				s.invalid = objectHiveRefusal
			}
		case "description", "friendly_name":
			v, ok := str()
			if !ok {
				bad("a string literal is expected")
				continue
			}
			if name == "description" {
				s.description = &v
			} else {
				s.friendlyName = &v
			}
		case "labels":
			l, ok := labelsLiteral(val)
			if !ok {
				bad("an array of (key, value) string literals is expected")
				continue
			}
			s.labels = l
		default:
			if s.unsupported == "" {
				s.unsupported = "the option " + name
			}
		}
	}
	return seen
}

// intervalLiteralMs reads 0, INTERVAL n UNIT or INTERVAL 'Y-M D H:M:S'
// YEAR TO SECOND as milliseconds.
func intervalLiteralMs(val []token) (int64, bool) {
	if len(val) == 1 && val[0].kind == tokWord && val[0].text == "0" {
		return 0, true
	}
	if len(val) < 3 || !val[0].is("INTERVAL") {
		return 0, false
	}
	if len(val) == 5 && val[2].is("YEAR") && val[3].is("TO") && val[4].is("SECOND") {
		s, ok := stringLiteral(val[1])
		if !ok {
			return 0, false
		}
		return parseIntervalMs(s)
	}
	if len(val) != 3 {
		return 0, false
	}
	n := val[1].text
	if s, ok := stringLiteral(val[1]); ok {
		n = s
	}
	v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	unit := map[string]int64{"SECOND": 1000, "MINUTE": 60000, "HOUR": 3600000, "DAY": 86400000}[strings.ToUpper(val[2].text)]
	if unit == 0 || val[2].kind != tokWord {
		return 0, false
	}
	return v * unit, true
}

var intervalForm = regexp.MustCompile(`^(\d+)-(\d+) (\d+) (\d+):(\d+):(\d+)(?:\.\d+)?$`)

// parseIntervalMs reads the canonical INTERVAL text, Y-M D H:M:S, the
// form tables.insert's maxStaleness takes, as milliseconds; a year or
// month is refused (a staleness is at most 7 days).
func parseIntervalMs(s string) (int64, bool) {
	m := intervalForm.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n := make([]int64, 7)
	for i := 1; i <= 6; i++ {
		n[i], _ = strconv.ParseInt(m[i], 10, 64)
	}
	if n[1] != 0 || n[2] != 0 {
		return 0, false
	}
	return ((n[3]*24+n[4])*60+n[5])*60000 + n[6]*1000, true
}

// formatIntervalMs is ms as the canonical INTERVAL text.
func formatIntervalMs(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("0-0 %d %d:%d:%d", s/86400, s%86400/3600, s%3600/60, s%60)
}

// check is why BigQuery refuses s (400), or "".
func (s objectSpec) check() string {
	switch {
	case s.invalid != "":
		return s.invalid
	case !strings.EqualFold(s.metadata, "SIMPLE"):
		return fmt.Sprintf("Invalid value for object_metadata: %q. An object table takes object_metadata = 'SIMPLE'.", s.metadata)
	case s.connection == "":
		return "An object table needs a connection: CREATE EXTERNAL TABLE ... WITH CONNECTION {`PROJECT_ID.REGION.CONNECTION_ID` | DEFAULT}."
	case len(s.uris) == 0:
		return "An object table needs uris, the Cloud Storage paths of its objects (gs://bucket_name/[folder_name/]*)."
	}
	for _, u := range s.uris {
		rest, ok := strings.CutPrefix(u, "gs://")
		bucket, name, _ := strings.Cut(rest, "/")
		switch {
		case !ok || bucket == "" || name == "":
			return fmt.Sprintf("Invalid uri %q: an object table's uris are gs://bucket_name/[folder_name/]object_name, with at most one * wildcard.", u)
		case strings.Count(name, "*") > 1 || strings.Contains(bucket, "*"):
			return fmt.Sprintf("Invalid uri %q: only one * wildcard is supported in a Cloud Storage path.", u)
		}
	}
	if s.cacheMode != "" && s.cacheMode != "AUTOMATIC" && s.cacheMode != "MANUAL" {
		return fmt.Sprintf("Invalid value for metadata_cache_mode: %q. It is AUTOMATIC or MANUAL.", s.cacheMode)
	}
	if s.stalenessMs != 0 && (s.stalenessMs < 30*60000 || s.stalenessMs > 7*86400000) {
		return "max_staleness must be between 30 minutes and 7 days, or 0 to disable metadata caching."
	}
	if s.stalenessMs > 0 && s.cacheMode == "" {
		return "metadata_cache_mode must be set when max_staleness is greater than 0."
	}
	return ""
}

// resolveConnection checks s's connection for a table of dataset in loc:
// the connection's name as the table keeps it and its project number, or
// a refusal's code and message.
func (f front) resolveObjectConnection(r *http.Request, s objectSpec, loc string) (string, int64, int, string) {
	project := projectOf(f.base)
	loc = strings.ToLower(loc)
	if loc == "" {
		loc = "us"
	}
	var parts []string
	if s.connection == "DEFAULT" {
		id := f.settings.effective(project, loc, "default_cloud_resource_connection_id")
		if id == "" {
			id = defaultConnectionID
		}
		parts = []string{project, loc, id}
	} else if strings.HasPrefix(s.connection, "projects/") {
		p := strings.Split(s.connection, "/")
		if len(p) != 6 || p[2] != "locations" || p[4] != "connections" {
			return "", 0, 400, fmt.Sprintf("Invalid connection %q: it is PROJECT_ID.REGION.CONNECTION_ID.", s.connection)
		}
		parts = []string{p[1], p[3], p[5]}
	} else {
		parts = strings.Split(s.connection, ".")
		if len(parts) == 2 {
			parts = append([]string{project}, parts...)
		}
		if len(parts) != 3 {
			return "", 0, 400, fmt.Sprintf("Invalid connection %q: it is PROJECT_ID.REGION.CONNECTION_ID.", s.connection)
		}
	}
	number := connectionProject(parts[0], f.numbers)
	cn, ok := f.connections.lookup(number, parts[1], parts[2])
	if s.connection == "DEFAULT" {
		var code int
		var msg string
		if cn, code, msg = f.ensureDefaultConnection(r.Context(), project, number, parts[1], parts[2], cn); code != 0 {
			return "", 0, code, msg
		}
		ok = true
	}
	name := parts[0] + "." + strings.ToLower(parts[1]) + "." + parts[2]
	if !ok {
		return "", 0, 404, "Not found: Connection " + name
	}
	if !sameLocation(cn.Location, loc) {
		return "", 0, 400, fmt.Sprintf("Dataset location %s differs from connection location %s", loc, cn.Location)
	}
	if _, ok := cn.Fields["cloudResource"]; !ok {
		return "", 0, 400, "The connection " + name + " is not a Cloud resource connection: an object table needs a CLOUD_RESOURCE connection"
	}
	if code, msg := f.checkConnectionCaller(r.Context(), parts[0], project, cn); code != 0 {
		return "", 0, code, msg
	}
	return name, number, 0, ""
}

// checkConnectionCaller checks the caller's bigquery.connections.use on cn
// (its policy, or the connection project's).
func (f front) checkConnectionCaller(ctx context.Context, connProject, project string, cn *bqConnection) (int, string) {
	proj, code, msg := f.projectBindings(ctx, connProject)
	if code != 0 {
		return code, msg
	}
	caller := "serviceAccount:" + callerIdentity(project)
	if !bindingsGrant(cn.bindings(), caller, permConnectionsUse) && !bindingsGrant(proj, caller, permConnectionsUse) {
		return 403, fmt.Sprintf("Access Denied: Connection %s: Permission %s denied on connection %s (or it may not exist).",
			cn.name(), permConnectionsUse, cn.name())
	}
	return 0, ""
}

// createObjectTable makes s's table in dataset: the engine's table of the
// fixed schema, kept as an object table, its cache read once. op is the
// DDL operation (CREATE, REPLACE or SKIP); the table's resource is
// answered when made.
func (f front) createObjectTable(r *http.Request, dataset, table string, s objectSpec) (op string, meta []byte, code int, msg string) {
	if msg := s.check(); msg != "" {
		return "", nil, 400, msg
	}
	if s.unsupported != "" {
		return "", nil, 501, "Not implemented here: " + s.unsupported + " of an object table. CloudBurrow reads object_metadata, uris, " +
			"max_staleness, metadata_cache_mode, description, friendly_name and labels. Nothing was made."
	}
	if f.storage == nil {
		return "", nil, 501, "Not implemented here: an object table reads the instance's Cloud Storage, which the BigQuery front was not given (enable Cloud Storage). Nothing was made."
	}
	project := projectOf(f.base)
	status, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if status != http.StatusOK {
		return "", nil, status, errorMessage(got, status)
	}
	var ds struct {
		Location string `json:"location"`
	}
	_ = json.Unmarshal(got, &ds)
	conn, number, code, msg := f.resolveObjectConnection(r, s, ds.Location)
	if code != 0 {
		return "", nil, code, msg
	}
	op = "CREATE"
	if status, _ := f.get(r, tablePath(dataset, table)); status == http.StatusOK {
		switch {
		case s.ifNotExists:
			return "SKIP", nil, 0, ""
		case !s.replace:
			return "", nil, 409, fmt.Sprintf("Already Exists: Table %s:%s.%s", project, dataset, table)
		}
		if st, body := f.send(r, http.MethodDelete, tablePath(dataset, table), nil); st/100 != 2 {
			return "", nil, st, errorMessage(body, st)
		}
		op = "REPLACE"
	}
	res := map[string]any{"tableReference": map[string]string{"projectId": project, "datasetId": dataset, "tableId": table},
		"schema": map[string]any{"fields": objectTableSchema}}
	if s.description != nil {
		res["description"] = *s.description
	}
	if s.friendlyName != nil {
		res["friendlyName"] = *s.friendlyName
	}
	if len(s.labels) > 0 {
		res["labels"] = s.labels
	}
	body, _ := json.Marshal(res)
	status, made := f.send(r, http.MethodPost, "/datasets/"+url.PathEscape(dataset)+"/tables", body)
	if status != http.StatusOK {
		return "", nil, status, errorMessage(made, status)
	}
	var m struct {
		CreationTime string `json:"creationTime"`
	}
	_ = json.Unmarshal(made, &m)
	key := streamKey(project, dataset, table)
	ot := objectTable{Base: f.base, Dataset: dataset, Table: table, Created: m.CreationTime, Connection: conn, Number: number,
		URIs: s.uris, StalenessMs: s.stalenessMs, CacheMode: s.cacheMode}
	f.objects.put(key, ot)
	// "autopopulated when the object table is created": the first read;
	// a refusal (the service account's access) is the queries' to report.
	if code, msg := f.refreshObjectTable(r, key); code != 0 && f.objects.logf != nil {
		f.objects.logf("bigquery front: object table %s.%s: %s", dataset, table, msg)
	}
	_, meta = f.decoratedObjectTable(r, dataset, table)
	return op, meta, 0, ""
}

// gcsObjectMeta is the part of a Cloud Storage object resource an object
// table's row holds.
type gcsObjectMeta struct {
	Bucket      string            `json:"bucket"`
	Name        string            `json:"name"`
	Generation  string            `json:"generation"`
	ContentType string            `json:"contentType"`
	Size        string            `json:"size"`
	MD5Hash     string            `json:"md5Hash"`
	Updated     string            `json:"updated"`
	Metadata    map[string]string `json:"metadata"`
}

// listObjects lists the objects uri names: one object, or those its one *
// wildcard matches (any run of characters).
func (s *storageReader) listObjects(ctx context.Context, uri string) ([]gcsObjectMeta, int, error) {
	rest := strings.TrimPrefix(uri, "gs://")
	bucket, name, _ := strings.Cut(rest, "/")
	if !strings.Contains(name, "*") {
		var o gcsObjectMeta
		status, err := s.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(name), &o)
		if err != nil || status == http.StatusNotFound {
			return nil, status, err
		}
		if status != http.StatusOK {
			return nil, status, nil
		}
		return []gcsObjectMeta{o}, status, nil
	}
	prefix, suffix, _ := strings.Cut(name, "*")
	var out []gcsObjectMeta
	token := ""
	for {
		q := url.Values{"prefix": {prefix}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var list struct {
			Items         []gcsObjectMeta `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
		}
		status, err := s.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), &list)
		if err != nil || status != http.StatusOK {
			return nil, status, err
		}
		for _, it := range list.Items {
			if len(it.Name) >= len(prefix)+len(suffix) && strings.HasSuffix(it.Name, suffix) && !strings.HasSuffix(it.Name, "/") {
				out = append(out, it)
			}
		}
		if token = list.NextPageToken; token == "" {
			return out, http.StatusOK, nil
		}
	}
}

// permObjectsList is what the connection's service account needs to read
// an object table's bucket (Storage Object Viewer).
const permObjectsList = "storage.objects.list"

// checkBucketAccess checks that sa may list bucket's objects: through the
// project's policy or the bucket's.
func (f front) checkBucketAccess(r *http.Request, project, sa, bucket string) (int, string) {
	member := "serviceAccount:" + sa
	proj, code, msg := f.projectBindings(r.Context(), project)
	if code != 0 {
		return code, msg
	}
	if bindingsGrant(proj, member, permObjectsList) {
		return 0, ""
	}
	var pol struct {
		Bindings []policyBinding `json:"bindings"`
	}
	status, err := f.storage.do(r.Context(), "/storage/v1/b/"+url.PathEscape(bucket)+"/iam", &pol)
	if err == nil && status == http.StatusOK && bindingsGrant(pol.Bindings, member, permObjectsList) {
		return 0, ""
	}
	return 403, fmt.Sprintf("Access Denied: BigQuery BigQuery: Permission denied while globbing file pattern. %s does not have "+
		"%s access to the Google Cloud Storage bucket. Permission '%s' denied on resource (or it may not exist).",
		sa, permObjectsList, permObjectsList)
}

// refreshObjectTable reads key's objects from Cloud Storage into the
// engine's table: 0, or a refusal's code and message.
func (f front) refreshObjectTable(r *http.Request, key string) (int, string) {
	f.objects.refresh.Lock()
	defer f.objects.refresh.Unlock()
	f.objects.mu.Lock()
	ot, ok := f.objects.m[key]
	f.objects.mu.Unlock()
	if !ok {
		return 404, "Not found: Table " + key
	}
	parts := strings.Split(ot.Connection, ".")
	cn, ok := f.connections.lookup(ot.Number, parts[1], parts[2])
	if !ok {
		return 404, "Not found: Connection " + ot.Connection
	}
	project := projectOf(f.base)
	sa := cn.serviceAccount()
	seen := map[string]bool{}
	var objs []gcsObjectMeta
	for _, u := range ot.URIs {
		bucket, _, _ := strings.Cut(strings.TrimPrefix(u, "gs://"), "/")
		if code, msg := f.checkBucketAccess(r, project, sa, bucket); code != 0 {
			return code, msg
		}
		list, status, err := f.storage.listObjects(r.Context(), u)
		if err != nil {
			return 500, "cloudburrow: the instance's Cloud Storage did not answer listing " + u + ": " + err.Error()
		}
		if status != http.StatusOK && status != http.StatusNotFound {
			return 500, fmt.Sprintf("cloudburrow: the instance's Cloud Storage answered HTTP %d listing %s", status, u)
		}
		for _, o := range list {
			if id := o.Bucket + "/" + o.Name; !seen[id] {
				seen[id] = true
				objs = append(objs, o)
			}
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Bucket+"/"+objs[i].Name < objs[j].Bucket+"/"+objs[j].Name })
	legacy := false
	del, _ := json.Marshal(queryOptions{Query: "DELETE FROM " + quotePath([]string{ot.Dataset, ot.Table}) + " WHERE TRUE", UseLegacySQL: &legacy})
	if status, got := f.send(r, http.MethodPost, "/queries", del); status != http.StatusOK {
		return 500, "cloudburrow: could not clear the object table's cache: " + errorMessage(got, status)
	}
	// The rows are written by INSERT: the engine's insertAll keeps a JSON
	// value inside a STRUCT as a string (measured 2026-11-18).
	for start := 0; start < len(objs); start += 200 {
		var vals []string
		for _, o := range objs[start:min(start+200, len(objs))] {
			vals = append(vals, objectRowSQL(objectRow(o, ot.Connection)))
		}
		ins, _ := json.Marshal(queryOptions{Query: "INSERT INTO " + quotePath([]string{ot.Dataset, ot.Table}) +
			" (uri, generation, content_type, size, md5_hash, updated, metadata, ref) VALUES " + strings.Join(vals, ", "), UseLegacySQL: &legacy})
		if status, got := f.send(r, http.MethodPost, "/queries", ins); status != http.StatusOK {
			return 500, "cloudburrow: could not write the object table's cache: " + errorMessage(got, status)
		}
	}
	f.objects.mu.Lock()
	if cur, ok := f.objects.m[key]; ok && cur.Created == ot.Created {
		cur.Cached = f.now().UnixMilli()
		f.objects.m[key] = cur
		f.objects.saveLocked()
	}
	f.objects.mu.Unlock()
	return 0, ""
}

// objectRow is o's row of an object table of connection.
func objectRow(o gcsObjectMeta, connection string) map[string]any {
	uri := "gs://" + o.Bucket + "/" + o.Name
	row := map[string]any{"uri": uri}
	gen, _ := strconv.ParseInt(o.Generation, 10, 64)
	size, _ := strconv.ParseInt(o.Size, 10, 64)
	row["generation"] = gen
	row["size"] = size
	details := map[string]any{"size": size}
	if o.ContentType != "" {
		row["content_type"] = o.ContentType
		details["content_type"] = o.ContentType
	}
	if o.MD5Hash != "" {
		row["md5_hash"] = o.MD5Hash
		if b, err := base64.StdEncoding.DecodeString(o.MD5Hash); err == nil {
			details["md5_hash"] = hex.EncodeToString(b)
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, o.Updated); err == nil {
		row["updated"] = ts.UTC().Format("2006-01-02 15:04:05.999999 UTC")
		details["updated"] = ts.UnixMicro()
	}
	keys := make([]string, 0, len(o.Metadata))
	for k := range o.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	md := []map[string]string{}
	for _, k := range keys {
		md = append(md, map[string]string{"name": k, "value": o.Metadata[k]})
	}
	row["metadata"] = md
	row["ref"] = map[string]any{"uri": uri, "version": o.Generation, "authorizer": connection, "details": map[string]any{"gcs_metadata": details}}
	return row
}

// objectData reads the bytes of the object uri at generation, as the
// object table's connection may: the data pseudocolumn, for the
// functions that take it (#1775).
func (f front) objectData(r *http.Request, ot objectTable, uri string, generation int64) ([]byte, int, string) {
	rest := strings.TrimPrefix(uri, "gs://")
	bucket, name, _ := strings.Cut(rest, "/")
	parts := strings.Split(ot.Connection, ".")
	cn, ok := f.connections.lookup(ot.Number, parts[1], parts[2])
	if !ok {
		return nil, 404, "Not found: Connection " + ot.Connection
	}
	if code, msg := f.checkBucketAccess(r, projectOf(f.base), cn.serviceAccount(), bucket); code != 0 {
		return nil, code, msg
	}
	path := "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(name) + "?alt=media&generation=" + strconv.FormatInt(generation, 10)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, f.storage.base+path, nil)
	if err != nil {
		return nil, 500, err.Error()
	}
	resp, err := f.storage.client.Do(req)
	if err != nil {
		return nil, 500, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 404, "Not found: URI " + uri
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 500, err.Error()
	}
	return b, 0, ""
}

// stale reports whether a query of ot reads the objects again (above).
func (ot objectTable) stale(now time.Time) bool {
	if ot.StalenessMs == 0 || ot.Cached == 0 {
		return true
	}
	age := now.UnixMilli() - ot.Cached
	if age > ot.StalenessMs {
		return true
	}
	return ot.CacheMode == "AUTOMATIC" && age >= 30*60000
}

// objectTablesRead refreshes the stale object tables q names, before it
// runs: 0, or the refusal the query fails with.
func (f front) objectTablesRead(r *http.Request, q queryOptions) (int, string) {
	if !f.objects.any() {
		return 0, ""
	}
	low := strings.ToLower(q.Query)
	project := projectOf(f.base)
	f.objects.mu.Lock()
	var keys []string
	for k, ot := range f.objects.m {
		if projectOf(ot.Base) != project {
			continue
		}
		if strings.Contains(low, strings.ToLower(ot.Table)) && (strings.Contains(low, strings.ToLower(ot.Dataset)) ||
			strings.Contains(strings.ToLower(string(q.DefaultDataset)), `"`+strings.ToLower(ot.Dataset)+`"`)) {
			keys = append(keys, k)
		}
	}
	f.objects.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		f.objects.mu.Lock()
		ot := f.objects.m[k]
		f.objects.mu.Unlock()
		if _, live := f.objectTable(r, ot.Dataset, ot.Table); !live || !ot.stale(f.now()) {
			continue
		}
		if code, msg := f.refreshObjectTable(r, k); code != 0 {
			return code, msg
		}
	}
	return 0, ""
}

// writeTarget is the table a lone DML statement writes, or nil.
func writeTarget(sql string) []string {
	t, lexed := lex(sql)
	if !lexed || len(t) < 2 {
		return nil
	}
	i := 1
	switch {
	case t[0].is("INSERT") || t[0].is("MERGE"):
		if t[i].is("INTO") {
			i++
		}
	case t[0].is("DELETE"):
		if t[i].is("FROM") {
			i++
		}
	case t[0].is("TRUNCATE"):
		if t[i].is("TABLE") {
			i++
		}
	case t[0].is("UPDATE"):
	default:
		return nil
	}
	p, _ := path(t, i)
	return p
}

// readOnlyObjectTable is the 400 of a write to an object table.
func readOnlyObjectTable(project, dataset, table string) string {
	return fmt.Sprintf("%s:%s.%s is an object table. Object tables are read-only, because they map to unstructured data objects in Cloud Storage.",
		project, dataset, table)
}

// serveObjectTableStatements carries out a lone CREATE EXTERNAL TABLE of
// an object table, a CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE, and refuses
// DML into an object table; otherwise it refreshes the object tables q
// reads (above) and reports false.
func (f front) serveObjectTableStatements(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.objects == nil {
		return false
	}
	project := projectOf(f.base)
	failed := func(code int, msg string) bool {
		reason := map[int]string{400: "invalidQuery", 403: "accessDenied", 404: "notFound", 409: "duplicate", 501: "notImplemented"}[code]
		if reason == "" {
			reason = "internalError"
		}
		if code == 501 {
			writeError(w, code, reason, msg)
			return true
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
		return true
	}
	done := func(ds, table, statementType, op string) bool {
		ddl := &ownDDL{target: tableRef{ProjectID: project, DatasetID: ds, TableID: table}, operation: op, statementType: statementType}
		if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return true
		}
		f.forward(w, r, jobText{query: q.Query, ddl: ddl})
		return true
	}
	if s, ok := parseObjectTableDDL(q.Query); ok {
		if dryRun(r, insert) {
			return false
		}
		ds, table, ok := tableOf(q, s.path)
		if !ok || len(s.path) == 3 && s.path[0] != project {
			return failed(400, "Table \""+strings.Join(s.path, ".")+"\" must be qualified with a dataset (e.g. dataset.table).")
		}
		op, _, code, msg := f.createObjectTable(r, ds, table, s)
		if code != 0 {
			return failed(code, msg)
		}
		return done(ds, table, "CREATE_EXTERNAL_TABLE", op)
	}
	if p, ok, msg := refreshCacheStatement(q.Query); ok {
		if msg != "" {
			return failed(400, msg)
		}
		ds, table, ok := tableOf(q, p)
		if !ok {
			return failed(400, "BQ.REFRESH_EXTERNAL_METADATA_CACHE takes the table's name, dataset.table or project.dataset.table.")
		}
		ot, live := f.objectTable(r, ds, table)
		if !live {
			if st, got := f.get(r, tablePath(ds, table)); st != http.StatusOK {
				return failed(404, errorMessage(got, st))
			}
			return failed(400, fmt.Sprintf("BQ.REFRESH_EXTERNAL_METADATA_CACHE refreshes a BigLake or object table with metadata caching; "+
				"%s:%s.%s is not one.", project, ds, table))
		}
		if ot.CacheMode != "MANUAL" {
			return failed(400, fmt.Sprintf("BQ.REFRESH_EXTERNAL_METADATA_CACHE refreshes a table whose metadata_cache_mode is MANUAL; "+
				"%s:%s.%s's is %s.", project, ds, table, cmpOr(ot.CacheMode, "not set")))
		}
		if dryRun(r, insert) {
			return false
		}
		if code, msg := f.refreshObjectTable(r, streamKey(project, ds, table)); code != 0 {
			return failed(code, msg)
		}
		return done(ds, table, "CALL", "")
	}
	calls, code, msg := f.checkObjectTransform(r, q) // EXTERNAL_OBJECT_TRANSFORM
	if code != 0 {
		return failed(code, msg)
	}
	if p := writeTarget(q.Query); p != nil {
		if ds, table, ok := tableOf(q, p); ok {
			if _, live := f.objectTable(r, ds, table); live {
				return failed(400, readOnlyObjectTable(project, ds, table))
			}
		}
	}
	if len(calls) > 0 {
		text, code, msg := f.rewriteObjectTransforms(r, q, calls)
		if code != 0 {
			return failed(code, msg)
		}
		if !setQueryText(r, insert, text) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return true
		}
		f.forward(w, r, jobText{query: q.Query})
		return true
	}
	if code, msg := f.objectTablesRead(r, q); code != 0 {
		return failed(code, msg)
	}
	return false
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// refreshCacheStatement reads sql as CALL
// BQ.REFRESH_EXTERNAL_METADATA_CACHE('table'): the table's path, or the
// 400's message.
func refreshCacheStatement(sql string) ([]string, bool, string) {
	if !strings.Contains(strings.ToUpper(sql), "REFRESH_EXTERNAL_METADATA_CACHE") {
		return nil, false, ""
	}
	t, lexed := lex(sql)
	if !lexed || len(t) < 2 || !t[0].is("CALL") {
		return nil, false, ""
	}
	if t[len(t)-1].punct(";") {
		t = t[:len(t)-1]
	}
	parts, next := path(t, 1)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "BQ") || !strings.EqualFold(parts[1], "REFRESH_EXTERNAL_METADATA_CACHE") {
		return nil, false, ""
	}
	if next+2 < len(t) && t[next].punct("(") && next+2 == len(t)-1 && t[len(t)-1].punct(")") {
		if s, ok := stringLiteral(t[next+1]); ok && s != "" {
			return strings.Split(s, "."), true, ""
		}
	}
	return nil, true, "BQ.REFRESH_EXTERNAL_METADATA_CACHE takes the table's name as one STRING literal."
}

// decoratedObjectTable is dataset.table's resource as BigQuery answers an
// object table's (above).
func (f front) decoratedObjectTable(r *http.Request, dataset, table string) (int, []byte) {
	status, got := f.get(r, tablePath(dataset, table))
	ot, live := f.objectTable(r, dataset, table)
	if status != http.StatusOK || !live {
		return status, got
	}
	var meta map[string]any
	if json.Unmarshal(got, &meta) != nil {
		return status, got
	}
	f.decorateObjectTable(meta, ot)
	b, _ := json.Marshal(meta)
	return status, b
}

func (f front) decorateObjectTable(meta map[string]any, ot objectTable) {
	meta["type"] = "EXTERNAL"
	conf := map[string]any{"sourceUris": ot.URIs, "objectMetadata": "SIMPLE", "connectionId": ot.Connection}
	if ot.CacheMode != "" {
		conf["metadataCacheMode"] = ot.CacheMode
	}
	meta["externalDataConfiguration"] = conf
	if ot.StalenessMs > 0 {
		meta["maxStaleness"] = formatIntervalMs(ot.StalenessMs)
	}
	for _, k := range []string{"numRows", "numBytes", "numLongTermBytes", "numPhysicalBytes", "numTotalLogicalBytes",
		"numActiveLogicalBytes", "numLongTermLogicalBytes", "streamingBuffer"} {
		delete(meta, k)
	}
}

// serveObjectTableGet answers tables.get of an object table; false for
// any other table.
func (f front) serveObjectTableGet(w http.ResponseWriter, r *http.Request, dataset, table string) bool {
	if !f.objects.any() {
		return false
	}
	if _, live := f.objectTable(r, dataset, table); !live {
		return false
	}
	status, b := f.decoratedObjectTable(r, dataset, table)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return true
}

// insertObjectTable is tables.insert of an externalDataConfiguration with
// objectMetadata; false (r's body as it was) for any other.
func (f front) insertObjectTable(w http.ResponseWriter, r *http.Request, dataset string) bool {
	if f.objects == nil {
		return false
	}
	b, err := readBody(r)
	setBody(r, b)
	if err != nil {
		return false
	}
	var body struct {
		TableReference struct {
			TableID string `json:"tableId"`
		} `json:"tableReference"`
		Description  *string           `json:"description"`
		FriendlyName *string           `json:"friendlyName"`
		Labels       map[string]string `json:"labels"`
		MaxStaleness string            `json:"maxStaleness"`
		Schema       *struct {
			Fields []json.RawMessage `json:"fields"`
		} `json:"schema"`
		External *struct {
			ObjectMetadata    string          `json:"objectMetadata"`
			SourceURIs        []string        `json:"sourceUris"`
			ConnectionID      string          `json:"connectionId"`
			MetadataCacheMode string          `json:"metadataCacheMode"`
			SourceFormat      string          `json:"sourceFormat"`
			Hive              json.RawMessage `json:"hivePartitioningOptions"`
		} `json:"externalDataConfiguration"`
	}
	if json.Unmarshal(b, &body) != nil || body.External == nil || body.External.ObjectMetadata == "" {
		return false
	}
	e := body.External
	s := objectSpec{metadata: e.ObjectMetadata, uris: e.SourceURIs, connection: e.ConnectionID, cacheMode: e.MetadataCacheMode,
		description: body.Description, friendlyName: body.FriendlyName, labels: body.Labels}
	if body.Schema != nil && len(body.Schema.Fields) > 0 {
		s.invalid = "An object table has a fixed schema: tables.insert of an objectMetadata takes no schema."
	}
	if len(e.Hive) > 0 && string(e.Hive) != "null" && s.invalid == "" {
		s.invalid = objectHiveRefusal
	}
	if e.SourceFormat != "" && s.invalid == "" {
		s.invalid = "An object table takes no sourceFormat: its rows are the objects' metadata."
	}
	if body.MaxStaleness != "" {
		ms, ok := parseIntervalMs(body.MaxStaleness)
		if !ok && s.invalid == "" {
			s.invalid = fmt.Sprintf("Invalid maxStaleness %q: it is an INTERVAL, Y-M D H:M:S.", body.MaxStaleness)
		}
		s.stalenessMs = ms
	}
	table := body.TableReference.TableID
	if msg := checkTableID(table); msg != "" || table == "" {
		writeError(w, http.StatusBadRequest, "invalid", cmpOr(msg, "tableReference.tableId is required"))
		return true
	}
	_, meta, code, msg := f.createObjectTable(r, dataset, table, s)
	if code != 0 {
		reason := map[int]string{400: "invalid", 403: "accessDenied", 404: "notFound", 409: "duplicate", 501: "notImplemented"}[code]
		writeError(w, code, cmpOr(reason, "internalError"), msg)
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(meta)
	return true
}

// listedObjectTables sets the type of the object tables in a tables.list
// answer to EXTERNAL.
func (f front) listedObjectTables(dataset string, body []byte) []byte {
	if !f.objects.any() {
		return body
	}
	var list map[string]any
	if json.Unmarshal(body, &list) != nil {
		return body
	}
	tables, _ := list["tables"].([]any)
	changed := false
	f.objects.mu.Lock()
	for _, t := range tables {
		m, _ := t.(map[string]any)
		ref, _ := m["tableReference"].(map[string]any)
		ot, ok := f.objects.m[streamKey(projectOf(f.base), dataset, str(ref, "tableId"))]
		if ok && str(m, "creationTime") == ot.Created {
			m["type"] = "EXTERNAL"
			changed = true
		}
	}
	f.objects.mu.Unlock()
	if !changed {
		return body
	}
	b, err := json.Marshal(list)
	if err != nil {
		return body
	}
	return b
}

// isObjectTable reports whether dataset.table is an object table.
func (f front) isObjectTable(r *http.Request, dataset, table string) bool {
	_, live := f.objectTable(r, dataset, table)
	return live
}

// objectRowSQL is row (objectRow's) as an INSERT's VALUES tuple.
func objectRowSQL(row map[string]any) string {
	opt := func(k string) string {
		if v, ok := row[k].(string); ok {
			return sqlString(v)
		}
		return "NULL"
	}
	updated := "NULL"
	if v, ok := row["updated"].(string); ok {
		updated = "TIMESTAMP " + sqlString(v)
	}
	var md []string
	for _, m := range row["metadata"].([]map[string]string) {
		md = append(md, "("+sqlString(m["name"])+", "+sqlString(m["value"])+")")
	}
	ref := row["ref"].(map[string]any)
	details, _ := json.Marshal(ref["details"])
	return fmt.Sprintf("(%s, %d, %s, %d, %s, %s, ARRAY<STRUCT<name STRING, value STRING>>[%s], "+
		"STRUCT<uri STRING, version STRING, authorizer STRING, details JSON>(%s, %s, %s, PARSE_JSON(%s)))",
		sqlString(row["uri"].(string)), row["generation"], opt("content_type"), row["size"], opt("md5_hash"), updated,
		strings.Join(md, ", "), sqlString(ref["uri"].(string)), sqlString(ref["version"].(string)), sqlString(ref["authorizer"].(string)),
		sqlString(string(details)))
}

// refuseObjectTableJob refuses a load or copy job (srcs, a copy's
// sources) that writes dest when dest is an object table, or that copies
// an object table: "Object tables are read-only" and "You cannot copy an
// external table" (https://docs.cloud.google.com/bigquery/docs/external-tables),
// which object tables are subject to ("Object tables are subject to the
// same limitations as all other BigQuery external tables",
// object-table-introduction). The source's refusal is copyJob's of any
// external table; neither text is measured.
func (f front) refuseObjectTableJob(w http.ResponseWriter, r *http.Request, dest *tableRef, srcs []tableRef) bool {
	if !f.objects.any() {
		return false
	}
	project := projectOf(f.base)
	ours := func(t *tableRef) bool {
		return t != nil && (t.ProjectID == "" || t.ProjectID == project) && t.DatasetID != "" && t.TableID != "" &&
			!strings.ContainsAny(t.TableID, "@$") && f.isObjectTable(r, t.DatasetID, t.TableID)
	}
	for i := range srcs {
		if ours(&srcs[i]) {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("%s is not allowed for this operation because it is currently a EXTERNAL.",
				tableName(project, srcs[i])))
			return true
		}
	}
	if d := dest; d != nil {
		id := d.TableID
		if i := strings.IndexAny(id, "$@"); i > 0 {
			id = id[:i]
		}
		t := tableRef{ProjectID: d.ProjectID, DatasetID: d.DatasetID, TableID: id}
		if ours(&t) {
			writeError(w, http.StatusBadRequest, "invalid", readOnlyObjectTable(project, d.DatasetID, id))
			return true
		}
	}
	return false
}

// checkObjectTransform checks each EXTERNAL_OBJECT_TRANSFORM(TABLE t,
// transform_types) in q, as Google documents the table function
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/table-functions-built-in,
// read 2026-11-18): "This function only supports object tables as
// inputs. Subqueries or any other types of tables aren't supported",
// transform_types "is an array of STRING literals", and "the only
// supported transform_types_array value is SIGNED_URL" (400 otherwise).
// A valid call is rewritten to a subquery with signed_url
// (signedurls.go, #1786): calls are the calls, in order. 0 when q has
// none. The texts are not measured.
func (f front) checkObjectTransform(r *http.Request, q queryOptions) (calls []objectTransform, code int, msg string) {
	if !strings.Contains(strings.ToUpper(q.Query), "EXTERNAL_OBJECT_TRANSFORM") {
		return nil, 0, ""
	}
	t, lexed := lex(q.Query)
	if !lexed {
		return nil, 0, ""
	}
	const notObject = "EXTERNAL_OBJECT_TRANSFORM only supports object tables as inputs: subqueries or any other types of tables aren't supported."
	for i := 0; i < len(t); i++ {
		if !t[i].is("EXTERNAL_OBJECT_TRANSFORM") || i+1 >= len(t) || !t[i+1].punct("(") {
			continue
		}
		j := i + 2
		if j >= len(t) || !t[j].is("TABLE") {
			return nil, 400, notObject
		}
		p, next := path(t, j+1)
		ds, table, ok := tableOf(q, p)
		if !ok || len(p) == 3 && p[0] != projectOf(f.base) {
			return nil, 400, notObject
		}
		if _, live := f.objectTable(r, ds, table); !live {
			if st, got := f.get(r, tablePath(ds, table)); st != http.StatusOK {
				return nil, 404, errorMessage(got, st)
			}
			return nil, 400, notObject
		}
		if next >= len(t) || !t[next].punct(",") || next+1 >= len(t) || !t[next+1].punct("[") {
			return nil, 400, "EXTERNAL_OBJECT_TRANSFORM's second argument, transform_types_array, is an array of STRING literals."
		}
		k := next + 2
		n := 0
		for ; k < len(t) && !t[k].punct("]"); k++ {
			if t[k].punct(",") {
				continue
			}
			v, ok := stringLiteral(t[k])
			if !ok {
				return nil, 400, "EXTERNAL_OBJECT_TRANSFORM's second argument, transform_types_array, is an array of STRING literals."
			}
			if !strings.EqualFold(v, "SIGNED_URL") {
				return nil, 400, fmt.Sprintf("Invalid transform type %q for EXTERNAL_OBJECT_TRANSFORM: the only supported transform type is SIGNED_URL.", v)
			}
			n++
		}
		if n == 0 {
			return nil, 400, "EXTERNAL_OBJECT_TRANSFORM needs a transform type: the only supported transform type is SIGNED_URL."
		}
		if k+1 >= len(t) || !t[k+1].punct(")") {
			return nil, 400, "EXTERNAL_OBJECT_TRANSFORM takes two arguments: TABLE object_table_name and transform_types_array."
		}
		calls = append(calls, objectTransform{pos: t[i].pos, end: t[k+1].end, dataset: ds, table: table})
		i = k + 1
	}
	return calls, 0, ""
}
