package storage

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Rapid Cache (#1505), to the discovery document's rapidCaches resource and
// google.storage.control.v2's RapidCache (grpccontrol.go serves the same
// records over gRPC). Google documents a Rapid Cache as an Anywhere Cache's
// sibling: a zonal cache of a bucket, of type rapid-cache or
// rapid-cache-ultra, with the same TTL (whole seconds, 1h to 7d, 24h by
// default) and admission policies, plus no_read_admission; its ID is its
// zone ("Currently, the key rapid_cache_id is the zone"). Unlike an
// Anywhere Cache, its update and its disable are long-running operations,
// and it has no pause or resume.
//
// As anywherecache.go, a cache is a record with no effect: CloudBurrow has no
// cache tier, so reads are served from the bucket. The lifecycle is kept:
//
//   - insert names a zone within the bucket's location, one cache per zone
//     (409 while one that is not disabled is there), and returns a create
//     operation; the cache is "creating" until it is done, then "running".
//     The same request ID returns the same operation.
//   - update sets ttl, admissionPolicy and ingestOnWrite of a running cache,
//     as an operation; while it runs pendingUpdate is true and another update
//     is refused.
//   - disable is an operation that takes a running cache to "disabled"; a
//     disabled cache is deleted after disabledGrace, and a create in its zone
//     replaces it.
//   - A create's operation can be cancelled (buckets.operations.cancel); the
//     cache is then removed.
//
// Managed folders name a bucket's Rapid Caches in rapidCacheConfig
// (managedfolders.go): each policy's key and rapidCacheId is a cache of the
// bucket.
//
// UNVERIFIED: every refusal's status and message, the operations'
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/storage/control/v2/storage_control.proto
// commonMetadata.type, whether a disabled cache can be replaced before its
// grace period ends, and the default cacheType (rapid-cache).

const rapidPrefix = "rapidcache/"

const (
	createRapidType  = "create-rapid-cache"
	updateRapidType  = "update-rapid-cache"
	disableRapidType = "disable-rapid-cache"

	rapidCacheType      = "rapid-cache"
	rapidCacheUltraType = "rapid-cache-ultra"
	noReadAdmission     = "no_read_admission"
)

func rapidKey(bucket, id string) string { return rapidPrefix + bucket + "/" + id }

func getRapid(tx Tx, bucket, id string) (cacheRecord, bool, error) {
	var c cacheRecord
	raw, ok := tx.Get(rapidKey(bucket, id))
	if !ok {
		return c, false, nil
	}
	return c, true, json.Unmarshal(raw, &c)
}

func putRapid(tx Tx, c cacheRecord) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tx.Put(rapidKey(c.Bucket, c.ID), raw)
	return nil
}

func rapidMissing(bucket, id string) *httpError {
	return notFound("The Rapid Cache %s does not exist in bucket %s.", id, bucket)
}

func isRapidOp(t string) bool {
	return t == createRapidType || t == updateRapidType || t == disableRapidType
}

// parseRapidAdmission reads a Rapid Cache's admissionPolicy. The control
// API's field documents no_read_admission, while gcloud alpha storage buckets
// rapid-caches sends its --admission-policy as no-read-admission or
// NO_READ_ADMISSION (and ADMIT_ON_FIRST_MISS): each spelling is read as the
// one value, stored as the control API documents it.
func parseRapidAdmission(v any) (string, error) {
	s, _ := v.(string)
	norm := strings.ReplaceAll(strings.ToLower(s), "_", "-")
	if norm == "no-read-admission" {
		return noReadAdmission, nil
	}
	a, err := parseAdmission(norm)
	if err != nil {
		return "", badRequest("Invalid argument: admissionPolicy %v must be no_read_admission, admit-on-first-miss or admit-on-second-miss.", v)
	}
	return a, nil
}

func parseCacheType(v any) (string, error) {
	switch s, _ := v.(string); strings.ToLower(s) {
	case rapidCacheType, rapidCacheUltraType:
		return strings.ToLower(s), nil
	}
	return "", badRequest("Invalid argument: cacheType %v must be rapid-cache or rapid-cache-ultra.", v)
}

