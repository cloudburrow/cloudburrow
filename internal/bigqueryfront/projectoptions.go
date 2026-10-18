package bigqueryfront

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Project and organization configurations (#1375, #1417).
//
// BigQuery sets them with
//
//	ALTER ORGANIZATION SET OPTIONS (`default_location` = 'LOCATION');
//	ALTER PROJECT [PROJECT_ID] SET OPTIONS (`region-us.default_time_zone` = 'America/Chicago');
//
// default_location is global, written without a prefix; every other
// configuration is per region, `region-REGION.NAME`. A statement sets
// global options or one region's, never both nor two regions; NULL clears
// one. A project's own setting wins over its organization's, which wins
// over BigQuery's default
// (https://cloud.google.com/bigquery/docs/default-configuration). The
// instance's organization is Resource Manager's one local organization
// (#1294), organizations/100000000001.
//
// The front keeps them in a file on its state directory, as it keeps the
// IAM policies (iam.go), dropped when the emulator restarts empty. They
// take effect at once (BigQuery's "can take up to 10 minutes"):
//
//   - default_location: the location of datasets.insert and CREATE SCHEMA
//     with none, and of every job that names none (location.go).
//   - default_time_zone: the zone of the time functions a query calls
//     without one (timezone.go), for jobs in that region.
//   - default_kms_key_name: the encryptionConfiguration of a table made
//     with tables.insert or a lone CREATE TABLE (#1456), in a dataset of
//     that region with no defaultEncryptionConfiguration and given none
//     itself (projectdefaults.go).
//   - default_query_job_timeout_ms: the timeout of a query job that gives
//     no jobTimeoutMs (#1456, jobtimeout.go).
//   - default_max_time_travel_hours and default_storage_billing_model:
//     those of a dataset made with datasets.insert in that region, given
//     none itself ("applies to new datasets only").
//   - default_sql_dialect_option 'only_google_sql': a legacy SQL query in
//     that region fails 400.
//
// Stored, and read back, but with nothing to bound: the queue timeouts
// (default_interactive_query_queue_timeout_ms,
// default_batch_query_queue_timeout_ms; an instance queues no job). With
// no effect: the rest of the documented options,
// which name reservations, connections, data policies or billing an
// instance has none of.
//
// In a script of several statements, an ALTER PROJECT or ALTER
// ORGANIZATION at the script's top level is carried out as the script
// starts, and the emulator runs a statement that does nothing in its
// place; one inside a block, IF or loop is carried out in its turn, as a
// statement of its own, by the front's run of the script (#1459), as are
// the script's others then; a dry run stops before an IF, FOR or WHILE
// (scriptdrystop.go, #1456), and one in a block it reaches is 501.
//
// `region-REGION`.INFORMATION_SCHEMA.PROJECT_OPTIONS has a row per option
// set on the project ("that differs from the default value"): the global
// ones and that region's, named without the prefix;
// ORGANIZATION_OPTIONS the organization's so; and
// EFFECTIVE_PROJECT_OPTIONS each option in effect, with option_set_level
// PROJECTS or ORGANIZATIONS and option_set_on_id the project ID or the
// organization's number. Whether BigQuery also gives a DEFAULT row for each
// option left unset is not documented, so no view does. option_description
// is the option table's text; project_number is NULL (an instance has
// none).

// orgNumber is the instance's organization, Resource Manager's
// OrganizationNumber (internal/service/resourcemanager/hierarchy.go).
const orgNumber = "100000000001"

// orgScope is the organization's key in projectOptions.opts.
const orgScope = "organizations/" + orgNumber

// optionSpec describes a configuration BigQuery documents.
type optionSpec struct {
	typ         string // STRING, INT64, BOOL or ARRAY<STRING>, option_type
	description string
	global      bool // set without a region prefix (default_location)
	projectOnly bool
	orgOnly     bool
	// check returns why value is refused, "" when it is accepted.
	check func(value string) string
}

func oneOf(name string, allowed ...string) func(string) string {
	return func(v string) string {
		for _, a := range allowed {
			if strings.EqualFold(v, a) {
				return ""
			}
		}
		return "Invalid value for " + name + ": '" + v + "'. Expected one of: " + strings.Join(allowed, ", ")
	}
}

func msRange(name string, lo, hi int64, offOK bool) func(string) string {
	return func(v string) string {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil && (offOK && n == -1 || n >= lo && n <= hi) {
			return ""
		}
		return "Invalid value for " + name + ": " + v + ". It must be between " + strconv.FormatInt(lo, 10) +
			" and " + strconv.FormatInt(hi, 10) + " milliseconds" + map[bool]string{true: ", or -1", false: ""}[offOK]
	}
}

const hours48 = 48 * 3600 * 1000

// projectOptionSpecs are the configurations the default-configuration page
// documents, by name.
var projectOptionSpecs = map[string]optionSpec{
	"default_location": {typ: "STRING", description: "Default location for the project", global: true},
	"default_time_zone": {typ: "STRING", description: "Default time zone for the project", check: func(v string) string {
		if validTimeZone(v) {
			return ""
		}
		return "Invalid time zone: " + v
	}},
	"default_kms_key_name": {typ: "STRING", description: "Default key name for the project", check: func(v string) string {
		p := strings.Split(v, "/")
		if len(p) == 8 && p[0] == "projects" && p[2] == "locations" && p[4] == "keyRings" && p[6] == "cryptoKeys" {
			return ""
		}
		return "Invalid value for default_kms_key_name: '" + v +
			"'. Expected projects/PROJECT/locations/LOCATION/keyRings/RING/cryptoKeys/KEY"
	}},
	"default_query_job_timeout_ms":               {typ: "INT64", description: "Default query timeout, in milliseconds", check: msRange("default_query_job_timeout_ms", 5*60*1000, hours48, false)},
	"default_interactive_query_queue_timeout_ms": {typ: "INT64", description: "Default timeout, in milliseconds, for queued interactive queries", check: msRange("default_interactive_query_queue_timeout_ms", 1, hours48, true)},
	"default_batch_query_queue_timeout_ms":       {typ: "INT64", description: "Default timeout, in milliseconds, for queued batch queries", check: msRange("default_batch_query_queue_timeout_ms", 1, hours48, true)},
	"default_query_optimizer_options":            {typ: "STRING", description: "Default query optimizer options", check: oneOf("default_query_optimizer_options", "adaptive=on", "adaptive=off")},
	"default_sql_dialect_option":                 {typ: "STRING", description: "Default SQL dialect", check: oneOf("default_sql_dialect_option", "default_legacy_sql", "default_google_sql", "only_google_sql")},
	"reusable_column_aliases_mode":               {typ: "STRING", description: "Whether reusable column aliases are enabled"},
	"default_column_name_character_map":          {typ: "STRING", description: "Default column name character map", check: oneOf("default_column_name_character_map", "STRICT", "V1", "V2")},
	"default_max_time_travel_hours": {typ: "INT64", description: "Default time travel window, in hours, of new datasets", check: func(v string) string {
		if n, err := strconv.Atoi(v); err == nil && n >= 48 && n <= 168 && n%24 == 0 {
			return ""
		}
		return "Invalid value for default_max_time_travel_hours: " + v + ". It must be a multiple of 24 between 48 and 168"
	}},
	"default_storage_billing_model":            {typ: "STRING", description: "Default storage billing model of new datasets", check: oneOf("default_storage_billing_model", "PHYSICAL", "LOGICAL")},
	"default_cloud_resource_connection_id":     {typ: "STRING", description: "Default Cloud resource connection"},
	"default_data_policy_projects":             {typ: "ARRAY<STRING>", description: "Projects whose data policies are evaluated", orgOnly: true},
	"enable_reservation_based_fairness":        {typ: "BOOL", description: "Whether idle slots are shared by reservation", projectOnly: true},
	"enable_global_queries_execution":          {typ: "BOOL", description: "Whether global queries can run"},
	"enable_global_queries_data_access":        {typ: "BOOL", description: "Whether global queries can read data stored in the region"},
	"enable_info_schema_storage":               {typ: "BOOL", description: "Whether the storage views are enabled"},
	"reservation_override_mode":                {typ: "STRING", description: "How a query's reservation can be overridden"},
	"disable_on_demand_billing":                {typ: "BOOL", description: "Whether queries must use a reservation"},
	"preflight_fluid_autoscaling_reservations": {typ: "ARRAY<STRING>", description: "Reservations opted into fluid autoscaling"},
}

// projectOptions are the options ALTER PROJECT and ALTER ORGANIZATION set,
// by scope (a project ID, or orgScope) and key (the option's name, after
// `region-REGION.` for a regional one, the region lower case).
type projectOptions struct {
	mu   sync.Mutex
	opts map[string]map[string]string
	path string
	logf func(string, ...any)
	// googleDefault are the query jobs, "project/jobID", that asked no
	// dialect when their region's default_sql_dialect_option made
	// GoogleSQL the default: JOBS' query_dialect DEFAULT_GOOGLE_SQL
	// (#1595, infoschemajobs.go). Not kept across restarts.
	googleDefault map[string]bool
}

// maxGoogleDefault bounds googleDefault; past it the oldest are not
// told apart (the set starts again).
const maxGoogleDefault = 100000

// markGoogleDefault records that project's job id defaulted to GoogleSQL.
func (p *projectOptions) markGoogleDefault(project, id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.googleDefault == nil || len(p.googleDefault) >= maxGoogleDefault {
		p.googleDefault = map[string]bool{}
	}
	p.googleDefault[project+"/"+id] = true
}

// googleDefaulted reports whether markGoogleDefault recorded the job.
func (p *projectOptions) googleDefaulted(project, id string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.googleDefault[project+"/"+id]
}

// projectOptionsStateFile is the file the options are kept in, in dir.
func projectOptionsStateFile(dir string) string { return filepath.Join(dir, "projectoptions.json") }

// keep restores the options from path, and from then on keeps them there.
func (p *projectOptions) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved map[string]map[string]string
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the project options it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the project options it kept", path)
		saved = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts, p.path, p.logf = saved, path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	p.saveLocked()
}

