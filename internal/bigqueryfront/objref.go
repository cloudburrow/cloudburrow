package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ObjectRef values and the OBJ functions (#1980), as Google documents them
// (read 2026-11-19: https://docs.cloud.google.com/bigquery/docs/work-with-objectref
// and https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/objectref_functions):
//
//   - An ObjectRef is STRUCT<uri STRING, version STRING, authorizer STRING,
//     details JSON>: uri the Cloud Storage object, version its generation,
//     authorizer the Cloud resource connection of delegated access
//     ("region.connection" or "project.region.connection"), NULL for
//     direct access, and details {"gcs_metadata": {content_type,
//     md5_hash, size, updated}} once fetched, or {"errors": [...]}.
//   - OBJ.MAKE_REF(uri [, authorizer] [, version => v] [, details => j]),
//     OBJ.MAKE_REF(objectref_json) and OBJ.MAKE_REF(objectref, authorizer)
//     build one in SQL (the format alone is checked: a uri is
//     gs://bucket/object, an authorizer two or three dotted names, which is
//     written in its project.region.connection form);
//   - OBJ.FETCH_METADATA(ref | ARRAY<ref>) fills version and details from
//     Cloud Storage; a ref that cannot be read still succeeds, its details
//     {"errors": [{code, message, source: "OBJ.FETCH_METADATA"}]};
//   - OBJ.GET_ACCESS_URL(ref | ARRAY<ref>, 'r' | 'rw' [, INTERVAL]) is the
//     ObjectRefRuntime JSON {"obj_ref": {...}, "access_urls": {read_url,
//     write_url ('rw'), expiry_time}}, or on failure {"objectref": {uri,
//     authorizer}, "errors": [...]} (the page's two names); the duration
//     is 30 minutes to 6 hours, 6 hours when not given;
//   - OBJ.GET_READ_URL(ref) is STRUCT<url STRING, status STRING>, a
//     45-minute read URL or the failure.
//
// The URLs are V4 signed URLs (signedurls.go) of the instance's Cloud
// Storage, signed as the authorizer connection's service agent at the
// instance's BigQuery clock (clock:advance moves it), X-Goog-Expires the
// duration; Cloud Storage verifies them against the agent's certificate
// and judges expiry on its own clock. read_url is a GET, write_url a PUT.
//
// Access (the ObjectRef page): delegated access needs the caller's
// bigquery.objectRefs.read (bigquery.objectRefs.write for 'rw') on the
// connection, and the connection's service account storage.objects.get
// (storage.objects.create for 'rw') on the project or the bucket, as IAM
// policies say; the URL functions need delegated access. Direct access
// (no authorizer) reads as the caller, here the instance's account.
//
// The AI functions (aigenerate.go, modelremote.go) take ObjectRef prompt
// and content fields: the front checks the same access, then sends the
// object to the local Vertex AI endpoint as a fileData part, which reads
// it from the instance's Cloud Storage; the endpoint refuses what its
// model cannot read (the local generator reads images, the local embedder
// images, audio, video and PDF only with EmbeddingGemma 2), and that
// refusal is the row's status.
//
// OBJ.LIST (objListSQL) and an OBJ.GET_ACCESS_URL mode or duration that
// is an expression are served since #1983.
//
// UNVERIFIED (docs re-read 2026-11-19, the pages above): the wording of
// the errors beyond the documented "Connection credential for ... cannot
// be used"; that OBJ.MAKE_REF writes the authorizer in its project form
// (OBJ.FETCH_METADATA's example shows it so); that an out-of-range
// duration fails the query; that GET_ACCESS_URL fills obj_ref's details;
// and the signed URL's credential-scope region ("auto").

// objRefType is the ObjectRef type.
const objRefType = "STRUCT<uri STRING, version STRING, authorizer STRING, details JSON>"

const (
	permObjectRefsRead  = "bigquery.objectRefs.read"
	permObjectRefsWrite = "bigquery.objectRefs.write"
	permObjectsGet      = "storage.objects.get"
	permObjectsCreate   = "storage.objects.create"
	readURLExpiry       = 45 * time.Minute
)

// objRef is an ObjectRef as TO_JSON_STRING writes it.
type objRef struct {
	URI        string          `json:"uri"`
	Version    *string         `json:"version"`
	Authorizer *string         `json:"authorizer"`
	Details    json.RawMessage `json:"details"`
}

// parseObjRef reads raw as an ObjectRef, or as an ObjectRefRuntime's
// obj_ref; ok is false for anything else.
func parseObjRef(raw []byte) (objRef, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return objRef{}, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return objRef{}, false
	}
	if inner, ok := probe["obj_ref"]; ok {
		return parseObjRef(inner)
	}
	if _, ok := probe["uri"]; !ok {
		return objRef{}, false
	}
	var o objRef
	if json.Unmarshal(raw, &o) != nil || o.URI == "" {
		return objRef{}, false
	}
	for k := range probe {
		if k != "uri" && k != "version" && k != "authorizer" && k != "details" {
			return objRef{}, false
		}
	}
	if v := bytes.TrimSpace(o.Details); string(v) == "null" {
		o.Details = nil
	}
	return o, true
}

