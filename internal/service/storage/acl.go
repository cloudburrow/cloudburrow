package storage

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Access control lists (#1233), stored and returned, never enforced: the
// maintainer's 2026-10-15 decision, recorded in ADR-0006's amendment of that
// date. CloudBurrow authenticates no caller, so no request is ever allowed
// or refused by an ACL; a client reads back exactly what it set.
//
// Served, as docs.cloud.google.com/storage/docs/access-control/lists and the
// JSON API reference describe them:
//
//   - bucketAccessControls, defaultObjectAccessControls and
//     objectAccessControls: get, list, insert, patch, update and delete;
//   - the acl and defaultObjectAcl fields of a bucket, and the acl field of
//     an object, returned under projection=full;
//   - predefinedAcl and predefinedDefaultObjectAcl on buckets.insert, patch
//     and update, and predefinedAcl on objects.insert, patch and update.
//
// A new object takes its bucket's default object ACL. Under uniform
// bucket-level access every ACL method fails 400, and a full projection
// carries empty ACL lists (uniform-bucket-level-access docs).
//
// CloudBurrow has no caller identity, so the "object owner" that Google's
// predefined object ACLs grant OWNER is the bucket's project owners,
// project-owners-<number>. Project numbers are resource.ProjectNumber's.

// aclEntry is one stored entry: an entity and its role.
type aclEntry struct {
	Entity string `json:"entity"`
	Role   string `json:"role"`
}

// aclKind is which of the three ACLs a request addresses.
type aclKind int

const (
	bucketACL aclKind = iota
	defaultObjectACL
	objectACL
)

// entityRE is every entity form the ACL docs give: user-, group- and
// domain- scopes, project-<team>-<project>, allUsers and
// allAuthenticatedUsers.
var entityRE = regexp.MustCompile(`^(?:(user|group|domain)-(.+)|project-(owners|editors|viewers)-([a-z0-9-]+)|allUsers|allAuthenticatedUsers)$`)

// uniformRefusal is the 400 every ACL request on a bucket with uniform
// bucket-level access gets (uniform-bucket-level-access docs: such requests
// "fail with 400 Bad Request errors"). UNVERIFIED: the message.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/uniform-bucket-level-access
func uniformRefusal() error {
	return badRequest("Cannot use ACL API to manage access when uniform bucket-level access is enabled. Read more at https://cloud.google.com/storage/docs/uniform-bucket-level-access")
}

// validRoles are the roles each ACL accepts: an object's ACL has no WRITER
// (objectAccessControls reference: "OWNER, READER").
func validRole(kind aclKind, role string) bool {
	switch role {
	case "OWNER", "READER":
		return true
	case "WRITER":
		return kind == bucketACL
	}
	return false
}

// normalizeEntity checks an entity and spells a project entity by number,
// as Google returns it: project-owners-my-project is
// project-owners-<number of my-project>.
func normalizeEntity(entity string) (string, error) {
	m := entityRE.FindStringSubmatch(entity)
	if m == nil {
		// UNVERIFIED: the message.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
		return "", badRequest("Invalid argument: entity %q is not user-, group-, domain-, project-<team>-<project>, allUsers or allAuthenticatedUsers", entity)
	}
	if m[3] != "" {
		if _, err := strconv.ParseInt(m[4], 10, 64); err != nil {
			return "project-" + m[3] + "-" + strconv.FormatInt(resource.ProjectNumber(m[4]), 10), nil
		}
	}
	return entity, nil
}

// parseEntry reads one entry from a request body.
func parseEntry(kind aclKind, v any) (aclEntry, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return aclEntry{}, badRequest("Invalid argument: an access control entry must be an object")
	}
	entity, _ := m["entity"].(string)
	role, _ := m["role"].(string)
	if entity == "" {
		return aclEntry{}, required("entity")
	}
	if role == "" {
		return aclEntry{}, required("role")
	}
	e, err := normalizeEntity(entity)
	if err != nil {
		return aclEntry{}, err
	}
	role = strings.ToUpper(role)
	if !validRole(kind, role) {
		allowed := "OWNER or READER"
		if kind == bucketACL {
			allowed = "OWNER, READER or WRITER"
		}
		// UNVERIFIED: the message.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
		return aclEntry{}, badRequest("Invalid argument: role %q must be %s", role, allowed)
	}
	return aclEntry{Entity: e, Role: role}, nil
}

