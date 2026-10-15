package bigqueryfront

// Row access policies (#1218), which the emulator behind CloudBurrow does
// not have: the front keeps them and applies them itself.
//
//   - It keeps each table's policies (rowPolicies): made and dropped by
//     CREATE [OR REPLACE] ROW ACCESS POLICY, DROP ROW ACCESS POLICY and
//     DROP ALL ROW ACCESS POLICIES, and by the REST rowAccessPolicies
//     methods list, get, insert, update, delete and batchDelete.
//   - It filters reads of a table with policies as BigQuery documents
//     (https://cloud.google.com/bigquery/docs/row-level-security-intro): a
//     query of the table returns only the rows of the policies granted to
//     the caller, and none when no policy grants the caller. Each read of
//     the table in a FROM clause is sent as a subquery of the table with
//     the filters (filterQuery).
//   - What it cannot filter so is refused 501, never answered unfiltered:
//     tabledata.list, an extract or copy job of the table, a Storage Read
//     session, UPDATE, DELETE, MERGE and TRUNCATE of it, time travel and
//     wildcard reads of it, a view whose query reads it, and a table
//     function in a project with policies.
//
// The caller. CloudBurrow authenticates nobody (ADR-0004), so it cannot
// tell who sent a query. It takes every caller to be the one identity it
// gives out: the service account of its credentials and metadata server,
// cloudburrow-local@PROJECT.iam.gserviceaccount.com (internal/metadata,
// ServiceAccountEmail). A policy grants the caller when it names that
// account (serviceAccount:...), allAuthenticatedUsers or allUsers; any
// other grantee, user:, group: or domain:, never matches here. SESSION_USER()
// in a filter is that account.
//
// A table's policies go with the table: they are dropped when the table
// they were made on is gone or was made again (its creationTime changed),
// as BigQuery drops them with the table. They are kept in the front's state
// directory, so a restart of the front does not unfilter a table, and an
// emulator restart (empty) drops them.
//
// The IAM methods of a policy (getIamPolicy, testIamPermissions) stay 501:
// ADR-0006 stores IAM policies on four services only, BigQuery not among
// them (unserved.go).

import (
	"encoding/json"
	"errors"
	"fmt"
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

// callerIdentity is the identity CloudBurrow takes every caller to be in
// project (above): the service account its credentials present, as
// metadata.ServiceAccountEmail names it (a test holds the two together).
func callerIdentity(project string) string {
	return fmt.Sprintf("cloudburrow-local@%s.iam.gserviceaccount.com", project)
}

// grants reports whether grantee names the caller of project.
func grants(grantee, project string) bool {
	g := strings.TrimSpace(grantee)
	switch {
	case g == "allUsers" || g == "allAuthenticatedUsers":
		return true
	case strings.HasPrefix(g, "serviceAccount:"):
		return strings.EqualFold(strings.TrimPrefix(g, "serviceAccount:"), callerIdentity(project))
	}
	return false
}

// checkGrantee returns why grantee is not one BigQuery takes, or "".
func checkGrantee(grantee string) string {
	g := strings.TrimSpace(grantee)
	if g == "allUsers" || g == "allAuthenticatedUsers" {
		return ""
	}
	for _, p := range []string{"user:", "group:", "serviceAccount:", "domain:", "principal://", "principalSet://"} {
		if strings.HasPrefix(g, p) && len(g) > len(p) {
			return ""
		}
	}
	return fmt.Sprintf("Invalid grantee %q: give user:, group:, serviceAccount:, domain:, principal://, "+
		"principalSet://, allAuthenticatedUsers or allUsers.", grantee)
}

// rowPolicy is one row access policy, as the REST API gives it.
type rowPolicy struct {
	Ref struct {
		ProjectID string `json:"projectId"`
		DatasetID string `json:"datasetId"`
		TableID   string `json:"tableId"`
		PolicyID  string `json:"policyId"`
	} `json:"rowAccessPolicyReference"`
	Filter   string   `json:"filterPredicate"`
	Grantees []string `json:"grantees,omitempty"`
	Created  string   `json:"creationTime,omitempty"`
	Modified string   `json:"lastModifiedTime,omitempty"`
	Etag     string   `json:"etag,omitempty"`
}

// protectedTable is a table's policies, and the creationTime of the table
// they were made on.
type protectedTable struct {
	Key      string      `json:"key"`
	Created  string      `json:"tableCreationTime"`
	Policies []rowPolicy `json:"policies"`
}

// rowPolicies are the policies of every table, by policyKey.
type rowPolicies struct {
	mu     sync.Mutex
	tables map[string]*protectedTable
	path   string
	logf   func(string, ...any)
}

func policyKey(project, dataset, table string) string {
	return project + "\x00" + dataset + "\x00" + table
}

// rowPoliciesStateFile is the file the policies are kept in, in dir.
func rowPoliciesStateFile(dir string) string { return filepath.Join(dir, "rowpolicies.json") }

// keep restores the policies from path, and from then on keeps them there.
func (p *rowPolicies) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []protectedTable
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the row access policies it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the row access policies it kept", path)
		saved = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range saved {
		if saved[i].Key != "" && len(saved[i].Policies) > 0 {
			if p.tables == nil {
				p.tables = map[string]*protectedTable{}
			}
			t := saved[i]
			p.tables[t.Key] = &t
		}
	}
	p.path, p.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	p.saveLocked()
}

