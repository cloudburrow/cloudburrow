package bigqueryfront

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Per-project IAM of a caller the instance minted a token for (#1767).
// "Datasets, tables and jobs belong to a project", and BigQuery checks
// each request's permissions where they are: a job's bigquery.jobs.create
// on the job's project, and the data's own permissions
// (bigquery.tables.getData, bigquery.tables.updateData, ...) on the
// table's project and dataset (https://cloud.google.com/bigquery/docs/access-control,
// https://cloud.google.com/bigquery/docs/resource-hierarchy, re-read
// 2026-11-17).
//
// The caller is known the way Memorystore's IAM auth, Firestore with
// MongoDB compatibility and IAM Credentials know it: a bearer token the
// instance minted (internal/metadata's issued.go), matched by its SHA-256
// against the live tokens the host pushes here (PUT BigQueryIAMPath,
// every few seconds and as soon as a token is minted), never by asking
// Google. The host leaves out the tokens of the instance's own account,
// the ADC fixture's. With such a token, the account's roles on each project (the
// project's IAM policy, which the host gives with it) and on each dataset
// (its access entries, userByEmail or iamMember, read from the dataset)
// must grant the permission, or the request is 403 accessDenied with
// BigQuery's wording. A request with no bearer token, or one the instance
// did not mint (the ADC fixture's, a client's WithoutAuthentication), is
// served as before, as the instance's ADC account, which holds every
// permission: CloudBurrow has no other way to tell who sent it, and every
// client, test and console page that does not mint a token keeps working.
//
// Checked: jobs.insert and jobs.query (bigquery.jobs.create on the job's
// project; a load's or a query's destination, bigquery.tables.updateData;
// a copy's sources and a query's tables named after FROM or JOIN,
// bigquery.tables.getData; an extract's source, bigquery.tables.export);
// datasets.insert/get/patch/update/delete; tables.list/insert/get/patch/
// update/delete; tabledata.list and insertAll. Not checked: jobs.get,
// jobs.list and getQueryResults (a caller reads its own jobs), routines,
// models, row access policies, a table's own IAM policy, IAM conditions,
// and the tables a view or a routine reads.

// BigQueryIAMPath is where the host pushes BigQueryIAM to the front.
const BigQueryIAMPath = "/cloudburrow/v1/bigqueryIAM"

// BigQueryIAM is what the check reads: the live tokens this instance
// minted, each account's roles by project, and the custom roles' BigQuery
// permissions.
type BigQueryIAM struct {
	Tokens      []BigQueryIAMToken             `json:"tokens"`
	Roles       map[string]map[string][]string `json:"roles"`
	CustomRoles map[string][]string            `json:"customRoles,omitempty"`
}

// BigQueryIAMToken is a minted access token: its SHA-256 in hex, its
// account and its expiry.
type BigQueryIAMToken struct {
	Hash    string    `json:"hash"`
	Email   string    `json:"email"`
	Expires time.Time `json:"expires"`
}

// BigQueryRoles are the predefined roles that grant BigQuery permissions,
// with the ones the checks above read, from Google's BigQuery IAM roles
// (https://cloud.google.com/bigquery/docs/access-control#bigquery-roles,
// re-read 2026-11-17).
var BigQueryRoles = map[string][]string{
	"roles/owner":                   {"bigquery.*"},
	"roles/editor":                  {"bigquery.*"},
	"roles/bigquery.admin":          {"bigquery.*"},
	"roles/viewer":                  {"bigquery.datasets.get", "bigquery.tables.get", "bigquery.tables.list", "bigquery.tables.getData", "bigquery.tables.export", "bigquery.jobs.list"},
	"roles/bigquery.dataOwner":      {"bigquery.datasets.*", "bigquery.tables.*", "bigquery.routines.*", "bigquery.models.*"},
	"roles/bigquery.dataEditor":     {"bigquery.datasets.create", "bigquery.datasets.get", "bigquery.tables.*", "bigquery.routines.*", "bigquery.models.*"},
	"roles/bigquery.dataViewer":     {"bigquery.datasets.get", "bigquery.tables.get", "bigquery.tables.getData", "bigquery.tables.list", "bigquery.tables.export"},
	"roles/bigquery.metadataViewer": {"bigquery.datasets.get", "bigquery.tables.get", "bigquery.tables.list"},
	"roles/bigquery.user":           {"bigquery.jobs.create", "bigquery.jobs.list", "bigquery.datasets.create", "bigquery.tables.list", "bigquery.readsessions.*"},
	"roles/bigquery.jobUser":        {"bigquery.jobs.create"},
}

// datasetAccessRoles are a dataset access entry's basic roles, as
// BigQuery maps them ("READER ... roles/bigquery.dataViewer", WRITER
// dataEditor, OWNER dataOwner).
var datasetAccessRoles = map[string]string{
	"READER": "roles/bigquery.dataViewer",
	"WRITER": "roles/bigquery.dataEditor",
	"OWNER":  "roles/bigquery.dataOwner",
}

type iamState struct {
	mu  sync.Mutex
	iam *BigQueryIAM
	now func() time.Time
}

func (s *iamState) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT a BigQueryIAM", http.StatusMethodNotAllowed)
		return
	}
	var m BigQueryIAM
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.iam = &m
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// caller is the account of r's bearer token when the instance minted it
// and it is live, else "".
func (s *iamState) caller(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	h := hex.EncodeToString(sum[:])
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.iam == nil {
		return ""
	}
	for _, t := range s.iam.Tokens {
		if t.Hash == h && t.Email != "" && now.Before(t.Expires) {
			return strings.ToLower(t.Email)
		}
	}
	return ""
}

