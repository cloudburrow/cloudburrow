package storage

import (
	"net/http"
	"strconv"
	"time"
)

// Retention and holds (#500), to docs.cloud.google.com/storage/docs/bucket-lock,
// .../object-holds and .../object-lock, with the status codes and reasons
// the status-codes page lists.
//
//   - A bucket's retentionPolicy (1 s to 3,155,760,000 s) applies to every
//     object, existing and new: an object younger than the period cannot be
//     deleted or replaced (403 retentionPolicyNotMet), though a versioned
//     bucket may still make it noncurrent. lockRetentionPolicy needs
//     ifMetagenerationMatch and a policy (400 badRequest); a locked period
//     can grow but not shrink or go (400 badRequestException).
//   - temporaryHold and eventBasedHold block delete and replace (403
//     objectUnderActiveHold); metadata stays editable. Releasing an
//     event-based hold restarts the object's retention clock, and while it
//     is held no retentionExpirationTime is given. defaultEventBasedHold
//     puts the hold on each new object.
//   - Object retention {mode, retainUntilTime} needs a bucket created with
//     enableObjectRetention=true (400 invalid otherwise) and cannot be combined
//     with an event-based hold (400 invalid). Removing it, shortening it, or
//     locking an Unlocked one needs overrideUnlockedRetention=true (403
//     forbidden without); a Locked one can only be extended (403 forbidden).

const maxRetentionPeriod = 3155760000 // seconds, 100 years (bucket-lock docs)

type objectRetention struct {
	Mode        string    `json:"mode"`
	RetainUntil time.Time `json:"retainUntil"`
}

// retentionPeriod is the bucket's retention policy period; 0 means none.
func retentionPeriod(b bucketRecord) time.Duration {
	p, _ := b.Fields["retentionPolicy"].(map[string]any)
	n, _ := strconv.ParseInt(numberString(p["retentionPeriod"]), 10, 64)
	return time.Duration(n) * time.Second
}

func retentionLocked(b bucketRecord) bool {
	p, _ := b.Fields["retentionPolicy"].(map[string]any)
	locked, _ := p["isLocked"].(bool)
	return locked
}

func objectRetentionEnabled(b bucketRecord) bool {
	p, _ := b.Fields["objectRetention"].(map[string]any)
	return p["mode"] == "Enabled"
}

func defaultEventBasedHold(b bucketRecord) bool {
	on, _ := b.Fields["defaultEventBasedHold"].(bool)
	return on
}

// retentionStart is when an object's retention clock started: its creation,
// or the release of its last event-based hold.
func retentionStart(o objectRecord) time.Time {
	if !o.RetentionFrom.IsZero() {
		return o.RetentionFrom
	}
	return o.Created
}

// protect refuses to delete or replace o while it is held or retained.
// keptNoncurrent is a live version becoming noncurrent on a versioned
// bucket, which a retention period allows (bucket-lock docs); holds still
// refuse it (UNVERIFIED: the object-holds docs do not mention versioning).
func protect(b bucketRecord, o objectRecord, now time.Time, keptNoncurrent bool) error {
	if o.TemporaryHold || o.EventBasedHold {
		return errorf(http.StatusForbidden, "objectUnderActiveHold",
			"Object replacement or deletion is not allowed due to an active hold on the object.")
	}
	if keptNoncurrent {
		return nil
	}
	notMet := func(until time.Time) error {
		return errorf(http.StatusForbidden, "retentionPolicyNotMet",
			"Object '%s/%s' is under retention and cannot be replaced or deleted until %s.", o.Bucket, o.Name, rfc3339(until))
	}
	if d := retentionPeriod(b); d > 0 && now.Before(retentionStart(o).Add(d)) {
		return notMet(retentionStart(o).Add(d))
	}
	if o.Retention != nil && now.Before(o.Retention.RetainUntil) {
		return notMet(o.Retention.RetainUntil)
	}
	return nil
}

// checkRetentionPolicy validates a bucket's retentionPolicy and fixes its
// server-set parts: effectiveTime moves when the period changes, and a
// locked policy stays locked and cannot shrink or go.
func checkRetentionPolicy(next, old map[string]any, now time.Time) error {
	prev, _ := old["retentionPolicy"].(map[string]any)
	wasLocked, _ := prev["isLocked"].(bool)
	p, _ := next["retentionPolicy"].(map[string]any)
	if p == nil || p["retentionPeriod"] == nil {
		if wasLocked {
			return errorf(http.StatusBadRequest, "badRequestException", "The retention policy on a locked bucket cannot be removed.")
		}
		delete(next, "retentionPolicy")
		return nil
	}
	s := numberString(p["retentionPeriod"])
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > maxRetentionPeriod {
		return badRequest("Invalid argument: retentionPolicy.retentionPeriod %q must be 1 to %d seconds", s, maxRetentionPeriod)
	}
	oldN, _ := strconv.ParseInt(numberString(prev["retentionPeriod"]), 10, 64)
	if wasLocked && n < oldN {
		return errorf(http.StatusBadRequest, "badRequestException", "The retention period on a locked bucket cannot be reduced.")
	}
	out := map[string]any{"retentionPeriod": strconv.FormatInt(n, 10), "effectiveTime": rfc3339(now)}
	if prev != nil && n == oldN && prev["effectiveTime"] != nil {
		out["effectiveTime"] = prev["effectiveTime"]
	}
	if wasLocked {
		out["isLocked"] = true
	}
	next["retentionPolicy"] = out
	return nil
}

