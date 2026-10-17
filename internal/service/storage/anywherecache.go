package storage

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Anywhere Cache (#1444), to docs.cloud.google.com/storage/docs/anywhere-cache
// and .../using-anywhere-cache, the discovery document's anywhereCaches
// resource, and google.storage.control.v2's AnywhereCache (grpccontrol.go
// serves the same records over gRPC).
//
// A cache is a record with no effect: CloudBurrow keeps no cache tier, so a
// read is served from the bucket whatever caches it has, and nothing is
// ingested, admitted or evicted. What is kept is what Google documents of the
// resource and its lifecycle:
//
//   - insert names a zone within the bucket's location, at most one cache per
//     zone ("ANYWHERE_CACHE_ALREADY_EXISTS", 409), and returns a long-running
//     create operation; the cache is "creating" until the operation is done
//     (a few seconds here, pendingDelay), then "running". The TTL is whole
//     seconds from 1h to 7d, 24h by default; the admission policy is
//     admit-on-first-miss (the default) or admit-on-second-miss. A create
//     sent again with the same request ID returns the same operation.
//   - update changes ttl, admissionPolicy and ingestOnWrite of a running
//     cache, as a long-running operation; while it runs pendingUpdate is
//     true, and another update is refused.
//   - pause takes a running cache to "paused"; resume takes a paused or
//     disabled one back to "running"; disable takes a running or paused one
//     to "disabled", and a cache disabled for an hour is deleted.
//   - Only a create operation can be cancelled (buckets.operations.cancel):
//     the cache is then removed. An update's cannot.
//
// UNVERIFIED: the statuses and messages of every refusal, the cache's id
// ("{bucket}/{anywhereCacheId}", gcloud's CACHE_ID), the operation's
// commonMetadata.type of an update, and the multi-region and predefined
// dual-region zone lists.

const cachePrefix = "anywherecache/"

// pendingDelay is how long a long-running operation with work left (a cache
// create or update, a relocation's sync) runs before it is done. Nothing is
// copied or warmed locally, so it is short; it is not zero so that an
// operation can be seen running and cancelled, as on Google.
var pendingDelay = 3 * time.Second

// disabledGrace is how long a disabled cache can be resumed before it is
// deleted (anywhere-cache: "After this 1-hour grace period, the cache gets
// deleted").
const disabledGrace = time.Hour

type cacheRecord struct {
	Bucket        string    `json:"bucket"`
	ID            string    `json:"id"`
	Zone          string    `json:"zone"`
	CacheType     string    `json:"cacheType,omitempty"` // a Rapid Cache's (rapidcache.go)
	TTL           int64     `json:"ttl"`                 // seconds
	Admission     string    `json:"admission"`
	IngestOnWrite *bool     `json:"ingestOnWrite,omitempty"`
	State         string    `json:"state"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	PendingUpdate bool      `json:"pendingUpdate,omitempty"`
	Disabled      time.Time `json:"disabled,omitempty"`
	RequestID     string    `json:"requestId,omitempty"`
	CreateOp      string    `json:"createOp,omitempty"`
}

// cacheOp is what a cache operation carries: the values it creates or sets.
type cacheOp struct {
	ID            string `json:"id"`
	Zone          string `json:"zone"`
	TTL           int64  `json:"ttl"`
	Admission     string `json:"admission"`
	IngestOnWrite *bool  `json:"ingestOnWrite,omitempty"`
	// Set says which fields an update sets.
	Set []string `json:"set,omitempty"`
	// CacheType and CreateOp are a Rapid Cache's type and the create
	// operation of the cache an update or disable is for (rapidcache.go).
	CacheType string `json:"cacheType,omitempty"`
	CreateOp  string `json:"createOp,omitempty"`
}

const (
	cacheCreating = "creating"
	cacheRunning  = "running"
	cachePaused   = "paused"
	cacheDisabled = "disabled"

	createCacheType = "create-anywhere-cache"
	updateCacheType = "update-anywhere-cache"

	admitFirstMiss  = "admit-on-first-miss"
	admitSecondMiss = "admit-on-second-miss"
)

func cacheKey(bucket, id string) string { return cachePrefix + bucket + "/" + id }

func getCache(tx Tx, bucket, id string) (cacheRecord, bool, error) {
	var c cacheRecord
	raw, ok := tx.Get(cacheKey(bucket, id))
	if !ok {
		return c, false, nil
	}
	return c, true, json.Unmarshal(raw, &c)
}

func putCache(tx Tx, c cacheRecord) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tx.Put(cacheKey(c.Bucket, c.ID), raw)
	return nil
}

func cacheMissing(bucket, id string) *httpError {
	return notFound("The Anywhere Cache %s does not exist in bucket %s.", id, bucket)
}

// predefinedDualRegions are the regions of each predefined dual-region
// (docs.cloud.google.com/storage/docs/locations).
var predefinedDualRegions = map[string][]string{
	"NAM4": {"US-CENTRAL1", "US-EAST1"}, "EUR4": {"EUROPE-NORTH1", "EUROPE-WEST4"},
	"ASIA1": {"ASIA-NORTHEAST1", "ASIA-NORTHEAST2"}, "EUR5": {"EUROPE-WEST1", "EUROPE-WEST2"},
	"EUR7": {"EUROPE-WEST2", "EUROPE-WEST3"}, "EUR8": {"EUROPE-WEST3", "EUROPE-WEST6"},
}

// multiRegionPrefix is the region prefix of each multi-region's regions.
var multiRegionPrefix = map[string]string{"US": "US-", "EU": "EUROPE-", "ASIA": "ASIA-"}

// zoneInLocation says whether zone is within the bucket's location, as a
// cache's must be ("the cache must be created in a zone within the location
// of your bucket").
func zoneInLocation(b bucketRecord, zone string) bool {
	region := strings.ToUpper(zone[:strings.LastIndex(zone, "-")])
	loc, _ := b.Fields["location"].(string)
	loc = strings.ToUpper(loc)
	if locs := dataLocations(b.Fields["customPlacementConfig"]); len(locs) > 0 && !zonal(b) {
		for _, l := range locs {
			if l == region {
				return true
			}
		}
		return false
	}
	if rs, ok := predefinedDualRegions[loc]; ok {
		for _, l := range rs {
			if l == region {
				return true
			}
		}
		return false
	}
	if p, ok := multiRegionPrefix[loc]; ok {
		return strings.HasPrefix(region, p)
	}
	return region == loc
}

var durationRE = regexp.MustCompile(`^([0-9]+)(\.0+)?s$`)

// parseTTL reads a google-duration in whole seconds, from 1h to 7d.
func parseTTL(v any) (int64, error) {
	s, _ := v.(string)
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, badRequest("Invalid argument: ttl %v must be a duration in whole seconds, such as \"86400s\".", v)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n < 3600 || n > 7*86400 {
		return 0, badRequest("Invalid argument: ttl %s must be between 1h (3600s) and 7d (604800s).", s)
	}
	return n, nil
}

func parseAdmission(v any) (string, error) {
	s, _ := v.(string)
	switch strings.ToLower(s) {
	case admitFirstMiss, admitSecondMiss:
		return strings.ToLower(s), nil
	}
	return "", badRequest("Invalid argument: admissionPolicy %v must be admit-on-first-miss or admit-on-second-miss.", v)
}

// settle carries out the work of a bucket's caches and operations that is
// due: a create or update done, a disabled cache deleted after its grace
// period, a relocation's sync step finished.
func (s *Server) settle(tx Tx, bucket string) error {
	now := s.now()
	for _, k := range tx.List(operationPrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var op operationRecord
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if !op.Pending || op.Due.IsZero() || now.Before(op.Due) {
			continue
		}
		changed, err := s.advance(tx, &op, now)
		if err != nil {
			return err
		}
		if changed {
			if err := putOperation(tx, op); err != nil {
				return err
			}
		}
	}
	for _, k := range tx.List(cachePrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var c cacheRecord
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.State == cacheDisabled && !c.Disabled.IsZero() && !now.Before(c.Disabled.Add(disabledGrace)) {
			tx.Delete(k)
		}
	}
	return s.settleRapid(tx, bucket, now)
}

// advance does an operation's due work.
func (s *Server) advance(tx Tx, op *operationRecord, now time.Time) (bool, error) {
	switch op.Type {
	case createCacheType, updateCacheType:
		c, ok, err := getCache(tx, op.Bucket, op.Cache.ID)
		if err != nil {
			return false, err
		}
		if !ok {
			// Removed under it: the operation fails.
			op.finish(now, &opStatus{Code: 5, Message: "The Anywhere Cache was deleted."})
			return true, nil
		}
		if op.Type == createCacheType {
			c.State = cacheRunning
		} else {
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
		}
		c.Updated = now
		if err := putCache(tx, c); err != nil {
			return false, err
		}
		op.Result = &c
		op.finish(now, nil)
		return true, nil
	case createRapidType, updateRapidType, disableRapidType:
		return s.advanceRapid(tx, op, now)
	case relocateType:
		return s.advanceRelocationSync(tx, op, now)
	}
	return false, nil
}

// settled settles a bucket's due work in its own transaction.
func (s *Server) settled(bucket string) error {
	return s.meta.Update(func(tx Tx) error { return s.settle(tx, bucket) })
}

// --- JSON API v1 ---

func cacheV2Name(c cacheRecord) string {
	return folderResource(c.Bucket) + "/anywhereCaches/" + c.ID
}

func ttlString(n int64) string { return strconv.FormatInt(n, 10) + "s" }

func (s *Server) cacheJSON(r *http.Request, c cacheRecord) map[string]any {
	out := map[string]any{
		"kind": "storage#anywhereCache", "id": c.Bucket + "/" + c.ID,
		"selfLink": baseURL(r) + jsonPrefix + "b/" + escape(c.Bucket) + "/anywhereCaches/" + escape(c.ID),
		"bucket":   c.Bucket, "anywhereCacheId": c.ID, "zone": c.Zone,
		"ttl": ttlString(c.TTL), "admissionPolicy": c.Admission, "state": c.State,
		"createTime": rfc3339(c.Created), "updateTime": rfc3339(c.Updated),
		"pendingUpdate": c.PendingUpdate,
	}
	if c.IngestOnWrite != nil {
		out["ingestOnWrite"] = *c.IngestOnWrite
	}
	return out
}

// cacheV2JSON is the cache as google.storage.control.v2.AnywhereCache's
// JSON, an operation's response.
func cacheV2JSON(c cacheRecord) map[string]any {
	out := map[string]any{
		"name": cacheV2Name(c), "zone": c.Zone, "ttl": ttlString(c.TTL),
		"admissionPolicy": c.Admission, "state": c.State,
		"createTime": rfc3339(c.Created), "updateTime": rfc3339(c.Updated), "pendingUpdate": c.PendingUpdate,
	}
	if c.IngestOnWrite != nil {
		out["ingestOnWrite"] = *c.IngestOnWrite
	}
	return out
}

func cacheOpMetadata(op operationRecord, common map[string]any) map[string]any {
	t := "CreateAnywhereCacheMetadata"
	if op.Type == updateCacheType {
		t = "UpdateAnywhereCacheMetadata"
	}
	out := map[string]any{"@type": "type.googleapis.com/google.storage.control.v2." + t,
		"commonMetadata": common, "anywhereCacheId": op.Cache.ID, "zone": op.Cache.Zone}
	set := func(f string) bool {
		if op.Type == createCacheType {
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

// cacheRequestHeader carries a gRPC create's request_id to the JSON
// handler (grpccontrol.go): the JSON API has no such parameter.
const cacheRequestHeader = "X-Cloudburrow-Request-Id"

func (s *Server) anywhereCachesInsert(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	in := cacheOp{TTL: 86400, Admission: admitFirstMiss}
	for k, v := range body {
		switch k {
		case "zone":
			in.Zone, _ = v.(string)
		case "anywhereCacheId":
			in.ID, _ = v.(string)
		case "ttl":
			if in.TTL, err = parseTTL(v); err != nil {
				writeError(w, err)
				return
			}
		case "admissionPolicy":
			if in.Admission, err = parseAdmission(v); err != nil {
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
			writeError(w, badRequest("Invalid argument: %s is not an AnywhereCache field", k))
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
		writeError(w, badRequest("Invalid argument: anywhereCacheId %q must be the cache's zone %s.", in.ID, in.Zone))
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
		if old, ok, err := getCache(tx, bucket, in.ID); err != nil {
			return err
		} else if ok {
			if reqID != "" && old.RequestID == reqID && old.CreateOp != "" {
				raw, found := tx.Get(operationKey(bucket, old.CreateOp))
				if found {
					return json.Unmarshal(raw, &op)
				}
			}
			return errorf(http.StatusConflict, "conflict", "ANYWHERE_CACHE_ALREADY_EXISTS: an Anywhere Cache already exists in zone %s for bucket %s.", in.Zone, bucket)
		}
		now := s.now()
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: createCacheType, Created: now,
			Pending: true, Due: now.Add(pendingDelay), Cache: &in}
		c := cacheRecord{Bucket: bucket, ID: in.ID, Zone: in.Zone, TTL: in.TTL, Admission: in.Admission,
			IngestOnWrite: in.IngestOnWrite, State: cacheCreating, Created: now, Updated: now,
			RequestID: reqID, CreateOp: op.ID}
		if err := putCache(tx, c); err != nil {
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

func (s *Server) anywhereCachesUpdate(w http.ResponseWriter, r *http.Request) {
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
			if in.Admission, err = parseAdmission(v); err != nil {
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
		case "kind", "bucket", "id", "anywhereCacheId", "selfLink", "state", "createTime", "updateTime", "pendingUpdate", "zone":
			continue
		default:
			writeError(w, badRequest("Invalid argument: %s is not an AnywhereCache field", k))
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
		c, err := s.existingCache(tx, bucket, id)
		if err != nil {
			return err
		}
		if c.State != cacheRunning {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The Anywhere Cache %s is %s; only a running cache can be updated.", id, c.State)
		}
		if c.PendingUpdate {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The Anywhere Cache %s has an update in progress.", id)
		}
		in.Zone = c.Zone
		now := s.now()
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: updateCacheType, Created: now,
			Pending: true, Due: now.Add(pendingDelay), Cache: &in}
		c.PendingUpdate, c.Updated = true, now
		if err := putCache(tx, c); err != nil {
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

// existingCache settles the bucket and reads one of its caches.
func (s *Server) existingCache(tx Tx, bucket, id string) (cacheRecord, error) {
	if _, ok, err := s.getBucket(tx, bucket); err != nil {
		return cacheRecord{}, err
	} else if !ok {
		return cacheRecord{}, notFound("The specified bucket does not exist.")
	}
	if err := s.settle(tx, bucket); err != nil {
		return cacheRecord{}, err
	}
	c, ok, err := getCache(tx, bucket, id)
	if err != nil {
		return c, err
	} else if !ok {
		return c, cacheMissing(bucket, id)
	}
	return c, nil
}

func (s *Server) anywhereCachesGet(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	var c cacheRecord
	err := s.meta.Update(func(tx Tx) (err error) {
		c, err = s.existingCache(tx, bucket, id)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.cacheJSON(r, c))
}

func (s *Server) anywhereCachesList(w http.ResponseWriter, r *http.Request) {
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
		for _, k := range tx.List(cachePrefix + bucket + "/") {
			id := strings.TrimPrefix(k, cachePrefix+bucket+"/")
			if after != "" && id <= after {
				continue
			}
			if len(items) == max {
				next = last
				break
			}
			c, _, err := getCache(tx, bucket, id)
			if err != nil {
				return err
			}
			items, last = append(items, s.cacheJSON(r, c)), id
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{"kind": "storage#anywhereCaches", "items": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, out)
}

// anywhereCachesState is pause, resume or disable: from the states listed to
// the next one.
func (s *Server) anywhereCachesState(to string, from ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
		var c cacheRecord
		err := s.meta.Update(func(tx Tx) (err error) {
			c, err = s.existingCache(tx, bucket, id)
			if err != nil {
				return err
			}
			ok := false
			for _, f := range from {
				ok = ok || c.State == f
			}
			if !ok {
				return errorf(http.StatusBadRequest, "failedPrecondition", "The Anywhere Cache %s is %s and cannot be made %s.", id, c.State, to)
			}
			now := s.now()
			c.State, c.Updated = to, now
			c.Disabled = time.Time{}
			if to == cacheDisabled {
				c.Disabled = now
			}
			return putCache(tx, c)
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, r, http.StatusOK, s.cacheJSON(r, c))
	}
}