// parseACL reads an acl or defaultObjectAcl field: a list of entries, each
// entity at most once (the last wins).
func parseACL(kind aclKind, v any) ([]aclEntry, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, badRequest("Invalid argument: an ACL must be a list of entries")
	}
	out := []aclEntry{}
	for _, item := range list {
		e, err := parseEntry(kind, item)
		if err != nil {
			return nil, err
		}
		out = setEntry(out, e)
	}
	return out, nil
}

// setEntry adds e, or changes the role of the entry for its entity.
func setEntry(acl []aclEntry, e aclEntry) []aclEntry {
	out := append([]aclEntry{}, acl...)
	for i := range out {
		if out[i].Entity == e.Entity {
			out[i].Role = e.Role
			return out
		}
	}
	return append(out, e)
}

func findEntry(acl []aclEntry, entity string) (aclEntry, bool) {
	for _, e := range acl {
		if e.Entity == entity {
			return e, true
		}
	}
	return aclEntry{}, false
}

func removeEntry(acl []aclEntry, entity string) ([]aclEntry, bool) {
	out := []aclEntry{}
	found := false
	for _, e := range acl {
		if e.Entity == entity {
			found = true
			continue
		}
		out = append(out, e)
	}
	return out, found
}

func team(project, t string) string {
	return "project-" + t + "-" + strconv.FormatInt(resource.ProjectNumber(project), 10)
}

// predefinedBucketACL is a predefinedAcl for a bucket (buckets.insert
// reference).
func predefinedBucketACL(project, name string) ([]aclEntry, error) {
	owners := aclEntry{team(project, "owners"), "OWNER"}
	switch name {
	case "private":
		return []aclEntry{owners}, nil
	case "projectPrivate":
		return []aclEntry{owners, {team(project, "editors"), "OWNER"}, {team(project, "viewers"), "READER"}}, nil
	case "publicRead":
		return []aclEntry{owners, {"allUsers", "READER"}}, nil
	case "publicReadWrite":
		return []aclEntry{owners, {"allUsers", "WRITER"}}, nil
	case "authenticatedRead":
		return []aclEntry{owners, {"allAuthenticatedUsers", "READER"}}, nil
	}
	return nil, badRequest("Invalid argument: predefinedAcl %q must be authenticatedRead, private, projectPrivate, publicRead or publicReadWrite", name)
}

// predefinedObjectACL is a predefinedAcl for an object, or a
// predefinedDefaultObjectAcl for a bucket (objects.insert reference). The
// object owner is the project owners (see the top of this file).
func predefinedObjectACL(project, name string) ([]aclEntry, error) {
	owner := aclEntry{team(project, "owners"), "OWNER"}
	switch name {
	case "private", "bucketOwnerFullControl":
		return []aclEntry{owner}, nil
	case "bucketOwnerRead":
		// The bucket owners' READER is the owner's OWNER here: they are
		// the same entity.
		return []aclEntry{owner}, nil
	case "projectPrivate":
		return []aclEntry{owner, {team(project, "editors"), "OWNER"}, {team(project, "viewers"), "READER"}}, nil
	case "publicRead":
		return []aclEntry{owner, {"allUsers", "READER"}}, nil
	case "authenticatedRead":
		return []aclEntry{owner, {"allAuthenticatedUsers", "READER"}}, nil
	}
	return nil, badRequest("Invalid argument: predefined object ACL %q must be authenticatedRead, bucketOwnerFullControl, bucketOwnerRead, private, projectPrivate or publicRead", name)
}

// The stored ACLs. A record written before ACLs were stored has none, and
// reads as Google's default, projectPrivate.

func (b bucketRecord) acl() []aclEntry {
	if b.ACL != nil {
		return b.ACL
	}
	acl, _ := predefinedBucketACL(b.Project, "projectPrivate")
	return acl
}

func (b bucketRecord) defaultObjectACL() []aclEntry {
	if b.DefaultObjectACL != nil {
		return b.DefaultObjectACL
	}
	acl, _ := predefinedObjectACL(b.Project, "projectPrivate")
	return acl
}

func objectACLOf(b bucketRecord, o objectRecord) []aclEntry {
	if o.ACL != nil {
		return o.ACL
	}
	return b.defaultObjectACL()
}

// newObjectACL is the ACL a new object takes: its bucket's default object
// ACL, unless the request set one.
func newObjectACL(b bucketRecord) []aclEntry {
	return append([]aclEntry{}, b.defaultObjectACL()...)
}

// fullProjection says whether a response carries the ACLs: projection=full,
// or, where a method defaults to full (bucketsInsert, objects with an acl),
// def.
func fullProjection(r *http.Request, def bool) bool {
	if r == nil {
		return false
	}
	switch r.URL.Query().Get("projection") {
	case "full":
		return true
	case "noAcl":
		return false
	}
	return def
}