func strp(s string) *string { return &s }

// authorizer is o's authorizer, or "".
func (o objRef) authorizer() string {
	if o.Authorizer == nil {
		return ""
	}
	return *o.Authorizer
}

// sql is o as an ObjectRef literal.
func (o objRef) sql() string {
	opt := func(p *string) string {
		if p == nil {
			return "CAST(NULL AS STRING)"
		}
		return sqlString(*p)
	}
	return "STRUCT(" + sqlString(o.URI) + ", " + opt(o.Version) + ", " + opt(o.Authorizer) + ", " + jsonLiteral(o.Details) + ")"
}

// gcsObject is o's bucket and object name, or ok false.
func (o objRef) gcsObject() (bucket, name string, ok bool) {
	rest, isGS := strings.CutPrefix(o.URI, "gs://")
	bucket, name, _ = strings.Cut(rest, "/")
	return bucket, name, isGS && bucket != "" && name != ""
}

// gcsMetadata is the details.gcs_metadata of o, as object tables write it.
func gcsMetadata(o gcsObjectMeta) map[string]any {
	size, _ := strconv.ParseInt(o.Size, 10, 64)
	d := map[string]any{"size": size}
	if o.ContentType != "" {
		d["content_type"] = o.ContentType
	}
	if b, err := base64.StdEncoding.DecodeString(o.MD5Hash); err == nil && o.MD5Hash != "" {
		d["md5_hash"] = hex.EncodeToString(b)
	}
	if ts, err := time.Parse(time.RFC3339Nano, o.Updated); err == nil {
		d["updated"] = ts.UnixMicro()
	}
	return d
}

// objError is one entry of an ObjectRef's errors.
type objError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Source  string `json:"source"`
}

// normalizeAuthorizer is a as project.region.connection, or a message.
func (f front) normalizeAuthorizer(a string) (string, string) {
	if strings.HasPrefix(a, "projects/") {
		p := strings.Split(a, "/")
		if len(p) == 6 && p[2] == "locations" && p[4] == "connections" {
			return p[1] + "." + strings.ToLower(p[3]) + "." + p[5], ""
		}
	}
	parts := strings.Split(a, ".")
	if len(parts) == 2 {
		parts = append([]string{projectOf(f.base)}, parts...)
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Sprintf("Invalid authorizer %q: it is a connection, REGION.CONNECTION_ID or PROJECT_ID.REGION.CONNECTION_ID", a)
	}
	return parts[0] + "." + strings.ToLower(parts[1]) + "." + parts[2], ""
}

// credentialError is the documented failure of a connection that does
// not exist or that the caller may not use.
func credentialError(name string) string {
	p := strings.Split(name, ".")
	if len(p) != 3 {
		return "Connection credential for " + name + " cannot be used."
	}
	return "Connection credential for projects/" + p[0] + "/locations/" + p[1] + "/connections/" + p[2] +
		" cannot be used. Either the connection does not exist, or the user does not have sufficient permissions to use it."
}