func (p *projectOptions) saveLocked() {
	if p.path == "" {
		return
	}
	b, err := json.Marshal(p.opts)
	if err == nil {
		err = os.WriteFile(p.path, b, 0o600)
	}
	if err != nil && p.logf != nil {
		p.logf("bigquery front: keep the project options: %v", err)
	}
}

// reset drops every option (the emulator restarted, empty).
func (p *projectOptions) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = nil
	p.saveLocked()
}

// set sets scope's option key to value, or clears it when value is nil.
func (p *projectOptions) set(scope, key string, value *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value == nil {
		delete(p.opts[scope], key)
	} else {
		if p.opts == nil {
			p.opts = map[string]map[string]string{}
		}
		if p.opts[scope] == nil {
			p.opts[scope] = map[string]string{}
		}
		p.opts[scope][key] = *value
	}
	p.saveLocked()
}

// optionRow is one option as a view shows it.
type optionRow struct{ name, value, level, on string }

// list returns scope's options in region (lower case): the global ones and
// region's, named without the prefix, sorted by name.
func (p *projectOptions) list(scope, region string) []optionRow {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []optionRow
	for k, v := range p.opts[scope] {
		name, ok := k, !strings.Contains(k, ".")
		if !ok && strings.HasPrefix(k, "region-"+region+".") {
			name, ok = strings.TrimPrefix(k, "region-"+region+"."), true
		}
		if ok {
			out = append(out, optionRow{name: name, value: v})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

// effectiveList is project's options in effect in region: its own, and its
// organization's it does not set.
func (p *projectOptions) effectiveList(project, region string) []optionRow {
	own := p.list(project, region)
	seen := map[string]bool{}
	for i := range own {
		own[i].level, own[i].on = "PROJECTS", project
		seen[own[i].name] = true
	}
	for _, o := range p.list(orgScope, region) {
		if !seen[o.name] {
			o.level, o.on = "ORGANIZATIONS", orgNumber
			own = append(own, o)
		}
	}
	sort.Slice(own, func(a, b int) bool { return own[a].name < own[b].name })
	return own
}

// effective is project's option name in region (any case; ignored for a
// global option): the project's, else its organization's, else "".
func (p *projectOptions) effective(project, region, name string) string {
	if p == nil {
		return ""
	}
	key := name
	if !projectOptionSpecs[name].global {
		key = "region-" + strings.ToLower(region) + "." + name
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.opts[project][key]; ok {
		return v
	}
	return p.opts[orgScope][key]
}

// defaultLocation is project's default location: its default_location,
// else its organization's, else BigQuery's, US.
func (p *projectOptions) defaultLocation(project string) string {
	if v := p.effective(project, "", "default_location"); v != "" {
		return v
	}
	return defaultLocation
}

// nestedAlterProject reports whether sql, a script, has an ALTER PROJECT
// or ALTER ORGANIZATION inside a block, IF or loop, and no statement
// parseAlterProject refuses at its top level.
func nestedAlterProject(sql string) bool {
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	top := map[int]bool{}
	for _, st := range splitStatements(trimSemicolons(toks)) {
		if len(st) >= 2 && st[0].is("ALTER") && (st[1].is("PROJECT") || st[1].is("ORGANIZATION")) {
			top[st[0].pos] = true
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].is("ALTER") && (toks[i+1].is("PROJECT") || toks[i+1].is("ORGANIZATION")) && !top[toks[i].pos] {
			return hasControlFlow(sql)
		}
	}
	return false
}

// alterOptions is a parsed ALTER PROJECT or ALTER ORGANIZATION ... SET
// OPTIONS statement.
type alterOptions struct {
	// scope is the project ID, or orgScope.
	scope string
	// sets are the options it sets, by key, a nil value clearing one.
	sets map[string]*string
}

// parseAlterProject reads sql when its first statement is ALTER PROJECT or
// ALTER ORGANIZATION. ok is false for any other query; msg is why it is
// refused 501, and bad (with msg) a statement BigQuery fails 400. In a
// script of several statements, each top-level ALTER PROJECT or ALTER
// ORGANIZATION is read (all) and its byte range given (spans), to be
// replaced; spans is nil for a lone statement. ok is true when there is
// one.
func parseAlterProject(sql, project string) (all []alterOptions, spans [][2]int, msg string, bad, ok bool) {
	if !strings.Contains(strings.ToUpper(sql), "ALTER") {
		return nil, nil, "", false, false
	}
	toks, lexed := lex(sql)
	if !lexed {
		return nil, nil, "", false, false
	}
	stmts := splitStatements(trimSemicolons(toks))
	top := map[int]bool{}
	for _, st := range stmts {
		if len(st) >= 2 && st[0].is("ALTER") && (st[1].is("PROJECT") || st[1].is("ORGANIZATION")) {
			top[st[0].pos] = true
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].is("ALTER") && (toks[i+1].is("PROJECT") || toks[i+1].is("ORGANIZATION")) && !top[toks[i].pos] {
			return nil, nil, "ALTER " + strings.ToUpper(toks[i+1].text) + " inside a block, IF or loop of a script; " +
				"CloudBurrow carries out one at the script's top level", false, true
		}
	}
	if len(top) == 0 {
		return nil, nil, "", false, false
	}
	for _, st := range stmts {
		if len(st) < 2 || !st[0].is("ALTER") || !st[1].is("PROJECT") && !st[1].is("ORGANIZATION") {
			continue
		}
		a, m, isBad := parseAlterStatement(st, project)
		if m != "" {
			return nil, nil, m, isBad, true
		}
		all = append(all, a)
		spans = append(spans, [2]int{st[0].pos, st[len(st)-1].end})
	}
	if len(stmts) == 1 {
		spans = nil // a lone statement
	}
	return all, spans, "", false, true
}

// parseAlterStatement reads one ALTER PROJECT or ALTER ORGANIZATION
// statement, t.
func parseAlterStatement(t []token, project string) (a alterOptions, msg string, bad bool) {
	org := t[1].is("ORGANIZATION")
	what := "ALTER PROJECT"
	a.scope, a.sets = project, map[string]*string{}
	if org {
		what, a.scope = "ALTER ORGANIZATION", orgScope
	}
	i := 2
	if !org && i < len(t) && !t[i].is("SET") {
		parts, next := path(t, i)
		if len(parts) != 1 {
			return a, "Syntax error: expected a project ID after ALTER PROJECT", true
		}
		a.scope, i = parts[0], next
	}
	if i+2 >= len(t) || !t[i].is("SET") || !t[i+1].is("OPTIONS") || !t[i+2].punct("(") {
		return a, "Syntax error: expected SET OPTIONS (...) in " + what, true
	}
	i += 3
	region, global := "", false
	for i < len(t) && !t[i].punct(")") {
		if t[i].kind != tokWord && t[i].kind != tokQuoted {
			return a, "Syntax error: expected an option name in " + what + " SET OPTIONS", true
		}
		parts, next := path(t, i)
		i = next
		if i+1 >= len(t) || !t[i].punct("=") {
			return a, "Syntax error: expected = after an option name in " + what + " SET OPTIONS", true
		}
		i++
		full := strings.Join(parts, ".")
		name := strings.ToLower(parts[len(parts)-1])
		spec, known := projectOptionSpecs[name]
		var r string
		switch {
		case len(parts) == 2 && strings.HasPrefix(strings.ToLower(parts[0]), "region-") && len(parts[0]) > len("region-"):
			r = strings.ToLower(strings.TrimPrefix(strings.ToLower(parts[0]), "region-"))
		case len(parts) != 1:
			known = false
		}
		switch {
		case !known:
			return a, "Invalid option name: " + full + " in " + what + " SET OPTIONS", true
		case spec.global && r != "":
			return a, "Option " + name + " is a global option and must not be qualified with a region", true
		case !spec.global && r == "":
			return a, "Option " + name + " is a regional option and must be qualified with a region, as `region-REGION." + name + "`", true
		case org && spec.projectOnly:
			return a, "Option " + name + " can only be set on a project", true
		case !org && spec.orgOnly:
			return a, "Option " + name + " can only be set on an organization", true
		}
		if spec.global {
			global = true
		} else if region == "" {
			region = r
		} else if region != r {
			return a, "Options of more than one region cannot be set in one statement", true
		}
		if global && region != "" {
			return a, "Global and regional options cannot be set in one statement", true
		}
		key := name
		if r != "" {
			key = "region-" + r + "." + name
		}
		v, end, m := optionValue(t, i, name, spec)
		if m != "" {
			return a, m, true
		}
		i = end
		if v != nil && name == "default_location" {
			loc := canonLocation(*v)
			v = &loc
		}
		a.sets[key] = v
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	if i >= len(t) || i != len(t)-1 {
		return a, "Syntax error: unexpected text after " + what + " SET OPTIONS (...)", true
	}
	return a, "", false
}

// optionValue reads the value at t[i] of option name: NULL (nil), or a
// literal of its type, written as the views show it.
func optionValue(t []token, i int, name string, spec optionSpec) (v *string, next int, msg string) {
	if i >= len(t) {
		return nil, i, "Syntax error: expected a value for " + name
	}
	if t[i].is("NULL") {
		return nil, i + 1, ""
	}
	want := "Invalid value for " + name + ": expected a literal of type " + spec.typ + ", or NULL"
	var s string
	switch spec.typ {
	case "STRING":
		str, ok := stringLiteral(t[i])
		if !ok || str == "" {
			return nil, i, want
		}
		s, i = str, i+1
	case "INT64":
		neg := t[i].punct("-")
		if neg {
			i++
		}
		if i >= len(t) || t[i].kind != tokWord {
			return nil, i, want
		}
		n, err := strconv.ParseInt(t[i].text, 10, 64)
		if err != nil {
			return nil, i, want
		}
		if neg {
			n = -n
		}
		s, i = strconv.FormatInt(n, 10), i+1
	case "BOOL":
		if !t[i].is("TRUE") && !t[i].is("FALSE") {
			return nil, i, want
		}
		s, i = strings.ToLower(t[i].text), i+1
	case "ARRAY<STRING>":
		if t[i].is("ARRAY") {
			i++
			if i+2 < len(t) && t[i].punct("<") && t[i+1].is("STRING") && t[i+2].punct(">") {
				i += 3
			}
		}
		if i >= len(t) || !t[i].punct("[") {
			return nil, i, want
		}
		i++
		var items []string
		for i < len(t) && !t[i].punct("]") {
			str, ok := stringLiteral(t[i])
			if !ok {
				return nil, i, want
			}
			items = append(items, str)
			i++
			if i < len(t) && t[i].punct(",") {
				i++
			}
		}
		if i >= len(t) {
			return nil, i, want
		}
		b, _ := json.Marshal(items)
		s, i = string(b), i+1
	}
	if spec.check != nil {
		if m := spec.check(s); m != "" {
			return nil, i, m
		}
	}
	return &s, i, ""
}

// alterProjectOptions answers a query whose statement is ALTER PROJECT or
// ALTER ORGANIZATION, or a script with one at its top level (above), and
// reports whether it was one. The options are set; a lone statement is run
// as a statement that does nothing, keeping the client's text, and in a
// script each is replaced by one, and the script then served as any other.
func (f front) alterProjectOptions(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	all, spans, msg, bad, ok := parseAlterProject(q.Query, projectOf(f.base))
	if !ok {
		return false
	}
	if bad {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: msg})
		return true
	}
	if msg != "" && nestedAlterProject(q.Query) && !dryRun(r, insert) && !txnRequest(r) && f.settings != nil {
		// #1459: one inside a block, IF or loop is carried out in its
		// turn by the front's run of the script (scriptflow.go), each a
		// lone statement and child job of its own.
		return false
	}
	if msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+msg+". Nothing was run.")
		return true
	}
	if f.settings == nil {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER PROJECT. Nothing was run.")
		return true
	}
	if !dryRun(r, insert) {
		for _, a := range all {
			for key, v := range a.sets {
				f.settings.set(a.scope, key, v)
			}
		}
	}
	noop := "DROP TABLE IF EXISTS " + quotePath([]string{"_cloudburrow", scratchTable()})
	text := noop
	if spans != nil {
		var b strings.Builder
		last := 0
		for _, s := range spans {
			b.WriteString(q.Query[last:s[0]])
			b.WriteString(noop)
			last = s[1]
		}
		b.WriteString(q.Query[last:])
		text = b.String()
		if !setQueryText(r, insert, text) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return true
		}
		client := jobText{query: q.Query}
		q.Query = text
		rec := newRecorder()
		f.runQuery(rec, r, q, insert)
		f.clientText(w, rec, client)
		return true
	}
	if !setQueryText(r, insert, text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	// The job's statementType is the one the REST reference lists for the
	// statement (ALTER_PROJECT, ALTER_ORGANIZATION; #1595), not the no-op's,
	// and the no-op's DDL target is left out: the statement has none.
	st := "ALTER_PROJECT"
	if len(all) == 1 && all[0].scope == orgScope {
		st = "ALTER_ORGANIZATION"
	}
	f.forward(w, r, jobText{query: q.Query, ddl: &ownDDL{statementType: st}})
	return true
}

// onlyGoogleSQL reports whether the job's region has
// default_sql_dialect_option only_google_sql, which "rejects legacy SQL".
func (f front) onlyGoogleSQL(r *http.Request, insert bool) bool {
	return strings.EqualFold(f.dialectOption(r, insert), "only_google_sql")
}

// googleSQLDefault reports whether a query asking no dialect is GoogleSQL
// by its region's default_sql_dialect_option (default_google_sql or
// only_google_sql) rather than legacy SQL, BigQuery's default.
func (f front) googleSQLDefault(r *http.Request, insert bool) bool {
	switch strings.ToLower(f.dialectOption(r, insert)) {
	case "default_google_sql", "only_google_sql":
		return true
	}
	return false
}

// dialectOption is the job's region's default_sql_dialect_option, or "".
func (f front) dialectOption(r *http.Request, insert bool) string {
	if f.settings == nil || !f.settings.hasAny("default_sql_dialect_option") {
		return ""
	}
	loc := queryLocation(r, insert)
	if loc == "" {
		loc = f.settings.defaultLocation(projectOf(f.base))
	}
	return f.settings.effective(projectOf(f.base), loc, "default_sql_dialect_option")
}

// Project option views (#1375, #1417): region-qualified, like the job views.
const projectOptionsColumns = "STRUCT<option_name STRING, project_id STRING, project_number INT64, " +
	"option_description STRING, option_type STRING, option_value STRING>"

const effectiveProjectOptionsColumns = "STRUCT<project_id STRING, project_number INT64, option_name STRING, " +
	"option_description STRING, option_type STRING, option_set_level STRING, option_set_on_id STRING, option_value STRING>"

const organizationOptionsColumns = "STRUCT<option_name STRING, option_description STRING, option_type STRING, option_value STRING>"

func optionSQL(name string) (description, typ string) {
	s := projectOptionSpecs[name]
	return sqlString(s.description), sqlString(s.typ)
}

// projectViews are the region-qualified views of the project itself, not
// of its datasets (infoschemajobs.go reads them), given the query's
// region, lower case.
var projectViews = map[string]func(front, string) string{
	"PROJECT_OPTIONS": func(f front, region string) string {
		project := projectOf(f.base)
		var rows [][]string
		for _, o := range f.settings.list(project, region) {
			d, typ := optionSQL(o.name)
			rows = append(rows, []string{sqlString(o.name), sqlString(project), "NULL", d, typ, sqlString(o.value)})
		}
		return rowsSubquery(projectOptionsColumns, rows)
	},
	"EFFECTIVE_PROJECT_OPTIONS": func(f front, region string) string {
		project := projectOf(f.base)
		var rows [][]string
		for _, o := range f.settings.effectiveList(project, region) {
			d, typ := optionSQL(o.name)
			rows = append(rows, []string{sqlString(project), "NULL", sqlString(o.name), d, typ,
				sqlString(o.level), sqlString(o.on), sqlString(o.value)})
		}
		return rowsSubquery(effectiveProjectOptionsColumns, rows)
	},
	"ORGANIZATION_OPTIONS": func(f front, region string) string {
		var rows [][]string
		for _, o := range f.settings.list(orgScope, region) {
			d, typ := optionSQL(o.name)
			rows = append(rows, []string{sqlString(o.name), d, typ, sqlString(o.value)})
		}
		return rowsSubquery(organizationOptionsColumns, rows)
	},
}

func init() {
	for view := range projectViews {
		regionViews[view] = true // read region-qualified (informationSchemaJobs)
	}
}

// inRegion is build for the datasets in region (a location, any case):
// any other dataset is left out of a region-qualified view (#1375), as
// BigQuery's lists "only datasets in that region".
func inRegion(region string, build func(front, *http.Request, string, *datasetMeta) (string, int, string)) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
		if dataset == resultsDataset {
			return "", http.StatusNotFound, "" // the front's own (results.go); regionRows leaves it out
		}
		if loc := f.datasetLocation(r, dataset); loc != "" && !strings.EqualFold(loc, region) {
			return "", http.StatusNotFound, "" // left out, as regionRows leaves out a dataset deleted meanwhile
		}
		return build(f, r, dataset, m)
	}
}

// canonLocation writes a location as BigQuery names it: the multi-regions
// upper case (US, EU), a region lower case (us-central1).
func canonLocation(s string) string {
	if strings.EqualFold(s, "us") || strings.EqualFold(s, "eu") {
		return strings.ToUpper(s)
	}
	return strings.ToLower(s)
}