func checkProjection(r *http.Request) error {
	switch p := r.URL.Query().Get("projection"); p {
	case "", "full", "noAcl":
		return nil
	default:
		return badRequest("Invalid argument: projection=%s must be full or noAcl", p)
	}
}

// entryJSON is an entry as a BucketAccessControl or ObjectAccessControl.
// object is "" for a bucket's ACL and its default object ACL.
func entryJSON(r *http.Request, kind aclKind, bucket, object string, generation int64, tag string, e aclEntry) map[string]any {
	out := map[string]any{"entity": e.Entity, "role": e.Role, "etag": tag}
	base := baseURL(r) + jsonPrefix + "b/" + escape(bucket)
	switch kind {
	case bucketACL:
		out["kind"] = "storage#bucketAccessControl"
		out["bucket"] = bucket
		out["id"] = bucket + "/" + e.Entity
		out["selfLink"] = base + "/acl/" + escape(e.Entity)
	case defaultObjectACL:
		out["kind"] = "storage#objectAccessControl"
	case objectACL:
		gen := strconv.FormatInt(generation, 10)
		out["kind"] = "storage#objectAccessControl"
		out["bucket"], out["object"], out["generation"] = bucket, object, gen
		out["id"] = bucket + "/" + object + "/" + gen + "/" + e.Entity
		out["selfLink"] = base + "/o/" + escape(object) + "/acl/" + escape(e.Entity)
	}
	m := entityRE.FindStringSubmatch(e.Entity)
	switch {
	case m == nil:
	case m[1] == "domain":
		out["domain"] = m[2]
	case m[1] != "" && strings.Contains(m[2], "@"):
		out["email"] = m[2]
	case m[1] != "":
		out["entityId"] = m[2]
	case m[3] != "":
		out["projectTeam"] = map[string]any{"projectNumber": m[4], "team": m[3]}
	}
	return out
}

func aclJSON(r *http.Request, kind aclKind, bucket, object string, generation int64, tag string, acl []aclEntry) []any {
	out := []any{}
	for _, e := range acl {
		out = append(out, entryJSON(r, kind, bucket, object, generation, tag, e))
	}
	return out
}

// bucketACLFields adds acl, defaultObjectAcl and owner to a bucket under a
// full projection; under uniform access the lists are empty.
func bucketACLFields(r *http.Request, out map[string]any, b bucketRecord) {
	tag := etag(b.Metageneration)
	out["owner"] = map[string]any{"entity": team(b.Project, "owners")}
	if uniformAccess(b) {
		out["acl"], out["defaultObjectAcl"] = []any{}, []any{}
		return
	}
	out["acl"] = aclJSON(r, bucketACL, b.Name, "", 0, tag, b.acl())
	out["defaultObjectAcl"] = aclJSON(r, defaultObjectACL, b.Name, "", 0, tag, b.defaultObjectACL())
}

// objectACLFields adds acl and owner to an object under a full projection.
func objectACLFields(r *http.Request, out map[string]any, b bucketRecord, o objectRecord) {
	out["owner"] = map[string]any{"entity": team(b.Project, "owners")}
	if uniformAccess(b) {
		out["acl"] = []any{}
		return
	}
	out["acl"] = aclJSON(r, objectACL, o.Bucket, o.Name, o.Generation, objectETag(o), objectACLOf(b, o))
}

// bucketBodyACL applies a bucket body's acl and defaultObjectAcl, and the
// predefinedAcl and predefinedDefaultObjectAcl parameters, which take
// precedence (buckets.insert reference: "predefinedAcl ... overrides the
// acl property"). An ACL a body leaves out stays, on update as on patch,
// so that saving a bucket's other settings never resets its ACL; null
// resets it to the default.
func bucketBodyACL(r *http.Request, body map[string]any, b *bucketRecord) error {
	q := r.URL.Query()
	pa, pd := q.Get("predefinedAcl"), q.Get("predefinedDefaultObjectAcl")
	_, hasACL := body["acl"]
	_, hasDef := body["defaultObjectAcl"]
	if uniformAccess(*b) && (pa != "" || pd != "" || !empty(body["acl"]) || !empty(body["defaultObjectAcl"])) {
		return uniformRefusal()
	}
	if hasACL && body["acl"] != nil {
		acl, err := parseACL(bucketACL, body["acl"])
		if err != nil {
			return err
		}
		b.ACL = acl
	} else if hasACL {
		b.ACL = nil
	}
	if hasDef && body["defaultObjectAcl"] != nil {
		acl, err := parseACL(defaultObjectACL, body["defaultObjectAcl"])
		if err != nil {
			return err
		}
		b.DefaultObjectACL = acl
	} else if hasDef {
		b.DefaultObjectACL = nil
	}
	if pa != "" {
		acl, err := predefinedBucketACL(b.Project, pa)
		if err != nil {
			return err
		}
		b.ACL = acl
	}
	if pd != "" {
		acl, err := predefinedObjectACL(b.Project, pd)
		if err != nil {
			return err
		}
		b.DefaultObjectACL = acl
	}
	return nil
}

