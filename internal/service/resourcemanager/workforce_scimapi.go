package resourcemanager

// The SCIM provisioning endpoint of a workforce pool's SCIM tenant (#2068):
// Google's iamscim.googleapis.com, served on the IAM API's port at the
// tenant's base path /v1alpha1/tenants/UID. Behaviour follows SCIM 2.0
// (RFC 7643 and RFC 7644) with the limits and deviations Google documents
// in "SCIM provisioning for Workforce Identity Federation"
// (docs.cloud.google.com/iam/docs/workforce-identity-federation-scim),
// "Configure SCIM in OIDC or SAML IdP" (.../iam/docs/configure-scim-oidc-saml)
// and "SCIM audit logging" (.../iam/docs/audit-logging/audit-logging-iamscim),
// read 2026-11-20:
//
//   - requests carry one of the tenant's SCIM tokens as a bearer token; a
//     missing, unknown or deleted token, or a deleted tenant, is HTTP 401
//     with a body that is not SCIM JSON (Google documents that deviation);
//   - /Users serves create, get, list, replace (PUT), PATCH and delete;
//     /Groups the same without PUT; /Schemas and /ServiceProviderConfig
//     are read only; /Me, /Bulk, /.search (Search) and /ResourceTypes are
//     not supported (501 here);
//   - filters support only eq, combined with and; no other operator, no
//     or, not or grouping;
//   - there is no pagination: startIndex is always 1, at most 100
//     resources are answered, and totalResults is the number answered;
//   - a group's get, and a list of groups without a filter on members,
//     answer an empty members list; a list filtered on members answers
//     them;
//   - the tenant's claim mapping is evaluated on every user and group:
//     google.subject (and google.group for a group) must be non-empty
//     (400 invalidValue), unique in the tenant (409 uniqueness) and never
//     change (400 mutability); a user has at most one email, of type work;
//     password is not supported and never stored; a user's groups
//     attribute is read only.
//
// A provisioned user's google.subject is a workforce pool subject
// (principal://iam.googleapis.com/locations/global/workforcePools/POOL/subject/SUBJECT),
// recorded when the user is provisioned; a group's google.group is the
// principalSet://.../group/GROUP its members are in. When the tenant's
// provider has scimUsage set, IAM reads group memberships (and, with
// ENABLED_FOR_USERS_GROUPS, custom attributes and the user's presence)
// from SCIM at every check, not from the sign-in token
// (workforce_scimauthz.go).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	scimPathPrefix  = "/v1alpha1/tenants/"
	scimUserPrefix  = "iam/wfscimusers/"
	scimGroupPrefix = "iam/wfscimgroups/"
	scimMaxResults  = 100
	scimMaxBody     = 1 << 20
	scimSubjectMax  = 127

	scimUserSchema   = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema  = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimEntSchema    = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	scimListSchema   = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema  = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema  = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimSPCSchema    = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimSchemaSchema = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimContentType  = "application/scim+json"
)

// IsScimPath is a SCIM tenant's endpoint path.
func IsScimPath(p string) bool { return strings.HasPrefix(p, scimPathPrefix) }

// scimError is a SCIM error response (RFC 7644 §3.12).
type scimError struct {
	Status int
	Type   string
	Detail string
}

func (e *scimError) Error() string { return e.Detail }

func scimErr(status int, typ, format string, a ...any) *scimError {
	return &scimError{Status: status, Type: typ, Detail: fmt.Sprintf(format, a...)}
}

func writeScim(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", scimContentType)
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeScimError(w http.ResponseWriter, e *scimError) {
	body := map[string]any{"schemas": []string{scimErrorSchema}, "status": strconv.Itoa(e.Status), "detail": e.Detail}
	if e.Type != "" {
		body["scimType"] = e.Type
	}
	writeScim(w, e.Status, body)
}

// scimResource is a stored user or group: the resource as SCIM renders
// it, and its google.subject or google.group.
type scimResource struct {
	Res map[string]any `json:"res"`
	Key string         `json:"key"`
}

// scimTenantCtx is the tenant a request authenticated to.
type scimTenantCtx struct {
	rec  *wfScimRecord
	base string // this endpoint's base URL, for meta.location
}

// scimTenantByUID is the live tenant whose base path names uid. The caller
// holds wifMu.
func (r *Registry) scimTenantByUID(uid string) *wfScimRecord {
	keys, err := r.st.List(wfScimTenantPrefix)
	if err != nil {
		return nil
	}
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TenantV != nil && rec.UID == uid {
			return rec
		}
	}
	return nil
}

