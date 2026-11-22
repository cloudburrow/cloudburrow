package logging

// Sink volume and log bucket ingestion (#2021), for Cloud Monitoring's
// logging.googleapis.com/exports/* and logging.googleapis.com/billing/*
// metrics (docs.cloud.google.com/monitoring/api/metrics_gcp_i_o, read
// 2026-11-20) and the console's Log Router Volume column and Logs Storage
// summary:
//
//   - exports/byte_count, exports/log_entry_count: the bytes and entries a
//     sink routed. A sink to a log bucket or a project routes an entry when
//     WriteLogEntries stores it; a sink to Pub/Sub, BigQuery or Cloud
//     Storage when the destination took it (deliver.go), so an entry
//     waiting for its hourly Cloud Storage file is not counted yet;
//   - exports/error_count: the entries a sink failed to deliver;
//   - billing/bytes_ingested: the bytes of the entries written into log
//     buckets, an entry counted once per bucket that stores it, by the
//     entry's monitored resource type, in the bucket's project. Google
//     leaves _Required out (required.go), so the audit entries its sink
//     routes are not counted.
//
// An entry's size is its LogEntry's protocol-buffer size, the measure the
// 10 MB WriteLogEntries limit uses. Counts are kept per minute of the
// Logging clock, in memory, for volumeRetention (Monitoring's retention of
// these metrics), and are lost with the instance.

import (
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/proto"
)

// volumeRetention is how long counts are kept: Cloud Monitoring keeps
// these metrics' points six weeks.
const volumeRetention = 6 * 7 * 24 * time.Hour

// SinkVolume is what one sink routed in one minute.
type SinkVolume struct {
	// End is the end of the minute, (End-1m, End].
	End time.Time
	// Sink is the sink's resource name; Destination as the sink names it.
	Sink, Destination string
	Bytes, Entries    int64
	Errors            int64
}

// Ingestion is what one project's log buckets took in one minute, for one
// monitored resource type.
type Ingestion struct {
	End          time.Time
	ResourceType string
	Bytes        int64
}

type sinkCount struct{ bytes, entries, errors int64 }

type volumes struct {
	mu sync.Mutex
	// sinks: sink name, then destination, then minute end.
	sinks map[string]map[string]map[time.Time]*sinkCount
	// ingest: project, then resource type, then minute end.
	ingest map[string]map[string]map[time.Time]int64
	pruned time.Time
}

func minuteEnd(t time.Time) time.Time {
	m := t.Truncate(time.Minute)
	if m.Equal(t) {
		return m
	}
	return m.Add(time.Minute)
}

func entryBytes(es []*loggingpb.LogEntry) int64 {
	var n int64
	for _, e := range es {
		n += int64(proto.Size(e))
	}
	return n
}

// prune drops counts past volumeRetention, at most once a minute. The
// caller holds v.mu.
func (v *volumes) prune(now time.Time) {
	if now.Sub(v.pruned) < time.Minute {
		return
	}
	v.pruned = now
	cut := now.Add(-volumeRetention)
	for _, byDest := range v.sinks {
		for _, byMin := range byDest {
			for t := range byMin {
				if t.Before(cut) {
					delete(byMin, t)
				}
			}
		}
	}
	for _, byType := range v.ingest {
		for _, byMin := range byType {
			for t := range byMin {
				if t.Before(cut) {
					delete(byMin, t)
				}
			}
		}
	}
}

// countSink records n entries of size bytes routed by sink, and failed
// entries that were not delivered.
func (s *Server) countSink(sink, dest string, entries, bytes, failed int64) {
	if entries == 0 && failed == 0 {
		return
	}
	now := s.clock()
	end := minuteEnd(now)
	v := &s.vol
	v.mu.Lock()
	defer v.mu.Unlock()
	v.prune(now)
	if v.sinks == nil {
		v.sinks = map[string]map[string]map[time.Time]*sinkCount{}
	}
	if v.sinks[sink] == nil {
		v.sinks[sink] = map[string]map[time.Time]*sinkCount{}
	}
	if v.sinks[sink][dest] == nil {
		v.sinks[sink][dest] = map[time.Time]*sinkCount{}
	}
	c := v.sinks[sink][dest][end]
	if c == nil {
		c = &sinkCount{}
		v.sinks[sink][dest][end] = c
	}
	c.entries += entries
	c.bytes += bytes
	c.errors += failed
}

