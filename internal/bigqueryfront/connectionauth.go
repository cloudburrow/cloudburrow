package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// Connection permissions of remote models (#1765), as BigQuery documents
// them (re-read 2026-11-17):
//
//   - the caller needs bigquery.connections.use on the connection, to
//     create a remote model over it and to run a query of one: "Use the
//     default connection: BigQuery Connection User
//     (roles/bigquery.connectionUser)", and the role pages list the roles
//     that include it (https://docs.cloud.google.com/bigquery/docs/default-connections,
//     https://docs.cloud.google.com/iam/docs/roles-permissions/bigquery);
//   - the connection's service account needs the Vertex AI (Agent
//     Platform) User role, roles/aiplatform.user, in the project of the
//     model: "you need to grant the Agent Platform User role to the
//     connection's service account"
//     (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-remote-model).
//
// ADR-0006 is amended for this exception to its rule 1 (#1765). The
// caller is the one identity the front takes every caller to be,
// callerIdentity, as row access policies do (rowpolicies.go). A
// permission is held through an unconditioned binding, on the
// connection's own policy (the Connection API's setIamPolicy) or on the
// project's (Resource Manager), whose role includes it in
// internal/iamroles' catalogue of Google's predefined roles, or is
// roles/owner or roles/editor, to the principal as serviceAccount:,
// allUsers or allAuthenticatedUsers. Conditional bindings, custom roles,
// folders and the organization grant nothing here.
//
// UNVERIFIED (docs re-read 2026-11-17: the two pages above, which quote no
// error text): the texts of the two refusals, and that the basic roles
// Owner and Editor include bigquery.connections.use.

const (
	permConnectionsUse = "bigquery.connections.use"
	// permVertexPredict is what roles/aiplatform.user gives a
	// connection's service account for generateContent and :predict.
	permVertexPredict = "aiplatform.endpoints.predict"
)

// policyBinding is one binding of a JSON IAM policy.
type policyBinding struct {
	Role      string          `json:"role"`
	Members   []string        `json:"members"`
	Condition json.RawMessage `json:"condition"`
}

