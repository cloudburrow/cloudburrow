package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// IAM enforcement (#1879). With `up --iam-enforcement=on` the server is
// given the host's check address (--iam-check), and every JSON, XML and
// gRPC request is checked before it is served, by the host's one
// evaluator (internal/iamcheck, internal/iameval): the permission
// Google's "IAM permissions for JSON methods" names for the method
// (https://docs.cloud.google.com/storage/docs/access-control/iam-json,
// read 2026-11-19; the permissions it names only for some request
// contents, such as a full projection's storage.buckets.getIamPolicy, are
// not checked), on the bucket or object, against the bucket's own policy,
// the managed folders' that hold the object, and the bucket's project's,
// folders' and organization's. allUsers and allAuthenticatedUsers members
// grant as they do on Google. A bucket without uniform bucket-level access
// also grants through its ACLs and its objects' (Google's "Access control
// lists": "a user only needs permission from either IAM or ACLs"); with
// uniform bucket-level access the ACLs are ignored. A denial is Google's
// 403 "<principal> does not have <permission> access to the Google Cloud
// Storage object. Permission '<permission>' denied on resource (or it may
// not exist).", as JSON, as XML AccessDenied with the text in Details, and
// as gRPC PERMISSION_DENIED. A signed URL is checked as its signer (#1887):
// the service account whose key signed it, or the HMAC key's account, as
// Google checks the signing account holds the permission. The ACLs'
// project-owners-, project-editors- and project-viewers- entities are the
// principals holding the basic roles Owner, Editor and Viewer on the
// project their number names (#1887).

// target is what a request is checked on: a bucket, an object in it, or,
// for buckets.list and insert and the HMAC keys, a project.
type target struct {
	project string
	bucket  string
	object  string
}

func (t target) noun() string {
	switch {
	case t.object != "":
		return "Google Cloud Storage object"
	case t.bucket != "":
		return "Google Cloud Storage bucket"
	}
	return "Google Cloud project"
}

func (t target) resource() string {
	switch {
	case t.object != "":
		return "projects/_/buckets/" + t.bucket + "/objects/" + t.object
	case t.bucket != "":
		return "projects/_/buckets/" + t.bucket
	}
	return "projects/" + t.project
}

func (t target) typ() string {
	switch {
	case t.object != "":
		return "storage.googleapis.com/Object"
	case t.bucket != "":
		return "storage.googleapis.com/Bucket"
	}
	return "cloudresourcemanager.googleapis.com/Project"
}

// iamDenied is Google's 403 for permission on t, as principal ("" is
// Anonymous caller, with 401).
func iamDenied(principal, permission string, t target) *httpError {
	if principal == "" {
		return errorf(http.StatusUnauthorized, "required", "Anonymous caller does not have %s access to the %s. Permission '%s' denied on resource (or it may not exist).",
			permission, t.noun(), permission)
	}
	return errorf(http.StatusForbidden, "forbidden", "%s does not have %s access to the %s. Permission '%s' denied on resource (or it may not exist).",
		principal, permission, t.noun(), permission)
}

// unauthenticated is Google's 401 for a token it did not issue.
func unauthenticated() *httpError {
	return errorf(http.StatusUnauthorized, "authError", "Invalid Credentials")
}

// enforcing reports whether requests are checked.
func (s *Server) enforcing() bool { return s.iamCheck.Enabled() }

// authorize checks that the bearer's caller holds every permission in
// all on t; nil when enforcement is off. any, when non-empty, is a set of
// alternatives of which one suffices (objects.move's source).
func (s *Server) authorize(r *http.Request, t target, all ...string) error {
	if !s.enforcing() || len(all) == 0 {
		return nil
	}
	if signer := s.signerOf(r); signer != "" {
		return s.authorizeCaller(r, "", signer, t, all...)
	}
	bearer := iamcheck.Bearer(r.Header.Get("Authorization"))
	if bearer == "" {
		// No bearer token is the instance's own account, as the host
		// decides (iameval.CheckRemote), so the probes, the console and
		// the instance's own components need no round trip.
		return nil
	}
	return s.authorizeCaller(r, bearer, "", t, all...)
}