// settleRapid deletes a bucket's Rapid Caches disabled past their grace.
func (s *Server) settleRapid(tx Tx, bucket string, now time.Time) error {
	for _, k := range tx.List(rapidPrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var c cacheRecord
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.State == cacheDisabled && !c.Disabled.IsZero() && !now.Before(c.Disabled.Add(disabledGrace)) {
			tx.Delete(k)
		}
	}
	return nil
}

// advanceRapid does a Rapid Cache operation's due work.
func (s *Server) advanceRapid(tx Tx, op *operationRecord, now time.Time) (bool, error) {
	c, ok, err := getRapid(tx, op.Bucket, op.Cache.ID)
	if err != nil {
		return false, err
	}
	if !ok || (op.Type != createRapidType && c.CreateOp != op.Cache.CreateOp) {
		op.finish(now, &opStatus{Code: 5, Message: "The Rapid Cache was deleted."})
		return true, nil
	}
	switch op.Type {
	case createRapidType:
		c.State = cacheRunning
	case updateRapidType:
		for _, f := range op.Cache.Set {
			switch f {
			case "ttl":
				c.TTL = op.Cache.TTL
			case "admissionPolicy":
				c.Admission = op.Cache.Admission
			case "ingestOnWrite":
				c.IngestOnWrite = op.Cache.IngestOnWrite
			}
		}
		c.PendingUpdate = false
	case disableRapidType:
		c.State, c.Disabled = cacheDisabled, now
	}
	c.Updated = now
	if err := putRapid(tx, c); err != nil {
		return false, err
	}
	op.Result = &c
	op.finish(now, nil)
	return true, nil
}

func rapidV2Name(c cacheRecord) string { return folderResource(c.Bucket) + "/rapidCaches/" + c.ID }

func (s *Server) rapidJSON(r *http.Request, c cacheRecord) map[string]any {
	out := map[string]any{
		"kind": "storage#rapidCache", "id": c.Bucket + "/" + c.ID,
		"selfLink": baseURL(r) + jsonPrefix + "b/" + escape(c.Bucket) + "/rapidCaches/" + escape(c.ID),
		"bucket":   c.Bucket, "rapidCacheId": c.ID, "zone": c.Zone, "cacheType": c.CacheType,
		"ttl": ttlString(c.TTL), "admissionPolicy": c.Admission, "state": c.State,
		"createTime": rfc3339(c.Created), "updateTime": rfc3339(c.Updated),
		"pendingUpdate": c.PendingUpdate,
	}
	if c.IngestOnWrite != nil {
		out["ingestOnWrite"] = *c.IngestOnWrite
	}
	return out
}

// rapidV2JSON is the cache as google.storage.control.v2.RapidCache's JSON,
// an operation's response.
func rapidV2JSON(c cacheRecord) map[string]any {
	return map[string]any{
		"name": rapidV2Name(c), "zone": c.Zone, "cacheType": c.CacheType, "ttl": ttlString(c.TTL),
		"admissionPolicy": c.Admission, "state": c.State,
		"createTime": rfc3339(c.Created), "updateTime": rfc3339(c.Updated), "pendingUpdate": c.PendingUpdate,
	}
}

func rapidOpMetadata(op operationRecord, common map[string]any) map[string]any {
	t := map[string]string{createRapidType: "CreateRapidCacheMetadata", updateRapidType: "UpdateRapidCacheMetadata",
		disableRapidType: "DisableRapidCacheMetadata"}[op.Type]
	out := map[string]any{"@type": "type.googleapis.com/google.storage.control.v2." + t,
		"commonMetadata": common, "rapidCacheId": op.Cache.ID, "zone": op.Cache.Zone}
	if op.Cache.CacheType != "" {
		out["cacheType"] = op.Cache.CacheType
	}
	if op.Type == disableRapidType {
		return out
	}
	set := func(f string) bool {
		if op.Type == createRapidType {
			return true
		}
		for _, v := range op.Cache.Set {
			if v == f {
				return true
			}
		}
		return false
	}
	if set("ttl") {
		out["ttl"] = ttlString(op.Cache.TTL)
	}
	if set("admissionPolicy") {
		out["admissionPolicy"] = op.Cache.Admission
	}
	if set("ingestOnWrite") && op.Cache.IngestOnWrite != nil {
		out["ingestOnWrite"] = *op.Cache.IngestOnWrite
	}
	return out
}