// refAccess checks o's access, for writing when write is set: the
// connection's service account of delegated access ("" for direct
// access), its connection name, or a failure's code and message.
func (f front) refAccess(r *http.Request, o objRef, write bool) (sa, name string, code int, msg string) {
	bucket, _, ok := o.gcsObject()
	if !ok {
		return "", "", 400, "Invalid ObjectRef uri " + strconv.Quote(o.URI) + ": it is gs://BUCKET/OBJECT"
	}
	if o.authorizer() == "" {
		return "", "", 0, ""
	}
	name, bad := f.normalizeAuthorizer(o.authorizer())
	if bad != "" {
		return "", "", 400, bad
	}
	p := strings.Split(name, ".")
	cn, found := f.connections.lookup(connectionProject(p[0], f.numbers), p[1], p[2])
	if !found {
		return "", name, 400, credentialError(name)
	}
	if _, ok := cn.Fields["cloudResource"]; !ok {
		return "", name, 400, "The connection " + name + " is not a Cloud resource connection: an ObjectRef's authorizer is a CLOUD_RESOURCE connection"
	}
	perm := permObjectRefsRead
	if write {
		perm = permObjectRefsWrite
	}
	if code, _ := f.checkConnectionPerm(r, p[0], cn, perm); code != 0 {
		return "", name, 400, credentialError(name) + " The caller needs " + perm + " on the connection (BigQuery ObjectRef Reader or Admin)."
	}
	sa = cn.serviceAccount()
	objPerm := permObjectsGet
	if write {
		objPerm = permObjectsCreate
	}
	if code, msg := f.checkStoragePerm(r, sa, bucket, objPerm); code != 0 {
		return "", name, code, msg
	}
	return sa, name, 0, ""
}

// checkConnectionPerm checks the caller's perm on cn: its policy, or the
// connection project's.
func (f front) checkConnectionPerm(r *http.Request, connProject string, cn *bqConnection, perm string) (int, string) {
	proj, code, msg := f.projectBindings(r.Context(), connProject)
	if code != 0 {
		return code, msg
	}
	caller := f.members(r.Context(), "serviceAccount:"+callerIdentity(projectOf(f.base)))
	if !bindingsGrant(cn.bindings(), caller, perm) && !bindingsGrant(proj, caller, perm) {
		return 403, fmt.Sprintf("Access Denied: Connection %s: Permission %s denied on connection %s (or it may not exist).", cn.name(), perm, cn.name())
	}
	return 0, ""
}

// checkStoragePerm checks that sa holds perm on bucket: through the
// project's policy or the bucket's.
func (f front) checkStoragePerm(r *http.Request, sa, bucket, perm string) (int, string) {
	if f.storage == nil {
		return 501, "Not implemented here: an ObjectRef reads the instance's Cloud Storage, which the BigQuery front was not given (enable Cloud Storage). Nothing was run."
	}
	member := f.members(r.Context(), "serviceAccount:"+sa)
	proj, code, msg := f.projectBindings(r.Context(), projectOf(f.base))
	if code != 0 {
		return code, msg
	}
	if bindingsGrant(proj, member, perm) {
		return 0, ""
	}
	var pol struct {
		Bindings []policyBinding `json:"bindings"`
	}
	status, err := f.storage.do(r.Context(), "/storage/v1/b/"+url.PathEscape(bucket)+"/iam", &pol)
	if err == nil && status == http.StatusOK && bindingsGrant(pol.Bindings, member, perm) {
		return 0, ""
	}
	return 403, fmt.Sprintf("Access Denied: %s does not have %s access to the Google Cloud Storage bucket %s. Permission '%s' denied on resource (or it may not exist).",
		sa, perm, bucket, perm)
}

// objectMeta reads o's object (at its version, when it has one).
func (f front) objectMeta(r *http.Request, o objRef) (gcsObjectMeta, int, string) {
	bucket, name, _ := o.gcsObject()
	path := "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(name)
	if o.Version != nil && *o.Version != "" {
		path += "?generation=" + url.QueryEscape(*o.Version)
	}
	var m gcsObjectMeta
	status, err := f.storage.do(r.Context(), path, &m)
	switch {
	case err != nil:
		return m, 500, "cloudburrow: could not read the object's metadata: " + err.Error()
	case status == http.StatusNotFound:
		return m, 404, "Not found: URI " + o.URI
	case status != http.StatusOK:
		return m, status, fmt.Sprintf("Cloud Storage answered %d reading %s", status, o.URI)
	}
	return m, 0, ""
}

// fetchMetadata is OBJ.FETCH_METADATA of o: o filled from Cloud Storage,
// or its details holding the failure.
func (f front) fetchMetadata(r *http.Request, o objRef) objRef {
	fail := func(code int, msg string) objRef {
		d, _ := json.Marshal(map[string]any{"errors": []objError{{code, msg, "OBJ.FETCH_METADATA"}}})
		o.Details = d
		return o
	}
	if f.storage == nil {
		return fail(501, "Not implemented here: the BigQuery front was not given the instance's Cloud Storage.")
	}
	_, name, code, msg := f.refAccess(r, o, false)
	if name != "" {
		o.Authorizer = strp(name)
	}
	if code != 0 {
		return fail(code, msg)
	}
	m, code, msg := f.objectMeta(r, o)
	if code != 0 {
		return fail(code, msg)
	}
	o.Version = strp(m.Generation)
	o.Details, _ = json.Marshal(map[string]any{"gcs_metadata": gcsMetadata(m)})
	return o
}