// setUploadACL sets a new object's ACL from its upload: predefinedAcl
// over the metadata's acl, and the bucket's default object ACL without
// either (checkNewObject).
func setUploadACL(b bucketRecord, o *objectRecord, meta uploadMeta) error {
	var raw any
	if len(meta.ACL) > 0 {
		if err := json.Unmarshal(meta.ACL, &raw); err != nil {
			return badRequest("Invalid argument: acl must be a list of entries")
		}
	}
	acl, ok, err := objectRequestACL(b, meta.PredefinedACL, raw, raw != nil)
	if err != nil {
		return err
	}
	if ok {
		o.ACL = acl
	}
	return nil
}

// objectRequestACL is the ACL an object request sets: predefinedAcl p over
// the body's acl. ok is false when it sets none.
func objectRequestACL(b bucketRecord, p string, rawACL any, hasACL bool) (acl []aclEntry, ok bool, err error) {
	if uniformAccess(b) && (p != "" || !empty(rawACL)) {
		return nil, false, uniformRefusal()
	}
	if p != "" {
		acl, err = predefinedObjectACL(b.Project, p)
		return acl, err == nil, err
	}
	if hasACL && rawACL != nil {
		acl, err = parseACL(objectACL, rawACL)
		return acl, err == nil, err
	}
	return nil, false, nil
}

// The three ACL resources. Each path is b/{bucket}/acl[/{entity}],
// b/{bucket}/defaultObjectAcl[/{entity}] or
// b/{bucket}/o/{object}/acl[/{entity}].

// aclTarget is what one ACL request addresses.
type aclTarget struct {
	kind           aclKind
	bucket, object string
	entity         string // "" for list and insert
}

func aclRequest(r *http.Request, kind aclKind, withEntity bool) aclTarget {
	t := aclTarget{kind: kind, bucket: pathVar(r, jsonPrefix, 1)}
	i := 3
	if kind == objectACL {
		t.object = pathVar(r, jsonPrefix, 3)
		i = 5
	}
	if withEntity {
		t.entity = pathVar(r, jsonPrefix, i)
	}
	return t
}

// aclState is the stored ACL a request reads or changes, with the record
// it belongs to.
type aclState struct {
	b    bucketRecord
	o    objectRecord
	live bool
}

func (st aclState) list(kind aclKind) []aclEntry {
	switch kind {
	case bucketACL:
		return st.b.acl()
	case defaultObjectACL:
		return st.b.defaultObjectACL()
	}
	return objectACLOf(st.b, st.o)
}

func (st aclState) tag(kind aclKind) string {
	if kind == objectACL {
		return objectETag(st.o)
	}
	return etag(st.b.Metageneration)
}

func (st aclState) render(r *http.Request, t aclTarget, e aclEntry) map[string]any {
	return entryJSON(r, t.kind, t.bucket, t.object, st.o.Generation, st.tag(t.kind), e)
}

// loadACL reads the bucket (and the object, at the request's generation)
// and refuses under uniform access.
func (s *Server) loadACL(tx Tx, r *http.Request, t aclTarget) (aclState, error) {
	var st aclState
	b, ok, err := s.getBucket(tx, t.bucket)
	if err != nil {
		return st, err
	} else if !ok {
		return st, notFound("The specified bucket does not exist.")
	}
	st.b = b
	if uniformAccess(b) {
		return st, uniformRefusal()
	}
	if t.kind == objectACL {
		o, live, exists, err := findVersion(tx, t.bucket, t.object, r.URL.Query().Get("generation"))
		if err != nil {
			return st, err
		}
		if !exists {
			return st, notFound("No such object: %s/%s", t.bucket, t.object)
		}
		st.o, st.live = o, live
	} else {
		pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
		if err != nil {
			return st, err
		}
		if err := pre.check(b.Metageneration, r.Method == http.MethodGet); err != nil {
			return st, err
		}
	}
	return st, nil
}