// scimAuthenticate checks the bearer token against the tenant's live
// tokens. The caller holds wifMu.
func (r *Registry) scimAuthenticate(uid, authz string) *wfScimRecord {
	secret, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok {
		secret, ok = strings.CutPrefix(authz, "bearer ")
	}
	secret = strings.TrimSpace(secret)
	if !ok || secret == "" {
		return nil
	}
	t := r.scimTenantByUID(uid)
	if t == nil || t.Deleted {
		return nil
	}
	if p, err := r.wfPool(t.Pool); err != nil || p.Deleted {
		return nil
	}
	if pr, err := r.wfProvider(t.Pool, t.Provider); err != nil || pr.Deleted {
		return nil
	}
	keys, err := r.st.List(wfScimTokenPrefix + t.Pool + "/" + t.Provider + "/" + t.Tenant + "/")
	if err != nil {
		return nil
	}
	want := scimThumbprint(secret)
	for _, k := range keys {
		if rec := r.wfScimLoad(k); rec != nil && rec.TokenV != nil && !rec.Deleted && rec.Thumbprint == want {
			return t
		}
	}
	return nil
}

// ServeScim serves a SCIM tenant's endpoint.
func (r *Registry) ServeScim(w http.ResponseWriter, req *http.Request) {
	rest := strings.TrimPrefix(req.URL.Path, scimPathPrefix)
	uid, sub, _ := strings.Cut(rest, "/")
	wifMu.Lock()
	t := r.scimAuthenticate(uid, req.Header.Get("Authorization"))
	wifMu.Unlock()
	if t == nil {
		// Google answers an invalid token with 401 and a body that is not
		// SCIM JSON.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("WWW-Authenticate", `Bearer realm="iamscim"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "Unauthorized\n")
		return
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	tc := &scimTenantCtx{rec: t, base: scheme + "://" + req.Host + scimPathPrefix + uid}
	if err := r.serveScim(w, req, tc, strings.Split(strings.Trim(sub, "/"), "/")); err != nil {
		writeScimError(w, err)
	}
}

func (r *Registry) serveScim(w http.ResponseWriter, req *http.Request, tc *scimTenantCtx, seg []string) *scimError {
	kind := seg[0]
	id := ""
	if len(seg) > 1 {
		id = seg[1]
	}
	if len(seg) > 2 {
		return scimErr(http.StatusNotFound, "", "No such resource: %s.", req.URL.Path)
	}
	switch kind {
	case "Users", "Groups":
	case "ServiceProviderConfig":
		if req.Method != http.MethodGet || id != "" {
			return scimErr(http.StatusMethodNotAllowed, "", "ServiceProviderConfig is read only.")
		}
		writeScim(w, http.StatusOK, scimServiceProviderConfig(tc))
		return nil
	case "Schemas":
		if req.Method != http.MethodGet {
			return scimErr(http.StatusMethodNotAllowed, "", "Schemas are read only.")
		}
		return serveScimSchemas(w, tc, id)
	case "Me", "Bulk", ".search", "ResourceTypes":
		return scimErr(http.StatusNotImplemented, "", "The %s endpoint is not supported by Workforce Identity Federation's SCIM API.", kind)
	default:
		return scimErr(http.StatusNotFound, "", "No such resource: %s.", req.URL.Path)
	}
	if id == ".search" {
		return scimErr(http.StatusNotImplemented, "", "Search (POST .search) is not supported by Workforce Identity Federation's SCIM API.")
	}
	user := kind == "Users"
	switch {
	case req.Method == http.MethodGet && id == "":
		return r.scimList(w, req, tc, user)
	case req.Method == http.MethodGet:
		wifMu.Lock()
		res := r.scimLoad(tc.rec.UID, user, id)
		wifMu.Unlock()
		if res == nil {
			return scimNotFound(kind, id)
		}
		writeScim(w, http.StatusOK, scimView(res.Res, user, false, req))
		return nil
	case req.Method == http.MethodPost && id == "":
		body, err := scimBody(req)
		if err != nil {
			return err
		}
		out, subject, serr := r.scimCreate(tc, user, body)
		if serr != nil {
			return serr
		}
		if subject != "" {
			// The provisioned user is a workforce pool subject.
			_ = r.recordWorkforceSubject(tc.rec.Pool, subject)
		}
		writeScim(w, http.StatusCreated, scimView(out, user, true, req))
		return nil
	case req.Method == http.MethodPut && id != "":
		if !user {
			return scimErr(http.StatusNotImplemented, "", "PUT is not supported on Groups: use PATCH.")
		}
		body, err := scimBody(req)
		if err != nil {
			return err
		}
		out, serr := r.scimUpdate(tc, user, id, func(cur map[string]any) (map[string]any, *scimError) { return body, nil })
		if serr != nil {
			return serr
		}
		writeScim(w, http.StatusOK, scimView(out, user, false, req))
		return nil
	case req.Method == http.MethodPatch && id != "":
		body, err := scimBody(req)
		if err != nil {
			return err
		}
		ops, serr := scimPatchOps(body)
		if serr != nil {
			return serr
		}
		out, serr := r.scimUpdate(tc, user, id, func(cur map[string]any) (map[string]any, *scimError) {
			for _, op := range ops {
				if err := applyScimPatch(cur, op, user); err != nil {
					return nil, err
				}
			}
			return cur, nil
		})
		if serr != nil {
			return serr
		}
		writeScim(w, http.StatusOK, scimView(out, user, false, req))
		return nil
	case req.Method == http.MethodDelete && id != "":
		if err := r.scimDelete(tc, user, id); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return scimErr(http.StatusMethodNotAllowed, "", "%s is not supported on %s.", req.Method, req.URL.Path)
}

func scimNotFound(kind, id string) *scimError {
	return scimErr(http.StatusNotFound, "", "Resource %s/%s not found.", kind, id)
}

func scimBody(req *http.Request) (map[string]any, *scimError) {
	b, err := io.ReadAll(io.LimitReader(req.Body, scimMaxBody+1))
	if err != nil {
		return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "read the request: %v", err)
	}
	if len(b) > scimMaxBody {
		return nil, scimErr(http.StatusRequestEntityTooLarge, "tooLarge", "The request is larger than %d bytes.", scimMaxBody)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "The request body is not a JSON object.")
	}
	return m, nil
}

