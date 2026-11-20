package storagejobs

// Storage Insights activity data (#1979,
// docs.cloud.google.com/storage/docs/insights/dataset-tables-and-schemas):
// object_events_view, project_activity_view, bucket_activity_view and
// bucket_region_activity_view, made from the instance's Cloud Storage
// request log, the same record its Data Access audit log entries are made
// from (internal/service/storage/requestlog.go, cmd/cloudburrow/storageaudit.go).
//
// The host hands each batch of requests to Runtime.RecordActivity, which
// stamps them on the shared clock and keeps them in a bounded buffer
// (maxActivityRequests, at most MaxActivityRetentionPeriodDays old). Each
// snapshot aggregates the requests since the configuration's previous
// snapshot (Google's snapshotStartTime and snapshotEndTime) over the
// buckets and projects of its scope, and adds an events_view row of
// eventCode 3 per activity view. A configuration keeps its activity rows
// for activityDataRetentionPeriodDays (the dataset's retention when unset,
// as Google documents; 0 keeps none).

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// ActivityRequest is one served Cloud Storage request.
type ActivityRequest struct {
	Time             time.Time // when it completed, on the wall clock
	Method           string    // the JSON API method, storage.objects.get
	Bucket, Object   string
	ProjectNumber    int64
	Location         string
	BucketGeneration int64
	Status           int
	DurationMicros   int64
	RequestBytes     int64
	ResponseBytes    int64
	RequestID        string
	Host, HTTPMethod string
	Referrer         string
}

// maxActivityRequests bounds the requests kept between snapshots.
const maxActivityRequests = 200000

type activityLog struct {
	mu   sync.Mutex
	reqs []ActivityRequest // by clock time, oldest first
	// flush, when set, pulls the requests not handed over yet.
	flush func(ctx context.Context)
}

// RecordActivity keeps requests for the activity views, each stamped on
// the shared clock as it read when the request was served.
func (rt *Runtime) RecordActivity(reqs []ActivityRequest) {
	if len(reqs) == 0 {
		return
	}
	l := &rt.activity
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range reqs {
		r.Time = rt.Clock.At(r.Time)
		l.reqs = append(l.reqs, r)
	}
	sort.SliceStable(l.reqs, func(i, k int) bool { return l.reqs[i].Time.Before(l.reqs[k].Time) })
	cut := rt.Clock.Now().Add(-MaxActivityRetentionPeriodDays * snapshotInterval)
	i := sort.Search(len(l.reqs), func(i int) bool { return l.reqs[i].Time.After(cut) })
	if over := len(l.reqs) - maxActivityRequests; over > i {
		i = over
	}
	if i > 0 {
		l.reqs = append(l.reqs[:0], l.reqs[i:]...)
	}
}

// UseActivityFlush sets how the requests not handed over yet are pulled
// before a snapshot.
func (rt *Runtime) UseActivityFlush(f func(ctx context.Context)) {
	rt.activity.mu.Lock()
	rt.activity.flush = f
	rt.activity.mu.Unlock()
}

func (rt *Runtime) flushActivity(ctx context.Context) {
	rt.activity.mu.Lock()
	f := rt.activity.flush
	rt.activity.mu.Unlock()
	if f != nil {
		f(ctx)
	}
}

// activityBetween is the requests in (from, to].
func (rt *Runtime) activityBetween(from, to time.Time) []ActivityRequest {
	l := &rt.activity
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ActivityRequest
	for _, r := range l.reqs {
		if r.Time.After(from) && !r.Time.After(to) {
			out = append(out, r)
		}
	}
	return out
}

// activityKeys are the snapshot rows activityDataRetentionPeriodDays keeps.
var activityKeys = map[string]bool{"object_events": true, "project_activity": true, "bucket_activity": true,
	"bucket_region_activity": true}

var activityViews = []string{"object_events_view", "project_activity_view", "bucket_activity_view", "bucket_region_activity_view"}