// authorizeCaller checks the caller of bearer, or signer, the account a
// verified signed URL names.
func (s *Server) authorizeCaller(r *http.Request, bearer, signer string, t target, all ...string) error {
	req, b, hasBucket, objACL := s.checkRequest(t)
	req.Bearer, req.Signer, req.Permissions = bearer, signer, all
	d, err := s.iamCheck.Check(r.Context(), req)
	if err != nil {
		return errorf(http.StatusServiceUnavailable, "backendError", "IAM check unavailable: %v", err)
	}
	if d.Unauthenticated {
		return unauthenticated()
	}
	if d.ScopeInsufficient {
		// A token whose scopes do not cover the call (#2179).
		return errorf(http.StatusForbidden, "insufficientPermissions", "Request had insufficient authentication scopes.")
	}
	for _, p := range all {
		if d.Holds(p) || (hasBucket && !uniformAccess(b) && aclGrants(p, t, b, objACL, aclMembers(d, b))) {
			continue
		}
		return iamDenied(d.Email, p, t)
	}
	return nil
}

// checkRequest is the check of t without its caller: the bucket's own
// policy and the managed folders' that hold the object.
func (s *Server) checkRequest(t target) (iamcheck.Request, bucketRecord, bool, []aclEntry) {
	req := iamcheck.Request{Service: "storage.googleapis.com", Resource: t.resource(), Type: t.typ(), Project: t.project}
	var b bucketRecord
	var ok bool
	var objACL []aclEntry
	if t.bucket == "" {
		return req, b, false, nil
	}
	_ = s.meta.View(func(tx Tx) error {
		var err error
		if b, ok, err = s.getBucket(tx, t.bucket); err != nil || !ok {
			return err
		}
		if t.object != "" {
			for _, mf := range managedFolderNames(t.object) {
				raw, found := tx.Get(managedFolderKey(t.bucket, mf))
				if !found {
					continue
				}
				var rec managedFolderRecord
				if json.Unmarshal(raw, &rec) == nil && rec.Policy != nil {
					req.Own = append(req.Own, iamcheck.Policy{Resource: "//storage.googleapis.com/projects/_/buckets/" + t.bucket + "/managedFolders/" + mf,
						Policy: storedPolicy(rec.Policy)})
				}
			}
			if o, exists, err := getObject(tx, t.bucket, t.object); err == nil && exists {
				objACL = o.ACL
			}
		}
		return nil
	})
	if !ok {
		return req, b, false, nil
	}
	req.Project = b.Project
	req.Own = append(req.Own, iamcheck.Policy{Resource: "//storage.googleapis.com/projects/_/buckets/" + t.bucket, Policy: storedPolicy(b.Policy)})
	return req, b, true, objACL
}

// managedFolderNames are the managed folders that can hold object, the
// deepest first ("a/b/" and "a/" for "a/b/c").
func managedFolderNames(object string) []string {
	var out []string
	for i := len(object) - 1; i >= 0; i-- {
		if object[i] == '/' {
			out = append(out, object[:i+1])
		}
	}
	return out
}