func scimKey(uid string, user bool, id string) string {
	if user {
		return scimUserPrefix + uid + "/" + id
	}
	return scimGroupPrefix + uid + "/" + id
}

// scimLoad reads a user or group. The caller holds wifMu.
func (r *Registry) scimLoad(uid string, user bool, id string) *scimResource {
	b, err := r.st.Get(scimKey(uid, user, id))
	if err != nil {
		return nil
	}
	var res scimResource
	if json.Unmarshal(b, &res) != nil || res.Res == nil {
		return nil
	}
	return &res
}

// scimAll reads every user or group of a tenant, in ID order. The caller
// holds wifMu.
func (r *Registry) scimAll(uid string, user bool) []*scimResource {
	keys, err := r.st.List(scimKey(uid, user, ""))
	if err != nil {
		return nil
	}
	sort.Strings(keys)
	out := make([]*scimResource, 0, len(keys))
	for _, k := range keys {
		b, err := r.st.Get(k)
		if err != nil {
			continue
		}
		var res scimResource
		if json.Unmarshal(b, &res) == nil && res.Res != nil {
			out = append(out, &res)
		}
	}
	return out
}

func (r *Registry) scimSave(uid string, user bool, res *scimResource) *scimError {
	b, err := json.Marshal(res)
	if err != nil {
		return scimErr(http.StatusInternalServerError, "", "encode the resource: %v", err)
	}
	if err := r.st.Put(scimKey(uid, user, scimString(res.Res["id"])), b); err != nil {
		return scimErr(http.StatusInternalServerError, "", "store the resource: %v", err)
	}
	return nil
}

// scimPurge removes a hard-deleted tenant's users and groups. The caller
// holds wifMu.
func (r *Registry) scimPurge(uid string) {
	for _, user := range []bool{true, false} {
		keys, _ := r.st.List(scimKey(uid, user, ""))
		for _, k := range keys {
			_ = r.st.Delete(k)
		}
	}
}

func scimString(v any) string {
	s, _ := v.(string)
	return s
}

// scimNormalize checks a user or group as written and keeps what SCIM
// stores: the schemas, the read-only attributes dropped, active as a
// boolean.
func scimNormalize(in map[string]any, user bool) (map[string]any, *scimError) {
	want := scimGroupSchema
	if user {
		want = scimUserSchema
	}
	schemas, _ := in["schemas"].([]any)
	found := false
	for _, s := range schemas {
		if strings.EqualFold(scimString(s), want) {
			found = true
		}
	}
	if !found {
		return nil, scimErr(http.StatusBadRequest, "invalidValue", "schemas must include %s.", want)
	}
	out := map[string]any{}
	for k, v := range in {
		switch strings.ToLower(k) {
		case "id", "meta":
			// Assigned by the service provider.
		case "password":
			// Not supported: never stored or answered.
		case "groups":
			if !user {
				out[k] = v
			}
			// A user's groups are read only (members of Groups).
		case "active":
			b, err := scimBool(v)
			if err != nil {
				return nil, err
			}
			out["active"] = b
		default:
			out[k] = v
		}
	}
	if user {
		if strings.TrimSpace(scimString(out["userName"])) == "" {
			return nil, scimErr(http.StatusBadRequest, "invalidValue", "userName is required.")
		}
		if emails, ok := out["emails"]; ok && emails != nil {
			list, isList := emails.([]any)
			if !isList || len(list) > 1 {
				return nil, scimErr(http.StatusBadRequest, "invalidValue", "A user may have at most one email address, and its type must be work.")
			}
			for _, e := range list {
				em, _ := e.(map[string]any)
				if em == nil || !strings.EqualFold(scimString(em["type"]), "work") {
					return nil, scimErr(http.StatusBadRequest, "invalidValue", "A user's email address must have type work.")
				}
			}
		}
		if _, ok := out["active"]; !ok {
			out["active"] = true
		}
	} else if strings.TrimSpace(scimString(out["displayName"])) == "" {
		return nil, scimErr(http.StatusBadRequest, "invalidValue", "displayName is required.")
	}
	return out, nil
}