// storeACL writes a changed ACL back, advancing the metageneration of the
// bucket or the object it belongs to.
func (s *Server) storeACL(tx Tx, st *aclState, kind aclKind, acl []aclEntry) error {
	now := s.now()
	switch kind {
	case bucketACL, defaultObjectACL:
		if kind == bucketACL {
			st.b.ACL = acl
		} else {
			st.b.DefaultObjectACL = acl
		}
		st.b.Metageneration++
		st.b.Updated = now
		return putBucket(tx, st.b)
	}
	st.o.ACL = acl
	st.o.Metageneration++
	st.o.Updated = now
	if err := putVersion(tx, st.o, st.live); err != nil {
		return err
	}
	return emit(tx, objectEvent{Type: EventMetadataUpdate, Object: st.o, Time: now})
}

func notFoundEntry(t aclTarget) error {
	// UNVERIFIED: the message.
	// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
	return notFound("No such access control entry: %s", t.entity)
}

func (s *Server) aclList(kind aclKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := aclRequest(r, kind, false)
		var st aclState
		err := s.meta.View(func(tx Tx) (err error) {
			st, err = s.loadACL(tx, r, t)
			return err
		})
		if err != nil {
			writeError(w, err)
			return
		}
		listKind := "storage#objectAccessControls"
		if kind == bucketACL {
			listKind = "storage#bucketAccessControls"
		}
		writeResponse(w, r, http.StatusOK, map[string]any{"kind": listKind,
			"items": aclJSON(r, kind, t.bucket, t.object, st.o.Generation, st.tag(kind), st.list(kind))})
	}
}

func (s *Server) aclGet(kind aclKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := aclRequest(r, kind, true)
		var st aclState
		var e aclEntry
		err := s.meta.View(func(tx Tx) (err error) {
			if st, err = s.loadACL(tx, r, t); err != nil {
				return err
			}
			entity, err := normalizeEntity(t.entity)
			if err != nil {
				return err
			}
			var ok bool
			if e, ok = findEntry(st.list(kind), entity); !ok {
				return notFoundEntry(t)
			}
			return nil
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, r, http.StatusOK, st.render(r, t, e))
	}
}

// aclSet is insert (entity from the body) and patch and update (entity
// from the path). Each sets the entity's role, adding the entry if it is
// not there: the Go client's ACLHandle.Set adds entries with update.
func (s *Server) aclSet(kind aclKind, fromPath bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := aclRequest(r, kind, fromPath)
		body, err := readBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if fromPath {
			if be, _ := body["entity"].(string); be == "" {
				body["entity"] = t.entity
			}
		}
		var st aclState
		var e aclEntry
		err = s.meta.Update(func(tx Tx) (err error) {
			if st, err = s.loadACL(tx, r, t); err != nil {
				return err
			}
			if e, err = parseEntry(kind, body); err != nil {
				return err
			}
			if fromPath {
				pe, err := normalizeEntity(t.entity)
				if err != nil {
					return err
				}
				if pe != e.Entity {
					// UNVERIFIED: the message.
					// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
					return badRequest("Invalid argument: the body's entity %q is not the path's %q", e.Entity, pe)
				}
			}
			return s.storeACL(tx, &st, kind, setEntry(st.list(kind), e))
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, r, http.StatusOK, st.render(r, t, e))
	}
}

func (s *Server) aclDelete(kind aclKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := aclRequest(r, kind, true)
		err := s.meta.Update(func(tx Tx) error {
			st, err := s.loadACL(tx, r, t)
			if err != nil {
				return err
			}
			entity, err := normalizeEntity(t.entity)
			if err != nil {
				return err
			}
			acl, found := removeEntry(st.list(kind), entity)
			if !found {
				return notFoundEntry(t)
			}
			return s.storeACL(tx, &st, kind, acl)
		})
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// registerACL binds the eighteen ACL methods.
func (s *Server) registerACL() {
	for prefix, kind := range map[string]aclKind{
		"storage.bucketAccessControls.":        bucketACL,
		"storage.defaultObjectAccessControls.": defaultObjectACL,
		"storage.objectAccessControls.":        objectACL,
	} {
		s.handlers[prefix+"list"] = s.aclList(kind)
		s.handlers[prefix+"get"] = s.aclGet(kind)
		s.handlers[prefix+"insert"] = s.aclSet(kind, false)
		s.handlers[prefix+"patch"] = s.aclSet(kind, true)
		s.handlers[prefix+"update"] = s.aclSet(kind, true)
		s.handlers[prefix+"delete"] = s.aclDelete(kind)
	}
}