// activityDays is a configuration's activity data retention in days.
func activityDays(retention int32, activity *int32) int32 {
	if activity == nil {
		return retention
	}
	return *activity
}

// requestOperation is a request's kind as the activity views count it:
// read, write, update, delete or list.
func requestOperation(method string) string {
	short := method[strings.LastIndex(method, ".")+1:]
	switch {
	case short == "list" || strings.HasSuffix(short, "List"):
		return "list"
	case short == "delete" || short == "deleteRecursive" || method == "xml.DELETE":
		return "delete"
	case short == "insert" || short == "compose" || short == "copy" || short == "rewrite" || short == "restore" ||
		short == "bulkRestore" || short == "move" || short == "create" || method == "xml.PUT" || method == "xml.POST":
		return "write"
	case short == "patch" || short == "update" || short == "setIamPolicy" || short == "lockRetentionPolicy" || short == "rename":
		return "update"
	}
	return "read"
}

type activityCount struct {
	reqs, reads, writes, updates, deletes, lists, reqBytes, respBytes, c2xx, c4xx, c5xx, c400, c401, c403, c429 int64
}

func (a *activityCount) add(r ActivityRequest) {
	a.reqs++
	switch requestOperation(r.Method) {
	case "read":
		a.reads++
	case "write":
		a.writes++
	case "update":
		a.updates++
	case "delete":
		a.deletes++
	case "list":
		a.lists++
	}
	a.reqBytes += r.RequestBytes
	a.respBytes += r.ResponseBytes
	switch {
	case r.Status >= 200 && r.Status < 300:
		a.c2xx++
	case r.Status >= 400 && r.Status < 500:
		a.c4xx++
	case r.Status >= 500:
		a.c5xx++
	}
	switch r.Status {
	case 400:
		a.c400++
	case 401:
		a.c401++
	case 403:
		a.c403++
	case 429:
		a.c429++
	}
}

func (a *activityCount) row(row map[string]any) map[string]any {
	for k, v := range map[string]int64{"totalRequests": a.reqs, "totalReads": a.reads, "totalWrites": a.writes,
		"totalUpdates": a.updates, "totalDeletes": a.deletes, "totalLists": a.lists, "totalRequestBytes": a.reqBytes,
		"totalResponseBytes": a.respBytes, "count4xx": a.c4xx, "count5xx": a.c5xx, "count2xx": a.c2xx, "count400": a.c400,
		"count401": a.c401, "count403": a.c403, "count429": a.c429} {
		row[k] = v
	}
	return row
}

// objectPrefix is the prefix an object's name is under: up to and with its
// last "/", "" for a name with none.
func objectPrefix(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i+1]
	}
	return ""
}

// topPrefixes is the top 5 prefixes by count, as {prefix, count} records.
func topPrefixes(m map[string]int64) []map[string]any {
	type pc struct {
		p string
		n int64
	}
	var all []pc
	for p, n := range m {
		if n > 0 {
			all = append(all, pc{p, n})
		}
	}
	sort.Slice(all, func(i, k int) bool {
		if all[i].n != all[k].n {
			return all[i].n > all[k].n
		}
		return all[i].p < all[k].p
	})
	out := []map[string]any{}
	for i := 0; i < len(all) && i < 5; i++ {
		out = append(out, map[string]any{"prefix": all[i].p, "count": all[i].n})
	}
	return out
}