func scimBool(v any) (bool, *scimError) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		if x, err := strconv.ParseBool(strings.ToLower(b)); err == nil {
			return x, nil
		}
	}
	return false, scimErr(http.StatusBadRequest, "invalidValue", "active must be a boolean.")
}

// scimClaim evaluates the tenant's mapping of key (google.subject or
// google.group) on res.
func scimClaim(mapping map[string]string, key string, res map[string]any, user bool) (string, *scimError) {
	expr := mapping[key]
	if expr == "" {
		return "", scimErr(http.StatusBadRequest, "invalidValue", "The SCIM tenant's claim mapping has no %s: groups cannot be provisioned.", key)
	}
	v, err := scimEval(expr, res, user)
	if err != nil || v == "" {
		return "", scimErr(http.StatusBadRequest, "invalidValue", "The attribute mapped to %s (%s) is null or empty.", key, expr)
	}
	if key == "google.subject" && len(v) > scimSubjectMax {
		return "", scimErr(http.StatusBadRequest, "invalidValue", "google.subject is longer than %d bytes.", scimSubjectMax)
	}
	return v, nil
}

// scimEval evaluates a claim mapping expression on a user or group, to a
// string.
func scimEval(expr string, res map[string]any, user bool) (string, error) {
	env, err := scimCELEnv()
	if err != nil {
		return "", err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return "", iss.Err()
	}
	prg, err := env.Program(ast)
	if err != nil {
		return "", err
	}
	vars := map[string]any{"user": map[string]any{}, "group": map[string]any{}}
	if user {
		vars["user"] = res
	} else {
		vars["group"] = res
	}
	v, _, err := prg.Eval(vars)
	if err != nil {
		return "", err
	}
	switch x := v.Value().(type) {
	case string:
		return x, nil
	case nil:
		return "", nil
	default:
		return fmt.Sprint(x), nil
	}
}

