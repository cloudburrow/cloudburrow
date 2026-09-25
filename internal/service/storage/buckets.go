package storage

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Buckets (#490): insert, get, list, patch, update and delete, with
// metageneration and its preconditions. A bucket's settable fields are kept
// as the JSON the client sent, so a patch is a JSON merge patch
// (RFC 7386), as the API's patch semantics are: an omitted field stays, null
// clears it, and within an object (labels) a null member removes that key.

const bucketPrefix = "bucket/"

type bucketRecord struct {
	Name           string         `json:"name"`
	Project        string         `json:"project"`
	Created        time.Time      `json:"created"`
	Updated        time.Time      `json:"updated"`
	Metageneration int64          `json:"metageneration"`
	Generation     int64          `json:"generation"`
	Fields         map[string]any `json:"fields"`
	// SoftDeleted and HardDelete are set on a soft-deleted bucket (#499).
	SoftDeleted time.Time `json:"softDeleted,omitempty"`
	HardDelete  time.Time `json:"hardDelete,omitempty"`
	// Policy is the bucket's IAM policy, stored and never enforced (#504).
	Policy *bucketPolicy `json:"policy,omitempty"`
}

// bucketFields says how each Bucket property (the discovery schema's 38) is
// treated in a request: kept (stored and echoed), stored (kept and echoed,
// with no behaviour behind it: docs/compatibility.md has a row for each),
// output-only (ignored, as Google ignores it), or refused by name (#503).
var bucketFields = map[string]string{
	"location": "kept", "storageClass": "kept", "labels": "kept", "versioning": "kept",
	"defaultEventBasedHold": "kept", "softDeletePolicy": "kept", "retentionPolicy": "kept",
	"lifecycle": "kept", "cors": "kept",

	"website": "stored", "logging": "stored", "encryption": "stored", "billing": "stored",
	"iamConfiguration": "stored", "rpo": "stored", "autoclass": "stored", "customPlacementConfig": "stored",
	"hierarchicalNamespace": "stored",

	"etag": "output", "generation": "output", "hardDeleteTime": "output", "id": "output", "kind": "output",
	"locationType": "output", "metageneration": "output", "projectNumber": "output", "selfLink": "output",
	"softDeleteTime": "output", "timeCreated": "output", "updated": "output", "owner": "output",
	"satisfiesPZI": "output", "satisfiesPZS": "output", "name": "output", "objectRetention": "output",

	"ipFilter": "IP filtering is not implemented", "acl": "ACL methods are not implemented",
	"defaultObjectAcl": "ACL methods are not implemented",
}

// storedObjects are the stored fields whose value is an object; rpo is a
// string.
var storedObjects = map[string]bool{"website": true, "logging": true, "encryption": true, "billing": true,
	"iamConfiguration": true, "autoclass": true, "customPlacementConfig": true, "hierarchicalNamespace": true}

var storageClasses = map[string]bool{"STANDARD": true, "NEARLINE": true, "COLDLINE": true, "ARCHIVE": true,
	"MULTI_REGIONAL": true, "REGIONAL": true, "DURABLE_REDUCED_AVAILABILITY": true}

// bucketNameRE and the checks in validBucketName follow
// docs.cloud.google.com/storage/docs/buckets#naming.
var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[a-z0-9]$`)

func validBucketName(name string) error {
	switch {
	case len(name) < 3 || len(name) > 222 || !bucketNameRE.MatchString(name):
		return badRequest("Invalid bucket name: %q. Names are 3-63 characters (222 with dots), lowercase letters, digits, dashes, underscores and dots, starting and ending with a letter or digit.", name)
	case net.ParseIP(name) != nil:
		return badRequest("Invalid bucket name: %q. A bucket name cannot be an IP address.", name)
	case strings.HasPrefix(name, "goog") || strings.Contains(name, "google"):
		return badRequest("Invalid bucket name: %q. A bucket name cannot begin with \"goog\" or contain \"google\".", name)
	}
	for _, part := range strings.Split(name, ".") {
		if len(part) > 63 {
			return badRequest("Invalid bucket name: %q. Each dot-separated component is at most 63 characters.", name)
		}
	}
	return nil
}

// readBody decodes a JSON request body of at most 1 MiB, numbers kept exact.
func readBody(r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil {
		return nil, badRequest("the request body could not be read")
	}
	if len(b) > 1<<20 {
		return nil, badRequest("the request body exceeds 1 MiB")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return body, nil
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(&body); err != nil {
		return nil, errorf(http.StatusBadRequest, "parseError", "Parse Error: the request body is not a JSON object")
	}
	return body, nil
}

// checkBucketFields refuses what is not kept yet, and unknown properties.
// An unkept field whose value says nothing (null, or empty: the Go client
// sends "lifecycle":{"rule":[]} with every create that has attributes) is
// accepted, since there is nothing in it to drop.
func checkBucketFields(body map[string]any) error {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		how, ok := bucketFields[k]
		switch {
		case !ok:
			return badRequest("Invalid argument: %s is not a Bucket field", k)
		case how == "kept", how == "stored", how == "output", empty(body[k]):
		default:
			return badRequest("The bucket field %q is not supported by CloudBurrow yet (%s); it is refused rather than dropped", k, how)
		}
	}
	return nil
}

// empty reports a JSON value that carries nothing: null, or an object or
// array holding only such values.
func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		for _, e := range x {
			if !empty(e) {
				return false
			}
		}
		return true
	case []any:
		for _, e := range x {
			if !empty(e) {
				return false
			}
		}
		return true
	}
	return false
}

// validateKept checks the kept fields' values.
func validateKept(f map[string]any) error {
	for k, v := range f {
		if storedObjects[k] && v != nil {
			if _, ok := v.(map[string]any); !ok {
				return badRequest("Invalid argument: %s must be an object", k)
			}
		}
	}
	if v, ok := f["rpo"]; ok && v != nil {
		if s, _ := v.(string); s != "DEFAULT" && s != "ASYNC_TURBO" {
			return badRequest("Invalid argument: rpo %v must be DEFAULT or ASYNC_TURBO", v)
		}
	}
	if _, err := parseLifecycle(f["lifecycle"]); err != nil {
		return err
	}
	if _, err := parseCORS(f["cors"]); err != nil {
		return err
	}
	if err := checkSoftDeletePolicy(f); err != nil {
		return err
	}
	if v, ok := f["storageClass"]; ok && v != nil {
		if s, _ := v.(string); !storageClasses[strings.ToUpper(s)] {
			return badRequest("Invalid argument: storageClass %v", v)
		} else {
			f["storageClass"] = strings.ToUpper(s)
		}
	}
	if v, ok := f["labels"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		if !isMap {
			return badRequest("Invalid argument: labels must be an object")
		}
		for k, lv := range m {
			if _, isStr := lv.(string); !isStr {
				return badRequest("Invalid argument: label %q must be a string", k)
			}
		}
	}
	for _, k := range []string{"versioning", "softDeletePolicy"} {
		if v, ok := f[k]; ok && v != nil {
			if _, isMap := v.(map[string]any); !isMap {
				return badRequest("Invalid argument: %s must be an object", k)
			}
		}
	}
	if v, ok := f["defaultEventBasedHold"]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return badRequest("Invalid argument: defaultEventBasedHold must be a boolean")
		}
	}
	return nil
}

// mergePatch applies an RFC 7386 JSON merge patch to target.
func mergePatch(target map[string]any, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			tm, _ := target[k].(map[string]any)
			if tm == nil {
				tm = map[string]any{}
			}
			mergePatch(tm, pm)
			target[k] = tm
			continue
		}
		target[k] = v
	}
}

// kept returns only the kept fields of body.
func kept(body map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		if how := bucketFields[k]; how == "kept" || how == "stored" {
			out[k] = v
		}
	}
	return out
}

// defaultBucketFields fills what a create leaves out, as Google does:
// location US, storageClass STANDARD, and the 7-day soft-delete policy
// (docs.cloud.google.com/storage/docs/soft-delete). Soft delete itself is #499.
func defaultBucketFields(f map[string]any, created time.Time) {
	if s, _ := f["location"].(string); s == "" {
		f["location"] = "US"
	} else {
		f["location"] = strings.ToUpper(s)
	}
	if f["storageClass"] == nil {
		f["storageClass"] = "STANDARD"
	}
	if f["softDeletePolicy"] == nil {
		f["softDeletePolicy"] = map[string]any{"retentionDurationSeconds": "604800"}
	}
	if p, ok := f["softDeletePolicy"].(map[string]any); ok && p["effectiveTime"] == nil {
		p["effectiveTime"] = rfc3339(created)
	}
}

func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// etag is the metageneration as Google encodes it: the base64 of a protobuf
// varint field 1 ("CAE=" is metageneration 1).
func etag(metageneration int64) string {
	b := []byte{0x08}
	b = binary.AppendUvarint(b, uint64(metageneration))
	return base64.StdEncoding.EncodeToString(b)
}

func locationType(loc string) string {
	switch loc {
	case "US", "EU", "ASIA":
		return "multi-region"
	case "NAM4", "EUR4", "ASIA1", "EUR5", "EUR7", "EUR8":
		return "dual-region"
	}
	return "region"
}

func (s *Server) bucketJSON(r *http.Request, b bucketRecord) map[string]any {
	out := map[string]any{}
	for k, v := range b.Fields {
		out[k] = v
	}
	loc, _ := out["location"].(string)
	out["kind"] = "storage#bucket"
	out["id"] = b.Name
	out["name"] = b.Name
	out["selfLink"] = baseURL(r) + jsonPrefix + "b/" + escape(b.Name)
	out["projectNumber"] = strconv.FormatInt(resource.ProjectNumber(b.Project), 10)
	out["metageneration"] = strconv.FormatInt(b.Metageneration, 10)
	out["generation"] = strconv.FormatInt(b.Generation, 10)
	out["etag"] = etag(b.Metageneration)
	out["timeCreated"] = rfc3339(b.Created)
	out["updated"] = rfc3339(b.Updated)
	out["locationType"] = locationType(loc)
	if !b.SoftDeleted.IsZero() {
		out["softDeleteTime"], out["hardDeleteTime"] = rfc3339(b.SoftDeleted), rfc3339(b.HardDelete)
	}
	return out
}

// preconditions are ifMetagenerationMatch and ifMetagenerationNotMatch.
type preconditions struct {
	match, notMatch *int64
}

func parsePreconditions(r *http.Request, names ...string) (preconditions, error) {
	var p preconditions
	for i, name := range names {
		v := r.URL.Query().Get(name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return p, badRequest("Invalid argument: %s=%q is not a number", name, v)
		}
		if i == 0 {
			p.match = &n
		} else {
			p.notMatch = &n
		}
	}
	return p, nil
}

// check applies metageneration preconditions. A failed NotMatch on a read is
// 304 Not Modified; anything else that fails is 412 conditionNotMet.
func (p preconditions) check(current int64, read bool) error {
	if p.match != nil && *p.match != current {
		return preconditionFailed("At least one of the pre-conditions you specified did not hold.")
	}
	if p.notMatch != nil && *p.notMatch == current {
		if read {
			return errorf(http.StatusNotModified, "notModified", "not modified")
		}
		return preconditionFailed("At least one of the pre-conditions you specified did not hold.")
	}
	return nil
}

// pathVar is the unescaped value of the i-th segment of a JSON API path.
func pathVar(r *http.Request, prefix string, i int) string {
	segs := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), prefix), "/")
	if i >= len(segs) {
		return ""
	}
	v, err := url.PathUnescape(segs[i])
	if err != nil {
		return segs[i]
	}
	return v
}

func (s *Server) getBucket(tx Tx, name string) (bucketRecord, bool, error) {
	var b bucketRecord
	raw, ok := tx.Get(bucketPrefix + name)
	if !ok {
		return b, false, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, false, err
	}
	return b, true, nil
}

func putBucket(tx Tx, b bucketRecord) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tx.Put(bucketPrefix+b.Name, raw)
	return nil
}

func (s *Server) bucketsInsert(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		writeError(w, required("project"))
		return
	}
	for _, p := range []string{"predefinedAcl", "predefinedDefaultObjectAcl"} {
		if r.URL.Query().Get(p) != "" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s is not implemented: ACL methods are not implemented", p))
			return
		}
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	name, _ := body["name"].(string)
	if name == "" {
		writeError(w, required("name"))
		return
	}
	if err := validBucketName(name); err != nil {
		writeError(w, err)
		return
	}
	if err := checkBucketFields(body); err != nil {
		writeError(w, err)
		return
	}
	f := kept(body)
	if err := validateKept(f); err != nil {
		writeError(w, err)
		return
	}
	now := s.now()
	if err := checkRetentionPolicy(f, nil, now); err != nil {
		writeError(w, err)
		return
	}
	// Object retention is enabled only at creation here (object-lock docs:
	// existing buckets enable it in the console), and never disabled.
	if r.URL.Query().Get("enableObjectRetention") == "true" {
		f["objectRetention"] = map[string]any{"mode": "Enabled"}
	}
	b := bucketRecord{Name: name, Project: project, Created: now, Updated: now, Metageneration: 1, Generation: now.UnixMicro(), Fields: f}
	defaultBucketFields(b.Fields, now)
	err = s.meta.Update(func(tx Tx) error {
		if _, exists, err := s.getBucket(tx, name); err != nil {
			return err
		} else if exists {
			return conflict("Your previous request to create the named bucket succeeded and you already own it.")
		}
		return putBucket(tx, b)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.bucketJSON(r, b))
}

func (s *Server) bucketsGet(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	if r.URL.Query().Get("softDeleted") == "true" {
		s.bucketsGetSoftDeleted(w, r, name)
		return
	}
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	var b bucketRecord
	err = s.meta.View(func(tx Tx) error {
		var ok bool
		var gerr error
		if b, ok, gerr = s.getBucket(tx, name); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		return pre.check(b.Metageneration, true)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.bucketJSON(r, b))
}

func (s *Server) bucketsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := q.Get("project")
	if project == "" {
		writeError(w, required("project"))
		return
	}
	max := 1000
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: maxResults=%q", v))
			return
		}
		if n > 0 && n < max {
			max = n
		}
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			writeError(w, badRequest("Invalid argument: pageToken"))
			return
		}
		after = string(b)
	}
	prefix := q.Get("prefix")
	var items []any
	next := ""
	if q.Get("softDeleted") == "true" {
		// Soft-deleted buckets (#499), by name then generation; the page
		// token is the last one's "<name>/<generation>".
		err := s.meta.View(func(tx Tx) error {
			bs, err := s.softBuckets(tx, project, prefix)
			if err != nil {
				return err
			}
			lastID := ""
			for _, b := range bs {
				id := strings.TrimPrefix(softBucketKey(b.Name, b.Generation), softBucketPrefix)
				if after != "" && id <= after {
					continue
				}
				if len(items) == max {
					next = base64.RawURLEncoding.EncodeToString([]byte(lastID))
					break
				}
				items, lastID = append(items, s.bucketJSON(r, b)), id
			}
			return nil
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeBucketList(w, r, items, next)
		return
	}
	err := s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(bucketPrefix + prefix) {
			name := strings.TrimPrefix(key, bucketPrefix)
			if name <= after {
				continue
			}
			b, ok, err := s.getBucket(tx, name)
			if err != nil {
				return err
			}
			if !ok || b.Project != project {
				continue
			}
			if len(items) == max {
				next = base64.RawURLEncoding.EncodeToString([]byte(items[len(items)-1].(map[string]any)["name"].(string)))
				break
			}
			items = append(items, s.bucketJSON(r, b))
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeBucketList(w, r, items, next)
}

func writeBucketList(w http.ResponseWriter, r *http.Request, items []any, next string) {
	resp := map[string]any{"kind": "storage#buckets"}
	if len(items) > 0 {
		resp["items"] = items
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}

// bucketsModify is patch (merge) and update (replace).
func (s *Server) bucketsModify(replace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := pathVar(r, jsonPrefix, 1)
		pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
		if err != nil {
			writeError(w, err)
			return
		}
		for _, p := range []string{"predefinedAcl", "predefinedDefaultObjectAcl"} {
			if r.URL.Query().Get(p) != "" {
				writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s is not implemented: ACL methods are not implemented", p))
				return
			}
		}
		body, err := readBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := checkBucketFields(body); err != nil {
			writeError(w, err)
			return
		}
		var b bucketRecord
		err = s.meta.Update(func(tx Tx) error {
			var ok bool
			var gerr error
			if b, ok, gerr = s.getBucket(tx, name); gerr != nil {
				return gerr
			} else if !ok {
				return notFound("The specified bucket does not exist.")
			}
			if err := pre.check(b.Metageneration, false); err != nil {
				return err
			}
			next := kept(body)
			old := map[string]any{"softDeletePolicy": copyMap(b.Fields["softDeletePolicy"]), "retentionPolicy": copyMap(b.Fields["retentionPolicy"])}
			if loc, ok := next["location"].(string); ok && !strings.EqualFold(loc, b.Fields["location"].(string)) {
				// UNVERIFIED: Google's answer to changing a bucket's location.
				return badRequest("The location of a bucket cannot be changed.")
			}
			if replace {
				next["location"] = b.Fields["location"]
				if next["softDeletePolicy"] == nil {
					next["softDeletePolicy"] = b.Fields["softDeletePolicy"]
				}
				defaultBucketFields(next, b.Created)
			} else {
				merged := b.Fields
				mergePatch(merged, next)
				next = merged
				if next["storageClass"] == nil {
					next["storageClass"] = "STANDARD"
				}
			}
			if err := validateKept(next); err != nil {
				return err
			}
			stampSoftDeletePolicy(next, old, s.now())
			if err := checkRetentionPolicy(next, old, s.now()); err != nil {
				return err
			}
			next["objectRetention"] = b.Fields["objectRetention"]
			if next["objectRetention"] == nil {
				delete(next, "objectRetention")
			}
			b.Fields = next
			b.Metageneration++
			b.Updated = s.now()
			return putBucket(tx, b)
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, r, http.StatusOK, s.bucketJSON(r, b))
	}
}

func (s *Server) bucketsDelete(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	err = s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, name)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if err := pre.check(b.Metageneration, false); err != nil {
			return err
		}
		if len(tx.List(objectPrefix+name+"/")) > 0 || len(tx.List(noncurrentPrefix+name+"/")) > 0 {
			// UNVERIFIED for the JSON API: no page states this status. The XML
			// API documents 409 BucketNotEmpty, which this mirrors. Noncurrent
			// versions count as content (#498).
			return conflict("The bucket you tried to delete is not empty.")
		}
		tx.Delete(bucketPrefix + name)
		deleteNotifications(tx, name)
		return softDeleteBucket(tx, b, s.now())
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// objectPrefix keys object metadata by bucket; objects are #491.
const objectPrefix = "object/"
