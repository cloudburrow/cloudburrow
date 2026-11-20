package iameval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The eleventh round's checks (#1879): Resource Manager v3, the IAM API
// (roles, service accounts and their keys and policies), IAM v2 deny
// policies, the Organization Policy API v2, and every Eventarc and
// Eventarc Publishing method.

// Check is one permission on one resource.
type Check struct {
	Permission, Resource string
	// Service, when set, is the table the check is made in (another
	// service's resource, round 12); nil is the method's own.
	Service *Service
}

// extra runs m's Extra checks on resource.
func (e *Enforcer) extra(ctx context.Context, svc *Service, m *Method, bearer, resource string) error {
	if m.Extra == nil {
		return nil
	}
	parentOf := e.ParentOf
	if parentOf == nil {
		parentOf = func(string) string { return "" }
	}
	for _, c := range m.Extra(resource, parentOf) {
		if c.Permission == "" || c.Resource == "" {
			continue
		}
		in := svc
		if c.Service != nil {
			in = c.Service
		}
		if err := e.Authorize(ctx, in, bearer, c.Permission, c.Resource); err != nil {
			return err
		}
	}
	return nil
}

// visible reports whether the caller of bearer holds permission on
// resource; err is set only for a token this instance did not mint.
func (e *Enforcer) visible(ctx context.Context, svc *Service, bearer, permission, resource string) (bool, error) {
	err := e.Authorize(ctx, svc, bearer, permission, resource)
	if status.Code(err) == codes.Unauthenticated {
		return false, err
	}
	return err == nil, nil
}

// filterGRPC serves a Filter method: its answer keeps the resources the
// caller holds m.Permission on.
func (e *Enforcer) filterGRPC(ctx context.Context, svc *Service, m *Method, req any, handler grpc.UnaryHandler) (any, error) {
	b := bearer(ctx)
	if _, err := e.visible(ctx, svc, b, m.Permission, ""); status.Code(err) == codes.Unauthenticated {
		return nil, err
	}
	resp, err := handler(ctx, req)
	msg, ok := resp.(proto.Message)
	if err != nil || !ok {
		return resp, err
	}
	pm := msg.ProtoReflect()
	fd := pm.Descriptor().Fields().ByName(protoreflect.Name(m.Filter))
	if fd == nil || !fd.IsList() || fd.Kind() != protoreflect.MessageKind {
		return resp, nil
	}
	list := pm.Mutable(fd).List()
	kept := pm.NewField(fd).List()
	for i := 0; i < list.Len(); i++ {
		item := list.Get(i).Message()
		nf := item.Descriptor().Fields().ByName("name")
		if nf == nil {
			continue
		}
		ok, err := e.visible(ctx, svc, b, m.Permission, item.Get(nf).String())
		if err != nil {
			return nil, err
		}
		if ok {
			kept.Append(list.Get(i))
		}
	}
	pm.Set(fd, protoreflect.ValueOfList(kept))
	return resp, nil
}