// scimMembers validates a group's members: each a user of the tenant. The
// caller holds wifMu.
func (r *Registry) scimMembers(uid string, res map[string]any) *scimError {
	raw, ok := res["members"]
	if !ok || raw == nil {
		return nil
	}
	list, isList := raw.([]any)
	if !isList {
		return scimErr(http.StatusBadRequest, "invalidValue", "members must be a list.")
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(list))
	for _, m := range list {
		mm, _ := m.(map[string]any)
		id := scimString(mm["value"])
		if id == "" {
			return scimErr(http.StatusBadRequest, "invalidValue", "Every member needs a value: the ID of a user.")
		}
		if r.scimLoad(uid, true, id) == nil {
			return scimErr(http.StatusBadRequest, "invalidValue", "The member %s is not a user of this SCIM tenant.", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, map[string]any{"value": id})
	}
	res["members"] = out
	return nil
}

// scimUnique reports a conflict when another user or group of the tenant
// has key. The caller holds wifMu.
func (r *Registry) scimUnique(uid string, user bool, id, key string) *scimError {
	for _, x := range r.scimAll(uid, user) {
		if x.Key == key && scimString(x.Res["id"]) != id {
			what := "google.group"
			if user {
				what = "google.subject"
			}
			return scimErr(http.StatusConflict, "uniqueness", "Another resource of this SCIM tenant has the %s %q.", what, key)
		}
	}
	return nil
}

func (r *Registry) scimCreate(tc *scimTenantCtx, user bool, body map[string]any) (map[string]any, string, *scimError) {
	res, err := scimNormalize(body, user)
	if err != nil {
		return nil, "", err
	}
	claim := "google.group"
	if user {
		claim = "google.subject"
	}
	wifMu.Lock()
	defer wifMu.Unlock()
	uid := tc.rec.UID
	key, err := scimClaim(tc.rec.TenantV.ClaimMapping, claim, res, user)
	if err != nil {
		return nil, "", err
	}
	if err := r.scimUnique(uid, user, "", key); err != nil {
		return nil, "", err
	}
	if !user {
		if err := r.scimMembers(uid, res); err != nil {
			return nil, "", err
		}
	}
	now := r.now().UTC().Format(time.RFC3339Nano)
	id := uuid.NewString()
	kind := "Group"
	if user {
		kind = "User"
	}
	res["id"] = id
	res["meta"] = map[string]any{"resourceType": kind, "created": now, "lastModified": now, "location": tc.base + "/" + kind + "s/" + id}
	if err := r.scimSave(uid, user, &scimResource{Res: res, Key: key}); err != nil {
		return nil, "", err
	}
	if user && res["active"] == true {
		return res, key, nil
	}
	return res, "", nil
}

// scimUpdate replaces or patches a user or group with change, keeping its
// id, meta.created and mapped claim.
func (r *Registry) scimUpdate(tc *scimTenantCtx, user bool, id string, change func(map[string]any) (map[string]any, *scimError)) (map[string]any, *scimError) {
	wifMu.Lock()
	defer wifMu.Unlock()
	uid := tc.rec.UID
	cur := r.scimLoad(uid, user, id)
	kind := "Groups"
	if user {
		kind = "Users"
	}
	if cur == nil {
		return nil, scimNotFound(kind, id)
	}
	meta, _ := cur.Res["meta"].(map[string]any)
	work := scimCopy(cur.Res)
	next, err := change(work)
	if err != nil {
		return nil, err
	}
	res, err := scimNormalize(next, user)
	if err != nil {
		return nil, err
	}
	claim := "google.group"
	if user {
		claim = "google.subject"
	}
	key, err := scimClaim(tc.rec.TenantV.ClaimMapping, claim, res, user)
	if err != nil {
		return nil, err
	}
	if key != cur.Key {
		return nil, scimErr(http.StatusBadRequest, "mutability", "The value mapped to %s cannot change (from %q to %q): delete the resource and create it again.", claim, cur.Key, key)
	}
	if !user {
		if err := r.scimMembers(uid, res); err != nil {
			return nil, err
		}
	}
	res["id"] = id
	m := map[string]any{}
	for k, v := range meta {
		m[k] = v
	}
	m["lastModified"] = r.now().UTC().Format(time.RFC3339Nano)
	res["meta"] = m
	if err := r.scimSave(uid, user, &scimResource{Res: res, Key: key}); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *Registry) scimDelete(tc *scimTenantCtx, user bool, id string) *scimError {
	wifMu.Lock()
	defer wifMu.Unlock()
	uid := tc.rec.UID
	kind := "Groups"
	if user {
		kind = "Users"
	}
	if r.scimLoad(uid, user, id) == nil {
		return scimNotFound(kind, id)
	}
	if err := r.st.Delete(scimKey(uid, user, id)); err != nil {
		return scimErr(http.StatusInternalServerError, "", "delete the resource: %v", err)
	}
	if user {
		// A deleted user leaves every group.
		for _, g := range r.scimAll(uid, false) {
			list, _ := g.Res["members"].([]any)
			kept := make([]any, 0, len(list))
			for _, m := range list {
				if mm, _ := m.(map[string]any); scimString(mm["value"]) != id {
					kept = append(kept, m)
				}
			}
			if len(kept) != len(list) {
				g.Res["members"] = kept
				_ = r.scimSave(uid, false, g)
			}
		}
	}
	return nil
}

func scimCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// scimView renders a resource: its schemas, and for a group members only
// when asked (a list filtered on members, or the create's answer).
func scimView(res map[string]any, user, withMembers bool, req *http.Request) map[string]any {
	out := scimCopy(res)
	if !user && !withMembers {
		out["members"] = []any{}
	}
	q := req.URL.Query()
	return scimProject(out, q.Get("attributes"), q.Get("excludedAttributes"))
}

// scimProject applies attributes and excludedAttributes (RFC 7644 §3.9)
// to top-level attributes; id, schemas and meta are always returned.
func scimProject(res map[string]any, attrs, excluded string) map[string]any {
	split := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, a := range strings.Split(s, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			if i := strings.LastIndex(a, ":"); i >= 0 && strings.HasPrefix(a, "urn:") {
				a = a[i+1:]
			}
			top, _, _ := strings.Cut(a, ".")
			out[strings.ToLower(top)] = true
		}
		return out
	}
	keep := func(k string) bool { return k == "id" || k == "schemas" || k == "meta" }
	if inc := split(attrs); len(inc) > 0 {
		for k := range res {
			if !keep(k) && !inc[strings.ToLower(k)] {
				delete(res, k)
			}
		}
	}
	for k := range split(excluded) {
		for rk := range res {
			if strings.EqualFold(rk, k) && !keep(rk) {
				delete(res, rk)
			}
		}
	}
	return res
}

// ---- list and filter

func (r *Registry) scimList(w http.ResponseWriter, req *http.Request, tc *scimTenantCtx, user bool) *scimError {
	q := req.URL.Query()
	f, err := parseScimFilter(q.Get("filter"))
	if err != nil {
		return err
	}
	limit := scimMaxResults
	if c := q.Get("count"); c != "" {
		n, perr := strconv.Atoi(c)
		if perr != nil {
			return scimErr(http.StatusBadRequest, "invalidValue", "count must be an integer.")
		}
		if n >= 0 && n < limit {
			limit = n
		}
	}
	withMembers := !user && f.mentions("members")
	wifMu.Lock()
	all := r.scimAll(tc.rec.UID, user)
	wifMu.Unlock()
	out := []any{}
	for _, x := range all {
		if len(out) >= limit {
			break
		}
		if f.match(x.Res) {
			out = append(out, scimView(x.Res, user, withMembers, req))
		}
	}
	writeScim(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(out),
		"startIndex": 1, "itemsPerPage": len(out), "Resources": out})
	return nil
}

// scimFilter is a conjunction of attrPath eq value comparisons.
type scimFilter []scimCmp

type scimCmp struct {
	path  []string
	value any
}

func (f scimFilter) mentions(attr string) bool {
	for _, c := range f {
		if strings.EqualFold(c.path[0], attr) {
			return true
		}
	}
	return false
}