// storedPolicy is a stored bucket policy as the evaluator reads it.
func storedPolicy(p *bucketPolicy) *iampolicy.Stored {
	if p == nil {
		return nil
	}
	out := &iampolicy.Stored{}
	for _, b := range p.Bindings {
		bd := iampolicy.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			bd.Condition = &iampolicy.Condition{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		out.Bindings = append(out.Bindings, bd)
	}
	return out
}

// aclGrants is whether the bucket's or the object's ACL gives one of
// members permission, by Google's "Access control lists" roles: an
// object's READER reads it and its metadata, its OWNER also changes it
// and its ACL; a bucket's READER lists its objects and reads its
// metadata, its WRITER also creates, overwrites and deletes objects, and
// its OWNER also changes the bucket and its ACLs.
func aclGrants(permission string, t target, b bucketRecord, objACL []aclEntry, members []string) bool {
	entities := map[string]bool{}
	for _, m := range members {
		switch {
		case strings.HasPrefix(m, "project-"):
			entities[m] = true
		case m == "allusers":
			entities["allusers"] = true
		case m == "allauthenticatedusers":
			entities["allauthenticatedusers"] = true
		default:
			kind, v, ok := strings.Cut(m, ":")
			if !ok {
				continue
			}
			if kind == "serviceaccount" {
				kind = "user"
			}
			entities[kind+"-"+v] = true
		}
	}
	roleOf := func(acl []aclEntry) string {
		best := ""
		for _, e := range acl {
			if !entities[strings.ToLower(e.Entity)] {
				continue
			}
			if rank(e.Role) > rank(best) {
				best = e.Role
			}
		}
		return best
	}
	if t.object != "" {
		switch role := roleOf(objACL); {
		case role == "OWNER" && (permission == "storage.objects.update" || permission == "storage.objects.getIamPolicy" || permission == "storage.objects.setIamPolicy"):
			return true
		case role != "" && permission == "storage.objects.get":
			return true
		}
	}
	role := roleOf(b.ACL)
	switch permission {
	case "storage.objects.list", "storage.buckets.get":
		return role != ""
	case "storage.objects.create", "storage.objects.delete":
		return rank(role) >= rank("WRITER")
	case "storage.buckets.update", "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy":
		return role == "OWNER"
	}
	return false
}

// aclMembers are d's members and its project teams' ACL entities on b's
// project (project-owners-NUMBER, ...).
func aclMembers(d iamcheck.Decision, b bucketRecord) []string {
	out := append([]string(nil), d.Members...)
	for _, team := range d.Teams {
		out = append(out, "project-"+team+"-"+strconv.FormatInt(resource.ProjectNumber(b.Project), 10))
	}
	return out
}

// signerOf is the account a signed request's signature names, "" for an
// unsigned request: V4's credential's account (an HMAC key's service
// account for an HMAC signature), V2's GoogleAccessId. It is read only
// after verifySignedURL accepted the signature.
func (s *Server) signerOf(r *http.Request) string {
	q := r.URL.Query()
	if !isSigned(q) {
		return ""
	}
	if id := q.Get("GoogleAccessId"); id != "" {
		return id
	}
	for _, p := range []string{"X-Goog-", "X-Amz-"} {
		cred := q.Get(p + "Credential")
		if cred == "" {
			continue
		}
		id, _, _ := strings.Cut(cred, "/")
		if alg := q.Get(p + "Algorithm"); strings.Contains(alg, "HMAC") {
			return s.hmacAccount(id)
		}
		return id
	}
	return ""
}

// hmacAccount is the service account of the HMAC key accessID.
func (s *Server) hmacAccount(accessID string) string {
	email := ""
	_ = s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(hmacPrefix) {
			if !strings.HasSuffix(key, "/"+accessID) {
				continue
			}
			raw, _ := tx.Get(key)
			var k hmacKey
			if json.Unmarshal(raw, &k) == nil && k.AccessID == accessID {
				email = k.Email
			}
		}
		return nil
	})
	return email
}

func rank(role string) int {
	switch role {
	case "READER":
		return 1
	case "WRITER":
		return 2
	case "OWNER":
		return 3
	}
	return 0
}