// filterJSON serves a Filter method over JSON, as filterGRPC does.
func (e *Enforcer) filterJSON(w http.ResponseWriter, r *http.Request, svc *Service, m *Method, next http.Handler) {
	b := Bearer(r.Header.Get("Authorization"))
	if _, err := e.visible(r.Context(), svc, b, m.Permission, ""); err != nil {
		WriteJSONStatus(w, err)
		return
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	if rec.Code == http.StatusOK {
		if out, err := e.filterBody(r.Context(), svc, m, b, body); err != nil {
			WriteJSONStatus(w, err)
			return
		} else if out != nil {
			body = out
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

// filterBody is a JSON answer with only the visible resources of its
// Filter field, nil to keep it as it is.
func (e *Enforcer) filterBody(ctx context.Context, svc *Service, m *Method, bearer string, body []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, nil
	}
	key := lowerCamel(m.Filter)
	raw, ok := doc[key]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, nil
	}
	kept := []json.RawMessage{}
	for _, it := range items {
		var named struct {
			Name      string `json:"name"`
			ProjectID string `json:"projectId"`
		}
		_ = json.Unmarshal(it, &named)
		name := named.Name
		if named.ProjectID != "" && !strings.HasPrefix(name, "projects/") {
			// Resource Manager v1's projects are named by their ID; their
			// name is the display name.
			name = "projects/" + named.ProjectID
		}
		ok, err := e.visible(ctx, svc, bearer, m.Permission, name)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 {
		delete(doc, key)
	} else {
		doc[key], _ = json.Marshal(kept)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// methodRows are a table's methods: each row's permission and request field
// under prefix.
func methodRows(prefix string, in map[string]Method) map[string]Method {
	out := make(map[string]Method, len(in))
	for name, m := range in {
		out[prefix+name] = m
	}
	return out
}

// HierarchyNode is the Resource Manager resource a full resource name
// ("//cloudresourcemanager.googleapis.com/projects/123") names
// ("projects/123"), "" for another service's resource.
func HierarchyNode(full string) string {
	rel, ok := strings.CutPrefix(full, "//cloudresourcemanager.googleapis.com/")
	if !ok {
		return ""
	}
	return rel
}

// tagBindingChecks are a tag binding's name's checks
// ("tagBindings/{url-encoded resource}/tagValues/{id}"): permission on the
// tag value, and node on the resource when it is Resource Manager's.
func tagBindingChecks(name, value, node string) []Check {
	rest, ok := strings.CutPrefix(name, "tagBindings/")
	if !ok {
		return nil
	}
	i := strings.LastIndex(rest, "/tagValues/")
	if i < 0 {
		return nil
	}
	full, err := url.PathUnescape(rest[:i])
	if err != nil {
		return nil
	}
	out := []Check{{Permission: value, Resource: "tagValues/" + rest[i+len("/tagValues/"):]}}
	if n := HierarchyNode(full); n != "" {
		out = append(out, Check{Permission: node, Resource: n})
	}
	return out
}

// rmDeny is Resource Manager's refusal: "Permission 'P' denied on resource
// '//cloudresourcemanager.googleapis.com/R' (or it may not exist).", as
// gcloud reports a v3 call's (UNVERIFIED, re-read 2026-11-20: the
// reference pages give no wording), with ErrorInfo IAM_PERMISSION_DENIED.
func rmDeny(permission, resource string) error {
	full := resource
	if !strings.HasPrefix(full, "//") {
		full = "//cloudresourcemanager.googleapis.com/" + resource
	}
	return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, full),
		permission, full, true)
}

// ResourceManager is Resource Manager v3's table: each method's
// permission from its REST reference
// (https://docs.cloud.google.com/resource-manager/reference/rest/v3, read
// 2026-11-20), or, where a page names none (tag bindings, tag holds,
// getNamespaced), the permission of its name in "Resource Manager roles
// and permissions" (https://docs.cloud.google.com/iam/docs/roles-permissions/resourcemanager).
// projects.move needs resourcemanager.projects.move "on the project, on
// the project's current and proposed new parent"; folders.move
// resourcemanager.folders.move "on the folder's current and proposed new
// parent". A search answers only what the caller may get. Tag keys and
// values keep their own policies, a tag value's key's applying to it, and
// the key's parent's ancestry above both.
var ResourceManager = &Service{
	Name:              "cloudresourcemanager.googleapis.com",
	PolicyCollections: []string{"tagKeys", "tagValues"},
	Types: map[string]string{"folders": "cloudresourcemanager.googleapis.com/Folder",
		"organizations": "cloudresourcemanager.googleapis.com/Organization",
		"tagKeys":       "cloudresourcemanager.googleapis.com/TagKey", "tagValues": "cloudresourcemanager.googleapis.com/TagValue"},
	Methods: rmMethods(),
	Deny:    rmDeny,
}

func rmMethods() map[string]Method {
	out := map[string]Method{}
	add := func(svc string, m map[string]Method) {
		for k, v := range methodRows("/google.cloud.resourcemanager.v3."+svc+"/", m) {
			out[k] = v
		}
	}
	add("Projects", map[string]Method{
		"GetProject":     {Permission: "resourcemanager.projects.get", Field: "name"},
		"ListProjects":   {Permission: "resourcemanager.projects.list", Field: "parent"},
		"SearchProjects": {Permission: "resourcemanager.projects.get", Filter: "projects"},
		// A project with no parent is any caller's to create.
		"CreateProject":   {Permission: "resourcemanager.projects.create", Field: "project.parent", Optional: true},
		"UpdateProject":   {Permission: "resourcemanager.projects.update", Field: "project.name"},
		"DeleteProject":   {Permission: "resourcemanager.projects.delete", Field: "name"},
		"UndeleteProject": {Permission: "resourcemanager.projects.undelete", Field: "name"},
		"MoveProject": {Permission: "resourcemanager.projects.move", Field: "name",
			Also: []Also{{Field: "destination_parent", Permission: "resourcemanager.projects.move"}},
			Extra: func(name string, parentOf func(string) string) []Check {
				return []Check{{Permission: "resourcemanager.projects.move", Resource: parentOf(name)}}
			}},
		"GetIamPolicy": {Permission: "resourcemanager.projects.getIamPolicy", Field: "resource"},
		"SetIamPolicy": {Permission: "resourcemanager.projects.setIamPolicy", Field: "resource"},
	})
	add("Folders", map[string]Method{
		"GetFolder":      {Permission: "resourcemanager.folders.get", Field: "name"},
		"ListFolders":    {Permission: "resourcemanager.folders.list", Field: "parent"},
		"SearchFolders":  {Permission: "resourcemanager.folders.get", Filter: "folders"},
		"CreateFolder":   {Permission: "resourcemanager.folders.create", Field: "folder.parent"},
		"UpdateFolder":   {Permission: "resourcemanager.folders.update", Field: "folder.name"},
		"DeleteFolder":   {Permission: "resourcemanager.folders.delete", Field: "name"},
		"UndeleteFolder": {Permission: "resourcemanager.folders.undelete", Field: "name"},
		// On the current and the new parent, not the folder itself.
		"MoveFolder": {Field: "name",
			Also: []Also{{Field: "destination_parent", Permission: "resourcemanager.folders.move"}},
			Extra: func(name string, parentOf func(string) string) []Check {
				return []Check{{Permission: "resourcemanager.folders.move", Resource: parentOf(name)}}
			}},
		"GetIamPolicy": {Permission: "resourcemanager.folders.getIamPolicy", Field: "resource"},
		"SetIamPolicy": {Permission: "resourcemanager.folders.setIamPolicy", Field: "resource"},
	})
	add("Organizations", map[string]Method{
		"GetOrganization":     {Permission: "resourcemanager.organizations.get", Field: "name"},
		"SearchOrganizations": {Permission: "resourcemanager.organizations.get", Filter: "organizations"},
		"GetIamPolicy":        {Permission: "resourcemanager.organizations.getIamPolicy", Field: "resource"},
		"SetIamPolicy":        {Permission: "resourcemanager.organizations.setIamPolicy", Field: "resource"},
	})
	add("TagKeys", map[string]Method{
		"ListTagKeys":         {Permission: "resourcemanager.tagKeys.list", Field: "parent"},
		"GetTagKey":           {Permission: "resourcemanager.tagKeys.get", Field: "name"},
		"GetNamespacedTagKey": {Permission: "resourcemanager.tagKeys.get", Field: "name"},
		"CreateTagKey":        {Permission: "resourcemanager.tagKeys.create", Field: "tag_key.parent"},
		"UpdateTagKey":        {Permission: "resourcemanager.tagKeys.update", Field: "tag_key.name"},
		"DeleteTagKey":        {Permission: "resourcemanager.tagKeys.delete", Field: "name"},
		"GetIamPolicy":        {Permission: "resourcemanager.tagKeys.getIamPolicy", Field: "resource"},
		"SetIamPolicy":        {Permission: "resourcemanager.tagKeys.setIamPolicy", Field: "resource"},
	})
	add("TagValues", map[string]Method{
		"ListTagValues":         {Permission: "resourcemanager.tagValues.list", Field: "parent"},
		"GetTagValue":           {Permission: "resourcemanager.tagValues.get", Field: "name"},
		"GetNamespacedTagValue": {Permission: "resourcemanager.tagValues.get", Field: "name"},
		"CreateTagValue":        {Permission: "resourcemanager.tagValues.create", Field: "tag_value.parent"},
		"UpdateTagValue":        {Permission: "resourcemanager.tagValues.update", Field: "tag_value.name"},
		"DeleteTagValue":        {Permission: "resourcemanager.tagValues.delete", Field: "name"},
		"GetIamPolicy":          {Permission: "resourcemanager.tagValues.getIamPolicy", Field: "resource"},
		"SetIamPolicy":          {Permission: "resourcemanager.tagValues.setIamPolicy", Field: "resource"},
	})
	// A binding's resource is a full resource name: Resource Manager's
	// own (a project, folder or organization) is checked with
	// resourcemanager.hierarchyNodes.*; another service's resource with
	// that service's own permission (round12_tags.go).
	node := func(permission string) func(string) string {
		return func(full string) string {
			if HierarchyNode(full) == "" {
				return ""
			}
			return permission
		}
	}
	other := func(verb string) func(string, func(string) string) []Check {
		return func(full string, _ func(string) string) []Check { return tagResourceChecks(full, verb) }
	}
	add("TagBindings", map[string]Method{
		"ListTagBindings": {Field: "parent", PermissionFor: node("resourcemanager.hierarchyNodes.listTagBindings"),
			Extra: other("listTagBindings")},
		"ListEffectiveTags": {Field: "parent", PermissionFor: node("resourcemanager.hierarchyNodes.listEffectiveTags"),
			Extra: other("listEffectiveTags")},
		"CreateTagBinding": {Field: "tag_binding.parent", PermissionFor: node("resourcemanager.hierarchyNodes.createTagBinding"),
			Also: []Also{{Field: "tag_binding.tag_value", Permission: "resourcemanager.tagValueBindings.create"},
				{Field: "tag_binding.tag_value_namespaced_name", Permission: "resourcemanager.tagValueBindings.create"}},
			Extra: other("createTagBinding")},
		"DeleteTagBinding": {Field: "name", Extra: func(name string, _ func(string) string) []Check {
			return append(tagBindingChecks(name, "resourcemanager.tagValueBindings.delete", "resourcemanager.hierarchyNodes.deleteTagBinding"),
				tagResourceChecks(tagBindingResource(name), "deleteTagBinding")...)
		}},
	})
	add("TagHolds", map[string]Method{
		"CreateTagHold": {Permission: "resourcemanager.tagHolds.create", Field: "parent"},
		"ListTagHolds":  {Permission: "resourcemanager.tagHolds.list", Field: "parent"},
		"DeleteTagHold": {Permission: "resourcemanager.tagHolds.delete", Field: "name"},
	})
	return out
}

// iamDeny is the IAM API's refusal, "Permission 'P' denied on resource (or
// it may not exist).", which names no resource, with ErrorInfo
// IAM_PERMISSION_DENIED (UNVERIFIED, re-read 2026-11-20: the
// reference pages give no wording).
func iamDeny(permission, resource string) error {
	return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource (or it may not exist).", permission), permission, resource, true)
}

// customRole is permission for a custom role's name ("projects/P/roles/R",
// "organizations/O/roles/R"), "" for a predefined role's ("roles/R"), which
// every caller may read.
func customRole(permission string) func(string) string {
	return func(name string) string {
		if strings.HasPrefix(name, "projects/") || strings.HasPrefix(name, "organizations/") {
			return permission
		}
		return ""
	}
}

// iamMethods are the IAM API v1's methods, each with the permission its
// REST reference names (https://docs.cloud.google.com/iam/docs/reference/rest,
// read 2026-11-20) or, where a page names none (a service account's
// getIamPolicy and setIamPolicy), the permission of its name in "IAM
// roles and permissions". A service account's getIamPolicy and
// setIamPolicy are checked on the service account, whose own policy and
// its project's ancestry apply. roles.list of the predefined roles "doesn't
// require a resource", nor does reading a predefined role.
// queryGrantableRoles, queryTestablePermissions, queryAuditableServices
// and lintPolicy name no permission and are not checked.
func iamMethods() map[string]Method {
	return methodRows("/google.iam.admin.v1.IAM/", map[string]Method{
		"ListServiceAccounts":      {Permission: "iam.serviceAccounts.list", Field: "name"},
		"GetServiceAccount":        {Permission: "iam.serviceAccounts.get", Field: "name"},
		"CreateServiceAccount":     {Permission: "iam.serviceAccounts.create", Field: "name"},
		"UpdateServiceAccount":     {Permission: "iam.serviceAccounts.update", Field: "name"},
		"PatchServiceAccount":      {Permission: "iam.serviceAccounts.update", Field: "service_account.name"},
		"DeleteServiceAccount":     {Permission: "iam.serviceAccounts.delete", Field: "name"},
		"UndeleteServiceAccount":   {Permission: "iam.serviceAccounts.undelete", Field: "name"},
		"EnableServiceAccount":     {Permission: "iam.serviceAccounts.enable", Field: "name"},
		"DisableServiceAccount":    {Permission: "iam.serviceAccounts.disable", Field: "name"},
		"SignBlob":                 {Permission: "iam.serviceAccounts.signBlob", Field: "name"},
		"SignJwt":                  {Permission: "iam.serviceAccounts.signJwt", Field: "name"},
		"GetIamPolicy":             {Permission: "iam.serviceAccounts.getIamPolicy", Field: "resource"},
		"SetIamPolicy":             {Permission: "iam.serviceAccounts.setIamPolicy", Field: "resource"},
		"ListServiceAccountKeys":   {Permission: "iam.serviceAccountKeys.list", Field: "name"},
		"GetServiceAccountKey":     {Permission: "iam.serviceAccountKeys.get", Field: "name"},
		"CreateServiceAccountKey":  {Permission: "iam.serviceAccountKeys.create", Field: "name"},
		"UploadServiceAccountKey":  {Permission: "iam.serviceAccountKeys.create", Field: "name"},
		"DeleteServiceAccountKey":  {Permission: "iam.serviceAccountKeys.delete", Field: "name"},
		"DisableServiceAccountKey": {Permission: "iam.serviceAccountKeys.disable", Field: "name"},
		"EnableServiceAccountKey":  {Permission: "iam.serviceAccountKeys.enable", Field: "name"},
		"ListRoles":                {Permission: "iam.roles.list", Field: "parent", Optional: true},
		"GetRole":                  {Field: "name", PermissionFor: customRole("iam.roles.get")},
		"CreateRole":               {Permission: "iam.roles.create", Field: "parent"},
		"UpdateRole":               {Permission: "iam.roles.update", Field: "name"},
		"DeleteRole":               {Permission: "iam.roles.delete", Field: "name"},
		"UndeleteRole":             {Permission: "iam.roles.undelete", Field: "name"},
	})
}

// DenyAttachment is the resource a deny policy's name or parent
// ("policies/cloudresourcemanager.googleapis.com%2Fprojects%2FP/denypolicies[/ID]")
// is attached to ("projects/P"), "" for another.
func DenyAttachment(name string) string {
	rest, ok := strings.CutPrefix(name, "policies/")
	if !ok {
		return ""
	}
	enc, _, _ := strings.Cut(rest, "/")
	point, err := url.PathUnescape(enc)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(point, "cloudresourcemanager.googleapis.com/")
}

// DenyPolicies is IAM v2's table of deny policies: iam.denypolicies.* on
// the policy's attachment point, the organization, folder or project
// ("Deny access to resources", https://docs.cloud.google.com/iam/docs/deny-access,
// read 2026-11-20). None of these permissions can itself be denied, so an
// owner who denied itself a permission can always delete the policy.
var DenyPolicies = &Service{
	Name:  "iam.googleapis.com",
	Types: map[string]string{"denypolicies": "iam.googleapis.com/DenyPolicy"},
	Methods: methodRows("/google.iam.v2.Policies/", map[string]Method{
		"ListPolicies": {Permission: "iam.denypolicies.list", Field: "parent"},
		"GetPolicy":    {Permission: "iam.denypolicies.get", Field: "name"},
		"CreatePolicy": {Permission: "iam.denypolicies.create", Field: "parent"},
		"UpdatePolicy": {Permission: "iam.denypolicies.update", Field: "policy.name"},
		"DeletePolicy": {Permission: "iam.denypolicies.delete", Field: "name"},
	}),
	Deny: newerDeny,
}

// OrgPolicy is the Organization Policy API v2's table, each method's
// permission from its REST reference
// (https://docs.cloud.google.com/organization-policy/reference/rest/v2,
// read 2026-11-20). policies.get and getEffectivePolicy name
// orgpolicy.policies.get, which Google's permission index lacks; the
// index's orgpolicy.policy.get (Organization Policy Viewer's) is checked.
var OrgPolicy = &Service{
	Name: "orgpolicy.googleapis.com",
	Methods: methodRows("/google.cloud.orgpolicy.v2.OrgPolicy/", map[string]Method{
		"ListConstraints":        {Permission: "orgpolicy.constraints.list", Field: "parent"},
		"ListPolicies":           {Permission: "orgpolicy.policies.list", Field: "parent"},
		"GetPolicy":              {Permission: "orgpolicy.policy.get", Field: "name"},
		"GetEffectivePolicy":     {Permission: "orgpolicy.policy.get", Field: "name"},
		"CreatePolicy":           {Permission: "orgpolicy.policies.create", Field: "parent"},
		"UpdatePolicy":           {Permission: "orgpolicy.policies.update", Field: "policy.name"},
		"DeletePolicy":           {Permission: "orgpolicy.policies.delete", Field: "name"},
		"CreateCustomConstraint": {Permission: "orgpolicy.customConstraints.create", Field: "parent"},
		"UpdateCustomConstraint": {Permission: "orgpolicy.customConstraints.update", Field: "custom_constraint.name"},
		"GetCustomConstraint":    {Permission: "orgpolicy.customConstraints.get", Field: "name"},
		"ListCustomConstraints":  {Permission: "orgpolicy.customConstraints.list", Field: "parent"},
		"DeleteCustomConstraint": {Permission: "orgpolicy.customConstraints.delete", Field: "name"},
	}),
	Deny: newerDeny,
}

// eventarcMethods are Eventarc's methods, each with the permission its
// REST reference names
// (https://docs.cloud.google.com/eventarc/docs/reference/rest/v1, read
// 2026-11-20: eventarc.COLLECTION.VERB on parent for a list or a create,
// on the resource otherwise; listEnrollments of a message bus
// eventarc.messageBuses.get), or, where a page names none (the Google
// channel config, the Publishing API), the permission of its name in
// "Eventarc roles and permissions" (https://docs.cloud.google.com/eventarc/docs/access-control):
// eventarc.googleChannelConfigs.get and .update, and
// eventarc.messageBuses.publish, eventarc.channels.publish and
// eventarc.channelConnections.publish.
func eventarcMethods() map[string]Method {
	out := map[string]Method{}
	for _, c := range []struct{ coll, msg, field string }{
		{"Trigger", "triggers", "trigger"}, {"Channel", "channels", "channel"},
		{"ChannelConnection", "channelConnections", "channel_connection"}, {"MessageBus", "messageBuses", "message_bus"},
		{"Enrollment", "enrollments", "enrollment"}, {"Pipeline", "pipelines", "pipeline"},
		{"GoogleApiSource", "googleApiSources", "google_api_source"},
	} {
		p := "/google.cloud.eventarc.v1.Eventarc/"
		plural := c.coll + "s"
		if c.coll == "MessageBus" {
			plural = "MessageBuses"
		}
		out[p+"Get"+c.coll] = Method{Permission: "eventarc." + c.msg + ".get", Field: "name"}
		out[p+"List"+plural] = Method{Permission: "eventarc." + c.msg + ".list", Field: "parent"}
		out[p+"Create"+c.coll] = Method{Permission: "eventarc." + c.msg + ".create", Field: "parent"}
		out[p+"Delete"+c.coll] = Method{Permission: "eventarc." + c.msg + ".delete", Field: "name"}
		if c.coll != "ChannelConnection" {
			out[p+"Update"+c.coll] = Method{Permission: "eventarc." + c.msg + ".update", Field: c.field + ".name"}
		}
	}
	p := "/google.cloud.eventarc.v1.Eventarc/"
	// A trigger's service account needs actAs (round 5).
	ct, ut := out[p+"CreateTrigger"], out[p+"UpdateTrigger"]
	ct.ActAs, ut.ActAs = []string{"trigger.service_account"}, []string{"trigger.service_account"}
	out[p+"CreateTrigger"], out[p+"UpdateTrigger"] = ct, ut
	out[p+"ListMessageBusEnrollments"] = Method{Permission: "eventarc.messageBuses.get", Field: "parent"}
	out[p+"GetProvider"] = Method{Permission: "eventarc.providers.get", Field: "name"}
	out[p+"ListProviders"] = Method{Permission: "eventarc.providers.list", Field: "parent"}
	out[p+"GetGoogleChannelConfig"] = Method{Permission: "eventarc.googleChannelConfigs.get", Field: "name"}
	out[p+"UpdateGoogleChannelConfig"] = Method{Permission: "eventarc.googleChannelConfigs.update", Field: "google_channel_config.name"}
	pub := "/google.cloud.eventarc.publishing.v1.Publisher/"
	out[pub+"Publish"] = Method{Permission: "eventarc.messageBuses.publish", Field: "message_bus"}
	out[pub+"PublishEvents"] = Method{Permission: "eventarc.channels.publish", Field: "channel"}
	out[pub+"PublishChannelConnectionEvents"] = Method{Permission: "eventarc.channelConnections.publish", Field: "channel_connection"}
	return out
}

func init() {
	IAM.Methods = iamMethods()
	IAM.Deny = iamDeny
	IAM.Types["keys"] = "iam.googleapis.com/ServiceAccountKey"
	IAM.Types["roles"] = "iam.googleapis.com/Role"
	Eventarc.Methods = eventarcMethods()
	for _, s := range []string{"Projects", "Folders", "Organizations", "TagKeys", "TagValues", "TagBindings", "TagHolds"} {
		Services["google.cloud.resourcemanager.v3."+s] = ResourceManager
	}
	Services["google.iam.admin.v1.IAM"] = IAM
	Services["google.iam.v2.Policies"] = DenyPolicies
	Services["google.cloud.orgpolicy.v2.OrgPolicy"] = OrgPolicy
	Services["google.cloud.eventarc.publishing.v1.Publisher"] = Eventarc
}

// FilterJSON serves a JSON list or search whose answer's field (a
// repeated resource, by name or v1 projectId) keeps only the resources the
// caller holds permission on.
func (e *Enforcer) FilterJSON(w http.ResponseWriter, r *http.Request, svc *Service, permission, field string, next http.Handler) {
	e.filterJSON(w, r, svc, &Method{Permission: permission, Filter: field}, next)
}

// TestPermissionsJSON serves a JSON testIamPermissions on resource with
// the requested permissions the caller holds.
func (e *Enforcer) TestPermissionsJSON(w http.ResponseWriter, r *http.Request, svc *Service, resource string, next http.Handler) {
	e.testPermissionsJSON(w, r, svc, resource, next)
}
