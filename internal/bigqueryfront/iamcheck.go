package bigqueryfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
// request.time, and the resource's effective tags (tagsFor, #1780). A
// job's creator is kept in the front's state directory (keepCreators,
// #1780). Custom roles are every one with bigquery. permissions the host
// gives. group: and domain: members, and a dataset's groupByEmail and
// domain entries, name the accounts of a Cloud Identity group (nested
// groups too) or of the instance's domain, which the host gives as
// Principals (#1812). The tables a
// view reads need bigquery.tables.getData of the caller unless they
// authorize the view or its dataset (checkView); a routine a query calls
// by its three- or two-part name needs bigquery.routines.get, and the
// tables its SQL body reads are checked as a view's, an authorized
// routine's excepted, a procedure's body for the routines it calls too
// (checkRoutines).

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
	// Tags are effective tags (#1780): a project's ("projects/P", its own
	// and its folders' and organization's), and the tags bound to a
	// dataset or a table itself (BigQueryTagPrefix+"projects/P/datasets/D"
	// [+"/tables/T"]), which the front completes with their resourceTags.
	Tags map[string][]iamcond.Tag `json:"tags,omitempty"`
	// TagValues are every tag value by its namespaced name
	// ("P/env/prod"), by which resourceTags name theirs.
	TagValues map[string]iamcond.Tag `json:"tagValues,omitempty"`
	// Principals are each account's members beyond its own (#1812):
	// "group:G" for every Cloud Identity group it belongs to, directly or
	// through nested groups, and "domain:D" for an account of the
	// instance's domain, all lower case. They match a dataset's
	// groupByEmail and domain entries, and group: and domain: members of a
	// table's or a routine's own policy.
	Principals map[string][]string `json:"principals,omitempty"`
}

// BigQueryTagPrefix is the service part of a BigQuery dataset's or
// table's full resource name, which a Resource Manager tag binding names
// ("//bigquery.googleapis.com/projects/P/datasets/D/tables/T",
// https://docs.cloud.google.com/bigquery/docs/tags).
const BigQueryTagPrefix = "//bigquery.googleapis.com/"

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
	// full serves a view's metadata with its query as the client wrote it
	// (views.go); handler sets it.
	full http.Handler
	// creators are the accounts that made the jobs a minted caller ran,
	// by project and job ID (jobs.get, jobs.list and jobs.cancel let a
	// job's creator read and cancel it).
	creators map[string]string
	// creatorsPath is the file the creators are kept in (#1780), "" for
	// none; logf reports a failure to write it.
	creatorsPath string
	logf         func(string, ...any)
}

// jobCreatorsStateFile is the file a job's creator is kept in, in dir.
func jobCreatorsStateFile(dir string) string { return filepath.Join(dir, "jobcreators.json") }

// keepCreators reads the creators kept at path and keeps them there
// (#1780): a job's creator outlives a restart of the front, as the job
// does in the emulator ("user_email", the job's creator, is part of the
// job, https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job).
func (s *iamState) keepCreators(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	saved := map[string]string{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the job creators it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the job creators it kept", path)
		saved = map[string]string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creators == nil {
		s.creators = map[string]string{}
	}
	for k, v := range saved {
		if _, ok := s.creators[k]; !ok {
			s.creators[k] = v
		}
	}
	s.creatorsPath, s.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	s.saveCreatorsLocked()
}

func (s *iamState) saveCreatorsLocked() {
	if s.creatorsPath == "" {
		return
	}
	b, err := json.Marshal(s.creators)
	if err == nil {
		tmp := s.creatorsPath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.creatorsPath)
		}
	}
	if err != nil && s.logf != nil {
		s.logf("bigquery front: keep the job creators: %v", err)
	}
}