// jsonPermissions are the permissions Google's table names for each JSON
// method, all of which a request needs, on the method's target.
var jsonPermissions = map[string][]string{
	"storage.buckets.delete":              {"storage.buckets.delete"},
	"storage.buckets.get":                 {"storage.buckets.get"},
	"storage.buckets.getIamPolicy":        {"storage.buckets.getIamPolicy"},
	"storage.buckets.insert":              {"storage.buckets.create"},
	"storage.buckets.list":                {"storage.buckets.list"},
	"storage.buckets.lockRetentionPolicy": {"storage.buckets.update"},
	"storage.buckets.patch":               {"storage.buckets.update"},
	"storage.buckets.update":              {"storage.buckets.update"},
	"storage.buckets.relocate":            {"storage.buckets.relocate"},
	"storage.buckets.setIamPolicy":        {"storage.buckets.setIamPolicy"},
	"storage.buckets.restore":             {"storage.buckets.restore"},
	"storage.objects.bulkRestore":         {"storage.buckets.restore", "storage.objects.create", "storage.objects.restore"},
	"storage.objects.delete":              {"storage.objects.delete"},
	"storage.objects.get":                 {"storage.objects.get"},
	"storage.objects.insert":              {"storage.objects.create"},
	"storage.objects.list":                {"storage.objects.list"},
	"storage.objects.patch":               {"storage.objects.update"},
	"storage.objects.update":              {"storage.objects.update"},
	"storage.objects.restore":             {"storage.objects.create", "storage.objects.restore"},
	"storage.objects.viewFullContext":     {"storage.objects.viewFullContext"},
	"storage.notifications.delete":        {"storage.buckets.update"},
	"storage.notifications.insert":        {"storage.buckets.update"},
	"storage.notifications.get":           {"storage.buckets.get"},
	"storage.notifications.list":          {"storage.buckets.get"},
	"storage.projects.hmacKeys.create":    {"storage.hmacKeys.create"},
	"storage.projects.hmacKeys.delete":    {"storage.hmacKeys.delete"},
	"storage.projects.hmacKeys.get":       {"storage.hmacKeys.get"},
	"storage.projects.hmacKeys.list":      {"storage.hmacKeys.list"},
	"storage.projects.hmacKeys.update":    {"storage.hmacKeys.update"},
	"storage.managedFolders.delete":       {"storage.managedFolders.delete"},
	"storage.managedFolders.get":          {"storage.managedFolders.get"},
	"storage.managedFolders.getIamPolicy": {"storage.managedFolders.getIamPolicy"},
	"storage.managedFolders.insert":       {"storage.managedFolders.create"},
	"storage.managedFolders.list":         {"storage.managedFolders.list"},
	"storage.managedFolders.update":       {"storage.managedFolders.update"},
	"storage.managedFolders.setIamPolicy": {"storage.managedFolders.setIamPolicy"},
	"storage.folders.get":                 {"storage.folders.get"},
	"storage.folders.insert":              {"storage.folders.create"},
	"storage.folders.list":                {"storage.folders.list"},
	"storage.folders.rename":              {"storage.folders.rename"},
	"storage.folders.delete":              {"storage.folders.delete"},
	"storage.folders.deleteRecursive":     {"storage.folders.delete"},
	// #1887: the rest of the built methods, as the same table names them
	// (anywhereCaches.*, projects.serviceAccount.get, objects.watchAll),
	// and getStorageLayout as its own reference does
	// (https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/getStorageLayout,
	// read 2026-11-19).
	"storage.buckets.getStorageLayout":    {"storage.objects.list"},
	"storage.projects.serviceAccount.get": {"resourcemanager.projects.get"},
	"storage.anywhereCaches.insert":       {"storage.anywhereCaches.create"},
	"storage.anywhereCaches.get":          {"storage.anywhereCaches.get"},
	"storage.anywhereCaches.list":         {"storage.anywhereCaches.list"},
	"storage.anywhereCaches.update":       {"storage.anywhereCaches.update"},
	"storage.anywhereCaches.pause":        {"storage.anywhereCaches.pause"},
	"storage.anywhereCaches.resume":       {"storage.anywhereCaches.resume"},
	"storage.anywhereCaches.disable":      {"storage.anywhereCaches.disable"},
	"storage.objects.watchAll":            {"storage.buckets.update"},
}

var (
	bucketACLRead  = []string{"storage.buckets.get", "storage.buckets.getIamPolicy"}
	bucketACLWrite = []string{"storage.buckets.get", "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy", "storage.buckets.update"}
	objectACLRead  = []string{"storage.objects.get", "storage.objects.getIamPolicy"}
	objectACLWrite = []string{"storage.objects.get", "storage.objects.getIamPolicy", "storage.objects.setIamPolicy", "storage.objects.update"}
)

func init() {
	for _, v := range []string{"get", "list"} {
		jsonPermissions["storage.bucketAccessControls."+v] = bucketACLRead
		jsonPermissions["storage.defaultObjectAccessControls."+v] = bucketACLRead
		jsonPermissions["storage.objectAccessControls."+v] = objectACLRead
	}
	for _, v := range []string{"delete", "insert", "patch", "update"} {
		jsonPermissions["storage.bucketAccessControls."+v] = bucketACLWrite
		jsonPermissions["storage.defaultObjectAccessControls."+v] = bucketACLWrite
		jsonPermissions["storage.objectAccessControls."+v] = objectACLWrite
	}
}

