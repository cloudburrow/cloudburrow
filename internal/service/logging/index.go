package logging

// Custom indexes (#2108): a LogBucket's index_configs, as
// docs.cloud.google.com/logging/docs/analyze/custom-index documents them
// (read 2026-11-21) and logging_config.proto's IndexConfig:
//
//   - each is a field_path ("The LogEntry field path to index", for example
//     jsonPayload.request.status) and a type, INDEX_TYPE_STRING or
//     INDEX_TYPE_INTEGER; create_time is output only. "You can add up to 20
//     fields per custom index." A path LogEntry does not define, an
//     unspecified type, a path given twice and a 21st field are
//     INVALID_ARGUMENT (the code and wording are ours, UNVERIFIED (docs
//     re-read 2026-11-21: https://docs.cloud.google.com/logging/docs/analyze/custom-index)).
//     The proto says some paths "are not eligible for indexing" without
//     naming them, so every LogEntry path is taken;
//   - they are set by CreateBucket and by UpdateBucket's index_configs mask
//     path, the _Default bucket's too, and read back by GetBucket and
//     ListBuckets. An index kept across an update (the same path and type)
//     keeps its create_time;
//   - they change results, as the page says: "Enabling an index on a field
//     changes how you must query that field." A query that reads the bucket
//     (resource_names naming one of its views, or a parent for _Default)
//     compares an indexed field as the index's type (logfilter's
//     indexedCmp): with a string index jsonPayload.value > 20 is a string
//     comparison, with an integer index an integer one. An index applies an
//     hour after it is made ("you must wait for an hour for the changes to
//     apply to your queries", on the Logging clock) and only to entries
//     received after it was made ("changes to custom indexes don't apply to
//     logs retroactively"); elsewhere a comparison is the field's own.
//     What they do for speed has no counterpart here.

import (
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/logfilter"
)

// MaxIndexConfigs is how many fields a bucket's custom index may have.
const MaxIndexConfigs = 20

// indexDelay is how long an index takes to apply to queries.
const indexDelay = time.Hour

// checkIndexConfigs validates a bucket's index_configs and stamps their
// create_time: an index old already had (same path and type) keeps its own.
func checkIndexConfigs(ics, old []*loggingpb.IndexConfig, now time.Time) error {
	if len(ics) > MaxIndexConfigs {
		return apierror.InvalidArgument("index_configs: at most %d fields can be indexed, not %d", MaxIndexConfigs, len(ics))
	}
	seen := map[string]bool{}
	for i, ic := range ics {
		if ic.GetFieldPath() == "" {
			return apierror.InvalidArgument("index_configs[%d].field_path is required", i)
		}
		path, err := logfilter.CanonicalPath(ic.GetFieldPath())
		if err != nil {
			return apierror.InvalidArgument("index_configs[%d].field_path %q is not a LogEntry field: %v", i, ic.GetFieldPath(), err)
		}
		if ic.GetType() != loggingpb.IndexType_INDEX_TYPE_STRING && ic.GetType() != loggingpb.IndexType_INDEX_TYPE_INTEGER {
			return apierror.InvalidArgument("index_configs[%d].type must be INDEX_TYPE_STRING or INDEX_TYPE_INTEGER, not %s", i, ic.GetType())
		}
		if seen[path] {
			return apierror.InvalidArgument("index_configs[%d]: field_path %q is indexed twice", i, ic.GetFieldPath())
		}
		seen[path] = true
		ic.CreateTime = timestamppb.New(now)
		for _, o := range old {
			if o.GetFieldPath() == ic.GetFieldPath() && o.GetType() == ic.GetType() && o.GetCreateTime() != nil {
				ic.CreateTime = o.GetCreateTime()
			}
		}
	}
	return nil
}

// compileIndexed compiles a query of a bucket with custom indexes, at now.
func compileIndexed(filter string, ics []*loggingpb.IndexConfig, now time.Time) (pred, error) {
	byPath := map[string]logfilter.Index{}
	for _, ic := range ics {
		path, err := logfilter.CanonicalPath(ic.GetFieldPath())
		if err != nil {
			continue
		}
		made := ic.GetCreateTime().AsTime()
		if now.Before(made.Add(indexDelay)) {
			continue
		}
		t := logfilter.IndexString
		if ic.GetType() == loggingpb.IndexType_INDEX_TYPE_INTEGER {
			t = logfilter.IndexInteger
		}
		byPath[path] = logfilter.Index{Type: t, Applies: func(r logfilter.Record) bool {
			e := r.(*entryRecord).e
			at := e.GetReceiveTimestamp()
			if at == nil {
				at = e.GetTimestamp()
			}
			return !at.AsTime().Before(made)
		}}
	}
	if len(byPath) == 0 {
		return compile(filter)
	}
	f, err := logfilter.Parse(filter, logfilter.Options{
		Source: func(arg string) (func(logfilter.Record) bool, error) { return sourcePred(arg, nil) },
		Index: func(path string) (logfilter.Index, bool) {
			ix, ok := byPath[path]
			return ix, ok
		},
	})
	if err != nil {
		return nil, apierror.InvalidArgument("%s", err.Error())
	}
	if f == nil {
		return func(*loggingpb.LogEntry) bool { return true }, nil
	}
	return func(e *loggingpb.LogEntry) bool { return f.Match(&entryRecord{e: e}) }, nil
}

// matcher is the query's predicate for this source: match, or the query
// compiled against the bucket's custom indexes.
func (src source) matcher(filter string, match pred, now time.Time) (pred, error) {
	if len(src.indexes) == 0 {
		return match, nil
	}
	return compileIndexed(filter, src.indexes, now)
}