// grantsPerm is whether one of perms grants p.
func grantsPerm(perms []string, p string) bool {
	for _, g := range perms {
		if g == "*" || g == p || strings.HasSuffix(g, ".*") && strings.HasPrefix(p, strings.TrimSuffix(g, "*")) {
			return true
		}
	}
	return false
}

func (s *iamState) rolePerms(role string) []string {
	if p, ok := BigQueryRoles[role]; ok {
		return p
	}
	if s.iam != nil {
		return s.iam.CustomRoles[role]
	}
	return nil
}

// onProject is whether email's roles on project grant perm.
func (s *iamState) onProject(email, project, perm string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.iam == nil {
		return false
	}
	for _, role := range s.iam.Roles[email][project] {
		if grantsPerm(s.rolePerms(role), perm) {
			return true
		}
	}
	return false
}

// iamCheck answers r 403 when its caller (above) lacks a permission it
// needs, reporting true; next reads datasets' access entries.
func (s *iamState) check(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if r.URL.Path == BigQueryIAMPath {
		s.serve(w, r)
		return true
	}
	email := s.caller(r)
	if email == "" {
		return false
	}
	project := pathProject(r)
	if project == "" {
		return false
	}
	// onDataset is whether email's roles on the project, or the dataset's
	// access entries, grant perm.
	onDataset := func(p, dataset, perm string) bool {
		if s.onProject(email, p, perm) {
			return true
		}
		rec := newRecorder()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/bigquery/v2/projects/"+url.PathEscape(p)+
			"/datasets/"+url.PathEscape(dataset), nil)
		if err != nil {
			return false
		}
		next.ServeHTTP(rec, req)
		var ds struct {
			Access []struct {
				Role        string `json:"role"`
				UserByEmail string `json:"userByEmail"`
				IAMMember   string `json:"iamMember"`
			} `json:"access"`
		}
		if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &ds) != nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, a := range ds.Access {
			_, member, _ := strings.Cut(a.IAMMember, ":")
			if !strings.EqualFold(a.UserByEmail, email) && !strings.EqualFold(member, email) {
				continue
			}
			role := a.Role
			if basic, ok := datasetAccessRoles[role]; ok {
				role = basic
			}
			if grantsPerm(s.rolePerms(role), perm) {
				return true
			}
		}
		return false
	}
	denyProject := func(perm string) bool {
		writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: Project "+project+": User does not have "+perm+
			" permission in project "+project+".")
		return true
	}
	denyDataset := func(p, dataset, perm string) bool {
		writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: Dataset "+p+":"+dataset+": Permission "+perm+
			" denied on dataset "+p+":"+dataset+" (or it may not exist).")
		return true
	}
	denyTable := func(t tableRef, perm string) bool {
		name := t.ProjectID + ":" + t.DatasetID + "." + t.TableID
		writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: Table "+name+": Permission "+perm+
			" denied on table "+name+" (or it may not exist).")
		return true
	}
	rest := strings.TrimPrefix(r.URL.Path, "/upload")
	rest = strings.TrimPrefix(rest, "/bigquery/v2")
	rest = strings.TrimPrefix(rest, "/projects/"+project)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	for i := range parts {
		if u, err := url.PathUnescape(parts[i]); err == nil {
			parts[i] = u
		}
	}
	m := r.Method
	switch {
	case len(parts) == 1 && (parts[0] == "jobs" || parts[0] == "queries") && m == http.MethodPost:
		if !s.onProject(email, project, "bigquery.jobs.create") {
			return denyProject("bigquery.jobs.create")
		}
		return s.checkJob(r, project, onDataset, denyTable)
	case len(parts) == 1 && parts[0] == "datasets" && m == http.MethodPost:
		if !s.onProject(email, project, "bigquery.datasets.create") {
			return denyProject("bigquery.datasets.create")
		}
	case len(parts) == 2 && parts[0] == "datasets":
		perm := map[string]string{http.MethodGet: "bigquery.datasets.get", http.MethodPatch: "bigquery.datasets.update",
			http.MethodPut: "bigquery.datasets.update", http.MethodDelete: "bigquery.datasets.delete"}[m]
		if perm != "" && !onDataset(project, parts[1], perm) {
			return denyDataset(project, parts[1], perm)
		}
	case len(parts) == 3 && parts[0] == "datasets" && parts[2] == "tables":
		perm := map[string]string{http.MethodGet: "bigquery.tables.list", http.MethodPost: "bigquery.tables.create"}[m]
		if perm != "" && !onDataset(project, parts[1], perm) {
			return denyDataset(project, parts[1], perm)
		}
	case len(parts) >= 4 && parts[0] == "datasets" && parts[2] == "tables":
		t := tableRef{ProjectID: project, DatasetID: parts[1], TableID: parts[3]}
		perm := ""
		switch {
		case len(parts) == 4:
			perm = map[string]string{http.MethodGet: "bigquery.tables.get", http.MethodPatch: "bigquery.tables.update",
				http.MethodPut: "bigquery.tables.update", http.MethodDelete: "bigquery.tables.delete"}[m]
		case len(parts) == 5 && parts[4] == "data" && m == http.MethodGet:
			perm = "bigquery.tables.getData"
		case len(parts) == 5 && parts[4] == "insertAll":
			perm = "bigquery.tables.updateData"
		}
		if perm != "" && !onDataset(project, t.DatasetID, perm) {
			return denyTable(t, perm)
		}
	}
	return false
}

