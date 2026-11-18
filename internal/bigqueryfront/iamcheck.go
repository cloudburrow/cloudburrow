package bigqueryfront

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
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
// update/delete; tabledata.list and insertAll. #1774 added: jobs.get and
// getQueryResults (bigquery.jobs.get, or the job's creator with
// bigquery.jobs.create), jobs.cancel (bigquery.jobs.update, or the
// creator), jobs.delete (bigquery.jobs.delete), jobs.list (a caller's own
// jobs with bigquery.jobs.create or list; allUsers needs listAll, or list,
// which shows the others' jobs redacted); routines.list/insert/get/
// update/delete, getIamPolicy and setIamPolicy; models.list/get/patch/
// delete; rowAccessPolicies.list/insert/get/update/delete/batchDelete and
// getIamPolicy; tables.getIamPolicy and setIamPolicy. A table's or
// routine's own IAM policy (iam.go) grants on it, and a dataset access
// entry's condition is evaluated. The roles are those of the project, its
// folders and its organization (the host flattens Resource Manager's
// hierarchy), and a conditional binding counts where its condition
// (internal/iamcond) holds for the checked resource now: resource.name,
// resource.type and resource.service as BigQuery gives them, and
// request.time; a tag condition grants nothing. A job's creator is known
// for the jobs a minted caller ran since the front started. Not checked:
// the tables a view or a routine reads.

// BigQueryIAMPath is where the host pushes BigQueryIAM to the front.
const BigQueryIAMPath = "/cloudburrow/v1/bigqueryIAM"