func (s *Server) rapidCachesInsert(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	in := cacheOp{TTL: 86400, Admission: admitFirstMiss, CacheType: rapidCacheType}
	for k, v := range body {
		switch k {
		case "zone":
			in.Zone, _ = v.(string)
		case "rapidCacheId":
			in.ID, _ = v.(string)
		case "cacheType":
			if in.CacheType, err = parseCacheType(v); err != nil {
				writeError(w, err)
				return
			}
		case "ttl":
			if in.TTL, err = parseTTL(v); err != nil {
				writeError(w, err)
				return
			}
		case "admissionPolicy":
			if in.Admission, err = parseRapidAdmission(v); err != nil {
				writeError(w, err)
				return
			}
		case "ingestOnWrite":
			b, ok := v.(bool)
			if !ok {
				writeError(w, badRequest("Invalid argument: ingestOnWrite must be a boolean."))
				return
			}
			in.IngestOnWrite = &b
		case "kind", "bucket", "id", "selfLink", "state", "createTime", "updateTime", "pendingUpdate":
		default:
			writeError(w, badRequest("Invalid argument: %s is not a RapidCache field", k))
			return
		}
	}
	if in.Zone == "" {
		writeError(w, required("zone"))
		return
	}
	in.Zone = strings.ToLower(in.Zone)
	if !zoneRE.MatchString(in.Zone) {
		writeError(w, badRequest("Invalid argument: zone %q is not a zone, such as us-central1-a.", in.Zone))
		return
	}
	if in.ID != "" && in.ID != in.Zone {
		writeError(w, badRequest("Invalid argument: rapidCacheId %q must be the cache's zone %s.", in.ID, in.Zone))
		return
	}
	in.ID = in.Zone
	reqID := r.Header.Get(cacheRequestHeader)
	var op operationRecord
	err = s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if err := s.settle(tx, bucket); err != nil {
			return err
		}
		if !zoneInLocation(b, in.Zone) {
			loc, _ := b.Fields["location"].(string)
			return badRequest("Invalid argument: zone %s is not within the bucket's location %s.", in.Zone, loc)
		}
		if old, ok, err := getRapid(tx, bucket, in.ID); err != nil {
			return err
		} else if ok {
			if reqID != "" && old.RequestID == reqID && old.CreateOp != "" {
				if raw, found := tx.Get(operationKey(bucket, old.CreateOp)); found {
					return json.Unmarshal(raw, &op)
				}
			}
			if old.State != cacheDisabled {
				return conflict("A Rapid Cache already exists in zone %s for bucket %s.", in.Zone, bucket)
			}
		}
		now := s.now()
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: createRapidType, Created: now,
			Pending: true, Due: now.Add(pendingDelay), Cache: &in}
		in.CreateOp = op.ID
		c := cacheRecord{Bucket: bucket, ID: in.ID, Zone: in.Zone, CacheType: in.CacheType, TTL: in.TTL,
			Admission: in.Admission, IngestOnWrite: in.IngestOnWrite, State: cacheCreating, Created: now,
			Updated: now, RequestID: reqID, CreateOp: op.ID}
		if err := putRapid(tx, c); err != nil {
			return err
		}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