func (f scimFilter) match(res map[string]any) bool {
	for _, c := range f {
		if !scimValuesMatch(scimResolve(res, c.path), c.value, scimCaseExact(c.path)) {
			return false
		}
	}
	return true
}

// scimCaseExact reports whether an attribute compares case-exactly (RFC
// 7643's caseExact): id, externalId and references do, names and
// addresses do not.
func scimCaseExact(path []string) bool {
	switch strings.ToLower(strings.Join(path, ".")) {
	case "id", "externalid", "members.value", "meta.location":
		return true
	}
	return false
}

// scimResolve collects the values at path, descending into lists.
func scimResolve(v any, path []string) []any {
	if len(path) == 0 {
		if list, ok := v.([]any); ok {
			return list
		}
		return []any{v}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if strings.EqualFold(k, path[0]) {
				return scimResolve(sub, path[1:])
			}
		}
	case []any:
		var out []any
		for _, e := range x {
			out = append(out, scimResolve(e, path)...)
		}
		return out
	}
	return nil
}

func scimValuesMatch(got []any, want any, exact bool) bool {
	for _, g := range got {
		switch w := want.(type) {
		case string:
			if s, ok := g.(string); ok && (s == w || (!exact && strings.EqualFold(s, w))) {
				return true
			}
		case nil:
			if g == nil {
				return true
			}
		default:
			if fmt.Sprint(g) == fmt.Sprint(w) {
				return true
			}
		}
	}
	return false
}

// parseScimFilter parses Google's subset of RFC 7644 §3.4.2.2 filters:
// attrPath eq value, joined with and.
func parseScimFilter(s string) (scimFilter, *scimError) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	toks, err := scimTokens(s)
	if err != nil {
		return nil, err
	}
	var f scimFilter
	i := 0
	for {
		if i+3 > len(toks) {
			return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter %q is incomplete: each comparison is attrPath eq value.", s)
		}
		attr, op, val := toks[i], toks[i+1], toks[i+2]
		if attr.quoted || op.quoted || strings.ContainsAny(attr.text, "[]()") {
			return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter %q is not supported: only attrPath eq value comparisons joined with and are.", s)
		}
		if !strings.EqualFold(op.text, "eq") {
			return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter operator %q is not supported: only eq (combined with and) is.", op.text)
		}
		path := attr.text
		if strings.HasPrefix(strings.ToLower(path), "urn:") {
			if j := strings.LastIndex(path, ":"); j >= 0 {
				prefix, rest := path[:j], path[j+1:]
				if strings.EqualFold(prefix, scimUserSchema) || strings.EqualFold(prefix, scimGroupSchema) {
					path = rest
				} else {
					path = prefix + "\x00" + rest
				}
			}
		}
		parts := strings.Split(path, ".")
		if len(parts) == 1 && strings.Contains(parts[0], "\x00") {
			// An extension attribute: urn...:attr.
			a, b, _ := strings.Cut(parts[0], "\x00")
			parts = []string{a, b}
		} else if strings.Contains(parts[0], "\x00") {
			a, b, _ := strings.Cut(parts[0], "\x00")
			parts = append([]string{a, b}, parts[1:]...)
		}
		var v any
		switch {
		case val.quoted:
			v = val.text
		case strings.EqualFold(val.text, "true"):
			v = true
		case strings.EqualFold(val.text, "false"):
			v = false
		case strings.EqualFold(val.text, "null"):
			v = nil
		default:
			n, perr := strconv.ParseFloat(val.text, 64)
			if perr != nil {
				return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter value %q is not a string, number, boolean or null.", val.text)
			}
			v = n
		}
		f = append(f, scimCmp{path: parts, value: v})
		i += 3
		if i == len(toks) {
			return f, nil
		}
		if toks[i].quoted || !strings.EqualFold(toks[i].text, "and") {
			return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter %q is not supported: comparisons can only be joined with and.", s)
		}
		i++
		if i == len(toks) {
			return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter %q ends with and.", s)
		}
	}
}

type scimToken struct {
	text   string
	quoted bool
}

func scimTokens(s string) ([]scimToken, *scimError) {
	var out []scimToken
	rs := []rune(s)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '"':
			var b strings.Builder
			i++
			closed := false
			for i < len(rs) {
				if rs[i] == '\\' && i+1 < len(rs) {
					b.WriteRune(rs[i+1])
					i += 2
					continue
				}
				if rs[i] == '"' {
					closed = true
					i++
					break
				}
				b.WriteRune(rs[i])
				i++
			}
			if !closed {
				return nil, scimErr(http.StatusBadRequest, "invalidFilter", "The filter %q has an unterminated string.", s)
			}
			out = append(out, scimToken{text: b.String(), quoted: true})
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '"' {
				j++
			}
			out = append(out, scimToken{text: string(rs[i:j])})
			i = j
		}
	}
	return out, nil
}

// ---- PATCH (RFC 7644 §3.5.2)

type scimPatchOp struct {
	op    string
	path  string
	value any
}