// accessURL is OBJ.GET_ACCESS_URL of o: the ObjectRefRuntime JSON.
func (f front) accessURL(r *http.Request, o objRef, write bool, d time.Duration) string {
	fail := func(code int, msg string) string {
		ref := map[string]any{"uri": o.URI}
		if o.Authorizer != nil {
			ref["authorizer"] = *o.Authorizer
		}
		b, _ := json.Marshal(map[string]any{"objectref": ref, "errors": []objError{{code, msg, "OBJ.GET_ACCESS_URL"}}})
		return string(b)
	}
	urls, o, code, msg := f.signedRefURLs(r, o, write, d)
	if code != 0 {
		return fail(code, msg)
	}
	ref := map[string]any{"uri": o.URI, "version": o.Version, "authorizer": o.Authorizer}
	if len(o.Details) > 0 {
		ref["details"] = o.Details
	}
	b, _ := json.Marshal(map[string]any{"obj_ref": ref, "access_urls": urls})
	return string(b)
}

// signedRefURLs signs o's read URL, and its write URL with write, for d:
// the access_urls, and o with its authorizer written out and, when the
// object can be read, its metadata.
func (f front) signedRefURLs(r *http.Request, o objRef, write bool, d time.Duration) (map[string]string, objRef, int, string) {
	if f.objects.public == "" {
		return nil, o, 501, "Not implemented here: a signed URL needs the address clients reach the instance's Cloud Storage at, which this BigQuery " +
			"was not given (bigquery-front --storage-public)."
	}
	if f.storage == nil {
		return nil, o, 501, "Not implemented here: the BigQuery front was not given the instance's Cloud Storage."
	}
	if o.authorizer() == "" {
		return nil, o, 400, "A signed URL of an ObjectRef needs delegated access: its authorizer must name a Cloud resource connection (OBJ.MAKE_REF(uri, authorizer))."
	}
	sa, name, code, msg := f.refAccess(r, o, write)
	if name != "" {
		o.Authorizer = strp(name)
	}
	if code != 0 {
		return nil, o, code, msg
	}
	if write {
		// rw: the read URL too.
		if code, msg := f.checkStoragePerm(r, sa, strings.SplitN(strings.TrimPrefix(o.URI, "gs://"), "/", 2)[0], permObjectsGet); code != 0 {
			return nil, o, code, msg
		}
	}
	if !strings.HasSuffix(sa, agentDomain) {
		return nil, o, 501, "Not implemented here: signing as " + sa + ", which is not a BigQuery connection's service agent."
	}
	key, err := f.objects.agents.get(sa)
	if err != nil {
		return nil, o, 500, "cloudburrow: could not make the service agent's key: " + err.Error()
	}
	if m, code, _ := f.objectMeta(r, o); code == 0 {
		if o.Version == nil {
			o.Version = strp(m.Generation)
		}
		if len(o.Details) == 0 {
			o.Details, _ = json.Marshal(map[string]any{"gcs_metadata": gcsMetadata(m)})
		}
	}
	now := f.now()
	if f.objects.signNow != nil {
		now = f.objects.signNow()
	}
	bucket, object, _ := o.gcsObject()
	urls := map[string]string{"expiry_time": now.Add(d).UTC().Format("2006-01-02T15:04:05Z")}
	if urls["read_url"], err = signURLFor(key, sa, f.objects.public, http.MethodGet, bucket, object, now, d); err != nil {
		return nil, o, 500, "cloudburrow: " + err.Error()
	}
	if write {
		if urls["write_url"], err = signURLFor(key, sa, f.objects.public, http.MethodPut, bucket, object, now, d); err != nil {
			return nil, o, 500, "cloudburrow: " + err.Error()
		}
	}
	return urls, o, 0, ""
}