func (s *Server) rapidCachesUpdate(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	in := cacheOp{ID: id}
	for k, v := range body {
		switch k {
		case "ttl":
			if in.TTL, err = parseTTL(v); err != nil {
				writeError(w, err)
				return
			}
		case "admissionPolicy":
			if in.Admission, err = parseRapidAdmission(v); err != nil {
				writeError(w, err)
				return
			}
		case "ingestOnWrite":
			b, ok := v.(bool)
			if !ok {
				writeError(w, badRequest("Invalid argument: ingestOnWrite must be a boolean."))
				return
			}
			in.IngestOnWrite = &b
		case "kind", "bucket", "id", "rapidCacheId", "selfLink", "state", "createTime", "updateTime", "pendingUpdate", "zone", "cacheType":
			continue
		default:
			writeError(w, badRequest("Invalid argument: %s is not a RapidCache field", k))
			return
		}
		in.Set = append(in.Set, k)
	}
	if len(in.Set) == 0 {
		writeError(w, badRequest("Invalid argument: an update sets at least one of ttl, admissionPolicy and ingestOnWrite."))
		return
	}
	var op operationRecord
	err = s.meta.Update(func(tx Tx) error {
		c, err := s.existingRapid(tx, bucket, id)
		if err != nil {
			return err
		}
		if c.State != cacheRunning {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The Rapid Cache %s is %s; only a running cache can be updated.", id, c.State)
		}
		if c.PendingUpdate {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The Rapid Cache %s has an update in progress.", id)
		}
		in.Zone, in.CacheType, in.CreateOp = c.Zone, c.CacheType, c.CreateOp
		now := s.now()
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: updateRapidType, Created: now,
			Pending: true, Due: now.Add(pendingDelay), Cache: &in}
		c.PendingUpdate, c.Updated = true, now
		if err := putRapid(tx, c); err != nil {
			return err
		}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

func (s *Server) rapidCachesDisable(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	var op operationRecord
	err := s.meta.Update(func(tx Tx) error {
		c, err := s.existingRapid(tx, bucket, id)
		if err != nil {
			return err
		}
		if c.State != cacheRunning {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The Rapid Cache %s is %s; only a running cache can be disabled.", id, c.State)
		}
		now := s.now()
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: disableRapidType, Created: now,
			Pending: true, Due: now.Add(pendingDelay),
			Cache: &cacheOp{ID: id, Zone: c.Zone, CacheType: c.CacheType, CreateOp: c.CreateOp}}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

// existingRapid settles the bucket and reads one of its Rapid Caches.
func (s *Server) existingRapid(tx Tx, bucket, id string) (cacheRecord, error) {
	if _, ok, err := s.getBucket(tx, bucket); err != nil {
		return cacheRecord{}, err
	} else if !ok {
		return cacheRecord{}, notFound("The specified bucket does not exist.")
	}
	if err := s.settle(tx, bucket); err != nil {
		return cacheRecord{}, err
	}
	c, ok, err := getRapid(tx, bucket, id)
	if err != nil {
		return c, err
	} else if !ok {
		return c, rapidMissing(bucket, id)
	}
	return c, nil
}

func (s *Server) rapidCachesGet(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	var c cacheRecord
	err := s.meta.Update(func(tx Tx) (err error) {
		c, err = s.existingRapid(tx, bucket, id)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.rapidJSON(r, c))
}

func (s *Server) rapidCachesList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	max := 1000
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: pageSize=%q", v))
			return
		}
		if n > 0 && n < max {
			max = n
		}
	}
	after := q.Get("pageToken")
	items := []any{}
	next := ""
	err := s.meta.Update(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if err := s.settle(tx, bucket); err != nil {
			return err
		}
		last := ""
		for _, k := range tx.List(rapidPrefix + bucket + "/") {
			id := strings.TrimPrefix(k, rapidPrefix+bucket+"/")
			if after != "" && id <= after {
				continue
			}
			if len(items) == max {
				next = last
				break
			}
			c, _, err := getRapid(tx, bucket, id)
			if err != nil {
				return err
			}
			items, last = append(items, s.rapidJSON(r, c)), id
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{"kind": "storage#rapidCaches", "items": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, out)
}