func (p *rowPolicies) saveLocked() {
	if p.path == "" {
		return
	}
	saved := make([]protectedTable, 0, len(p.tables))
	for _, t := range p.tables {
		saved = append(saved, *t)
	}
	sort.Slice(saved, func(a, b int) bool { return saved[a].Key < saved[b].Key })
	b, err := json.Marshal(saved)
	if err == nil {
		err = os.WriteFile(p.path, b, 0o600)
	}
	if err != nil {
		p.logf("bigquery front: keep the row access policies: %v", err)
	}
}

// reset drops every policy (the emulator restarted, empty).
func (p *rowPolicies) reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tables = nil
	p.saveLocked()
}

// inProject reports whether any table of project has policies.
func (p *rowPolicies) inProject(project string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.tables {
		if strings.HasPrefix(k, project+"\x00") {
			return true
		}
	}
	return false
}

// datasetTables returns the IDs of the tables of dataset with policies.
func (p *rowPolicies) datasetTables(project, dataset string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for k := range p.tables {
		parts := strings.Split(k, "\x00")
		if parts[0] == project && parts[1] == dataset {
			ids = append(ids, parts[2])
		}
	}
	return ids
}

// list returns a copy of the table's policies, and the creationTime of
// the table they were made on.
func (p *rowPolicies) list(key string) ([]rowPolicy, string) {
	if p == nil {
		return nil, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tables[key]
	if t == nil {
		return nil, ""
	}
	return append([]rowPolicy(nil), t.Policies...), t.Created
}

// drop removes the table's policies.
func (p *rowPolicies) drop(key string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.tables[key]; ok {
		delete(p.tables, key)
		p.saveLocked()
	}
}

// put stores pol on its table, made at created. replace allows an
// existing policy of its ID to be replaced; must requires one.
func (p *rowPolicies) put(pol rowPolicy, created string, replace, must bool) (rowPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := policyKey(pol.Ref.ProjectID, pol.Ref.DatasetID, pol.Ref.TableID)
	if p.tables == nil {
		p.tables = map[string]*protectedTable{}
	}
	t := p.tables[key]
	if t == nil || t.Created != created {
		t = &protectedTable{Key: key, Created: created}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	pol.Created, pol.Modified = now, now
	i := -1
	for j, old := range t.Policies {
		if old.Ref.PolicyID == pol.Ref.PolicyID {
			i = j
		}
	}
	switch {
	case i >= 0 && !replace:
		return rowPolicy{}, errPolicyExists
	case i < 0 && must:
		return rowPolicy{}, errPolicyMissing
	}
	pol.Etag = strconv.FormatInt(time.Now().UnixNano(), 36)
	if i >= 0 {
		pol.Created = t.Policies[i].Created
		t.Policies[i] = pol
	} else {
		t.Policies = append(t.Policies, pol)
	}
	p.tables[key] = t
	p.saveLocked()
	return pol, nil
}

// remove drops the policies ids of the table; each must exist unless
// ifExists.
func (p *rowPolicies) remove(key string, ids []string, ifExists bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tables[key]
	has := map[string]bool{}
	if t != nil {
		for _, pol := range t.Policies {
			has[pol.Ref.PolicyID] = true
		}
	}
	for _, id := range ids {
		if !has[id] && !ifExists {
			return fmt.Errorf("%w: %s", errPolicyMissing, id)
		}
	}
	if t == nil {
		return nil
	}
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	kept := t.Policies[:0]
	for _, pol := range t.Policies {
		if !drop[pol.Ref.PolicyID] {
			kept = append(kept, pol)
		}
	}
	t.Policies = kept
	if len(kept) == 0 {
		delete(p.tables, key)
	}
	p.saveLocked()
	return nil
}

var (
	errPolicyExists  = errors.New("row access policy already exists")
	errPolicyMissing = errors.New("row access policy not found")
)

// tableMeta is what the front reads of a table to apply policies.
type tableMeta struct {
	Type    string `json:"type"`
	Created string `json:"creationTime"`
	View    *struct {
		Query string `json:"query"`
	} `json:"view"`
	MaterializedView *struct {
		Query string `json:"query"`
	} `json:"materializedView"`
}

// tableInfo reads dataset.table of the emulator; ok is false when it is not
// there.
func (f front) tableInfo(r *http.Request, dataset, table string) (tableMeta, bool) {
	status, got := f.get(r, tablePath(dataset, table))
	var m tableMeta
	if status != http.StatusOK || json.Unmarshal(got, &m) != nil {
		return m, false
	}
	return m, true
}

// livePolicies returns the policies of dataset.table of project, dropping
// them when the table they were made on is gone (above).
func (f front) livePolicies(r *http.Request, project, dataset, table string) []rowPolicy {
	key := policyKey(project, dataset, table)
	pols, created := f.policies.list(key)
	if len(pols) == 0 {
		return nil
	}
	m, ok := f.tableInfo(r, dataset, table)
	if !ok || m.Created != created {
		f.policies.drop(key)
		return nil
	}
	return pols
}

// sessionUser is SESSION_USER() in a filter.
var sessionUser = regexp.MustCompile(`(?i)\bSESSION_USER\s*\(\s*\)`)

// policyFilter is the condition a read of a table with pols keeps rows by,
// for the caller of project: the filters of the policies granted to the
// caller, joined by OR, or FALSE when none is.
func policyFilter(pols []rowPolicy, project string) string {
	var ors []string
	for _, pol := range pols {
		for _, g := range pol.Grantees {
			if grants(g, project) {
				ors = append(ors, "("+sessionUser.ReplaceAllString(pol.Filter, strconv.Quote(callerIdentity(project)))+")")
				break
			}
		}
	}
	if len(ors) == 0 {
		return "FALSE"
	}
	return strings.Join(ors, " OR ")
}

// refusePolicies is the 501 message of a read CloudBurrow cannot filter.
func refusePolicies(what string) string {
	return "Not implemented here: " + what + " of a table with row access policies, which CloudBurrow cannot filter " +
		"by the policies. Nothing was read. Query the table instead (docs/compatibility.md)."
}

// sourcePositions returns the indexes of the tokens of stmt where a table
// read in a FROM clause starts (as statementTables finds them), and of the
// table a DML statement at its head writes, with whether it is that.
func sourcePositions(stmt []token) (sources map[int]bool, target int) {
	sources = map[int]bool{}
	target = -1
	if len(stmt) == 0 {
		return sources, target
	}
	switch {
	case stmt[0].is("UPDATE"):
		target = 1
	case stmt[0].is("DELETE") || stmt[0].is("MERGE"):
		target = 1
		if target < len(stmt) && (stmt[target].is("FROM") || stmt[target].is("INTO")) {
			target++
		}
	case stmt[0].is("TRUNCATE") && len(stmt) > 1 && stmt[1].is("TABLE"):
		target = 2
	}
	extract := make([]bool, len(stmt))
	var parens []bool
	for i, t := range stmt {
		switch {
		case t.punct("("):
			parens = append(parens, i > 0 && stmt[i-1].is("EXTRACT"))
		case t.punct(")") && len(parens) > 0:
			parens = parens[:len(parens)-1]
		}
		extract[i] = len(parens) > 0 && parens[len(parens)-1]
	}
	fromAt := map[int]bool{}
	depth := 0
	for i := 0; i < len(stmt); i++ {
		t := stmt[i]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			delete(fromAt, depth)
			depth--
		case t.is("FROM") && extract[i]:
		case t.is("FROM") && i >= 2 && stmt[i-1].is("DISTINCT") && (stmt[i-2].is("IS") || stmt[i-2].is("NOT")):
		case t.is("FROM") && i == 1 && stmt[0].is("DELETE"):
		case t.is("FROM") || t.is("JOIN"):
			fromAt[depth] = true
			sources[i+1] = true
		case t.punct(",") && fromAt[depth]:
			sources[i+1] = true
		case t.is("USING") && stmt[0].is("MERGE") && depth == 0:
			sources[i+1] = true
			delete(fromAt, depth)
		case t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("ORDER") ||
			t.is("LIMIT") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT") || t.is("ON") || t.is("USING") ||
			t.is("SELECT") || t.is("SET") || t.is("WHEN"):
			delete(fromAt, depth)
		}
	}
	return sources, target
}

// resolve returns the dataset and table a path of a query of project names,
// when it names one of project's.
func resolve(parts []string, project string) (dataset, table string, ok bool) {
	switch len(parts) {
	case 2:
		return parts[0], parts[1], true
	case 3:
		if parts[0] == project {
			return parts[1], parts[2], true
		}
	}
	return "", "", false
}

// filterQuery returns sql with each read of a table with policies sent as
// a subquery of the rows the caller may read (above), whether it changed
// it, and, when the query reads such a table in a way it cannot filter,
// why it is refused.
func (f front) filterQuery(r *http.Request, sql string) (string, bool, string) {
	project := projectOf(f.base)
	if !f.policies.inProject(project) {
		return sql, false, ""
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, false, ""
	}
	type edit struct {
		pos, end int
		text     string
	}
	var edits []edit
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		sources, target := sourcePositions(body)
		for j := 0; j < len(body); j++ {
			if body[j].kind != tokWord && body[j].kind != tokQuoted {
				continue
			}
			if body[j].kind == tokWord && reservedWords[strings.ToUpper(body[j].text)] {
				continue
			}
			parts, next := path(body, j)
			if next <= j {
				continue
			}
			dataset, table, named := resolve(parts, project)
			wildcard := named && (strings.HasSuffix(table, "*") ||
				next < len(body) && body[next].punct("*") && body[next].pos == body[next-1].end)
			if wildcard {
				prefix := strings.TrimSuffix(table, "*")
				for _, id := range f.policies.datasetTables(project, dataset) {
					if strings.HasPrefix(id, prefix) && len(f.livePolicies(r, project, dataset, id)) > 0 {
						return sql, false, refusePolicies("a wildcard read")
					}
				}
				j = next - 1
				continue
			}
			var pols []rowPolicy
			if named {
				pols = f.livePolicies(r, project, dataset, table)
			}
			switch {
			case j == target && len(pols) > 0:
				return sql, false, refusePolicies(strings.ToUpper(body[0].text))
			case sources[j] && named && next < len(body) && body[next].punct("("):
				return sql, false, refusePolicies("a table function in a project") +
					" CloudBurrow does not read a table function's body for the tables it reads."
			case sources[j] && len(pols) > 0:
				if next < len(body) && body[next].is("FOR") {
					return sql, false, refusePolicies("a time travel read (FOR SYSTEM_TIME AS OF)")
				}
				text := "(SELECT * FROM " + sql[body[j].pos:body[next-1].end] + " WHERE " + policyFilter(pols, project) + ")"
				if next >= len(body) || !body[next].is("AS") && (body[next].kind != tokWord && body[next].kind != tokQuoted ||
					body[next].kind == tokWord && reservedWords[strings.ToUpper(body[next].text)] || body[next].is("TABLESAMPLE")) {
					text += " AS " + quotePath([]string{table})
				}
				edits = append(edits, edit{body[j].pos, body[next-1].end, text})
			case sources[j] && named:
				if why := f.viewReadsProtected(r, project, dataset, table, 0); why != "" {
					return sql, false, why
				}
			}
			j = next - 1
		}
	}
	if len(edits) == 0 {
		return sql, false, ""
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(sql[last:])
	return b.String(), true, ""
}

// viewReadsProtected returns why a read of dataset.table is refused when it
// is a view whose query reads a table with policies, directly or through
// other views, or "".
func (f front) viewReadsProtected(r *http.Request, project, dataset, table string, depth int) string {
	m, ok := f.tableInfo(r, dataset, table)
	if !ok {
		return ""
	}
	q := ""
	switch {
	case m.View != nil:
		q = m.View.Query
	case m.MaterializedView != nil:
		q = m.MaterializedView.Query
	default:
		return ""
	}
	if depth > 4 {
		return refusePolicies("a read through views nested this deep, which may read a table") + ""
	}
	toks, ok := lex(q)
	if !ok {
		return ""
	}
	for j := 0; j < len(toks); j++ {
		if toks[j].kind != tokWord && toks[j].kind != tokQuoted {
			continue
		}
		parts, next := path(toks, j)
		if next <= j {
			continue
		}
		if ds, t, named := resolve(parts, project); named && (ds != dataset || t != table) {
			if len(f.livePolicies(r, project, ds, t)) > 0 {
				return refusePolicies("a read of the view " + dataset + "." + table + ", whose query reads " + ds + "." + t + ",")
			}
			if why := f.viewReadsProtected(r, project, ds, t, depth+1); why != "" {
				return why
			}
		}
		j = next - 1
	}
	return ""
}

// protectedRead returns why a read of dataset.table that the front cannot
// filter (what: tabledata.list, an extract job, ...) is refused, or "".
func (f front) protectedRead(r *http.Request, project, dataset, table, what string) string {
	if len(f.livePolicies(r, project, dataset, table)) > 0 {
		return refusePolicies(what)
	}
	return ""
}

// rowPolicyDDL is a CREATE or DROP ROW ACCESS POLICY statement.
type rowPolicyDDL struct {
	drop, all, replace, ifNotExists, ifExists bool
	id                                        string
	tablePath                                 []string
	grantees                                  []string
	filter                                    string
}

// parseRowPolicyDDL reads stmt, of the text sql, when it is a row access
// policy statement. isPolicy is false for any other statement; msg is why
// one is malformed.
func parseRowPolicyDDL(stmt []token, sql string) (d rowPolicyDDL, isPolicy bool, msg string) {
	i := 0
	at := func(words ...string) bool {
		for k, w := range words {
			if i+k >= len(stmt) || !stmt[i+k].is(w) {
				return false
			}
		}
		i += len(words)
		return true
	}
	switch {
	case at("CREATE"):
		d.replace = at("OR", "REPLACE")
		if !at("ROW", "ACCESS", "POLICY") {
			return d, false, ""
		}
		d.ifNotExists = at("IF", "NOT", "EXISTS")
	case at("DROP"):
		d.drop = true
		switch {
		case at("ALL", "ROW", "ACCESS", "POLICIES"):
			d.all = true
		case at("ROW", "ACCESS", "POLICY"):
			d.ifExists = at("IF", "EXISTS")
		default:
			return d, false, ""
		}
	default:
		return d, false, ""
	}
	syntax := func(want string) (rowPolicyDDL, bool, string) {
		return d, true, "Syntax error: expected " + want + " in the row access policy statement"
	}
	if !d.all {
		if i >= len(stmt) || stmt[i].kind != tokWord && stmt[i].kind != tokQuoted {
			return syntax("the policy's name")
		}
		d.id = stmt[i].text
		i++
	}
	if !at("ON") {
		return syntax("ON")
	}
	parts, next := path(stmt, i)
	if len(parts) == 0 {
		return syntax("the table's name")
	}
	d.tablePath, i = parts, next
	if d.drop {
		if i != len(stmt) {
			return syntax("the end of the statement")
		}
		return d, true, ""
	}
	if at("GRANT", "TO") {
		if i >= len(stmt) || !stmt[i].punct("(") {
			return syntax("( after GRANT TO")
		}
		i++
		for i < len(stmt) && !stmt[i].punct(")") {
			if stmt[i].kind != tokString {
				return syntax("a string literal grantee")
			}
			g, ok := unquoteString(stmt[i].text)
			if !ok {
				return syntax("a plain string literal grantee")
			}
			d.grantees = append(d.grantees, g)
			i++
			if i < len(stmt) && stmt[i].punct(",") {
				i++
			}
		}
		i++
	}
	if !at("FILTER", "USING") || i >= len(stmt) || !stmt[i].punct("(") {
		return syntax("FILTER USING (...)")
	}
	open, depth := i, 0
	for ; i < len(stmt); i++ {
		if stmt[i].punct("(") {
			depth++
		}
		if stmt[i].punct(")") {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if i >= len(stmt) || i != len(stmt)-1 {
		return syntax("one parenthesized filter at the end")
	}
	d.filter = strings.TrimSpace(sql[stmt[open].end:stmt[i].pos])
	if d.filter == "" {
		return syntax("a filter")
	}
	return d, true, ""
}

// unquoteString reads a '...' or "..." literal, with no prefix and no
// triple quotes.
func unquoteString(s string) (string, bool) {
	if len(s) < 2 || s[0] != s[len(s)-1] || s[0] != '\'' && s[0] != '"' || strings.HasPrefix(s, `'''`) || strings.HasPrefix(s, `"""`) {
		return "", false
	}
	inner := s[1 : len(s)-1]
	if s[0] == '\'' {
		inner = strings.ReplaceAll(inner, `\'`, `'`)
	}
	return strings.ReplaceAll(strings.ReplaceAll(inner, `\"`, `"`), `\\`, `\`), true
}

// policyStatements carries out the row access policy statements of sql
// (rewriteQuery). isPolicy is false when sql has none. A query that has
// them must have nothing else: then code is 0 when they were all carried
// out, and else the status they fail with and msg why.
func (f front) policyStatements(r *http.Request, sql, defaultDataset string) (isPolicy bool, code int, reason, msg string) {
	toks, ok := lex(sql)
	if !ok {
		return false, 0, "", ""
	}
	var stmts []rowPolicyDDL
	others := false
	for _, stmt := range splitStatements(toks) {
		if len(stmt) == 0 {
			continue
		}
		d, is, why := parseRowPolicyDDL(stmt, sql)
		if !is {
			others = true
			continue
		}
		if why != "" {
			return true, http.StatusBadRequest, "invalidQuery", why
		}
		stmts = append(stmts, d)
	}
	if len(stmts) == 0 {
		return false, 0, "", ""
	}
	if others {
		return true, http.StatusNotImplemented, "notImplemented", "Not implemented here: a row access policy statement " +
			"in a script with other statements. CloudBurrow carries out CREATE and DROP ROW ACCESS POLICY sent as queries " +
			"of their own. Nothing was run."
	}
	project := projectOf(f.base)
	for _, d := range stmts {
		parts := d.tablePath
		if len(parts) == 1 {
			if defaultDataset == "" {
				return true, http.StatusBadRequest, "invalid", fmt.Sprintf("Table %q must be qualified with a dataset (e.g. dataset.table).", parts[0])
			}
			parts = []string{defaultDataset, parts[0]}
		}
		dataset, table, named := resolve(parts, project)
		if !named {
			return true, http.StatusNotImplemented, "notImplemented", "Not implemented here: a row access policy on a " +
				"table of another project than " + project + ". Nothing was run."
		}
		m, ok := f.tableInfo(r, dataset, table)
		if !ok {
			return true, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", project, dataset, table)
		}
		key := policyKey(project, dataset, table)
		f.livePolicies(r, project, dataset, table) // drops stale ones
		switch {
		case d.all:
			f.policies.drop(key)
		case d.drop:
			if err := f.policies.remove(key, []string{d.id}, d.ifExists); err != nil {
				return true, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Row access policy %s on table %s:%s.%s", d.id, project, dataset, table)
			}
		default:
			pol := rowPolicy{Filter: d.filter, Grantees: d.grantees}
			pol.Ref.ProjectID, pol.Ref.DatasetID, pol.Ref.TableID, pol.Ref.PolicyID = project, dataset, table, d.id
			if code, reason, msg := f.checkPolicy(r, pol, m); code != 0 {
				return true, code, reason, msg
			}
			if _, err := f.policies.put(pol, m.Created, d.replace, false); err != nil {
				if d.ifNotExists {
					continue
				}
				return true, http.StatusConflict, "duplicate", fmt.Sprintf("Already Exists: Row access policy %s on table %s:%s.%s", d.id, project, dataset, table)
			}
		}
	}
	return true, 0, "", ""
}

// policyIDPattern: a policy ID has letters, digits and underscores, up to
// 256 characters (RowAccessPolicyReference.policyId).
var policyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,256}$`)

// checkPolicy returns why pol is refused on a table read as m, or a code of 0:
// its ID, its grantees and its filter, which must be a condition on the
// table's columns (checked by a query of the table that reads no rows).
func (f front) checkPolicy(r *http.Request, pol rowPolicy, m tableMeta) (int, string, string) {
	if !policyIDPattern.MatchString(pol.Ref.PolicyID) {
		return http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid row access policy ID %q: give letters, digits and underscores, up to 256 characters.", pol.Ref.PolicyID)
	}
	if m.Type == "VIEW" || m.Type == "MATERIALIZED_VIEW" || m.Type == "EXTERNAL" {
		return http.StatusBadRequest, "invalid", "Row access policies can be created on tables only, not on a " + m.Type + "."
	}
	if strings.TrimSpace(pol.Filter) == "" {
		return http.StatusBadRequest, "invalid", "Required parameter is missing: filterPredicate"
	}
	for _, g := range pol.Grantees {
		if msg := checkGrantee(g); msg != "" {
			return http.StatusBadRequest, "invalid", msg
		}
	}
	legacy := false
	probe := "SELECT 1 FROM " + quotePath([]string{pol.Ref.DatasetID, pol.Ref.TableID}) + " WHERE (" +
		sessionUser.ReplaceAllString(pol.Filter, strconv.Quote(callerIdentity(pol.Ref.ProjectID))) + ") LIMIT 0"
	req, _ := json.Marshal(queryOptions{Query: probe, UseLegacySQL: &legacy})
	status, got := f.send(r, http.MethodPost, "/queries", req)
	if status != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(got, &e)
		return http.StatusBadRequest, "invalidQuery", "Invalid row access policy filter: " + e.Error.Message
	}
	return 0, "", ""
}

// The REST methods of rowAccessPolicies.
var policiesRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/tables/([^/]+)/rowAccessPolicies(?:(:batchDelete)|/([^/:]+))?$`)

// serveRowPolicies answers the rowAccessPolicies methods list, get,
// insert, update, delete and batchDelete, and reports whether r was one.
func (f front) serveRowPolicies(w http.ResponseWriter, r *http.Request) bool {
	m := policiesRoute.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil {
		return false
	}
	var parts [4]string
	for i, s := range []string{m[1], m[2], m[3], m[5]} {
		u, err := url.PathUnescape(s)
		if err != nil {
			return false
		}
		parts[i] = u
	}
	project, dataset, table, id := parts[0], parts[1], parts[2], parts[3]
	batch := m[4] != ""
	method := r.Method
	switch {
	case batch && method != http.MethodPost,
		!batch && id == "" && method != http.MethodGet && method != http.MethodPost,
		id != "" && method != http.MethodGet && method != http.MethodPut && method != http.MethodDelete:
		return false
	}
	f.base = strings.TrimSuffix(r.URL.EscapedPath()[:strings.Index(r.URL.EscapedPath(), "/datasets/")], "/")
	meta, ok := f.tableInfo(r, dataset, table)
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s", project, dataset, table))
		return true
	}
	key := policyKey(project, dataset, table)
	pols := f.livePolicies(r, project, dataset, table)
	notFound := func(id string) {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Row access policy %s on table %s:%s.%s", id, project, dataset, table))
	}
	switch {
	case batch:
		var body struct {
			PolicyIDs []string `json:"policyIds"`
		}
		if _, ok := decode(r, &body); !ok || len(body.PolicyIDs) == 0 {
			writeError(w, http.StatusBadRequest, "invalid", "Required parameter is missing: policyIds")
			return true
		}
		if err := f.policies.remove(key, body.PolicyIDs, false); err != nil {
			notFound(strings.TrimPrefix(err.Error(), errPolicyMissing.Error()+": "))
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case id == "" && method == http.MethodGet:
		start := 0
		if t := r.URL.Query().Get("pageToken"); t != "" {
			n, err := strconv.Atoi(t)
			if err != nil || n < 0 || n > len(pols) {
				writeError(w, http.StatusBadRequest, "invalid", "Invalid page token "+t)
				return true
			}
			start = n
		}
		size := len(pols)
		if s := r.URL.Query().Get("pageSize"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				size = n
			}
		}
		end := min(start+size, len(pols))
		resp := map[string]any{"rowAccessPolicies": pols[start:end]}
		if end < len(pols) {
			resp["nextPageToken"] = strconv.Itoa(end)
		}
		writeJSON(w, http.StatusOK, resp)
	case method == http.MethodGet:
		for _, p := range pols {
			if p.Ref.PolicyID == id {
				writeJSON(w, http.StatusOK, p)
				return true
			}
		}
		notFound(id)
	case method == http.MethodDelete:
		if err := f.policies.remove(key, []string{id}, false); err != nil {
			notFound(id)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default: // insert, update
		var pol rowPolicy
		if _, ok := decode(r, &pol); !ok {
			writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the row access policy")
			return true
		}
		if id == "" {
			id = pol.Ref.PolicyID
		}
		if pol.Ref.PolicyID != "" && pol.Ref.PolicyID != id ||
			pol.Ref.ProjectID != "" && pol.Ref.ProjectID != project ||
			pol.Ref.DatasetID != "" && pol.Ref.DatasetID != dataset ||
			pol.Ref.TableID != "" && pol.Ref.TableID != table {
			writeError(w, http.StatusBadRequest, "invalid", "The rowAccessPolicyReference does not name the table and policy of the request's path.")
			return true
		}
		pol.Ref.ProjectID, pol.Ref.DatasetID, pol.Ref.TableID, pol.Ref.PolicyID = project, dataset, table, id
		if code, reason, msg := f.checkPolicy(r, pol, meta); code != 0 {
			writeError(w, code, reason, msg)
			return true
		}
		update := method == http.MethodPut
		saved, err := f.policies.put(pol, meta.Created, update, update)
		switch {
		case errors.Is(err, errPolicyExists):
			writeError(w, http.StatusConflict, "duplicate", fmt.Sprintf("Already Exists: Row access policy %s on table %s:%s.%s", id, project, dataset, table))
		case errors.Is(err, errPolicyMissing):
			notFound(id)
		default:
			writeJSON(w, http.StatusOK, saved)
		}
	}
	return true
}