func scimPatchOps(body map[string]any) ([]scimPatchOp, *scimError) {
	schemas, _ := body["schemas"].([]any)
	ok := false
	for _, s := range schemas {
		if strings.EqualFold(scimString(s), scimPatchSchema) {
			ok = true
		}
	}
	if !ok {
		return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "A PATCH request's schemas must be [%q].", scimPatchSchema)
	}
	raw, _ := body["Operations"].([]any)
	if raw == nil {
		for k, v := range body {
			if strings.EqualFold(k, "operations") {
				raw, _ = v.([]any)
			}
		}
	}
	if len(raw) == 0 {
		return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "A PATCH request needs at least one operation in Operations.")
	}
	out := make([]scimPatchOp, 0, len(raw))
	for _, x := range raw {
		m, _ := x.(map[string]any)
		if m == nil {
			return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "Each operation must be a JSON object.")
		}
		op := strings.ToLower(scimString(m["op"]))
		switch op {
		case "add", "replace", "remove":
		default:
			return nil, scimErr(http.StatusBadRequest, "invalidSyntax", "The operation %q is not add, replace or remove.", scimString(m["op"]))
		}
		value, hasValue := m["value"]
		if op != "remove" && !hasValue {
			return nil, scimErr(http.StatusBadRequest, "invalidValue", "The %s operation needs a value.", op)
		}
		out = append(out, scimPatchOp{op: op, path: strings.TrimSpace(scimString(m["path"])), value: value})
	}
	return out, nil
}

// scimPath is a parsed PATCH path: attr[filter].sub, attr in the base
// object, or in an extension's object when ext is set.
type scimPath struct {
	ext    string
	attr   string
	filter scimFilter
	sub    string
}

func parseScimPath(p string, user bool) (scimPath, *scimError) {
	var out scimPath
	if strings.HasPrefix(strings.ToLower(p), "urn:") {
		core := scimGroupSchema
		if user {
			core = scimUserSchema
		}
		switch {
		case len(p) > len(core) && strings.EqualFold(p[:len(core)+1], core+":"):
			p = p[len(core)+1:]
		case user && len(p) > len(scimEntSchema) && strings.EqualFold(p[:len(scimEntSchema)+1], scimEntSchema+":"):
			out.ext, p = scimEntSchema, p[len(scimEntSchema)+1:]
		case user && strings.EqualFold(p, scimEntSchema):
			out.ext = scimEntSchema
			return out, nil
		default:
			return out, scimErr(http.StatusBadRequest, "invalidPath", "The path %q names an unknown schema.", p)
		}
	}
	if i := strings.Index(p, "["); i >= 0 {
		j := strings.LastIndex(p, "]")
		if j < i {
			return out, scimErr(http.StatusBadRequest, "invalidPath", "The path %q has an unterminated filter.", p)
		}
		f, err := parseScimFilter(p[i+1 : j])
		if err != nil {
			return out, scimErr(http.StatusBadRequest, "invalidPath", "The path %q has an unsupported filter: %s", p, err.Detail)
		}
		out.attr, out.filter = p[:i], f
		out.sub = strings.TrimPrefix(p[j+1:], ".")
	} else {
		out.attr, out.sub, _ = strings.Cut(p, ".")
	}
	if out.attr == "" {
		return out, scimErr(http.StatusBadRequest, "invalidPath", "The path %q names no attribute.", p)
	}
	return out, nil
}

// scimField finds a key of m case-insensitively; "" when absent.
func scimField(m map[string]any, name string) string {
	for k := range m {
		if strings.EqualFold(k, name) {
			return k
		}
	}
	return ""
}

