package storage

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bucket relocation (#1444), to docs.cloud.google.com/storage/docs/
// bucket-relocation/overview and .../relocate-buckets, and the discovery
// document's buckets.relocate and buckets.operations.advanceRelocateBucket
// and cancel.
//
//   - buckets.relocate names a destinationLocation (and, for a configurable
//     dual-region, destinationCustomPlacementConfig) and returns a
//     long-running operation whose metadata is RelocateBucketMetadata. With
//     validateOnly it is a dry run: done at once, the bucket unchanged.
//   - A relocation without write downtime (between configurable
//     dual-regions, or a multi-region and a configurable dual-region, of one
//     multi-region code) syncs and then finishes by itself: the bucket's
//     location, locationType and customPlacementConfig change, and the
//     operation is done. Locally there is nothing to copy, so the sync step
//     takes pendingDelay, not Google's seven days or more.
//   - Every other relocation has write downtime: once synced its
//     finalizationState is READY, and it waits for advanceRelocateBucket
//     (gcloud storage buckets relocate --finalize), whose ttl (12h by
//     default) or expireTime is the write-downtime expiry. The final
//     synchronization has nothing to copy, so it finishes when it is
//     advanced, and no write-locked window is observable.
//   - cancel ends a relocation that is not finalized: relocationState
//     CANCELLED, the operation done with a CANCELLED error, the bucket where
//     it was. A done operation, and an Anywhere Cache update's, are refused.
//   - Legacy storage classes become STANDARD, as the overview says; only the
//     bucket's default class is kept, as objects carry no legacy class here.
//   - destinationKmsKeyName is refused 501: nothing is re-encrypted.
//
// UNVERIFIED: the statuses and messages of every refusal, the progress
// figures, estimatedWriteDowntimeDuration, and whether a relocation bumps
// the bucket's metageneration (it does here).

const relocateType = "relocate-bucket"

type relocation struct {
	Source          string    `json:"source"`
	SourcePlacement []string  `json:"sourcePlacement,omitempty"`
	Dest            string    `json:"dest"`
	DestPlacement   []string  `json:"destPlacement,omitempty"`
	ValidateOnly    bool      `json:"validateOnly,omitempty"`
	Downtime        bool      `json:"downtime,omitempty"`
	State           string    `json:"state"`        // relocationState
	Finalization    string    `json:"finalization"` // finalizationState
	Expire          time.Time `json:"expire,omitempty"`
	Updated         time.Time `json:"updated,omitempty"`
}

var regionRE = regexp.MustCompile(`^[A-Z]+-[A-Z]+[0-9]+$`)

// validLocation says whether loc (upper case) is a bucket location.
func validLocation(loc string) bool {
	if _, ok := multiRegionPrefix[loc]; ok {
		return true
	}
	if _, ok := predefinedDualRegions[loc]; ok {
		return true
	}
	return regionRE.MatchString(loc)
}

// unsupportedRelocation are the regions the overview says relocation does
// not serve, as a source or a destination.
var unsupportedRelocation = map[string]bool{"ME-CENTRAL1": true, "ME-WEST1": true}

func (s *Server) bucketsRelocate(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var rel relocation
	for k, v := range body {
		switch k {
		case "destinationLocation":
			s, _ := v.(string)
			rel.Dest = strings.ToUpper(s)
		case "destinationCustomPlacementConfig":
			rel.DestPlacement = dataLocations(v)
		case "validateOnly":
			switch b := v.(type) {
			case bool:
				rel.ValidateOnly = b
			case string: // the relocate-buckets page sends "true"
				rel.ValidateOnly, err = strconv.ParseBool(b)
				if err != nil {
					writeError(w, badRequest("Invalid argument: validateOnly must be a boolean."))
					return
				}
			}
		case "destinationKmsKeyName":
			if v != "" && v != nil {
				writeError(w, errorf(http.StatusNotImplemented, "notImplemented",
					"destinationKmsKeyName is not implemented: CloudBurrow does not re-encrypt a relocated bucket's objects"))
				return
			}
		default:
			writeError(w, badRequest("Invalid argument: %s is not a RelocateBucketRequest field", k))
			return
		}
	}
	if rel.Dest == "" {
		writeError(w, required("destinationLocation"))
		return
	}
	if !validLocation(rel.Dest) {
		writeError(w, badRequest("Invalid argument: %s is not a Cloud Storage location.", rel.Dest))
		return
	}
	if len(rel.DestPlacement) > 0 {
		if _, multi := multiRegionPrefix[rel.Dest]; !multi || len(rel.DestPlacement) != 2 {
			writeError(w, badRequest("Invalid argument: destinationCustomPlacementConfig names two regions of the multi-region destinationLocation."))
			return
		}
		for _, l := range rel.DestPlacement {
			if !regionRE.MatchString(l) || !strings.HasPrefix(l, multiRegionPrefix[rel.Dest]) {
				writeError(w, badRequest("Invalid argument: %s is not a region of %s.", l, rel.Dest))
				return
			}
		}
		sort.Strings(rel.DestPlacement)
	}
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
		if zonal(b) {
			return badRequest("Invalid argument: a Rapid storage (zonal) bucket cannot be relocated.")
		}
		src, _ := b.Fields["location"].(string)
		rel.Source = strings.ToUpper(src)
		rel.SourcePlacement = dataLocations(b.Fields["customPlacementConfig"])
		sort.Strings(rel.SourcePlacement)
		if unsupportedRelocation[rel.Source] || unsupportedRelocation[rel.Dest] {
			return badRequest("Invalid argument: bucket relocation does not support %s.", map[bool]string{true: rel.Source, false: rel.Dest}[unsupportedRelocation[rel.Source]])
		}
		if rel.Source == rel.Dest && strings.Join(rel.SourcePlacement, ",") == strings.Join(rel.DestPlacement, ",") {
			return badRequest("Invalid argument: the bucket is already in %s.", rel.Dest)
		}
		for _, k := range tx.List(operationPrefix + bucket + "/") {
			raw, _ := tx.Get(k)
			var o operationRecord
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			if o.Type == relocateType && o.Pending {
				return conflict("The bucket %s has a relocation in progress: %s.", bucket, operationName(o))
			}
		}
		_, multi := multiRegionPrefix[rel.Dest]
		rel.Downtime = !(multi && rel.Source == rel.Dest)
		now := s.now()
		rel.Updated = now
		op = operationRecord{Bucket: bucket, ID: newOperationID(), Type: relocateType, Created: now, Relocate: &rel}
		switch {
		case rel.ValidateOnly:
			rel.State, rel.Finalization = "SUCCEEDED", "NOT_REQUIRED"
			op.finish(now, nil)
		default:
			rel.State, rel.Finalization = "SYNCING", "NOT_REQUIRED"
			if rel.Downtime {
				rel.Finalization = "WAITING_ON_SYNC"
			}
			op.Pending, op.Due = true, now.Add(pendingDelay)
		}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

// advanceRelocationSync ends a relocation's sync step: one without write
// downtime is done, one with it is READY for advanceRelocateBucket.
func (s *Server) advanceRelocationSync(tx Tx, op *operationRecord, now time.Time) (bool, error) {
	rel := op.Relocate
	if rel == nil || rel.State != "SYNCING" {
		return false, nil
	}
	rel.Updated = now
	if rel.Downtime {
		rel.Finalization = "READY"
		op.Due = time.Time{}
		return true, nil
	}
	return true, s.relocated(tx, op, now)
}

// relocated moves the bucket to the destination and ends the operation.
func (s *Server) relocated(tx Tx, op *operationRecord, now time.Time) error {
	rel := op.Relocate
	b, ok, err := s.getBucket(tx, op.Bucket)
	if err != nil {
		return err
	}
	if !ok {
		rel.State = "FAILED"
		op.finish(now, &opStatus{Code: 5, Message: "The bucket was deleted."})
		return nil
	}
	b.Fields["location"] = rel.Dest
	if len(rel.DestPlacement) > 0 {
		locs := []any{}
		for _, l := range rel.DestPlacement {
			locs = append(locs, l)
		}
		b.Fields["customPlacementConfig"] = map[string]any{"dataLocations": locs}
	} else {
		delete(b.Fields, "customPlacementConfig")
	}
	switch class, _ := b.Fields["storageClass"].(string); strings.ToUpper(class) {
	case "DURABLE_REDUCED_AVAILABILITY", "REGIONAL", "MULTI_REGIONAL":
		b.Fields["storageClass"] = "STANDARD"
	}
	b.Metageneration++
	b.Updated = now
	if err := putBucket(tx, b); err != nil {
		return err
	}
	rel.State, rel.Updated = "SUCCEEDED", now
	if rel.Downtime {
		rel.Finalization = "FINALIZED"
	}
	op.finish(now, nil)
	return nil
}

func relocateMetadata(op operationRecord, common map[string]any) map[string]any {
	rel := op.Relocate
	pct := 100
	if rel.State == "SYNCING" {
		pct = 0
	} else if op.Pending {
		pct = 99
	}
	common["progressPercent"] = pct
	out := map[string]any{"@type": "type.googleapis.com/google.storage.control.v2.RelocateBucketMetadata",
		"commonMetadata": common, "sourceLocation": rel.Source, "destinationLocation": rel.Dest,
		"relocationState": rel.State, "finalizationState": rel.Finalization, "validateOnly": rel.ValidateOnly}
	if len(rel.DestPlacement) > 0 {
		out["destinationCustomPlacementConfig"] = map[string]any{"dataLocations": rel.DestPlacement}
	}
	if !rel.Expire.IsZero() {
		out["writeDowntimeExpireTime"] = rfc3339(rel.Expire)
	}
	return out
}

func (s *Server) operationsAdvanceRelocateBucket(w http.ResponseWriter, r *http.Request) {
	bucket, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ttl := 12 * time.Hour
	var expire time.Time
	for k, v := range body {
		str, _ := v.(string)
		switch k {
		case "ttl":
			m := durationRE.FindStringSubmatch(str)
			n, perr := int64(0), error(nil)
			if m != nil {
				n, perr = strconv.ParseInt(m[1], 10, 64)
			}
			if m == nil || perr != nil || n <= 0 {
				writeError(w, badRequest("Invalid argument: ttl %v must be a positive duration in seconds.", v))
				return
			}
			ttl = time.Duration(n) * time.Second
		case "expireTime":
			if expire, err = time.Parse(time.RFC3339Nano, str); err != nil {
				writeError(w, badRequest("Invalid argument: expireTime %v is not an RFC 3339 time.", v))
				return
			}
		default:
			writeError(w, badRequest("Invalid argument: %s is not an AdvanceRelocateBucketOperationRequest field", k))
			return
		}
	}
	err = s.meta.Update(func(tx Tx) error {
		op, err := s.settledOperation(tx, bucket, id)
		if err != nil {
			return err
		}
		if op.Type != relocateType {
			return badRequest("Invalid argument: %s is not a relocation.", operationName(op))
		}
		if !op.Pending || op.Relocate.Finalization != "READY" {
			return errorf(http.StatusBadRequest, "failedPrecondition",
				"The relocation %s cannot be advanced: its relocationState is %s and its finalizationState %s.",
				operationName(op), op.Relocate.State, op.Relocate.Finalization)
		}
		now := s.now()
		if expire.IsZero() {
			expire = now.Add(ttl)
		}
		op.Relocate.Expire = expire
		if err := s.relocated(tx, &op, now); err != nil {
			return err
		}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settledOperation settles the bucket and reads one of its operations.
func (s *Server) settledOperation(tx Tx, bucket, id string) (operationRecord, error) {
	var op operationRecord
	if _, ok, err := s.getBucket(tx, bucket); err != nil {
		return op, err
	} else if !ok {
		return op, notFound("The specified bucket does not exist.")
	}
	if err := s.settle(tx, bucket); err != nil {
		return op, err
	}
	raw, ok := tx.Get(operationKey(bucket, id))
	if !ok {
		return op, notFound("The operation %s does not exist in bucket %s.", id, bucket)
	}
	return op, json.Unmarshal(raw, &op)
}

// cancelOperation cancels a running Anywhere Cache create or relocation.
func (s *Server) cancelOperation(bucket, id string) error {
	return s.meta.Update(func(tx Tx) error {
		op, err := s.settledOperation(tx, bucket, id)
		if err != nil {
			return err
		}
		if !op.Pending {
			return errorf(http.StatusBadRequest, "failedPrecondition", "The operation %s is done and cannot be cancelled.", operationName(op))
		}
		now := s.now()
		switch op.Type {
		case createCacheType:
			tx.Delete(cacheKey(bucket, op.Cache.ID))
		case createRapidType:
			tx.Delete(rapidKey(bucket, op.Cache.ID))
		case relocateType:
			op.Relocate.State, op.Relocate.Updated = "CANCELLED", now
		default:
			// using-anywhere-cache: only a create's operation can be cancelled.
			return errorf(http.StatusBadRequest, "failedPrecondition", "The operation %s cannot be cancelled: only Anywhere Cache and Rapid Cache creates and bucket relocations can.", operationName(op))
		}
		op.Cancel = true
		op.finish(now, &opStatus{Code: 1, Message: "The operation was cancelled."})
		return putOperation(tx, op)
	})
}

func (s *Server) operationsCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.cancelOperation(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