// activityRows adds a snapshot's activity rows: the requests in (from, at]
// on the buckets the snapshot holds (buckets, by name) of its projects.
func (s *InsightsServer) activityRows(sn *snapshot, from, at time.Time, loc string, projects map[int64]bool,
	buckets map[string]map[string]any, objects map[string]map[string]any) {
	add := func(table string, row map[string]any) { sn.Rows[table] = append(sn.Rows[table], row) }
	start, end := ts(from), ts(at)
	byProject := map[int64]*activityCount{}
	type bucketAgg struct {
		count                         activityCount
		e429, reqBytes, respBytes, op map[string]int64
		gen                           int64
		project                       int64
	}
	byBucket := map[string]*bucketAgg{}
	for _, r := range s.rt.activityBetween(from, at) {
		if !projects[r.ProjectNumber] {
			continue
		}
		pa := byProject[r.ProjectNumber]
		if pa == nil {
			pa = &activityCount{}
			byProject[r.ProjectNumber] = pa
		}
		pa.add(r)
		b, ok := buckets[r.Bucket]
		if r.Bucket == "" || !ok {
			continue
		}
		ba := byBucket[r.Bucket]
		if ba == nil {
			ba = &bucketAgg{e429: map[string]int64{}, reqBytes: map[string]int64{}, respBytes: map[string]int64{},
				op: map[string]int64{}, project: r.ProjectNumber, gen: r.BucketGeneration}
			byBucket[r.Bucket] = ba
		}
		ba.count.add(r)
		if r.Object != "" {
			p := objectPrefix(r.Object)
			ba.op[p]++
			ba.reqBytes[p] += r.RequestBytes
			ba.respBytes[p] += r.ResponseBytes
			if r.Status == 429 {
				ba.e429[p]++
			}
		}
		op := requestOperation(r.Method)
		if r.Object == "" || (op != "write" && op != "update" && op != "delete" && r.Status != 429) {
			continue
		}
		row := map[string]any{"requestCompletionTimestamp": ts(r.Time), "project": r.ProjectNumber, "bucketName": r.Bucket,
			"objectName": r.Object, "requestBytes": r.RequestBytes, "responseBytes": r.ResponseBytes,
			"requestProcessingTimeMicros": r.DurationMicros, "requestId": r.RequestID, "requestHost": nullable(r.Host),
			"requestHttpMethod": r.HTTPMethod, "responseStatus": r.Status, "requestReferrer": nullable(r.Referrer),
			"location": nullable(str(b["location"]))}
		if op == "write" || op == "update" || op == "delete" {
			row["requestOperation"] = op
		}
		if r.Status == 429 {
			row["errorReason"] = "rateLimitExceeded"
		}
		if o, ok := objects[r.Bucket+"/"+r.Object]; ok && op != "delete" {
			row["size"], row["generation"], row["timeCreated"] = toInt(o["size"]), toInt(o["generation"]), tsOf(o["timeCreated"])
			row["storageClass"] = nullable(str(o["storageClass"]))
		}
		add("object_events", row)
	}
	var nums []int64
	for n := range byProject {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, k int) bool { return nums[i] < nums[k] })
	for _, n := range nums {
		add("project_activity", byProject[n].row(map[string]any{"snapshotStartTime": start, "snapshotEndTime": end, "project": n}))
	}
	var names []string
	for n := range byBucket {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ba := byBucket[n]
		bl := str(buckets[n]["location"])
		gen := ba.gen
		if g := toInt(buckets[n]["generation"]); g != 0 {
			gen = g
		}
		row := ba.count.row(map[string]any{"snapshotStartTime": start, "snapshotEndTime": end, "name": n, "generation": gen,
			"project": ba.project, "location": bl})
		row["topPrefixes429Errors"], row["topPrefixesRequestBytes"] = topPrefixes(ba.e429), topPrefixes(ba.reqBytes)
		row["topPrefixesResponseBytes"], row["topPrefixesHighestOperations"] = topPrefixes(ba.respBytes), topPrefixes(ba.op)
		add("bucket_activity", row)
		// Every request comes from this machine, which is in no Google
		// Cloud location: requestLocation is null.
		add("bucket_region_activity", map[string]any{"snapshotStartTime": start, "snapshotEndTime": end, "project": ba.project,
			"name": n, "generation": gen, "requestLocation": nil, "bucketLocation": bl, "requestBytes": ba.count.reqBytes,
			"responseBytes": ba.count.respBytes})
	}
	for _, v := range activityViews {
		add("events", map[string]any{"manifest": map[string]any{"snapshotTime": end, "viewName": v, "location": loc},
			"eventTime": ts(s.rt.Clock.Now()), "eventCode": "3"})
	}
}