// BigQueryIAM is what the check reads: the live tokens this instance
// minted, each account's roles by project, and the custom roles' BigQuery
// permissions.
type BigQueryIAM struct {
	Tokens      []BigQueryIAMToken             `json:"tokens"`
	Roles       map[string]map[string][]string `json:"roles"`
	CustomRoles map[string][]string            `json:"customRoles,omitempty"`
	// Conditional are each account's conditional bindings by project,
	// their own and inherited (#1774).
	Conditional map[string]map[string][]BigQueryIAMCondition `json:"conditional,omitempty"`
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
// and permissions reference
// (https://docs.cloud.google.com/bigquery/docs/access-control, re-read
// 2026-11-18). BigQueryRoleExcludes are the ones a role's wildcard does
// not grant.
var BigQueryRoles = map[string][]string{
	"roles/owner":          {"bigquery.*"},
	"roles/editor":         {"bigquery.*"},
	"roles/bigquery.admin": {"bigquery.*"},
	"roles/viewer": {"bigquery.datasets.get", "bigquery.datasets.getIamPolicy", "bigquery.tables.get", "bigquery.tables.list",
		"bigquery.tables.getData", "bigquery.tables.export", "bigquery.tables.getIamPolicy", "bigquery.jobs.get", "bigquery.jobs.list",
		"bigquery.routines.get", "bigquery.routines.list", "bigquery.models.getMetadata", "bigquery.models.list",
		"bigquery.rowAccessPolicies.get", "bigquery.rowAccessPolicies.list", "bigquery.rowAccessPolicies.getIamPolicy"},
	"roles/bigquery.dataOwner": {"bigquery.datasets.*", "bigquery.tables.*", "bigquery.routines.*", "bigquery.models.*",
		"bigquery.rowAccessPolicies.*"},
	"roles/bigquery.dataEditor": {"bigquery.datasets.create", "bigquery.datasets.get", "bigquery.datasets.getIamPolicy",
		"bigquery.tables.*", "bigquery.routines.*", "bigquery.models.*"},
	"roles/bigquery.dataViewer": {"bigquery.datasets.get", "bigquery.datasets.getIamPolicy", "bigquery.tables.get",
		"bigquery.tables.getData", "bigquery.tables.list", "bigquery.tables.export", "bigquery.tables.getIamPolicy",
		"bigquery.routines.get", "bigquery.routines.list", "bigquery.models.getData", "bigquery.models.getMetadata",
		"bigquery.models.list", "bigquery.models.export"},
	"roles/bigquery.metadataViewer": {"bigquery.datasets.get", "bigquery.datasets.getIamPolicy", "bigquery.tables.get",
		"bigquery.tables.list", "bigquery.tables.getIamPolicy", "bigquery.routines.get", "bigquery.routines.list",
		"bigquery.models.getMetadata", "bigquery.models.list"},
	"roles/bigquery.user": {"bigquery.jobs.create", "bigquery.jobs.list", "bigquery.datasets.create",
		"bigquery.datasets.getIamPolicy", "bigquery.tables.list", "bigquery.routines.list", "bigquery.models.list",
		"bigquery.readsessions.*"},
	"roles/bigquery.jobUser":        {"bigquery.jobs.create"},
	"roles/bigquery.resourceAdmin":  {"bigquery.jobs.get", "bigquery.jobs.list", "bigquery.jobs.listAll"},
	"roles/bigquery.resourceEditor": {"bigquery.jobs.get", "bigquery.jobs.list", "bigquery.jobs.listAll"},
	"roles/bigquery.resourceViewer": {"bigquery.jobs.get", "bigquery.jobs.list", "bigquery.jobs.listAll"},
	"roles/bigquery.securityAdmin": {"bigquery.rowAccessPolicies.*", "bigquery.tables.get", "bigquery.tables.list",
		"bigquery.tables.getIamPolicy", "bigquery.tables.setIamPolicy", "bigquery.routines.get", "bigquery.routines.list"},
	"roles/bigquery.filteredDataViewer": {"bigquery.rowAccessPolicies.getFilteredData"},
}

// BigQueryRoleExcludes are the permissions of a wildcard above that the
// role does not hold: Editor cannot cancel or list everyone's jobs, nor
// set a policy; Data Editor cannot set a table's policy.
var BigQueryRoleExcludes = map[string][]string{
	"roles/editor": {"bigquery.jobs.update", "bigquery.jobs.listAll", "bigquery.tables.setIamPolicy",
		"bigquery.datasets.setIamPolicy", "bigquery.rowAccessPolicies.setIamPolicy", "bigquery.routines.setIamPolicy"},
	"roles/bigquery.dataEditor": {"bigquery.tables.setIamPolicy", "bigquery.routines.setIamPolicy"},
}

// BigQueryIAMCondition is a conditional role binding: its role and its
// condition's CEL expression.
type BigQueryIAMCondition struct {
	Role       string `json:"role"`
	Expression string `json:"expression"`
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
	// policies are the tables' and routines' own IAM policies (iam.go).
	policies *iamPolicies
	// creators are the accounts that made the jobs a minted caller ran,
	// by project and job ID (jobs.get, jobs.list and jobs.cancel let a
	// job's creator read and cancel it).
	creators map[string]string
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

func (s *iamState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
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
	now := s.clock()
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

// roleGrants is whether role grants perm. s.mu is held.
func (s *iamState) roleGrants(role, perm string) bool {
	perms, ok := BigQueryRoles[role]
	if !ok && s.iam != nil {
		perms = s.iam.CustomRoles[role]
	}
	return grantsPerm(perms, perm) && !slices.Contains(BigQueryRoleExcludes[role], perm)
}

// bqResource is what a permission is checked on: a project (dataset ""),
// a dataset (kind ""), or a dataset's "tables", "routines" or "models".
type bqResource struct {
	project, dataset, kind, id string
}

// attrs are the IAM Conditions attributes of res, as BigQuery gives them:
// resource.name projects/P/datasets/D[/tables/T], resource.type
// bigquery.googleapis.com/Dataset or Table (Routine, Model), and
// resource.service bigquery.googleapis.com; a project's are Resource
// Manager's (https://docs.cloud.google.com/bigquery/docs/conditions,
// https://docs.cloud.google.com/iam/docs/conditions-resource-attributes).
// Tags are not known here: a tag condition grants nothing.
func (res bqResource) attrs(now time.Time) iamcond.Attributes {
	a := iamcond.Attributes{RequestTime: now, ResourceService: "bigquery.googleapis.com"}
	if res.dataset == "" {
		a.ResourceName = "projects/" + res.project
		a.ResourceType = "cloudresourcemanager.googleapis.com/Project"
		a.ResourceService = "cloudresourcemanager.googleapis.com"
		return a
	}
	a.ResourceName = "projects/" + res.project + "/datasets/" + res.dataset
	a.ResourceType = "bigquery.googleapis.com/Dataset"
	if res.kind != "" {
		a.ResourceName += "/" + res.kind + "/" + res.id
		a.ResourceType = "bigquery.googleapis.com/" + map[string]string{"tables": "Table", "routines": "Routine",
			"models": "Model"}[res.kind]
	}
	return a
}

// onProjectFor is whether email's roles on res's project (its own, its
// folders' and its organization's, and the conditional ones whose
// condition holds for res now) grant perm.
func (s *iamState) onProjectFor(email string, res bqResource, perm string) bool {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.iam == nil {
		return false
	}
	for _, role := range s.iam.Roles[email][res.project] {
		if s.roleGrants(role, perm) {
			return true
		}
	}
	for _, c := range s.iam.Conditional[email][res.project] {
		if s.roleGrants(c.Role, perm) && iamcond.Eval(c.Expression, res.attrs(now)) {
			return true
		}
	}
	return false
}

// onProject is whether email's roles on project grant perm.
func (s *iamState) onProject(email, project, perm string) bool {
	return s.onProjectFor(email, bqResource{project: project}, perm)
}

// memberIs is whether a policy member names email.
func memberIs(member, email string) bool {
	if member == "allUsers" || member == "allAuthenticatedUsers" {
		return true
	}
	kind, m, ok := strings.Cut(member, ":")
	return ok && (kind == "user" || kind == "serviceAccount") && strings.EqualFold(m, email)
}

// allowed is whether email holds perm on res: by its roles on the project
// (and up the hierarchy), the dataset's access entries, or, for a table or
// a routine, that resource's own IAM policy (tables.setIamPolicy,
// routines.setIamPolicy), conditions evaluated for res.
func (s *iamState) allowed(r *http.Request, next http.Handler, email string, res bqResource, perm string) bool {
	if s.onProjectFor(email, res, perm) {
		return true
	}
	get := func(path string, v any) bool {
		rec := newRecorder()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/bigquery/v2/projects/"+url.PathEscape(res.project)+
			"/datasets/"+url.PathEscape(res.dataset)+path, nil)
		if err != nil || next == nil {
			return false
		}
		next.ServeHTTP(rec, req)
		return rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), v) == nil
	}
	now := s.clock()
	var ds struct {
		Access []struct {
			Role        string `json:"role"`
			UserByEmail string `json:"userByEmail"`
			IAMMember   string `json:"iamMember"`
			Condition   *struct {
				Expression string `json:"expression"`
			} `json:"condition"`
		} `json:"access"`
	}
	if get("", &ds) {
		dsAttrs := bqResource{project: res.project, dataset: res.dataset}.attrs(now)
		s.mu.Lock()
		for _, a := range ds.Access {
			if !strings.EqualFold(a.UserByEmail, email) && (a.IAMMember == "" || !memberIs(a.IAMMember, email)) {
				continue
			}
			role := a.Role
			if basic, ok := datasetAccessRoles[role]; ok {
				role = basic
			}
			if a.Condition != nil && !iamcond.Eval(a.Condition.Expression, dsAttrs) {
				continue
			}
			if s.roleGrants(role, perm) {
				s.mu.Unlock()
				return true
			}
		}
		s.mu.Unlock()
	}
	if s.policies == nil || (res.kind != "tables" && res.kind != "routines") || res.id == "" {
		return false
	}
	var made struct {
		CreationTime string `json:"creationTime"`
	}
	if !get("/"+res.kind+"/"+url.PathEscape(res.id), &made) {
		return false
	}
	bindings := s.policies.bindings(iamKey(res.project, res.dataset, res.kind, res.id), made.CreationTime)
	attrs := res.attrs(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bindings {
		if !s.roleGrants(b.Role, perm) || !b.Grants(attrs) {
			continue
		}
		for _, m := range b.Members {
			if memberIs(m, email) {
				return true
			}
		}
	}
	return false
}

func jobKey(project, job string) string { return project + "\x00" + job }

// creator is the account that made the job, "" for one a minted caller
// did not run (the instance's ADC account's).
func (s *iamState) creator(project, job string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creators[jobKey(project, job)]
}

func (s *iamState) recordCreator(project, job, email string) {
	if job == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creators == nil {
		s.creators = map[string]string{}
	}
	s.creators[jobKey(project, job)] = email
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
	onDataset := func(p, dataset, perm string) bool {
		return s.allowed(r, next, email, bqResource{project: p, dataset: dataset}, perm)
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
	denyIn := func(what string, res bqResource, perm string) bool {
		name := res.project + ":" + res.dataset + "." + res.id
		writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: "+what+" "+name+": Permission "+perm+
			" denied on "+strings.ToLower(what)+" "+name+" (or it may not exist).")
		return true
	}
	denyTable := func(t tableRef, perm string) bool {
		return denyIn("Table", bqResource{project: t.ProjectID, dataset: t.DatasetID, kind: "tables", id: t.TableID}, perm)
	}
	onTable := func(t tableRef, perm string) bool {
		return s.allowed(r, next, email, bqResource{project: t.ProjectID, dataset: t.DatasetID, kind: "tables", id: t.TableID}, perm)
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
	// method splits a trailing ":verb" (tables.getIamPolicy, ...) off the
	// last part.
	verb := ""
	if last := parts[len(parts)-1]; strings.Contains(last, ":") {
		parts[len(parts)-1], verb, _ = strings.Cut(last, ":")
	}
	m := r.Method
	switch {
	case len(parts) == 1 && (parts[0] == "jobs" || parts[0] == "queries") && m == http.MethodPost:
		if !s.onProject(email, project, "bigquery.jobs.create") {
			return denyProject("bigquery.jobs.create")
		}
		return s.checkJob(r, project, onTable, denyTable)
	case len(parts) == 1 && parts[0] == "jobs" && m == http.MethodGet:
		return s.checkListJobs(r, email, project, denyProject)
	case len(parts) >= 2 && (parts[0] == "jobs" || parts[0] == "queries"):
		return s.checkJobAccess(w, r, email, project, parts, m)
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
	case len(parts) == 3 && parts[0] == "datasets" && (parts[2] == "tables" || parts[2] == "routines" || parts[2] == "models"):
		perm := map[string]string{http.MethodGet: "bigquery." + parts[2] + ".list", http.MethodPost: "bigquery." + parts[2] + ".create"}[m]
		if perm != "" && !onDataset(project, parts[1], perm) {
			return denyDataset(project, parts[1], perm)
		}
	case len(parts) == 4 && parts[0] == "datasets" && (parts[2] == "routines" || parts[2] == "models"):
		res := bqResource{project: project, dataset: parts[1], kind: parts[2], id: parts[3]}
		perm := ""
		if parts[2] == "routines" {
			perm = map[string]string{http.MethodGet: "bigquery.routines.get", http.MethodPut: "bigquery.routines.update",
				http.MethodPatch: "bigquery.routines.update", http.MethodDelete: "bigquery.routines.delete"}[m]
			if verb == "getIamPolicy" || verb == "setIamPolicy" {
				perm = "bigquery.routines." + verb
			} else if verb != "" {
				perm = ""
			}
		} else {
			perm = map[string]string{http.MethodGet: "bigquery.models.getMetadata", http.MethodPatch: "bigquery.models.updateMetadata",
				http.MethodDelete: "bigquery.models.delete"}[m]
		}
		if perm != "" && !s.allowed(r, next, email, res, perm) {
			return denyIn(map[string]string{"routines": "Routine", "models": "Model"}[parts[2]], res, perm)
		}
	case len(parts) >= 4 && parts[0] == "datasets" && parts[2] == "tables":
		t := tableRef{ProjectID: project, DatasetID: parts[1], TableID: parts[3]}
		perm := ""
		switch {
		case len(parts) == 4 && verb == "":
			perm = map[string]string{http.MethodGet: "bigquery.tables.get", http.MethodPatch: "bigquery.tables.update",
				http.MethodPut: "bigquery.tables.update", http.MethodDelete: "bigquery.tables.delete"}[m]
		case len(parts) == 4 && (verb == "getIamPolicy" || verb == "setIamPolicy"):
			perm = "bigquery.tables." + verb
		case len(parts) == 5 && parts[4] == "data" && m == http.MethodGet:
			perm = "bigquery.tables.getData"
		case len(parts) == 5 && parts[4] == "insertAll":
			perm = "bigquery.tables.updateData"
		case len(parts) == 5 && parts[4] == "rowAccessPolicies" && verb == "batchDelete":
			perm = "bigquery.rowAccessPolicies.delete"
		case len(parts) == 5 && parts[4] == "rowAccessPolicies" && verb == "":
			perm = map[string]string{http.MethodGet: "bigquery.rowAccessPolicies.list", http.MethodPost: "bigquery.rowAccessPolicies.create"}[m]
		case len(parts) == 6 && parts[4] == "rowAccessPolicies" && verb == "":
			perm = map[string]string{http.MethodGet: "bigquery.rowAccessPolicies.get", http.MethodPut: "bigquery.rowAccessPolicies.update",
				http.MethodDelete: "bigquery.rowAccessPolicies.delete"}[m]
		case len(parts) == 6 && parts[4] == "rowAccessPolicies" && verb == "getIamPolicy":
			perm = "bigquery.rowAccessPolicies.getIamPolicy"
		}
		if perm != "" && !onTable(t, perm) {
			return denyTable(t, perm)
		}
	}
	return false
}

// checkJobAccess checks jobs.get, getQueryResults, jobs.cancel and
// jobs.delete: a job's creator, with bigquery.jobs.create, reads and
// cancels it; anyone else needs bigquery.jobs.get (to read it),
// bigquery.jobs.update (to cancel it). Deleting a job's metadata needs
// bigquery.jobs.delete ("Manage jobs",
// https://docs.cloud.google.com/bigquery/docs/managing-jobs, read
// 2026-11-18).
func (s *iamState) checkJobAccess(w http.ResponseWriter, r *http.Request, email, project string, parts []string, m string) bool {
	job := parts[1]
	perm := ""
	switch {
	case len(parts) == 2 && m == http.MethodGet:
		perm = "bigquery.jobs.get"
	case len(parts) == 3 && parts[0] == "jobs" && parts[2] == "cancel" && m == http.MethodPost:
		perm = "bigquery.jobs.update"
	case len(parts) == 3 && parts[0] == "jobs" && parts[2] == "delete" && m == http.MethodDelete:
		perm = "bigquery.jobs.delete"
	default:
		return false
	}
	if s.onProject(email, project, perm) {
		return false
	}
	if perm != "bigquery.jobs.delete" && s.creator(project, job) == email && s.onProject(email, project, "bigquery.jobs.create") {
		return false
	}
	name := project + ":" + job
	if loc := r.URL.Query().Get("location"); loc != "" {
		name = project + ":" + loc + "." + job
	}
	writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: Job "+name+": User does not have "+perm+
		" permission for job "+name+".")
	return true
}

// checkListJobs checks jobs.list: a caller lists its own jobs with
// bigquery.jobs.create or bigquery.jobs.list; allUsers=true needs
// bigquery.jobs.listAll, or bigquery.jobs.list, with which the other
// users' jobs are listed redacted (their ID and state). Without allUsers
// only the caller's own jobs are listed (filterJobs).
func (s *iamState) checkListJobs(r *http.Request, email, project string, deny func(string) bool) bool {
	all := r.URL.Query().Get("allUsers") == "true"
	switch {
	case all && s.onProject(email, project, "bigquery.jobs.listAll"):
	case all && !s.onProject(email, project, "bigquery.jobs.list"):
		return deny("bigquery.jobs.listAll")
	case !all && !s.onProject(email, project, "bigquery.jobs.list") && !s.onProject(email, project, "bigquery.jobs.listAll") &&
		!s.onProject(email, project, "bigquery.jobs.create"):
		return deny("bigquery.jobs.list")
	}
	return false
}

// filterJobs rewrites a jobs.list answer for email: its own jobs, and,
// with allUsers and bigquery.jobs.list but not listAll, the others'
// redacted to their reference and state.
func (s *iamState) filterJobs(body []byte, email, project string, all bool) ([]byte, bool) {
	var list map[string]json.RawMessage
	if json.Unmarshal(body, &list) != nil {
		return nil, false
	}
	var jobs []map[string]json.RawMessage
	if raw, ok := list["jobs"]; ok && json.Unmarshal(raw, &jobs) != nil {
		return nil, false
	}
	kept := []map[string]json.RawMessage{}
	for _, j := range jobs {
		var ref struct {
			ProjectID string `json:"projectId"`
			JobID     string `json:"jobId"`
		}
		_ = json.Unmarshal(j["jobReference"], &ref)
		if ref.ProjectID == "" {
			ref.ProjectID = project
		}
		if s.creator(ref.ProjectID, ref.JobID) == email {
			kept = append(kept, j)
			continue
		}
		if all {
			red := map[string]json.RawMessage{}
			for _, k := range []string{"kind", "id", "jobReference", "state"} {
				if v, ok := j[k]; ok {
					red[k] = v
				}
			}
			kept = append(kept, red)
		}
	}
	if len(kept) == 0 {
		delete(list, "jobs")
	} else {
		b, _ := json.Marshal(kept)
		list["jobs"] = b
	}
	out, err := json.Marshal(list)
	return out, err == nil
}

// sqlTables are the tables a query names after FROM or JOIN: `p.d.t`,
// `d.t`, d.t, p.d.t.
var sqlTables = regexp.MustCompile("(?i)\\b(?:FROM|JOIN)\\s+(`[^`]+`(?:\\.`[^`]+`)*|[A-Za-z_][\\w-]*(?:\\.[A-Za-z_][\\w-]*){1,2})")

// checkJob checks the job's tables: a load's or a query's destination, a
// copy's sources and destination, an extract's source, a query's tables.
func (s *iamState) checkJob(r *http.Request, project string, onTable func(t tableRef, perm string) bool,
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
		if !onTable(ref, perm) {
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

// handler checks each request (check) before rest serves it. It records
// the creator of the jobs a minted caller runs, and filters a minted
// caller's jobs.list to the jobs it may see.
func (s *iamState) handler(engine, rest http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.check(w, r, engine) {
			return
		}
		email := ""
		if r.URL.Path != BigQueryIAMPath {
			email = s.caller(r)
		}
		project := pathProject(r)
		if email != "" && r.Method == http.MethodGet && strings.TrimSuffix(r.URL.Path, "/") == "/bigquery/v2/projects" {
			s.serveProjects(w, r, rest, email)
			return
		}
		if email == "" || project == "" {
			rest.ServeHTTP(w, r)
			return
		}
		tail := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/upload"), "/bigquery/v2")
		tail = strings.TrimPrefix(tail, "/projects/"+project)
		tail = strings.Trim(tail, "/")
		switch {
		case r.Method == http.MethodPost && (tail == "jobs" || tail == "queries"):
			rec := newRecorder()
			rest.ServeHTTP(rec, r)
			var job struct {
				JobReference struct {
					ProjectID string `json:"projectId"`
					JobID     string `json:"jobId"`
				} `json:"jobReference"`
			}
			if rec.status/100 == 2 && json.Unmarshal(rec.body.Bytes(), &job) == nil {
				p := job.JobReference.ProjectID
				if p == "" {
					p = project
				}
				s.recordCreator(p, job.JobReference.JobID, email)
			}
			rec.copyTo(w)
		case r.Method == http.MethodGet && tail == "jobs":
			all := r.URL.Query().Get("allUsers") == "true"
			if all && s.onProject(email, project, "bigquery.jobs.listAll") {
				rest.ServeHTTP(w, r)
				return
			}
			rec := newRecorder()
			rest.ServeHTTP(rec, r)
			if rec.status != http.StatusOK {
				rec.copyTo(w)
				return
			}
			out, ok := s.filterJobs(rec.body.Bytes(), email, project, all)
			if !ok {
				rec.copyTo(w)
				return
			}
			rec.header.Del("Content-Length")
			rec.body.Reset()
			rec.body.Write(out)
			rec.copyTo(w)
		default:
			rest.ServeHTTP(w, r)
		}
	})
}

// serveProjects answers a minted caller's projects.list with the projects
// it holds a role on ("Lists projects to which the user has been granted
// any project role", BigQuery's projects.list reference): the roles the
// host gives, those that grant BigQuery permissions, its own and
// inherited, a conditional one included.
func (s *iamState) serveProjects(w http.ResponseWriter, r *http.Request, rest http.Handler, email string) {
	rec := newRecorder()
	rest.ServeHTTP(rec, r)
	var list map[string]json.RawMessage
	var projects []map[string]json.RawMessage
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil ||
		(list["projects"] != nil && json.Unmarshal(list["projects"], &projects) != nil) {
		rec.copyTo(w)
		return
	}
	s.mu.Lock()
	var kept []map[string]json.RawMessage
	for _, p := range projects {
		var ref struct {
			ProjectID string `json:"projectId"`
		}
		_ = json.Unmarshal(p["projectReference"], &ref)
		if s.iam != nil && (len(s.iam.Roles[email][ref.ProjectID]) > 0 || len(s.iam.Conditional[email][ref.ProjectID]) > 0) {
			kept = append(kept, p)
		}
	}
	s.mu.Unlock()
	delete(list, "projects")
	if len(kept) > 0 {
		b, _ := json.Marshal(kept)
		list["projects"] = b
	}
	b, _ := json.Marshal(len(kept))
	list["totalItems"] = b
	out, _ := json.Marshal(list)
	rec.header.Del("Content-Length")
	rec.body.Reset()
	rec.body.Write(out)
	rec.copyTo(w)
}
