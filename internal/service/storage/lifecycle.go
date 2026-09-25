package storage

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Object Lifecycle Management (#501), to docs.cloud.google.com/storage/docs/lifecycle.
// A bucket's lifecycle.rule list is validated when it is set: an unknown
// action or condition is 400 naming it, and the old configuration stays.
// ApplyLifecycle runs the rules over every version on the server's clock:
// a rule's conditions combine with AND, Delete takes precedence over
// SetStorageClass, and among SetStorageClass actions the class with the
// lowest at-rest price wins. Delete does not take effect while a hold or an
// unmet retention period protects the object; deleting a live version makes
// it noncurrent (versioned bucket) or soft-deleted, as any delete does. It
// runs from Run and from POST /_cloudburrow/lifecycle, so tests need not
// wait. AbortIncompleteMultipartUpload removes XML multipart uploads (#508)
// by age, prefix and suffix. The maximum number of rules is not stated, so
// none is imposed.

// lifecyclePath triggers ApplyLifecycle. It cannot collide with the XML
// API, since no bucket name begins with "_".
const lifecyclePath = "/_cloudburrow/lifecycle"

// maxLifecycleAffixes is the documented limit of "up to 1000 prefixes and
// suffixes in total specified across all rules".
const maxLifecycleAffixes = 1000

// maxObjectSize is 5 TiB, the documented bound on sizeAboveBytes and
// sizeBelowBytes.
const maxObjectSize = 5 << 40

type lifecycleRule struct {
	Action    string
	Class     string
	Condition lifecycleCondition
}

type lifecycleCondition struct {
	Age, DaysSinceCustomTime, DaysSinceNoncurrentTime, NumNewerVersions *int64
	CreatedBefore, CustomTimeBefore, NoncurrentTimeBefore               *time.Time
	IsLive                                                              *bool
	Prefixes, Suffixes, Classes                                         []string
	SizeAbove, SizeBelow                                                *int64
}

var lifecycleConditions = map[string]bool{
	"age": true, "createdBefore": true, "customTimeBefore": true, "daysSinceCustomTime": true,
	"daysSinceNoncurrentTime": true, "isLive": true, "matchesPrefix": true, "matchesSuffix": true,
	"matchesStorageClass": true, "noncurrentTimeBefore": true, "numNewerVersions": true,
	"sizeAboveBytes": true, "sizeBelowBytes": true,
}

// parseLifecycle validates a bucket's lifecycle field and returns its rules.
func parseLifecycle(v any) ([]lifecycleRule, error) {
	if v == nil {
		return nil, nil
	}
	lc, ok := v.(map[string]any)
	if !ok {
		return nil, badRequest("Invalid argument: lifecycle must be an object")
	}
	for k := range lc {
		if k != "rule" {
			return nil, badRequest("Invalid argument: lifecycle.%s is not a lifecycle field", k)
		}
	}
	raw, _ := lc["rule"].([]any)
	if lc["rule"] != nil && raw == nil {
		return nil, badRequest("Invalid argument: lifecycle.rule must be a list")
	}
	var rules []lifecycleRule
	affixes := 0
	for i, rv := range raw {
		rm, ok := rv.(map[string]any)
		if !ok {
			return nil, badRequest("Invalid argument: lifecycle.rule[%d] must be an object", i)
		}
		for k := range rm {
			if k != "action" && k != "condition" {
				return nil, badRequest("Invalid argument: lifecycle.rule[%d].%s is not a rule field", i, k)
			}
		}
		act, _ := rm["action"].(map[string]any)
		var rule lifecycleRule
		rule.Action = str(act["type"])
		switch rule.Action {
		case "Delete":
		case "SetStorageClass":
			rule.Class = strings.ToUpper(str(act["storageClass"]))
			if !storageClasses[rule.Class] {
				return nil, badRequest("Invalid argument: lifecycle.rule[%d] SetStorageClass needs a storage class, not %q", i, str(act["storageClass"]))
			}
		case "AbortIncompleteMultipartUpload":
			// Only age, matchesPrefix and matchesSuffix go with it; "any
			// other conditions results in an error" (lifecycle docs).
			if cond, _ := rm["condition"].(map[string]any); cond != nil {
				for k := range cond {
					if k != "age" && k != "matchesPrefix" && k != "matchesSuffix" {
						return nil, badRequest("Invalid argument: the lifecycle action AbortIncompleteMultipartUpload takes only age, matchesPrefix and matchesSuffix, not %s", k)
					}
				}
			}
		case "":
			return nil, badRequest("Invalid argument: lifecycle.rule[%d] has no action type", i)
		default:
			return nil, badRequest("Invalid argument: %q is not a lifecycle action", rule.Action)
		}
		cond, _ := rm["condition"].(map[string]any)
		if rm["condition"] != nil && cond == nil {
			return nil, badRequest("Invalid argument: lifecycle.rule[%d].condition must be an object", i)
		}
		c := &rule.Condition
		for k, cv := range cond {
			if !lifecycleConditions[k] {
				return nil, badRequest("Invalid argument: %q is not a lifecycle condition", k)
			}
			var err error
			switch k {
			case "age":
				c.Age, err = condInt(k, cv, 0)
			case "daysSinceCustomTime":
				c.DaysSinceCustomTime, err = condInt(k, cv, 0)
			case "daysSinceNoncurrentTime":
				c.DaysSinceNoncurrentTime, err = condInt(k, cv, 0)
			case "numNewerVersions":
				c.NumNewerVersions, err = condInt(k, cv, 0)
			case "sizeAboveBytes":
				c.SizeAbove, err = condInt(k, cv, maxObjectSize)
			case "sizeBelowBytes":
				c.SizeBelow, err = condInt(k, cv, maxObjectSize)
			case "createdBefore":
				c.CreatedBefore, err = condDate(k, cv)
			case "customTimeBefore":
				c.CustomTimeBefore, err = condDate(k, cv)
			case "noncurrentTimeBefore":
				c.NoncurrentTimeBefore, err = condDate(k, cv)
			case "isLive":
				b, isBool := cv.(bool)
				if !isBool {
					err = badRequest("Invalid argument: lifecycle condition isLive must be a boolean")
				}
				c.IsLive = &b
			case "matchesPrefix":
				c.Prefixes, err = condStrings(k, cv)
				affixes += len(c.Prefixes)
			case "matchesSuffix":
				c.Suffixes, err = condStrings(k, cv)
				affixes += len(c.Suffixes)
			case "matchesStorageClass":
				c.Classes, err = condStrings(k, cv)
				for j, cl := range c.Classes {
					c.Classes[j] = strings.ToUpper(cl)
					if !storageClasses[c.Classes[j]] {
						err = badRequest("Invalid argument: lifecycle condition matchesStorageClass %q is not a storage class", cl)
					}
				}
			}
			if err != nil {
				return nil, err
			}
		}
		rules = append(rules, rule)
	}
	if affixes > maxLifecycleAffixes {
		return nil, badRequest("Invalid argument: lifecycle rules name %d prefixes and suffixes; at most %d are allowed", affixes, maxLifecycleAffixes)
	}
	return rules, nil
}

func condInt(k string, v any, max int64) (*int64, error) {
	n, err := strconv.ParseInt(numberString(v), 10, 64)
	if err != nil || n < 0 || max > 0 && n > max {
		return nil, badRequest("Invalid argument: lifecycle condition %s %v must be a non-negative integer", k, v)
	}
	return &n, nil
}

func condDate(k string, v any) (*time.Time, error) {
	t, err := time.Parse("2006-01-02", str(v))
	if err != nil {
		return nil, badRequest("Invalid argument: lifecycle condition %s %v must be a date, YYYY-MM-DD", k, v)
	}
	return &t, nil
}

func condStrings(k string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, badRequest("Invalid argument: lifecycle condition %s must be a list of strings", k)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, isStr := e.(string)
		if !isStr {
			return nil, badRequest("Invalid argument: lifecycle condition %s must be a list of strings", k)
		}
		out = append(out, s)
	}
	return out, nil
}