func applyScimPatch(res map[string]any, op scimPatchOp, user bool) *scimError {
	if op.path == "" {
		if op.op == "remove" {
			return scimErr(http.StatusBadRequest, "noTarget", "A remove operation needs a path.")
		}
		m, ok := op.value.(map[string]any)
		if !ok {
			return scimErr(http.StatusBadRequest, "invalidValue", "An %s operation without a path needs an object value.", op.op)
		}
		for k, v := range m {
			if sub, isMap := v.(map[string]any); isMap && strings.EqualFold(k, scimEntSchema) {
				for kk, vv := range sub {
					if err := applyScimPatch(res, scimPatchOp{op: op.op, path: k + ":" + kk, value: vv}, user); err != nil {
						return err
					}
				}
				continue
			}
			if err := applyScimPatch(res, scimPatchOp{op: op.op, path: k, value: v}, user); err != nil {
				return err
			}
		}
		return nil
	}
	p, err := parseScimPath(op.path, user)
	if err != nil {
		return err
	}
	base := res
	if p.ext != "" {
		k := scimField(res, p.ext)
		if k == "" {
			k = p.ext
			res[k] = map[string]any{}
			addScimSchema(res, p.ext)
		}
		m, _ := res[k].(map[string]any)
		if m == nil {
			m = map[string]any{}
			res[k] = m
		}
		base = m
		if p.attr == "" {
			// The extension object itself.
			if v, ok := op.value.(map[string]any); ok && op.op != "remove" {
				for kk, vv := range v {
					m[kk] = vv
				}
				return nil
			}
			if op.op == "remove" {
				delete(res, k)
				return nil
			}
			return scimErr(http.StatusBadRequest, "invalidValue", "The extension %s needs an object value.", p.ext)
		}
	}
	key := scimField(base, p.attr)
	if key == "" {
		key = p.attr
	}
	switch strings.ToLower(key) {
	case "id", "meta", "schemas":
		return scimErr(http.StatusBadRequest, "mutability", "%s is read only.", key)
	}
	cur, exists := base[key]
	if p.filter != nil {
		list, _ := cur.([]any)
		matched := false
		var kept []any
		for _, e := range list {
			em, _ := e.(map[string]any)
			if em == nil || !p.filter.match(em) {
				kept = append(kept, e)
				continue
			}
			matched = true
			switch {
			case op.op == "remove" && p.sub == "":
				// Dropped.
			case op.op == "remove":
				delete(em, scimFieldOr(em, p.sub))
				kept = append(kept, em)
			case p.sub == "":
				if v, ok := op.value.(map[string]any); ok {
					for kk, vv := range v {
						em[kk] = vv
					}
				}
				kept = append(kept, em)
			default:
				em[scimFieldOr(em, p.sub)] = op.value
				kept = append(kept, em)
			}
		}
		if !matched {
			if op.op == "remove" {
				return nil
			}
			// No element matched: add one with the filter's values, as
			// identity providers expect for emails[type eq "work"].value.
			em := map[string]any{}
			for _, c := range p.filter {
				if len(c.path) == 1 {
					em[c.path[0]] = c.value
				}
			}
			if p.sub != "" {
				em[p.sub] = op.value
			} else if v, ok := op.value.(map[string]any); ok {
				for kk, vv := range v {
					em[kk] = vv
				}
			}
			kept = append(kept, em)
		}
		if kept == nil {
			kept = []any{}
		}
		base[key] = kept
		return nil
	}
	if p.sub != "" {
		m, _ := cur.(map[string]any)
		if m == nil {
			if op.op == "remove" {
				return nil
			}
			m = map[string]any{}
			base[key] = m
		}
		if op.op == "remove" {
			delete(m, scimFieldOr(m, p.sub))
		} else {
			m[scimFieldOr(m, p.sub)] = op.value
		}
		return nil
	}
	switch op.op {
	case "remove":
		if !exists {
			return nil
		}
		if vals, ok := op.value.([]any); ok {
			// Remove the listed values of a multi-valued attribute (how
			// Entra ID removes group members).
			list, _ := cur.([]any)
			var kept []any
			for _, e := range list {
				drop := false
				for _, v := range vals {
					if scimSameValue(e, v) {
						drop = true
					}
				}
				if !drop {
					kept = append(kept, e)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			base[key] = kept
			return nil
		}
		delete(base, key)
	case "add":
		if list, ok := cur.([]any); ok && exists {
			add, isList := op.value.([]any)
			if !isList {
				add = []any{op.value}
			}
			for _, v := range add {
				dup := false
				for _, e := range list {
					if scimSameValue(e, v) {
						dup = true
					}
				}
				if !dup {
					list = append(list, v)
				}
			}
			base[key] = list
			return nil
		}
		if m, ok := cur.(map[string]any); ok {
			if v, isMap := op.value.(map[string]any); isMap {
				for kk, vv := range v {
					m[kk] = vv
				}
				return nil
			}
		}
		base[key] = scimAsList(key, op.value)
	case "replace":
		if m, ok := cur.(map[string]any); ok {
			if v, isMap := op.value.(map[string]any); isMap {
				for kk, vv := range v {
					m[scimFieldOr(m, kk)] = vv
				}
				return nil
			}
		}
		base[key] = scimAsList(key, op.value)
	}
	return nil
}

// scimAsList wraps a single value written to a multi-valued attribute.
func scimAsList(key string, v any) any {
	switch strings.ToLower(key) {
	case "members", "emails", "phonenumbers", "addresses", "photos", "ims", "entitlements", "roles", "x509certificates":
		if _, ok := v.([]any); !ok && v != nil {
			return []any{v}
		}
	}
	return v
}

func scimFieldOr(m map[string]any, name string) string {
	if k := scimField(m, name); k != "" {
		return k
	}
	return name
}

// scimSameValue compares multi-valued attribute elements by their value
// sub-attribute, or entirely.
func scimSameValue(a, b any) bool {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	if am != nil && bm != nil {
		av, bv := scimString(am["value"]), scimString(bm["value"])
		if av != "" || bv != "" {
			return av == bv
		}
	}
	return reflect.DeepEqual(a, b)
}

func addScimSchema(res map[string]any, urn string) {
	list, _ := res["schemas"].([]any)
	for _, s := range list {
		if strings.EqualFold(scimString(s), urn) {
			return
		}
	}
	res["schemas"] = append(list, urn)
}
