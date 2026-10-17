package bigqueryfront

// IAM policies of tables, views, routines and row access policies (#1231):
// stored and returned, never enforced, as ADR-0006 (amended by #1231)
// decides for Secret Manager, Cloud Tasks, Cloud KMS, Cloud Storage and
// Cloud Run. The emulator behind CloudBurrow has none of these methods; the
// front answers them itself.
//
//   - tables.getIamPolicy, setIamPolicy and testIamPermissions (a view is a
//     table), and routines.getIamPolicy, setIamPolicy and
//     testIamPermissions, through internal/iampolicy: a set returns a new
//     etag and one carrying a stale etag is refused 409 ABORTED; IAM
//     Conditions and audit configs are 501 naming the field; a version 3
//     policy without conditions is accepted; testIamPermissions returns
//     every requested permission on a resource that exists, and an empty
//     set on one that does not (iam_policy.proto; UNVERIFIED for BigQuery).
//   - rowAccessPolicies.getIamPolicy and testIamPermissions. The API has no
//     rowAccessPolicies.setIamPolicy: a row access policy's IAM policy is
//     its grantees, which RowAccessPolicy.grantees documents as the policy's
//     initial members, holding roles/bigquery.filteredDataViewer. So
//     getIamPolicy answers that one binding, from the grantees the front
//     already keeps (rowpolicies.go). It is the one policy here that decides
//     anything: the grantees choose which rows a query returns (#1218).
//
// Nothing reads a table's or a routine's policy: a query, a read and a call
// of the routine are allowed whatever it says.
//
// A policy goes with its resource. It is kept with the creationTime of the
// resource it was set on, and dropped when the resource is gone or was made
// again (its creationTime changed), as Google deletes it with the resource;
// a tables.delete or routines.delete through the front drops it at once.
// Policies are kept in the front's state directory (iampolicies.json), so a
// restart of the front keeps them, and an emulator restart (empty) drops
// them.

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
	"strings"
	"sync"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// filteredDataViewer is the role a row access policy's grantees hold.
const filteredDataViewer = "roles/bigquery.filteredDataViewer"