// refPart is the generateContent / embedContent part of an ObjectRef
// prompt field: a fileData of its object after its access is checked, or
// the row's failure status.
func (f front) refPart(r *http.Request, o objRef) (map[string]any, string) {
	if f.storage == nil {
		return nil, "Not implemented here: an ObjectRef reads the instance's Cloud Storage, which the BigQuery front was not given."
	}
	if _, _, code, msg := f.refAccess(r, o, false); code != 0 {
		return nil, msg
	}
	m, code, msg := f.objectMeta(r, o)
	if code != 0 {
		return nil, msg
	}
	if m.ContentType == "" {
		m.ContentType = "application/octet-stream"
	}
	return map[string]any{"fileData": map[string]string{"fileUri": o.URI, "mimeType": m.ContentType}}, ""
}

// promptPiece is one piece of a prompt, in order: text, or an ObjectRef.
type promptPiece struct {
	text string
	ref  *objRef
}

// promptPieces reads the prompt TO_JSON_STRING wrote: a string, an
// ObjectRef (or ObjectRefRuntime), or a STRUCT of STRING, ARRAY<STRING>,
// ObjectRef and ARRAY<ObjectRef> fields in order. ok is false for a NULL
// prompt.
func promptPieces(k string) ([]promptPiece, bool, int, string) {
	raw := bytes.TrimSpace([]byte(k))
	if string(raw) == "null" {
		return nil, false, 0, ""
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false, 500, "cloudburrow: unreadable prompt " + k
		}
		return []promptPiece{{text: s}}, true, 0, ""
	}
	if o, ok := parseObjRef(raw); ok {
		return []promptPiece{{ref: &o}}, true, 0, ""
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false, 400, "AI.GENERATE's prompt must be a STRING, an ObjectRef or a STRUCT"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_, _ = dec.Token()
	var out []promptPiece
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return nil, false, 500, "cloudburrow: unreadable prompt " + k
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false, 500, "cloudburrow: unreadable prompt " + k
		}
		v = bytes.TrimSpace(v)
		switch {
		case string(v) == "null":
			// CONCAT of a NULL is NULL.
			return nil, false, 0, ""
		case v[0] == '"':
			var s string
			_ = json.Unmarshal(v, &s)
			out = append(out, promptPiece{text: s})
		case v[0] == '[':
			var items []json.RawMessage
			if json.Unmarshal(v, &items) != nil {
				return nil, false, 500, "cloudburrow: unreadable prompt " + k
			}
			for _, it := range items {
				it = bytes.TrimSpace(it)
				switch {
				case string(it) == "null":
				case it[0] == '"':
					var s string
					_ = json.Unmarshal(it, &s)
					out = append(out, promptPiece{text: s})
				default:
					o, ok := parseObjRef(it)
					if !ok {
						return nil, false, 400, "AI.GENERATE's prompt STRUCT fields must be STRING, ARRAY<STRING>, ObjectRef or ARRAY<ObjectRef>"
					}
					out = append(out, promptPiece{ref: &o})
				}
			}
		default:
			o, ok := parseObjRef(v)
			if !ok {
				return nil, false, 400, "AI.GENERATE's prompt STRUCT fields must be STRING, ARRAY<STRING>, ObjectRef or ARRAY<ObjectRef>"
			}
			out = append(out, promptPiece{ref: &o})
		}
	}
	return out, true, 0, ""
}

// hasRef reports whether ps holds an ObjectRef.
func hasRef(ps []promptPiece) bool {
	for _, p := range ps {
		if p.ref != nil {
			return true
		}
	}
	return false
}

// joinText is ps' text pieces concatenated.
func joinText(ps []promptPiece) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.text)
	}
	return b.String()
}

// pieceParts are ps as generateContent parts, with suffix a last text
// part (when not empty): adjacent text pieces are one part. status is the
// first ObjectRef's failure.
func (f front) pieceParts(r *http.Request, ps []promptPiece, suffix string) ([]any, string) {
	var parts []any
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parts = append(parts, map[string]string{"text": text.String()})
			text.Reset()
		}
	}
	for _, p := range ps {
		if p.ref == nil {
			text.WriteString(p.text)
			continue
		}
		flush()
		part, status := f.refPart(r, *p.ref)
		if status != "" {
			return nil, status
		}
		parts = append(parts, part)
	}
	text.WriteString(suffix)
	flush()
	return parts, ""
}

// refPieces are ps' ObjectRef pieces.
func refPieces(ps []promptPiece) []promptPiece {
	var out []promptPiece
	for _, p := range ps {
		if p.ref != nil {
			out = append(out, p)
		}
	}
	return out
}