// sqlTables are the tables a query names after FROM or JOIN: `p.d.t`,
// `d.t`, d.t, p.d.t.
var sqlTables = regexp.MustCompile("(?i)\\b(?:FROM|JOIN)\\s+(`[^`]+`(?:\\.`[^`]+`)*|[A-Za-z_][\\w-]*(?:\\.[A-Za-z_][\\w-]*){1,2})")

// checkJob checks the job's tables: a load's or a query's destination, a
// copy's sources and destination, an extract's source, a query's tables.
func (s *iamState) checkJob(r *http.Request, project string, onDataset func(p, d, perm string) bool,
	deny func(tableRef, string) bool) bool {
	var job struct {
		Configuration struct {
			Query *struct {
				Query            string    `json:"query"`
				DestinationTable *tableRef `json:"destinationTable"`
				DefaultDataset   *struct {
					ProjectID string `json:"projectId"`
					DatasetID string `json:"datasetId"`
				} `json:"defaultDataset"`
			} `json:"query"`
			Load *struct {
				DestinationTable *tableRef `json:"destinationTable"`
			} `json:"load"`
			Copy *struct {
				SourceTable      *tableRef  `json:"sourceTable"`
				SourceTables     []tableRef `json:"sourceTables"`
				DestinationTable *tableRef  `json:"destinationTable"`
			} `json:"copy"`
			Extract *struct {
				SourceTable *tableRef `json:"sourceTable"`
			} `json:"extract"`
		} `json:"configuration"`
		// jobs.query's QueryRequest.
		Query          string `json:"query"`
		DefaultDataset *struct {
			ProjectID string `json:"projectId"`
			DatasetID string `json:"datasetId"`
		} `json:"defaultDataset"`
	}
	if !decodeJob(r, &job) {
		return false
	}
	need := func(t *tableRef, perm string) bool {
		if t == nil || t.DatasetID == "" {
			return false
		}
		ref := *t
		if ref.ProjectID == "" {
			ref.ProjectID = project
		}
		if i := strings.IndexAny(ref.TableID, "$@"); i > 0 {
			ref.TableID = ref.TableID[:i]
		}
		if !onDataset(ref.ProjectID, ref.DatasetID, perm) {
			return deny(ref, perm)
		}
		return false
	}
	c := job.Configuration
	sql, defProject, defDataset := job.Query, project, ""
	if job.DefaultDataset != nil {
		defDataset = job.DefaultDataset.DatasetID
		if job.DefaultDataset.ProjectID != "" {
			defProject = job.DefaultDataset.ProjectID
		}
	}
	if q := c.Query; q != nil {
		sql = q.Query
		if need(q.DestinationTable, "bigquery.tables.updateData") {
			return true
		}
		if q.DefaultDataset != nil {
			defDataset = q.DefaultDataset.DatasetID
			if q.DefaultDataset.ProjectID != "" {
				defProject = q.DefaultDataset.ProjectID
			}
		}
	}
	if l := c.Load; l != nil && need(l.DestinationTable, "bigquery.tables.updateData") {
		return true
	}
	if cp := c.Copy; cp != nil {
		srcs := cp.SourceTables
		if cp.SourceTable != nil {
			srcs = append(srcs, *cp.SourceTable)
		}
		for i := range srcs {
			if need(&srcs[i], "bigquery.tables.getData") {
				return true
			}
		}
		if need(cp.DestinationTable, "bigquery.tables.updateData") {
			return true
		}
	}
	if e := c.Extract; e != nil && need(e.SourceTable, "bigquery.tables.export") {
		return true
	}
	for _, m := range sqlTables.FindAllStringSubmatch(sql, -1) {
		path := strings.Split(strings.ReplaceAll(m[1], "`", ""), ".")
		var t tableRef
		switch len(path) {
		case 3:
			t = tableRef{ProjectID: path[0], DatasetID: path[1], TableID: path[2]}
		case 2:
			t = tableRef{ProjectID: project, DatasetID: path[0], TableID: path[1]}
		case 1:
			if defDataset == "" || strings.EqualFold(path[0], "UNNEST") {
				continue
			}
			t = tableRef{ProjectID: defProject, DatasetID: defDataset, TableID: path[0]}
		default:
			continue
		}
		if strings.HasPrefix(strings.ToLower(t.DatasetID), "region-") || strings.EqualFold(t.DatasetID, "INFORMATION_SCHEMA") ||
			strings.EqualFold(t.TableID, "INFORMATION_SCHEMA") {
			continue
		}
		if need(&t, "bigquery.tables.getData") {
			return true
		}
	}
	return false
}

// handler checks each request (check) before rest serves it.
func (s *iamState) handler(engine, rest http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.check(w, r, engine) {
			return
		}
		rest.ServeHTTP(w, r)
	})
}