var (
	// resourceIAMRoute is a table's or a routine's IAM method.
	resourceIAMRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/(tables|routines)/([^/:]+):(getIamPolicy|setIamPolicy|testIamPermissions)$`)
	// rowPolicyIAMRoute is a row access policy's IAM method.
	rowPolicyIAMRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/tables/([^/]+)/rowAccessPolicies/([^/:]+):(getIamPolicy|setIamPolicy|testIamPermissions)$`)
	// resourceRoute is a table or a routine, for its delete.
	resourceRoute = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/(tables|routines)/([^/:]+)$`)
)

// storedIAM is one resource's policy, and the creationTime of the resource
// it was set on.
type storedIAM struct {
	Key     string           `json:"key"`
	Created string           `json:"creationTime"`
	Policy  iampolicy.Stored `json:"policy"`
}

// iamPolicies are the policies of every table and routine, by iamKey.
type iamPolicies struct {
	mu       sync.Mutex
	policies map[string]*storedIAM
	path     string
	logf     func(string, ...any)
}

// iamKey names a table ("tables") or a routine ("routines").
func iamKey(project, dataset, kind, id string) string {
	return project + "\x00" + dataset + "\x00" + kind + "\x00" + id
}

// iamPoliciesStateFile is the file the policies are kept in, in dir.
func iamPoliciesStateFile(dir string) string { return filepath.Join(dir, "iampolicies.json") }

// keep restores the policies from path, and from then on keeps them there.
func (p *iamPolicies) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []storedIAM
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the IAM policies it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the IAM policies it kept", path)
		saved = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range saved {
		if saved[i].Key != "" {
			if p.policies == nil {
				p.policies = map[string]*storedIAM{}
			}
			s := saved[i]
			p.policies[s.Key] = &s
		}
	}
	p.path, p.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	p.saveLocked()
}

func (p *iamPolicies) saveLocked() {
	if p.path == "" {
		return
	}
	saved := make([]storedIAM, 0, len(p.policies))
	for _, s := range p.policies {
		saved = append(saved, *s)
	}
	sort.Slice(saved, func(a, b int) bool { return saved[a].Key < saved[b].Key })
	b, err := json.Marshal(saved)
	if err == nil {
		err = os.WriteFile(p.path, b, 0o600)
	}
	if err != nil {
		p.logf("bigquery front: keep the IAM policies: %v", err)
	}
}

// reset drops every policy (the emulator restarted, empty).
func (p *iamPolicies) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policies = nil
	p.saveLocked()
}

// drop removes the policy of key.
func (p *iamPolicies) drop(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.policies[key]; ok {
		delete(p.policies, key)
		p.saveLocked()
	}
}

// currentLocked is key's policy for a resource made at created: nil when
// it has none, or had one on a resource of its name since gone (dropped).
func (p *iamPolicies) currentLocked(key, created string) *iampolicy.Stored {
	s, ok := p.policies[key]
	if !ok {
		return nil
	}
	if s.Created != created {
		delete(p.policies, key)
		p.saveLocked()
		return nil
	}
	pol := s.Policy
	return &pol
}

// set applies req to key's policy and stores the result.
func (p *iamPolicies) set(key, created string, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next, err := iampolicy.Set(p.currentLocked(key, created), req)
	if err != nil {
		return nil, err
	}
	if p.policies == nil {
		p.policies = map[string]*storedIAM{}
	}
	p.policies[key] = &storedIAM{Key: key, Created: created, Policy: next}
	p.saveLocked()
	return next.Proto(), nil
}

// bindings returns key's policy's bindings, for a resource made at created
// (OBJECT_PRIVILEGES, infoschemaprivileges.go).
func (p *iamPolicies) bindings(key, created string) []iampolicy.Binding {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.currentLocked(key, created); s != nil {
		return s.Bindings
	}
	return nil
}

// ids returns, sorted, the IDs of the resources of kind of the project's
// dataset that have a policy kept (OBJECT_PRIVILEGES reads those alone,
// infoschemaprivileges.go): one kept for a resource since gone or made
// again is dropped when it is next read (currentLocked).
func (p *iamPolicies) ids(project, dataset, kind string) []string {
	prefix := iamKey(project, dataset, kind, "")
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for key := range p.policies {
		if id, ok := strings.CutPrefix(key, prefix); ok && id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// get returns key's policy.
func (p *iamPolicies) get(key, created string, opts *iampb.GetPolicyOptions) (*iampb.Policy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return iampolicy.Get(p.currentLocked(key, created), opts)
}

// resourceCreated reads a table of the emulator, or a routine the front
// knows (routinedefs.go), and returns its creationTime; ok is false when it
// is not there.
func (f front) resourceCreated(r *http.Request, project, dataset, kind, id string) (string, bool) {
	if kind == "routines" {
		def, ok := f.knownRoutine(r, project, dataset, id)
		if !ok {
			return "", false
		}
		return fmt.Sprint(def["creationTime"]), true
	}
	m, ok := f.tableInfo(r, dataset, id)
	return m.Created, ok
}

// writeIAMError answers err, an apierror, in BigQuery's error body.
func writeIAMError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	reason := map[int]string{
		http.StatusBadRequest:     "invalid",
		http.StatusConflict:       "aborted",
		http.StatusNotFound:       "notFound",
		http.StatusNotImplemented: "notImplemented",
	}[e.HTTPStatus()]
	if reason == "" {
		reason = "backendError"
	}
	writeError(w, e.HTTPStatus(), reason, e.Message)
}

// writeProto answers m in its JSON mapping, as the REST API spells it.
func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backendError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// decodeProto reads r's body, which may be empty, into m.
func decodeProto(r *http.Request, m proto.Message) bool {
	if r.Header.Get("Content-Encoding") != "" {
		return false
	}
	b, err := readBody(r)
	if err != nil {
		return false
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return true
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m) == nil
}

// serveIAM answers the IAM methods of tables, routines and row access
// policies, and reports whether r was one.
func (f front) serveIAM(w http.ResponseWriter, r *http.Request, iam *iamPolicies) bool {
	path := r.URL.EscapedPath()
	if r.Method != http.MethodPost {
		return false
	}
	if m := resourceIAMRoute.FindStringSubmatch(path); m != nil {
		parts, ok := unescapeAll(m[1:5])
		if !ok {
			return false
		}
		f.base = path[:strings.Index(path, "/projects/")] + "/projects/" + m[1]
		f.serveResourceIAM(w, r, iam, parts[0], parts[1], parts[2], parts[3], m[5])
		return true
	}
	if m := rowPolicyIAMRoute.FindStringSubmatch(path); m != nil {
		parts, ok := unescapeAll(m[1:5])
		if !ok {
			return false
		}
		f.base = path[:strings.Index(path, "/projects/")] + "/projects/" + m[1]
		f.serveRowPolicyIAM(w, r, parts[0], parts[1], parts[2], parts[3], m[5])
		return true
	}
	return false
}

// deletedIAMKey is the policy key of the table or routine r deletes, or "".
func deletedIAMKey(r *http.Request) string {
	if r.Method != http.MethodDelete {
		return ""
	}
	m := resourceRoute.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil {
		return ""
	}
	parts, ok := unescapeAll(m[1:])
	if !ok {
		return ""
	}
	return iamKey(parts[0], parts[1], parts[2], parts[3])
}

func unescapeAll(in []string) ([]string, bool) {
	out := make([]string, len(in))
	for i, s := range in {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		out[i] = u
	}
	return out, true
}

// serveResourceIAM answers method on a table or a routine (kind is
// "tables" or "routines").
func (f front) serveResourceIAM(w http.ResponseWriter, r *http.Request, iam *iamPolicies, project, dataset, kind, id, method string) {
	created, found := f.resourceCreated(r, project, dataset, kind, id)
	noun := map[string]string{"tables": "Table", "routines": "Routine"}[kind]
	notFound := func() {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: %s %s:%s.%s", noun, project, dataset, id))
	}
	key := iamKey(project, dataset, kind, id)
	switch method {
	case "getIamPolicy":
		var req iampb.GetIamPolicyRequest
		if !decodeProto(r, &req) {
			writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the GetIamPolicyRequest")
			return
		}
		if !found {
			notFound()
			return
		}
		p, err := iam.get(key, created, req.GetOptions())
		if err != nil {
			writeIAMError(w, err)
			return
		}
		writeProto(w, p)
	case "setIamPolicy":
		var req iampb.SetIamPolicyRequest
		if !decodeProto(r, &req) {
			writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the SetIamPolicyRequest")
			return
		}
		if !found {
			notFound()
			return
		}
		p, err := iam.set(key, created, &req)
		if err != nil {
			writeIAMError(w, err)
			return
		}
		writeProto(w, p)
	default: // testIamPermissions
		f.answerPermissions(w, r, found)
	}
}

// answerPermissions answers testIamPermissions: every requested permission
// when the resource exists, since nothing is denied (ADR-0006 rule 4), and
// an empty set when it does not, as iam_policy.proto documents (UNVERIFIED
// for BigQuery).
func (f front) answerPermissions(w http.ResponseWriter, r *http.Request, found bool) {
	var req iampb.TestIamPermissionsRequest
	if !decodeProto(r, &req) {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the TestIamPermissionsRequest")
		return
	}
	if !found {
		writeProto(w, &iampb.TestIamPermissionsResponse{})
		return
	}
	writeProto(w, iampolicy.Permissions(&req))
}

// serveRowPolicyIAM answers method on a row access policy: its grantees as
// roles/bigquery.filteredDataViewer (above).
func (f front) serveRowPolicyIAM(w http.ResponseWriter, r *http.Request, project, dataset, table, id, method string) {
	if method == "setIamPolicy" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: bigquery.rowAccessPolicies.setIamPolicy. "+
			"BigQuery has no such method: a row access policy's members are its grantees, which rowAccessPolicies.update changes.")
		return
	}
	var pol *rowPolicy
	if _, ok := f.tableInfo(r, dataset, table); ok {
		for _, p := range f.livePolicies(r, project, dataset, table) {
			if p.Ref.PolicyID == id {
				pol = &p
				break
			}
		}
	}
	if method == "testIamPermissions" {
		f.answerPermissions(w, r, pol != nil)
		return
	}
	var req iampb.GetIamPolicyRequest
	if !decodeProto(r, &req) {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the GetIamPolicyRequest")
		return
	}
	if pol == nil {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Row access policy %s on table %s:%s.%s", id, project, dataset, table))
		return
	}
	if _, err := iampolicy.Get(nil, req.GetOptions()); err != nil {
		writeIAMError(w, err)
		return
	}
	writeProto(w, rowPolicyIAM(*pol))
}

// rowPolicyIAM is a row access policy's IAM policy: its grantees holding
// roles/bigquery.filteredDataViewer, with the policy's etag.
func rowPolicyIAM(pol rowPolicy) *iampb.Policy {
	out := &iampb.Policy{Version: 1, Etag: []byte(pol.Etag)}
	if len(out.Etag) == 0 {
		out.Etag = []byte(pol.Modified)
	}
	if len(pol.Grantees) > 0 {
		out.Bindings = []*iampb.Binding{{Role: filteredDataViewer, Members: append([]string(nil), pol.Grantees...)}}
	}
	return out
}