// countIngestion records the entries written to log buckets: each stored
// entry once per bucket of a project that holds it, its own parent's
// _Default bucket unless hidden.
func (s *Server) countIngestion(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) {
	now := s.clock()
	end := minuteEnd(now)
	type key struct{ project, rtype string }
	add := map[key]int64{}
	for _, e := range es {
		size := int64(proto.Size(e))
		rtype := e.GetResource().GetType()
		if rtype == "" {
			rtype = "global"
		}
		r := routes[e]
		if own := entryParent(e); own.isProject() && (r == nil || !r.hidden) && !isRequiredLog(e) {
			add[key{own.id, rtype}] += size
		}
		if r == nil {
			continue
		}
		for _, b := range r.buckets {
			if p, ok := strings.CutPrefix(b, "projects/"); ok {
				p, _, _ = strings.Cut(p, "/")
				if !strings.HasSuffix(b, "/buckets/"+bucketRequired) {
					add[key{p, rtype}] += size
				}
			}
		}
	}
	if len(add) == 0 {
		return
	}
	v := &s.vol
	v.mu.Lock()
	defer v.mu.Unlock()
	v.prune(now)
	if v.ingest == nil {
		v.ingest = map[string]map[string]map[time.Time]int64{}
	}
	for k, n := range add {
		if v.ingest[k.project] == nil {
			v.ingest[k.project] = map[string]map[time.Time]int64{}
		}
		if v.ingest[k.project][k.rtype] == nil {
			v.ingest[k.project][k.rtype] = map[time.Time]int64{}
		}
		v.ingest[k.project][k.rtype][end] += n
	}
}

// SinkVolumes are what the sinks of parent (such as projects/P) routed in
// the minutes that end in (from, to].
func (s *Server) SinkVolumes(parent string, from, to time.Time) []SinkVolume {
	v := &s.vol
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []SinkVolume
	for sink, byDest := range v.sinks {
		if !strings.HasPrefix(sink, parent+"/sinks/") {
			continue
		}
		for dest, byMin := range byDest {
			for t, c := range byMin {
				if t.After(from) && !t.After(to) {
					out = append(out, SinkVolume{End: t, Sink: sink, Destination: dest, Bytes: c.bytes, Entries: c.entries, Errors: c.errors})
				}
			}
		}
	}
	return out
}

// Ingested is what project's log buckets took in the minutes that end in
// (from, to].
func (s *Server) Ingested(project string, from, to time.Time) []Ingestion {
	v := &s.vol
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []Ingestion
	for rtype, byMin := range v.ingest[project] {
		for t, n := range byMin {
			if t.After(from) && !t.After(to) {
				out = append(out, Ingestion{End: t, ResourceType: rtype, Bytes: n})
			}
		}
	}
	return out
}

// Now is the Logging clock, which the volumes are counted by.
func (s *Server) Now() time.Time { return s.clock() }

// sinkHit is one entry a sink routed to a log bucket or a project.
type sinkHit struct {
	sink, dest string
	e          *loggingpb.LogEntry
}

// countHits records what sinks routed to log buckets and projects.
func (s *Server) countHits(hits []sinkHit) {
	type key struct{ sink, dest string }
	n := map[key]*sinkCount{}
	for _, h := range hits {
		k := key{h.sink, "logging.googleapis.com/" + h.dest}
		if n[k] == nil {
			n[k] = &sinkCount{}
		}
		n[k].entries++
		n[k].bytes += int64(proto.Size(h.e))
	}
	for k, c := range n {
		s.countSink(k.sink, k.dest, c.entries, c.bytes, 0)
	}
}