// lifecycleVersion is one version as a rule sees it.
type lifecycleVersion struct {
	o     objectRecord
	live  bool
	newer int64 // versions newer than this one, the live one included
}

func day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// matches reports whether every condition of c holds for v at now.
func (c lifecycleCondition) matches(v lifecycleVersion, now time.Time) bool {
	o := v.o
	const d = 24 * time.Hour
	if c.Age != nil {
		// Age 0 is met at midnight UTC after creation; age N, N days after
		// it (lifecycle docs, "age").
		due := o.Created.Add(time.Duration(*c.Age) * d)
		if *c.Age == 0 {
			due = day(o.Created).Add(d)
		}
		if now.Before(due) {
			return false
		}
	}
	if c.CreatedBefore != nil && !o.Created.Before(*c.CreatedBefore) {
		return false
	}
	if c.CustomTimeBefore != nil && (o.CustomTime.IsZero() || !day(o.CustomTime).Before(*c.CustomTimeBefore)) {
		return false
	}
	if c.DaysSinceCustomTime != nil && (o.CustomTime.IsZero() || now.Before(o.CustomTime.Add(time.Duration(*c.DaysSinceCustomTime)*d))) {
		return false
	}
	if c.DaysSinceNoncurrentTime != nil && (v.live || now.Before(o.Deleted.Add(time.Duration(*c.DaysSinceNoncurrentTime)*d))) {
		return false
	}
	if c.NoncurrentTimeBefore != nil && (v.live || !day(o.Deleted).Before(*c.NoncurrentTimeBefore)) {
		return false
	}
	if c.IsLive != nil && *c.IsLive != v.live {
		return false
	}
	if c.NumNewerVersions != nil && v.newer < *c.NumNewerVersions {
		return false
	}
	if c.Prefixes != nil && !anyAffix(o.Name, c.Prefixes, strings.HasPrefix) {
		return false
	}
	if c.Suffixes != nil && !anyAffix(o.Name, c.Suffixes, strings.HasSuffix) {
		return false
	}
	if c.Classes != nil && !contains(c.Classes, o.StorageClass) {
		return false
	}
	if c.SizeAbove != nil && o.Size <= *c.SizeAbove {
		return false
	}
	if c.SizeBelow != nil && o.Size >= *c.SizeBelow {
		return false
	}
	return true
}