// iamJSONTarget is the bucket and object a JSON path ("b/B/o/O/...") names,
// the project query parameter's project for the paths under none.
func iamJSONTarget(r *http.Request, prefix, rest string) target {
	segs := strings.Split(rest, "/")
	t := target{project: r.URL.Query().Get("project")}
	if len(segs) >= 2 && segs[0] == "projects" {
		t.project = pathVar(r, prefix, 1)
		return t
	}
	if len(segs) < 2 || segs[0] != "b" {
		return t
	}
	t.bucket = pathVar(r, prefix, 1)
	if len(segs) >= 4 && segs[2] == "o" {
		t.object = pathVar(r, prefix, 3)
	}
	return t
}

// authorizeJSON checks a JSON method's request; nil when it may run.
func (s *Server) authorizeJSON(r *http.Request, id, prefix, rest string) error {
	if !s.enforcing() {
		return nil
	}
	t := iamJSONTarget(r, prefix, rest)
	segs := strings.Split(rest, "/")
	switch id {
	case "storage.objects.copy", "storage.objects.rewrite":
		// b/S/o/SO/copyTo/b/D/o/DO: get on the source, create on the
		// destination.
		if err := s.authorize(r, t, "storage.objects.get"); err != nil {
			return err
		}
		if len(segs) >= 9 {
			return s.authorize(r, target{bucket: pathVar(r, prefix, 6), object: pathVar(r, prefix, 8)}, "storage.objects.create")
		}
		return nil
	case "storage.objects.compose":
		if err := s.authorize(r, target{bucket: t.bucket}, "storage.objects.get"); err != nil {
			return err
		}
		return s.authorize(r, t, "storage.objects.create")
	case "storage.objects.move":
		// b/B/o/S/moveTo/o/D: storage.objects.move on the source, or
		// storage.objects.delete and storage.objects.get; create on the
		// destination.
		if s.authorize(r, t, "storage.objects.move") != nil {
			if err := s.authorize(r, t, "storage.objects.delete", "storage.objects.get"); err != nil {
				return err
			}
		}
		if len(segs) >= 7 {
			return s.authorize(r, target{bucket: t.bucket, object: pathVar(r, prefix, 6)}, "storage.objects.create")
		}
		return nil
	case "storage.objects.list", "storage.objects.insert", "storage.objects.bulkRestore", "storage.objects.watchAll", "storage.managedFolders.list",
		"storage.managedFolders.insert", "storage.folders.list", "storage.folders.insert", "storage.notifications.list",
		"storage.notifications.insert":
		t.object = ""
	case "storage.managedFolders.get", "storage.managedFolders.delete", "storage.managedFolders.update",
		"storage.managedFolders.getIamPolicy", "storage.managedFolders.setIamPolicy", "storage.folders.get",
		"storage.folders.delete", "storage.folders.deleteRecursive", "storage.folders.rename",
		"storage.notifications.get", "storage.notifications.delete", "storage.buckets.getIamPolicy",
		"storage.buckets.setIamPolicy", "storage.bucketAccessControls.get", "storage.bucketAccessControls.list",
		"storage.defaultObjectAccessControls.get", "storage.defaultObjectAccessControls.list":
		t.object = ""
	}
	if id == "storage.objects.insert" && t.object == "" {
		if name := r.URL.Query().Get("name"); name != "" {
			t.object = name
		}
	}
	return s.authorize(r, t, jsonPermissions[id]...)
}