// bucketsLockRetentionPolicy is buckets.lockRetentionPolicy.
func (s *Server) bucketsLockRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	v := r.URL.Query().Get("ifMetagenerationMatch")
	if v == "" {
		writeError(w, required("ifMetagenerationMatch"))
		return
	}
	want, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		writeError(w, badRequest("Invalid argument: ifMetagenerationMatch=%q is not a number", v))
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
		if b.Metageneration != want {
			return preconditionFailed("At least one of the pre-conditions you specified did not hold.")
		}
		p, _ := b.Fields["retentionPolicy"].(map[string]any)
		if p == nil {
			return errorf(http.StatusBadRequest, "badRequest", "You cannot lock a retention policy if the requested bucket doesn't have a retention policy.")
		}
		p["isLocked"] = true
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

// applyProtection writes a body's holds and retention onto o, from old, the
// version as stored (zero for a new object). Releasing an event-based hold
// restarts the retention clock at now. override is
// overrideUnlockedRetention.
func applyProtection(o *objectRecord, old objectRecord, body map[string]any, replace, override bool, now time.Time) error {
	flag := func(k string, dst *bool) error {
		v, ok := body[k]
		switch {
		case !ok:
			if replace {
				*dst = false
			}
		case v == nil:
			*dst = false
		default:
			b, isBool := v.(bool)
			if !isBool {
				return badRequest("Invalid argument: %s must be a boolean", k)
			}
			*dst = b
		}
		return nil
	}
	if err := flag("temporaryHold", &o.TemporaryHold); err != nil {
		return err
	}
	if err := flag("eventBasedHold", &o.EventBasedHold); err != nil {
		return err
	}
	if old.EventBasedHold && !o.EventBasedHold {
		o.RetentionFrom = now
	}
	if v, ok := body["retention"]; ok || replace {
		var next *objectRetention
		if m, isMap := v.(map[string]any); isMap && !empty(m) {
			mode, until := str(m["mode"]), str(m["retainUntilTime"])
			t, err := time.Parse(time.RFC3339Nano, until)
			if mode != "Unlocked" && mode != "Locked" || until == "" || err != nil {
				return errorf(http.StatusBadRequest, "invalid", "A retention configuration needs mode Unlocked or Locked and an RFC 3339 retainUntilTime.")
			}
			if !t.After(now) {
				return errorf(http.StatusBadRequest, "invalid", "The retainUntilTime %s is not in the future.", until)
			}
			next = &objectRetention{Mode: mode, RetainUntil: t}
		} else if v != nil && !isMap {
			return badRequest("Invalid argument: retention must be an object")
		}
		if err := retentionChange(old.Retention, next, override); err != nil {
			return err
		}
		o.Retention = next
	}
	if o.Retention != nil && o.EventBasedHold {
		return errorf(http.StatusBadRequest, "invalid", "An object cannot have a retention configuration and an event-based hold at the same time.")
	}
	return nil
}

// retentionChange applies object-lock's rules to a change from old to next.
func retentionChange(old, next *objectRetention, override bool) error {
	if old == nil {
		return nil
	}
	loosens := next == nil || next.RetainUntil.Before(old.RetainUntil)
	forbidden := func(msg string) error { return errorf(http.StatusForbidden, "forbidden", "%s", msg) }
	if old.Mode == "Locked" {
		if loosens || next.Mode != "Locked" {
			return forbidden("A Locked retention configuration can only be extended.")
		}
		return nil
	}
	if (loosens || next.Mode == "Locked") && !override {
		return forbidden("Removing, shortening or locking an Unlocked retention configuration needs overrideUnlockedRetention=true.")
	}
	return nil
}

// checkNewObject applies the bucket to a new version: its default
// event-based hold, and object retention only where enabled.
func checkNewObject(b bucketRecord, o *objectRecord) error {
	if defaultEventBasedHold(b) {
		o.EventBasedHold = true
	}
	if o.Retention != nil && !objectRetentionEnabled(b) {
		return errorf(http.StatusBadRequest, "invalid", "The bucket %s does not have object retention enabled.", b.Name)
	}
	if o.Retention != nil && o.EventBasedHold {
		return errorf(http.StatusBadRequest, "invalid", "An object cannot have a retention configuration and an event-based hold at the same time.")
	}
	return nil
}

// fresh clears what a new object does not inherit from the one it is made
// from (a copy or rewrite source, a restored version): holds, retention and
// its retention clock.
func fresh(o *objectRecord) {
	o.TemporaryHold, o.EventBasedHold, o.Retention, o.RetentionFrom = false, false, nil, time.Time{}
}

// protectionJSON adds holds, retention and retentionExpirationTime, which
// is not given while an event-based hold is active (Object resource).
func protectionJSON(out map[string]any, o objectRecord, period time.Duration) {
	if o.TemporaryHold {
		out["temporaryHold"] = true
	}
	if o.EventBasedHold {
		out["eventBasedHold"] = true
	}
	if o.Retention != nil {
		out["retention"] = map[string]any{"mode": o.Retention.Mode, "retainUntilTime": rfc3339(o.Retention.RetainUntil)}
	}
	if period > 0 && !o.EventBasedHold {
		out["retentionExpirationTime"] = rfc3339(retentionStart(o).Add(period))
	}
}