func anyAffix(name string, affixes []string, has func(string, string) bool) bool {
	for _, a := range affixes {
		if has(name, a) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// classRank orders storage classes by at-rest price, cheapest first
// (docs.cloud.google.com/storage/pricing: Archive < Coldline < Nearline <
// Standard). The legacy classes rank with Standard, and Durable Reduced
// Availability above it, since the lifecycle docs allow it to move to any
// class.
func classRank(c string) int {
	switch c {
	case "ARCHIVE":
		return 0
	case "COLDLINE":
		return 1
	case "NEARLINE":
		return 2
	case "DURABLE_REDUCED_AVAILABILITY":
		return 4
	}
	return 3
}

// decide returns the action the rules take on v: "Delete", a storage class
// to set, or "" for none.
func decide(rules []lifecycleRule, v lifecycleVersion, now time.Time) (del bool, class string) {
	for _, r := range rules {
		if !r.Condition.matches(v, now) {
			continue
		}
		switch r.Action {
		case "Delete":
			del = true
		case "SetStorageClass":
			// A transition goes only to a cheaper class (the docs' table:
			// Standard to Nearline, Coldline or Archive, and so on).
			if classRank(r.Class) < classRank(v.o.StorageClass) && (class == "" || classRank(r.Class) < classRank(class)) {
				class = r.Class
			}
		}
	}
	return del, class
}

// LifecycleResult counts what one ApplyLifecycle did.
type LifecycleResult struct {
	Deleted      int `json:"deleted"`
	ClassChanged int `json:"storageClassChanged"`
	Protected    int `json:"protected"`
	// Aborted counts incomplete XML multipart uploads removed (#508).
	Aborted int `json:"multipartUploadsAborted"`
}

// ApplyLifecycle runs every bucket's lifecycle rules once, at the server's
// current time.
func (s *Server) ApplyLifecycle() (LifecycleResult, error) {
	var res LifecycleResult
	now := s.now()
	err := s.meta.Update(func(tx Tx) error {
		for _, key := range tx.List(bucketPrefix) {
			b, ok, err := s.getBucket(tx, strings.TrimPrefix(key, bucketPrefix))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			rules, err := parseLifecycle(b.Fields["lifecycle"])
			if err != nil || len(rules) == 0 {
				continue
			}
			for _, rule := range rules {
				if rule.Action == "AbortIncompleteMultipartUpload" {
					n, err := abortIncompleteUploads(tx, b.Name, rule.Condition, now)
					if err != nil {
						return err
					}
					res.Aborted += n
				}
			}
			for _, name := range listNames(tx, b.Name, "", listAllVersions) {
				if err := s.applyToObject(tx, b, name, rules, now, &res); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return res, err
}

func (s *Server) applyToObject(tx Tx, b bucketRecord, name string, rules []lifecycleRule, now time.Time, res *LifecycleResult) error {
	vs, err := s.listVersions(tx, b, name, listAllVersions)
	if err != nil {
		return err
	}
	live, hasLive, err := getObject(tx, b.Name, name)
	if err != nil {
		return err
	}
	// Decide on every version first, as the rules see the object now: a
	// delete that makes the live version noncurrent does not make it
	// eligible again in the same pass.
	type step struct {
		v     lifecycleVersion
		del   bool
		class string
	}
	var steps []step
	for i, o := range vs {
		v := lifecycleVersion{o: o, live: hasLive && o.Generation == live.Generation, newer: int64(len(vs) - 1 - i)}
		if del, class := decide(rules, v, now); del || class != "" {
			steps = append(steps, step{v, del, class})
		}
	}
	for _, st := range steps {
		o := st.v.o
		if st.del {
			if err := protect(b, o, now, st.v.live && versioningEnabled(b)); err != nil {
				res.Protected++
				continue
			}
			if st.v.live {
				if err := retireLive(tx, b, name, now, 0); err != nil {
					return err
				}
			} else {
				if err := deleteVersion(tx, o); err != nil {
					return err
				}
				if err := removed(tx, b, o, now, 0); err != nil {
					return err
				}
			}
			res.Deleted++
			continue
		}
		o.StorageClass, o.ClassUpdated = st.class, now
		if err := putVersion(tx, o, st.v.live); err != nil {
			return err
		}
		res.ClassChanged++
	}
	return nil
}

// serveLifecycle is POST /_cloudburrow/lifecycle: run the rules now.
func (s *Server) serveLifecycle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "POST %s runs the lifecycle rules", lifecyclePath))
		return
	}
	res, err := s.ApplyLifecycle()
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