// xmlPermission is the permission of an XML API request on bucket and
// object, "" for one that is not checked.
func xmlPermission(r *http.Request, bucket, object string) (target, string) {
	t := target{bucket: bucket, object: object}
	q := r.URL.Query()
	switch {
	case bucket == "":
		t.project = r.Header.Get("X-Goog-Project-Id")
		return t, "storage.buckets.list"
	case object == "" && (q.Has("acl") || q.Has("defaultObjectAcl") || q.Has("iam")):
		if r.Method == http.MethodGet {
			return t, "storage.buckets.getIamPolicy"
		}
		return t, "storage.buckets.setIamPolicy"
	case object == "" && r.Method == http.MethodGet && (q.Has("cors") || q.Has("lifecycle") || q.Has("location") ||
		q.Has("storageClass") || q.Has("versioning") || q.Has("billing") || q.Has("logging") || q.Has("website") ||
		q.Has("encryption") || q.Has("encryptionConfig") || q.Has("tagging")):
		return t, "storage.buckets.get"
	case object == "" && r.Method == http.MethodGet:
		return t, "storage.objects.list"
	case object == "" && r.Method == http.MethodHead:
		return t, "storage.buckets.get"
	case object == "" && r.Method == http.MethodPut && len(q) == 0:
		t.bucket, t.project = "", r.Header.Get("X-Goog-Project-Id")
		return t, "storage.buckets.create"
	case object == "" && r.Method == http.MethodPut:
		return t, "storage.buckets.update"
	case object == "" && r.Method == http.MethodDelete:
		return t, "storage.buckets.delete"
	case object == "" && r.Method == http.MethodPost:
		// A form POST uploads the object its key field names; the
		// handler's target is checked when the body is read.
		return t, "storage.objects.create"
	case q.Has("acl"):
		if r.Method == http.MethodGet {
			return t, "storage.objects.getIamPolicy"
		}
		return t, "storage.objects.setIamPolicy"
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		return t, "storage.objects.get"
	case r.Method == http.MethodPut, r.Method == http.MethodPost:
		return t, "storage.objects.create"
	case r.Method == http.MethodDelete:
		return t, "storage.objects.delete"
	}
	return t, ""
}

// authorizeXML checks an XML API request; it writes the denial and
// returns false when the request may not run.
func (s *Server) authorizeXML(w http.ResponseWriter, r *http.Request, bucket, object string) bool {
	if !s.enforcing() {
		return true
	}
	t, perm := xmlPermission(r, bucket, object)
	err := s.authorize(r, t, perm)
	if r.Method == http.MethodPut && object != "" && r.Header.Get("X-Goog-Copy-Source") != "" && err == nil {
		src := strings.TrimPrefix(r.Header.Get("X-Goog-Copy-Source"), "/")
		sb, so, _ := strings.Cut(src, "/")
		err = s.authorize(r, target{bucket: sb, object: so}, "storage.objects.get")
	}
	if err == nil {
		return true
	}
	writeXMLDenial(w, r, err)
	return false
}

// writeXMLDenial is the XML API's AccessDenied, Google's text in Details.
func writeXMLDenial(w http.ResponseWriter, r *http.Request, err error) {
	e, ok := err.(*httpError)
	if !ok || (e.status != http.StatusForbidden && e.status != http.StatusUnauthorized) {
		writeXMLFromError(w, r, err, "")
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(e.status)
	if r.Method == http.MethodHead {
		return
	}
	code, msg := "AccessDenied", "Access denied."
	if e.reason == "authError" {
		code, msg = "AuthenticationRequired", "Authentication required."
	}
	_, _ = fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>%s</Code><Message>%s</Message><Details>%s</Details></Error>",
		code, msg, xmlEscape(e.message))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", "\"", "&quot;").Replace(s)
}

// heldPermissions are those of perms the caller holds on t, as Google's
// testIamPermissions answers; every one when enforcement is off (ADR-0006
// rule 4).
func (s *Server) heldPermissions(r *http.Request, t target, perms []string) []string {
	if !s.enforcing() || iamcheck.Bearer(r.Header.Get("Authorization")) == "" {
		return perms
	}
	req, b, hasBucket, objACL := s.checkRequest(t)
	req.Bearer, req.Permissions = iamcheck.Bearer(r.Header.Get("Authorization")), perms
	d, err := s.iamCheck.Check(r.Context(), req)
	if err != nil || d.Unauthenticated {
		return []string{}
	}
	held := []string{}
	for _, p := range perms {
		if d.Holds(p) || (hasBucket && !uniformAccess(b) && aclGrants(p, t, b, objACL, aclMembers(d, b))) {
			held = append(held, p)
		}
	}
	return held
}