// roleGrants reports whether role includes perm.
func roleGrants(role, perm string) bool {
	if role == "roles/owner" || role == "roles/editor" {
		return true
	}
	r := iamroles.Predefined(role)
	if r == nil {
		return false
	}
	for _, p := range r.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// bindingsGrant reports whether bindings give perm to an account whose
// members (its own and its groups', principals) are members.
func bindingsGrant(bindings []policyBinding, members []string, perm string) bool {
	for _, b := range bindings {
		if len(b.Condition) > 0 && string(b.Condition) != "null" {
			continue
		}
		if !roleGrants(b.Role, perm) {
			continue
		}
		for _, m := range b.Members {
			m = strings.TrimSpace(m)
			if m == "allUsers" || m == "allAuthenticatedUsers" {
				return true
			}
			for _, x := range members {
				if strings.EqualFold(m, x) {
					return true
				}
			}
		}
	}
	return false
}

// projectBindings reads project's IAM policy from Resource Manager.
func (f front) projectBindings(ctx context.Context, project string) ([]policyBinding, int, string) {
	if f.resourceManager == "" {
		return nil, 501, "Not implemented here: a remote model's permissions are read from the project's IAM policy in Resource Manager, " +
			"which the BigQuery front was not given (enable Resource Manager). Nothing was run."
	}
	u := strings.TrimRight(f.resourceManager, "/") + "/v1/projects/" + url.PathEscape(project) + ":getIamPolicy"
	status, body, err := serviceCall(ctx, "Resource Manager projects.getIamPolicy of "+project, http.MethodPost, u,
		"application/json", []byte(`{"options":{"requestedPolicyVersion":3}}`))
	if err != nil {
		return nil, 500, "cloudburrow: could not read the project's IAM policy from Resource Manager: " + err.Error()
	}
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return nil, 0, ""
	}
	var p struct {
		Bindings []policyBinding `json:"bindings"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &p) != nil {
		return nil, 500, "cloudburrow: could not read the project's IAM policy from Resource Manager: " + errorMessage(body, status)
	}
	return p.Bindings, 0, ""
}

// serviceAccount is a CLOUD_RESOURCE connection's service
// account, or "".
func (cn *bqConnection) serviceAccount() string {
	var props struct {
		ServiceAccountID string `json:"serviceAccountId"`
	}
	_ = json.Unmarshal(cn.Fields["cloudResource"], &props)
	return props.ServiceAccountID
}

// connectionBindings are the connection's own policy's bindings.
func (cn *bqConnection) bindings() []policyBinding {
	var p struct {
		Bindings []policyBinding `json:"bindings"`
	}
	_ = json.Unmarshal(cn.Policy, &p)
	return p.Bindings
}

// connectionName is projects/{number}/locations/{location}/connections/{id}.
func (cn *bqConnection) name() string {
	return "projects/" + cn.Project + "/locations/" + cn.Location + "/connections/" + cn.ID
}

// checkConnectionUse checks the two permissions a remote model's calls
// need on connection cn of connProject, for a model in project: 0 and "",
// or the refusal's code and message. Nothing is sent to the local Vertex
// AI endpoint when it fails.
func (f front) checkConnectionUse(ctx context.Context, connProject, project string, cn *bqConnection) (int, string) {
	proj, code, msg := f.projectBindings(ctx, project)
	if code != 0 {
		return code, msg
	}
	connProj := proj
	if connProject != project {
		if connProj, code, msg = f.projectBindings(ctx, connProject); code != 0 {
			return code, msg
		}
	}
	caller := f.members(ctx, callerMemberOf(ctx, project))
	if !bindingsGrant(cn.bindings(), caller, permConnectionsUse) && !bindingsGrant(connProj, caller, permConnectionsUse) {
		return 403, fmt.Sprintf("Access Denied: Connection %s: Permission %s denied on connection %s (or it may not exist).",
			cn.name(), permConnectionsUse, cn.name())
	}
	sa := cn.serviceAccount()
	if sa == "" || !bindingsGrant(proj, f.members(ctx, "serviceAccount:"+sa), permVertexPredict) {
		return 400, fmt.Sprintf("%s does not have the permission to access or use the endpoint. Please grant the Vertex AI user role "+
			"(roles/aiplatform.user) to the %s on project %s.", sa, sa, project)
	}
	return 0, ""
}

// remoteConnection is the connection a remote model keeps, looked up
// again: it may have been deleted since.
func (f front) remoteConnection(rm *remoteModel) (*bqConnection, bool) {
	parts := strings.Split(rm.Connection, "/")
	if len(parts) != 6 {
		return nil, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, false
	}
	return f.connections.lookup(n, parts[3], parts[5])
}

// Permissions the DEFAULT connection's set-up needs: "When the service
// creates the connection through the DEFAULT keyword, the user needs the
// BigQuery Admin role" and Project IAM Admin to grant its service account
// (https://docs.cloud.google.com/bigquery/docs/default-connections, read
// 2026-11-17).
const (
	permConnectionsCreate = "bigquery.connections.create"
	permProjectSetIAM     = "resourcemanager.projects.setIamPolicy"
)

// ensureDefaultConnection makes the project's default connection in loc
// when it is missing (a CLOUD_RESOURCE connection named id, by default
// __default_cloudresource_connection__), and grants its service account
// what the resource needs, as BigQuery does (grant; nil for a remote model
// over Vertex AI: roles/aiplatform.user on the project, when it lacks it):
// each only when the caller may (the connection's create permission; the
// project's or bucket's setIamPolicy), else the refusal names what is
// missing (#1765, #2104).
func (f front) ensureDefaultConnection(ctx context.Context, project string, number int64, loc, id string, cn *bqConnection) (*bqConnection, int, string) {
	return f.ensureDefaultConnectionFor(ctx, project, number, loc, id, cn, nil)
}

// ensureDefaultConnectionFor is ensureDefaultConnection with grant, which
// grants the connection's service account sa the roles its resource needs.
func (f front) ensureDefaultConnectionFor(ctx context.Context, project string, number int64, loc, id string, cn *bqConnection,
	grant func(sa string) (int, string)) (*bqConnection, int, string) {
	proj, code, msg := f.projectBindings(ctx, project)
	if code != 0 {
		return nil, code, msg
	}
	caller := f.members(ctx, "serviceAccount:"+callerIdentity(project))
	if cn == nil {
		if !bindingsGrant(proj, caller, permConnectionsCreate) {
			return nil, 403, fmt.Sprintf("Access Denied: Project %s: The project has no default connection in %s, and making one needs "+
				"permission %s (the BigQuery Admin role) on the project.", project, loc, permConnectionsCreate)
		}
		var err error
		if cn, err = f.connections.createCloudResource(number, loc, id); err != nil {
			return nil, 500, "cloudburrow: could not make the default connection: " + err.Error()
		}
	}
	sa := cn.serviceAccount()
	if grant != nil {
		if sa != "" {
			if code, msg := grant(sa); code != 0 {
				return nil, code, msg
			}
		}
		return cn, 0, ""
	}
	if sa != "" && !bindingsGrant(proj, f.members(ctx, "serviceAccount:"+sa), permVertexPredict) && bindingsGrant(proj, caller, permProjectSetIAM) {
		if code, msg := f.grantProjectRole(ctx, project, "roles/aiplatform.user", "serviceAccount:"+sa); code != 0 {
			return nil, code, msg
		}
	}
	return cn, 0, ""
}

// grantProjectRole adds member to role on project's policy in Resource
// Manager, read and written back with its etag.
func (f front) grantProjectRole(ctx context.Context, project, role, member string) (int, string) {
	base := strings.TrimRight(f.resourceManager, "/") + "/v1/projects/" + url.PathEscape(project)
	status, body, err := serviceCall(ctx, "Resource Manager projects.getIamPolicy of "+project, http.MethodPost, base+":getIamPolicy",
		"application/json", []byte(`{"options":{"requestedPolicyVersion":3}}`))
	if err != nil || status != http.StatusOK {
		return 500, "cloudburrow: could not read the project's IAM policy to grant " + role + ": " + errorMessage(body, status)
	}
	var p map[string]any
	if json.Unmarshal(body, &p) != nil {
		return 500, "cloudburrow: the project's IAM policy is unreadable"
	}
	bindings, _ := p["bindings"].([]any)
	done := false
	for _, b := range bindings {
		m, _ := b.(map[string]any)
		if m["role"] == role && m["condition"] == nil {
			ms, _ := m["members"].([]any)
			m["members"] = append(ms, member)
			done = true
			break
		}
	}
	if !done {
		bindings = append(bindings, map[string]any{"role": role, "members": []any{member}})
	}
	p["bindings"] = bindings
	req, _ := json.Marshal(map[string]any{"policy": p})
	status, body, err = serviceCall(ctx, "Resource Manager projects.setIamPolicy of "+project, http.MethodPost, base+":setIamPolicy",
		"application/json", req)
	if err != nil || status != http.StatusOK {
		return 500, "cloudburrow: could not grant " + role + " on the project: " + errorMessage(body, status)
	}
	return 0, ""
}

// members are the policy members that name member's account: member
// itself and group:G for each Cloud Identity group it belongs to, directly
// or through nested groups (#1812), which the front asks Resource
// Manager's Cloud Identity surface for (searchTransitiveGroups). The
// accounts checked here are service accounts, which no domain: member
// names. Without Resource Manager, or when it does not answer, member
// alone: a group then grants nothing, never more.
func (f front) members(ctx context.Context, member string) []string {
	out := []string{member}
	_, email, ok := strings.Cut(member, ":")
	if !ok || f.resourceManager == "" {
		return out
	}
	q := url.Values{}
	q.Set("query", "member_key_id == '"+strings.ReplaceAll(email, "'", "")+"' && 'cloudidentity.googleapis.com/groups.discussion_forum' in labels")
	q.Set("pageSize", "1000")
	for page := 0; page < 20; page++ {
		u := strings.TrimRight(f.resourceManager, "/") + "/v1/groups/-/memberships:searchTransitiveGroups?" + q.Encode()
		status, body, err := serviceCall(ctx, "Cloud Identity searchTransitiveGroups of "+email, http.MethodGet, u, "", nil)
		if err != nil || status != http.StatusOK {
			return out
		}
		var resp struct {
			Memberships []struct {
				GroupKey struct {
					ID string `json:"id"`
				} `json:"groupKey"`
			} `json:"memberships"`
			NextPageToken string `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &resp) != nil {
			return out
		}
		for _, m := range resp.Memberships {
			out = append(out, "group:"+m.GroupKey.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		q.Set("pageToken", resp.NextPageToken)
	}
	return out
}

// callerMemberOf is the policy member a remote model's caller is checked
// as: the account of the request's minted token (iamcheck.go's caller,
// #1879), else the one identity the front takes every other caller to be.
func callerMemberOf(ctx context.Context, project string) string {
	email := mintedCaller(ctx)
	switch {
	case email == "":
		return "serviceAccount:" + callerIdentity(project)
	case strings.HasSuffix(email, ".gserviceaccount.com"):
		return "serviceAccount:" + email
	}
	return "user:" + email
}

// Default connection roles on Cloud Storage (#2104): "Cloud Storage
// BigLake tables" and "Object tables": roles/storage.legacyBucketReader and
// roles/storage.legacyObjectReader; "Apache Iceberg managed tables":
// roles/storage.legacyBucketWriter and roles/storage.legacyObjectOwner, on
// the bucket; setting them needs storage.buckets.getIamPolicy and
// storage.buckets.setIamPolicy
// (https://docs.cloud.google.com/bigquery/docs/default-connections, read
// 2026-11-21).
var (
	defaultReaderRoles  = []string{"roles/storage.legacyBucketReader", "roles/storage.legacyObjectReader"}
	defaultIcebergRoles = []string{"roles/storage.legacyBucketWriter", "roles/storage.legacyObjectOwner"}
)

const permBucketSetIAM = "storage.buckets.setIamPolicy"

// grantBucketRoles is the default connection's grant (above) of roles on
// each bucket to sa, a role it already holds there left as it is. A
// caller who may not set the bucket's policy (its project's, as the
// instance's buckets inherit) is refused, as BigQuery's statement fails
// "if you don't have administrative privileges on the Cloud Storage ...
// resource" (wording CloudBurrow's, UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/bigquery/docs/default-connections)).
func (f front) grantBucketRoles(ctx context.Context, project string, buckets []string, roles []string) func(sa string) (int, string) {
	return func(sa string) (int, string) {
		if f.storage == nil {
			return 0, ""
		}
		proj, code, msg := f.projectBindings(ctx, project)
		if code != 0 {
			return code, msg
		}
		caller := f.members(ctx, "serviceAccount:"+callerIdentity(project))
		member := "serviceAccount:" + sa
		seen := map[string]bool{}
		for _, bucket := range buckets {
			if bucket == "" || seen[bucket] {
				continue
			}
			seen[bucket] = true
			u := strings.TrimRight(f.storage.base, "/") + "/storage/v1/b/" + url.PathEscape(bucket) + "/iam"
			status, body, err := serviceCall(ctx, "Cloud Storage getIamPolicy of "+bucket, http.MethodGet, u, "", nil)
			if err != nil || status != http.StatusOK {
				if status == http.StatusNotFound {
					return 404, "Not found: Cloud Storage bucket " + bucket
				}
				return 500, "cloudburrow: could not read the IAM policy of the bucket " + bucket + ": " + errorMessage(body, status)
			}
			var p map[string]any
			if json.Unmarshal(body, &p) != nil {
				return 500, "cloudburrow: the IAM policy of the bucket " + bucket + " is unreadable"
			}
			bindings, _ := p["bindings"].([]any)
			changed := false
			for _, role := range roles {
				held := false
				for _, b := range bindings {
					m, _ := b.(map[string]any)
					if m["role"] != role || m["condition"] != nil {
						continue
					}
					ms, _ := m["members"].([]any)
					for _, x := range ms {
						held = held || x == member
					}
					if !held {
						m["members"] = append(ms, member)
						held, changed = true, true
					}
					break
				}
				if !held {
					bindings = append(bindings, map[string]any{"role": role, "members": []any{member}})
					changed = true
				}
			}
			if !changed {
				continue
			}
			if !bindingsGrant(proj, caller, permBucketSetIAM) {
				return 403, fmt.Sprintf("Access Denied: Bucket %s: granting the default connection's service account %s %s needs "+
					"permission %s on the bucket.", bucket, sa, strings.Join(roles, " and "), permBucketSetIAM)
			}
			p["bindings"] = bindings
			req, _ := json.Marshal(p)
			status, body, err = serviceCall(ctx, "Cloud Storage setIamPolicy of "+bucket, http.MethodPut, u, "application/json", req)
			if err != nil || status != http.StatusOK {
				return 500, "cloudburrow: could not grant " + strings.Join(roles, " and ") + " on the bucket " + bucket + ": " + errorMessage(body, status)
			}
		}
		return 0, ""
	}
}