// resetCreators forgets every creator (the emulator restarted, empty).
func (s *iamState) resetCreators() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creators = nil
	s.saveCreatorsLocked()
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
// Its effective tags are tags when known (s.tagsFor), else a tag
// condition grants nothing.
func (res bqResource) attrs(now time.Time, tags []iamcond.Tag, known bool) iamcond.Attributes {
	a := iamcond.Attributes{RequestTime: now, ResourceService: "bigquery.googleapis.com", Tags: tags, TagsKnown: known}
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
func (s *iamState) onProjectFor(r *http.Request, email string, res bqResource, perm string) bool {
	now := s.clock()
	s.mu.Lock()
	if s.iam == nil {
		s.mu.Unlock()
		return false
	}
	for _, role := range s.iam.Roles[email][res.project] {
		if s.roleGrants(role, perm) {
			s.mu.Unlock()
			return true
		}
	}
	var conds []BigQueryIAMCondition
	for _, c := range s.iam.Conditional[email][res.project] {
		if s.roleGrants(c.Role, perm) {
			conds = append(conds, c)
		}
	}
	s.mu.Unlock()
	if len(conds) == 0 {
		return false
	}
	var attrs *iamcond.Attributes
	for _, c := range conds {
		if attrs == nil {
			a := res.attrs(now, nil, false)
			if usesTags(c.Expression) {
				a = s.tagAttrs(r, res, now)
			}
			attrs = &a
		} else if !attrs.TagsKnown && usesTags(c.Expression) {
			a := s.tagAttrs(r, res, now)
			attrs = &a
		}
		if iamcond.Eval(c.Expression, *attrs) {
			return true
		}
	}
	return false
}

// usesTags is whether a condition reads the resource's tags.
func usesTags(expr string) bool { return strings.Contains(expr, "Tag") }

// tagsFor are res's effective tags (#1780) and whether they are known: the
// tags bound to it, and its resourceTags, a nearer value of a key
// replacing a farther one, up through a table's dataset to the project's
// effective tags (its folders' and organization's), as Resource Manager's
// tags are inherited down the hierarchy ("Tags can be attached directly to
// a resource or inherited from parent resources",
// https://docs.cloud.google.com/bigquery/docs/tags, read 2026-11-18). A
// routine or a model carries its dataset's. With r nil only the bound
// tags are read, not resourceTags.
func (s *iamState) tagsFor(r *http.Request, res bqResource) []iamcond.Tag {
	own := func(path, name string) []iamcond.Tag {
		var meta struct {
			ResourceTags map[string]string `json:"resourceTags"`
		}
		var out []iamcond.Tag
		if r != nil && s.full != nil {
			rec := newRecorder()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
			if err == nil {
				s.full.ServeHTTP(rec, req)
				if rec.status == http.StatusOK {
					_ = json.Unmarshal(rec.body.Bytes(), &meta)
				}
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.iam == nil {
			return nil
		}
		out = append(out, s.iam.Tags[name]...)
		keys := make([]string, 0, len(meta.ResourceTags))
		for k := range meta.ResourceTags {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			v := meta.ResourceTags[k]
			t, ok := s.iam.TagValues[k+"/"+v]
			if !ok {
				t = iamcond.Tag{NamespacedKey: k, ShortValue: v}
			}
			out = append(out, t)
		}
		return out
	}
	var chain [][]iamcond.Tag
	if res.dataset != "" {
		dsPath := "/bigquery/v2/projects/" + url.PathEscape(res.project) + "/datasets/" + url.PathEscape(res.dataset)
		dsName := BigQueryTagPrefix + "projects/" + res.project + "/datasets/" + res.dataset
		if res.kind == "tables" && res.id != "" {
			chain = append(chain, own(dsPath+"/tables/"+url.PathEscape(res.id), dsName+"/tables/"+res.id))
		}
		chain = append(chain, own(dsPath, dsName))
	}
	s.mu.Lock()
	if s.iam != nil {
		chain = append(chain, s.iam.Tags["projects/"+res.project])
	}
	s.mu.Unlock()
	seen := map[string]bool{}
	var out []iamcond.Tag
	for _, level := range chain {
		for _, t := range level {
			if seen[t.NamespacedKey] {
				continue
			}
			seen[t.NamespacedKey] = true
			out = append(out, t)
		}
	}
	return out
}

// tagAttrs are res's attributes with its effective tags.
func (s *iamState) tagAttrs(r *http.Request, res bqResource, now time.Time) iamcond.Attributes {
	return res.attrs(now, s.tagsFor(r, res), true)
}

// onProject is whether email's roles on project grant perm.
func (s *iamState) onProject(email, project, perm string) bool {
	return s.onProjectFor(nil, email, bqResource{project: project}, perm)
}

// memberIs is whether a policy member names email: as its own user: or
// serviceAccount: member, or as one of principals, its groups and domain
// (BigQueryIAM.Principals, #1812).
func memberIs(member, email string, principals []string) bool {
	if member == "allUsers" || member == "allAuthenticatedUsers" {
		return true
	}
	kind, m, ok := strings.Cut(member, ":")
	if !ok {
		return false
	}
	switch kind {
	case "user", "serviceAccount":
		return strings.EqualFold(m, email)
	case "group", "domain":
		return slices.Contains(principals, kind+":"+strings.ToLower(m))
	}
	return false
}

// principalsLocked are email's groups and domain; s.mu is held.
func (s *iamState) principalsLocked(email string) []string {
	if s.iam == nil {
		return nil
	}
	return s.iam.Principals[strings.ToLower(email)]
}

// entryNames is whether a dataset access entry names email: its
// userByEmail, its groupByEmail or domain (through principals), or its
// iamMember.
func entryNames(userByEmail, groupByEmail, domain, iamMember, email string, principals []string) bool {
	switch {
	case userByEmail != "":
		return strings.EqualFold(userByEmail, email)
	case groupByEmail != "":
		return slices.Contains(principals, "group:"+strings.ToLower(groupByEmail))
	case domain != "":
		return slices.Contains(principals, "domain:"+strings.ToLower(domain))
	case iamMember != "":
		return memberIs(iamMember, email, principals)
	}
	return false
}

// allowed is whether email holds perm on res: by its roles on the project
// (and up the hierarchy), the dataset's access entries, or, for a table or
// a routine, that resource's own IAM policy (tables.setIamPolicy,
// routines.setIamPolicy), conditions evaluated for res.
func (s *iamState) allowed(r *http.Request, next http.Handler, email string, res bqResource, perm string) bool {
	if s.onProjectFor(r, email, res, perm) {
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
			Role         string `json:"role"`
			UserByEmail  string `json:"userByEmail"`
			GroupByEmail string `json:"groupByEmail"`
			Domain       string `json:"domain"`
			IAMMember    string `json:"iamMember"`
			Condition    *struct {
				Expression string `json:"expression"`
			} `json:"condition"`
		} `json:"access"`
	}
	if get("", &ds) {
		dsRes := bqResource{project: res.project, dataset: res.dataset}
		dsAttrs := dsRes.attrs(now, nil, false)
		for _, a := range ds.Access {
			if a.Condition != nil && usesTags(a.Condition.Expression) {
				dsAttrs = s.tagAttrs(r, dsRes, now)
				break
			}
		}
		s.mu.Lock()
		principals := s.principalsLocked(email)
		for _, a := range ds.Access {
			if !entryNames(a.UserByEmail, a.GroupByEmail, a.Domain, a.IAMMember, email, principals) {
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
	attrs := res.attrs(now, nil, false)
	for _, b := range bindings {
		if b.Condition != nil && usesTags(b.Condition.Expression) {
			attrs = s.tagAttrs(r, res, now)
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	principals := s.principalsLocked(email)
	for _, b := range bindings {
		if !s.roleGrants(b.Role, perm) || !b.Grants(attrs) {
			continue
		}
		for _, m := range b.Members {
			if memberIs(m, email, principals) {
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
	if s.creators[jobKey(project, job)] == email {
		return
	}
	s.creators[jobKey(project, job)] = email
	s.saveCreatorsLocked()
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
		if s.checkModels(jobSQL(r), project, func(t tableRef, perm string) bool {
			res := bqResource{project: t.ProjectID, dataset: t.DatasetID, kind: "models", id: t.TableID}
			if s.allowed(r, next, email, res, perm) {
				return false
			}
			return denyIn("Model", res, perm)
		}) {
			return true
		}
		if s.checkJob(r, project, onTable, denyTable, func(t tableRef) bool {
			return s.checkView(r, t, 0, onTable, denyTable)
		}) {
			return true
		}
		return s.checkRoutines(r, next, email, jobSQL(r), project, 0, onTable, denyTable, denyIn)
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

// sqlModels are the models ML functions name: "(MODEL m" or ", MODEL m".
var sqlModels = regexp.MustCompile("(?i)[(,]\\s*MODEL\\s+(`[^`]+`(?:\\.`[^`]+`)*|[A-Za-z_][\\w-]*(?:\\.[A-Za-z_][\\w-]*){0,2})")

// checkModels checks the models ML functions in sql name: "run
// inference" needs bigquery.models.getData on the model (#1879; "Generate
// text", Required permissions,
// https://docs.cloud.google.com/bigquery/docs/generate-text, read
// 2026-11-20). deny answers a denial and reports true.
func (s *iamState) checkModels(sql, project string, deny func(tableRef, string) bool) bool {
	for _, m := range sqlModels.FindAllStringSubmatch(sql, -1) {
		path := strings.Split(strings.ReplaceAll(m[1], "`", ""), ".")
		var t tableRef
		switch len(path) {
		case 3:
			t = tableRef{ProjectID: path[0], DatasetID: path[1], TableID: path[2]}
		case 2:
			t = tableRef{ProjectID: project, DatasetID: path[0], TableID: path[1]}
		default:
			continue
		}
		if deny(t, "bigquery.models.getData") {
			return true
		}
	}
	return false
}

// sqlTableArgs are the tables table-valued functions' TABLE arguments name.
var sqlTableArgs = regexp.MustCompile("(?i)[(,]\\s*TABLE\\s+(`[^`]+`(?:\\.`[^`]+`)*|[A-Za-z_][\\w-]*(?:\\.[A-Za-z_][\\w-]*){1,2})")

// mintedCallerKey keys the account of a minted token in a request's context.
type mintedCallerKey struct{}

// withMintedCaller is ctx with the account of the request's minted token.
func withMintedCaller(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, mintedCallerKey{}, email)
}

// mintedCaller is the account of the request's minted token, "" for none.
func mintedCaller(ctx context.Context) string {
	email, _ := ctx.Value(mintedCallerKey{}).(string)
	return email
}

// checkJob checks the job's tables: a load's or a query's destination, a
// copy's sources and destination, an extract's source, a query's tables.
func (s *iamState) checkJob(r *http.Request, project string, onTable func(t tableRef, perm string) bool,
	deny func(tableRef, string) bool, viewReads func(tableRef) bool) bool {
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
	// A table-valued function's TABLE argument (ML.GENERATE_TEXT(MODEL m,
	// TABLE t)) is read as a FROM is (#1879).
	for _, m := range sqlTableArgs.FindAllStringSubmatch(sql, -1) {
		path := strings.Split(strings.ReplaceAll(m[1], "`", ""), ".")
		var t tableRef
		switch len(path) {
		case 3:
			t = tableRef{ProjectID: path[0], DatasetID: path[1], TableID: path[2]}
		case 2:
			t = tableRef{ProjectID: project, DatasetID: path[0], TableID: path[1]}
		default:
			continue
		}
		if need(&t, "bigquery.tables.getData") || viewReads(t) {
			return true
		}
	}
	for _, ix := range sqlTables.FindAllStringSubmatchIndex(sql, -1) {
		if strings.HasPrefix(strings.TrimLeft(sql[ix[1]:], " \t\r\n"), "(") {
			// A table-valued function (FROM ML.GENERATE_TEXT(...)), not a
			// table: its MODEL and TABLE arguments are checked (#1879).
			continue
		}
		m := []string{sql[ix[0]:ix[1]], sql[ix[2]:ix[3]]}
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
		if need(&t, "bigquery.tables.getData") || viewReads(t) {
			return true
		}
	}
	return false
}

// handler checks each request (check) before rest serves it. It records
// the creator of the jobs a minted caller runs, and filters a minted
// caller's jobs.list to the jobs it may see.
func (s *iamState) handler(engine, rest http.Handler) http.Handler {
	s.full = rest
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.check(w, r, engine) {
			return
		}
		email := ""
		if r.URL.Path != BigQueryIAMPath {
			email = s.caller(r)
		}
		project := pathProject(r)
		if email != "" {
			// A remote model's connection is checked for this caller
			// (connectionauth.go, #1879).
			r = r.WithContext(withMintedCaller(r.Context(), email))
		}
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

// maxViewDepth is how many views deep checkView reads.
const maxViewDepth = 16

// checkView checks the tables a view a query names reads, when t is a view
// (#1774): each needs bigquery.tables.getData of the caller, unless the
// table's dataset authorizes the view (an access entry's view naming it,
// an authorized view) or the view's dataset (an access entry's dataset,
// an authorized dataset), which grants the view's readers its rows
// ("Authorized views", "Authorized datasets",
// https://docs.cloud.google.com/bigquery/docs/authorized-views,
// https://docs.cloud.google.com/bigquery/docs/authorized-datasets). A view
// reading a view is checked the same way. It answers 403 and reports true
// when one is denied.
func (s *iamState) checkView(r *http.Request, t tableRef, depth int, onTable func(tableRef, string) bool,
	deny func(tableRef, string) bool) bool {
	if depth >= maxViewDepth || s.full == nil {
		return false
	}
	get := func(path string, v any) bool {
		rec := newRecorder()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
		if err != nil {
			return false
		}
		s.full.ServeHTTP(rec, req)
		return rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), v) == nil
	}
	var meta struct {
		Type string `json:"type"`
		View *struct {
			Query string `json:"query"`
		} `json:"view"`
	}
	if !get("/bigquery/v2/projects/"+url.PathEscape(t.ProjectID)+"/datasets/"+url.PathEscape(t.DatasetID)+"/tables/"+
		url.PathEscape(t.TableID), &meta) || meta.Type != "VIEW" || meta.View == nil {
		return false
	}
	return s.checkReads(r, "tables", t, meta.View.Query, depth, onTable, deny)
}

// checkReads checks the tables sql reads, sql being the query of the view
// or the body of the routine owner (kind "tables" or "routines"), as
// checkView says: an access entry naming owner (view, routine) or its
// dataset authorizes it.
func (s *iamState) checkReads(r *http.Request, kind string, owner tableRef, sql string, depth int,
	onTable func(tableRef, string) bool, deny func(tableRef, string) bool) bool {
	t := owner
	get := func(path string, v any) bool {
		rec := newRecorder()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
		if err != nil {
			return false
		}
		s.full.ServeHTTP(rec, req)
		return rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), v) == nil
	}
	for _, m := range sqlTables.FindAllStringSubmatch(sql, -1) {
		path := strings.Split(strings.ReplaceAll(m[1], "`", ""), ".")
		var u tableRef
		switch len(path) {
		case 3:
			u = tableRef{ProjectID: path[0], DatasetID: path[1], TableID: path[2]}
		case 2:
			u = tableRef{ProjectID: t.ProjectID, DatasetID: path[0], TableID: path[1]}
		default:
			continue
		}
		if strings.HasPrefix(strings.ToLower(u.DatasetID), "region-") || strings.EqualFold(u.DatasetID, "INFORMATION_SCHEMA") ||
			strings.EqualFold(u.TableID, "INFORMATION_SCHEMA") {
			continue
		}
		var ds struct {
			Access []struct {
				View    *tableRef `json:"view"`
				Routine *struct {
					ProjectID string `json:"projectId"`
					DatasetID string `json:"datasetId"`
					RoutineID string `json:"routineId"`
				} `json:"routine"`
				Dataset *struct {
					Dataset struct {
						ProjectID string `json:"projectId"`
						DatasetID string `json:"datasetId"`
					} `json:"dataset"`
				} `json:"dataset"`
			} `json:"access"`
		}
		authorized := false
		if get("/bigquery/v2/projects/"+url.PathEscape(u.ProjectID)+"/datasets/"+url.PathEscape(u.DatasetID), &ds) {
			for _, a := range ds.Access {
				if v := a.Routine; kind == "routines" && v != nil && v.ProjectID == t.ProjectID && v.DatasetID == t.DatasetID &&
					v.RoutineID == t.TableID {
					authorized = true
				}
				if v := a.View; kind == "tables" && v != nil && v.ProjectID == t.ProjectID && v.DatasetID == t.DatasetID && v.TableID == t.TableID {
					authorized = true
				}
				if d := a.Dataset; d != nil && d.Dataset.ProjectID == t.ProjectID && d.Dataset.DatasetID == t.DatasetID {
					authorized = true
				}
			}
		}
		if authorized {
			continue
		}
		if !onTable(u, "bigquery.tables.getData") {
			return deny(u, "bigquery.tables.getData")
		}
		if s.checkView(r, u, depth+1, onTable, deny) {
			return true
		}
	}
	return false
}

// sqlRoutines are the persistent functions and procedures a query calls:
// by a three-part name (`p.d.f`(, p.d.f(, `p`.`d`.`f`() or a two-part one
// (d.f(, the job's project's). A one-part name is not a persistent
// routine's: the emulator answers "Function not found" for a dataset's
// function called so, a default dataset or not, as Google requires a
// persistent UDF's name to be qualified with its dataset ("you must
// qualify the name with the dataset",
// https://docs.cloud.google.com/bigquery/docs/user-defined-functions, read
// 2026-11-18).
var sqlRoutines = regexp.MustCompile("(`[^`]+`(?:\\.`[^`]+`)*|[A-Za-z_][\\w-]*(?:\\.[A-Za-z_]\\w*){1,2})\\s*\\(")

// maxCallDepth is how many procedures deep checkRoutines reads.
const maxCallDepth = 8

// checkRoutines checks the routines sql calls (#1774, #1780): each needs
// bigquery.routines.get on it ("bigquery.routines.get on the dataset" to
// invoke a UDF, https://docs.cloud.google.com/bigquery/docs/user-defined-functions-python,
// read 2026-11-18), and the tables a SQL function's body or a stored
// procedure's body reads are checked as a view's (checkReads), an
// authorized routine's excepted ("You can authorize table functions,
// user-defined functions (UDFs), and stored procedures",
// https://docs.cloud.google.com/bigquery/docs/authorized-routines, read
// 2026-11-18). A procedure's body is checked for the routines it calls in
// turn. A JavaScript or Python routine's body is code, not SQL: it reads
// no table, and calling it needs bigquery.routines.get alone. A two-part
// name is resolved in the project; one that names no routine there (a
// built-in function's SAFE.f, NET.f, ...) is not a routine call.
func (s *iamState) checkRoutines(r *http.Request, next http.Handler, email, sql, project string,
	depth int, onTable func(tableRef, string) bool, deny func(tableRef, string) bool, denyIn func(string, bqResource, string) bool) bool {
	if depth >= maxCallDepth {
		return false
	}
	seen := map[bqResource]bool{}
	for _, m := range sqlRoutines.FindAllStringSubmatch(sql, -1) {
		raw := strings.ReplaceAll(m[1], "`", "")
		path := strings.Split(raw, ".")
		var res bqResource
		switch len(path) {
		case 3:
			res = bqResource{project: path[0], dataset: path[1], kind: "routines", id: path[2]}
		case 2:
			res = bqResource{project: project, dataset: path[0], kind: "routines", id: path[1]}
		default:
			continue
		}
		if seen[res] || res.project == "" || res.dataset == "" || res.id == "" {
			continue
		}
		seen[res] = true
		var def struct {
			DefinitionBody string `json:"definitionBody"`
			Language       string `json:"language"`
			RoutineType    string `json:"routineType"`
		}
		found := false
		if s.full != nil {
			rec := newRecorder()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/bigquery/v2/projects/"+url.PathEscape(res.project)+
				"/datasets/"+url.PathEscape(res.dataset)+"/routines/"+url.PathEscape(res.id), nil)
			if err == nil {
				s.full.ServeHTTP(rec, req)
				found = rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), &def) == nil
			}
		}
		if !found && len(path) != 3 {
			continue
		}
		if !s.allowed(r, next, email, res, "bigquery.routines.get") {
			return denyIn("Routine", res, "bigquery.routines.get")
		}
		if !found || (def.Language != "" && def.Language != "SQL") {
			continue
		}
		owner := tableRef{ProjectID: res.project, DatasetID: res.dataset, TableID: res.id}
		if s.checkReads(r, "routines", owner, def.DefinitionBody, 0, onTable, deny) {
			return true
		}
		if def.RoutineType == "PROCEDURE" && s.checkRoutines(r, next, email, def.DefinitionBody, res.project, depth+1,
			onTable, deny, denyIn) {
			return true
		}
	}
	return false
}

// jobSQL is the query of the job in r's body, "" for none.
func jobSQL(r *http.Request) string {
	var job struct {
		Configuration struct {
			Query *struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"configuration"`
		Query string `json:"query"`
	}
	if !decodeJob(r, &job) {
		return ""
	}
	if q := job.Configuration.Query; q != nil {
		return q.Query
	}
	return job.Query
}
